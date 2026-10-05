package gateway

import (
	"bytes"
	"encoding/json"
)

// llmUsage is the upstream LLM's token count for one request. Input includes
// cached and cache-write tokens.
type llmUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Cached     int `json:"cached,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
}

// maxJSONBody caps how much of a non-streamed response is kept for usage.
const maxJSONBody = 4 << 20

// usageTap watches a response body as it streams to the client and keeps
// only what it needs: SSE data lines that mention usage, or a bounded JSON body.
type usageTap struct {
	line  []byte
	kept  [][]byte
	whole []byte
	sse   *bool
	over  bool
}

func (t *usageTap) Write(p []byte) (int, error) {
	n := len(p)
	if t.sse == nil {
		trimmed := bytes.TrimLeft(p, " \t\r\n")
		if len(trimmed) == 0 {
			return n, nil
		}
		sse := trimmed[0] != '{' && trimmed[0] != '['
		t.sse = &sse
	}
	if !*t.sse {
		if len(t.whole)+len(p) > maxJSONBody {
			t.over = true
		} else {
			t.whole = append(t.whole, p...)
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(t.line) < 1<<20 {
				t.line = append(t.line, p...)
			}
			break
		}
		t.line = append(t.line, p[:i]...)
		t.keepLine()
		p = p[i+1:]
	}
	return n, nil
}

func (t *usageTap) keepLine() {
	line := bytes.TrimRight(t.line, "\r")
	if bytes.HasPrefix(line, []byte("data:")) && bytes.Contains(line, []byte(`"usage`)) {
		t.kept = append(t.kept, append([]byte(nil), bytes.TrimSpace(line[5:])...))
	}
	t.line = t.line[:0]
}

// usage merges the usage objects seen, field by field, taking the largest:
// Anthropic reports input at message_start and output at message_delta.
func (t *usageTap) usage() *llmUsage {
	if len(t.line) > 0 {
		t.keepLine()
	}
	var docs [][]byte
	if t.sse != nil && *t.sse {
		docs = t.kept
	} else if !t.over && len(t.whole) > 0 {
		docs = [][]byte{t.whole}
	}
	var out *llmUsage
	for _, d := range docs {
		u := parseUsage(d)
		if u == nil {
			continue
		}
		if out == nil {
			out = &llmUsage{}
		}
		out.Input = max(out.Input, u.Input)
		out.Output = max(out.Output, u.Output)
		out.Cached = max(out.Cached, u.Cached)
		out.CacheWrite = max(out.CacheWrite, u.CacheWrite)
		out.Reasoning = max(out.Reasoning, u.Reasoning)
	}
	return out
}

type rawUsage struct {
	InputTokens        *int `json:"input_tokens"`
	OutputTokens       int  `json:"output_tokens"`
	CacheRead          *int `json:"cache_read_input_tokens"`
	CacheCreation      *int `json:"cache_creation_input_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// parseUsage reads usage from the document root, response (Responses API
// events) or message (Anthropic message_start).
func parseUsage(doc []byte) *llmUsage {
	var d struct {
		Usage    *rawUsage `json:"usage"`
		Response *struct {
			Usage *rawUsage `json:"usage"`
		} `json:"response"`
		Message *struct {
			Usage *rawUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return nil
	}
	u := d.Usage
	if u == nil && d.Response != nil {
		u = d.Response.Usage
	}
	if u == nil && d.Message != nil {
		u = d.Message.Usage
	}
	if u == nil {
		return nil
	}
	in := 0
	if u.InputTokens != nil {
		in = *u.InputTokens
	}
	if u.CacheRead != nil || u.CacheCreation != nil {
		read, write := deref(u.CacheRead), deref(u.CacheCreation)
		return &llmUsage{Input: in + read + write, Output: u.OutputTokens, Cached: read, CacheWrite: write}
	}
	return &llmUsage{
		Input:     in,
		Output:    u.OutputTokens,
		Cached:    u.InputTokensDetails.CachedTokens,
		Reasoning: u.OutputTokensDetails.ReasoningTokens,
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
