package gateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// adapter reads one wire format into an input and writes a decision back into
// the request body. Rewrites go through typesafe.Fields so every key the
// gateway does not touch keeps its order and value.
type adapter interface {
	// toInput returns the normalized request, or a non-empty skip reason.
	toInput(body []byte, maxMessageChars int) (in input, model, skip string, err error)
	// apply rewrites body for d; false means the decision cannot be applied.
	apply(body *typesafe.Fields, d decision) bool
}

type messagesAdapter struct{}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type messagesRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type        *string `json:"type"`
		Name        string  `json:"name"`
		Description string  `json:"description"`
		InputSchema struct {
			Properties json.RawMessage `json:"properties"`
		} `json:"input_schema"`
	} `json:"tools"`
	ToolChoice *struct {
		Type string `json:"type"`
	} `json:"tool_choice"`
	Thinking *struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

func (messagesAdapter) toInput(body []byte, maxMessageChars int) (input, string, string, error) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return input{}, "", "", err
	}
	clip := func(s string) string { return truncate(s, maxMessageChars) }
	in := input{System: textOf(req.System), ToolChoice: choiceAuto, Steer: steerToolChoice}

	for _, t := range req.Tools {
		if t.Type == nil || *t.Type == "custom" {
			in.Tools = append(in.Tools, tool{Kind: kindFunction, Name: t.Name, Description: t.Description, Params: propertyNames(t.InputSchema.Properties)})
		} else {
			in.Tools = append(in.Tools, tool{Kind: kindHosted, Name: t.Name, Description: t.Description})
		}
	}

	names := map[string]string{}
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			in.Turns = append(in.Turns, textTurn(m.Role, clip(s)))
			continue
		}
		var blocks []messagesBlock
		_ = json.Unmarshal(m.Content, &blocks)
		var text []string
		flush := func() {
			if len(text) > 0 {
				in.Turns = append(in.Turns, textTurn(m.Role, clip(strings.Join(text, "\n"))))
				text = nil
			}
		}
		for _, b := range blocks {
			switch {
			case b.Type == "tool_use" || b.Type == "server_tool_use":
				flush()
				names[b.ID] = b.Name
				in.Turns = append(in.Turns, callTurn(b.Name, clip(string(b.Input))))
			case b.Type == "tool_result" || strings.HasSuffix(b.Type, "_tool_result"):
				flush()
				name := names[b.ToolUseID]
				if name == "" {
					name = "unknown"
				}
				in.Turns = append(in.Turns, resultTurn(name, clip(textOf(b.Content))))
			case b.Type == "thinking" || b.Type == "redacted_thinking":
			case b.Text != nil:
				text = append(text, *b.Text)
			case b.Type != "":
				text = append(text, "["+b.Type+"]")
			default:
				text = append(text, "[attachment]")
			}
		}
		flush()
	}

	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto":
		case "any":
			in.ToolChoice = choiceRequired
		default:
			in.ToolChoice = choiceDecided
		}
	}
	// Forcing tool_choice would invalidate the prompt cache and is rejected
	// with extended thinking, so those requests only get a hint.
	thinking := req.Thinking != nil && req.Thinking.Type != "" && req.Thinking.Type != "disabled"
	if thinking || bytes.Contains(body, []byte(`"cache_control"`)) {
		in.Steer = steerHint
	}
	return in, req.Model, "", nil
}

func hintText(name string) string {
	return `<system-reminder>A tool-routing model suggests the "` + name +
		`" tool is the most relevant next step. Ignore this if it does not fit what the user actually asked for.</system-reminder>`
}

func (messagesAdapter) apply(body *typesafe.Fields, d decision) bool {
	switch d.Mode {
	case modeForced:
		choice := typesafe.NewFields().Set("type", "tool").Set("name", d.Tool.Name)
		if old, ok := fieldsAt(body, "tool_choice"); ok {
			if v, ok := old.Get("disable_parallel_tool_use"); ok {
				choice.Set("disable_parallel_tool_use", v)
			}
		}
		body.Set("tool_choice", choice)
		return true
	case modeNone:
		body.Set("tool_choice", typesafe.NewFields().Set("type", "none"))
		return true
	case modeHint:
		v, _ := body.Get("messages")
		messages, _ := v.([]any)
		if len(messages) == 0 {
			return false
		}
		last, ok := messages[len(messages)-1].(*typesafe.Fields)
		if !ok {
			return false
		}
		// Claude Code may end the conversation with a system message after the
		// user turn; the reminder belongs there as well.
		if role, _ := last.Get("role"); role != "user" && role != "system" {
			return false
		}
		note := typesafe.NewFields().Set("type", "text").Set("text", hintText(d.Tool.Name))
		switch content, _ := last.Get("content"); c := content.(type) {
		case string:
			last.Set("content", []any{typesafe.NewFields().Set("type", "text").Set("text", c), note})
		case []any:
			last.Set("content", append(c, note))
		default:
			return false
		}
		return true
	}
	return false
}

// propertyNames lists a JSON Schema's property names in declaration order.
func propertyNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	v, err := typesafe.DecodeOrdered(raw)
	if err != nil {
		return nil
	}
	if f, ok := v.(*typesafe.Fields); ok {
		return f.Keys()
	}
	return nil
}

func fieldsAt(f *typesafe.Fields, key string) (*typesafe.Fields, bool) {
	v, ok := f.Get(key)
	if !ok {
		return nil, false
	}
	out, ok := v.(*typesafe.Fields)
	return out, ok
}
