package gateway

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// Modes a routed request ends in.
const (
	modePassthrough = "passthrough"
	modeHint        = "hint"
	modeForced      = "forced"
	modeNone        = "none"
)

// Reserved choices in Jev's questions.
const (
	noTool      = "no_tool_needed"
	noneOfThese = "none_of_these"
)

const (
	// maxTools is the most tools one choice question offers; larger sets are shortlisted.
	maxTools = 120
	// shortlistPerShard is how many tools each shard passes to the final question.
	shortlistPerShard = 3
	// maxCriteria bounds shortlisting to one request of at most this many shards.
	maxCriteria          = 255
	maxDescriptionChars  = 1024
	questionCharBudget   = 48000
	keyTool              = "tool"
	keyNeedsTool         = "needs_tool"
	wantsToolFloor       = 0.3
	noToolCeiling        = 0.7
	shortlistInstruction = "Given the conversation, which of these tools would best advance the user's latest request if the assistant called it next?"
	toolInstruction      = "Given the conversation, what should the assistant do next? Pick the single tool whose call best advances the user's latest request."
	noToolCriterion      = "No tool call is needed right now: the assistant should reply to the user in plain text (answer directly, ask a clarifying question, or report results that tools already returned)."
	needsToolInstruction = "Does the assistant need to call one of its tools now, rather than reply to the user in plain text?"
)

var safeToolName = regexp.MustCompile(`^[\p{L}\p{N}_.:/-]{1,128}$`)

// jevRecord is what Jev answered, kept on the event for the dashboard.
type jevRecord struct {
	Choice           string             `json:"choice,omitempty"`
	Confidence       float64            `json:"confidence,omitempty"`
	NeedsTool        float64            `json:"needsTool,omitempty"`
	TopProbabilities map[string]float64 `json:"topProbabilities,omitempty"`
	Model            string             `json:"model,omitempty"`
	InputTokens      int                `json:"inputTokens,omitempty"`
	LatencyMs        int64              `json:"latencyMs"`
	Shortlist        []string           `json:"shortlist,omitempty"`
	Error            string             `json:"error,omitempty"`
}

// decision is how a request leaves the router.
type decision struct {
	Mode   string
	Reason string
	Tool   *tool
	Jev    *jevRecord
}

func passthrough(reason string, rec *jevRecord) decision {
	return decision{Mode: modePassthrough, Reason: reason, Jev: rec}
}

// jevClient is the part of typesafe.Client the router uses; tests fake it.
type jevClient interface {
	SystemOne(ctx context.Context, req typesafe.Request) (*typesafe.Response, error)
}

// router asks Jev about a normalized request.
type router struct {
	jev             jevClient
	model           string
	minConfidence   float64
	forceNone       bool
	budget          time.Duration
	maxStateChars   int
	maxMessageChars int
}

// skipReason says why a request cannot be routed at all, or "".
func skipReason(in input) string {
	switch {
	case len(in.Turns) == 0:
		return "no_messages"
	case len(in.Tools) == 0:
		return "no_tools"
	case len(in.Tools) > maxTools*maxCriteria:
		return "too_many_tools"
	}
	seen := map[string]bool{}
	for _, t := range in.Tools {
		switch {
		case !safeToolName.MatchString(t.Name):
			return "unsafe_tool_name"
		case seen[t.Name]:
			return "duplicate_tool_names"
		case t.Name == noTool || t.Name == noneOfThese:
			return "reserved_tool_name"
		}
		seen[t.Name] = true
	}
	if in.ToolChoice == choiceDecided {
		return "tool_choice_already_decided"
	}
	return ""
}

// toolCriteria describes each tool in a choice question, sharing the question
// budget so a long tool list does not crowd out the conversation.
func toolCriteria(tools []tool) *typesafe.Fields {
	limit := min(maxDescriptionChars, questionCharBudget/max(1, len(tools)))
	f := typesafe.NewFields()
	for _, t := range tools {
		if d := strings.TrimSpace(t.Description); d != "" {
			r := []rune(d)
			f.Set(t.Name, string(r[:min(len(r), limit)]))
		} else if len(t.Params) > 0 {
			f.Set(t.Name, "Parameters: "+strings.Join(t.Params, ", "))
		} else {
			f.Set(t.Name, nil)
		}
	}
	return f
}

// decide asks Jev which tool comes next and turns the answer into a decision.
// Any Jev failure ends in passthrough: the gateway never fails a request.
func (r *router) decide(ctx context.Context, in input) decision {
	if reason := skipReason(in); reason != "" {
		return passthrough(reason, nil)
	}
	ctx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
	state := buildState(in, r.maxStateChars, r.maxMessageChars)
	start := time.Now()
	rec := &jevRecord{}
	fail := func(reason string, err error) decision {
		rec.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			rec.Error = err.Error()
		}
		return passthrough(reason, rec)
	}

	candidates := in.Tools
	if len(candidates) > maxTools {
		short, err := r.shortlist(ctx, state, candidates, rec)
		if err != nil {
			return fail("jev_error", err)
		}
		if len(short) == 0 {
			return fail("jev_unexpected_answer", nil)
		}
		candidates = short
		for _, t := range short {
			rec.Shortlist = append(rec.Shortlist, t.Name)
		}
	}

	criteria := toolCriteria(candidates)
	if in.ToolChoice != choiceRequired {
		criteria.Set(noTool, noToolCriterion)
	}
	questions := typesafe.NewFields().
		Set(keyTool, typesafe.Choice(toolInstruction, criteria)).
		Set(keyNeedsTool, typesafe.Noul(needsToolInstruction, nil, nil))
	resp, err := r.jev.SystemOne(ctx, typesafe.Request{State: state, Model: r.model, Questions: questions})
	if err != nil {
		return fail("jev_error", err)
	}
	rec.Model = resp.Model
	rec.InputTokens += resp.Usage.InputTokens
	rec.LatencyMs = time.Since(start).Milliseconds()
	toolAns, ok1 := resp.Answers[keyTool]
	needsAns, ok2 := resp.Answers[keyNeedsTool]
	if !ok1 || !ok2 || toolAns.Type != "choice" || needsAns.Type != "noul" || toolAns.Choice == "" {
		return passthrough("jev_unexpected_answer", rec)
	}
	rec.Choice, rec.Confidence, rec.NeedsTool = toolAns.Choice, toolAns.Confidence, needsAns.Noul
	rec.TopProbabilities = top(toolAns.Probabilities, 3)

	wantsTool := toolAns.Choice != noTool
	switch {
	case toolAns.Confidence < r.minConfidence:
		return passthrough("low_confidence", rec)
	case wantsTool && needsAns.Noul < wantsToolFloor, !wantsTool && needsAns.Noul > noToolCeiling:
		return passthrough("jev_answers_disagree", rec)
	case !wantsTool:
		// tool_choice none would also break the prompt cache, so hint requests keep theirs.
		if r.forceNone && in.Steer != steerHint {
			return decision{Mode: modeNone, Jev: rec}
		}
		return passthrough(noTool, rec)
	}
	var picked *tool
	for i := range candidates {
		if candidates[i].Name == toolAns.Choice {
			picked = &candidates[i]
			break
		}
	}
	switch {
	case picked == nil:
		return passthrough("jev_unknown_tool", rec)
	case picked.Kind == kindHosted:
		return decision{Mode: modePassthrough, Reason: "hosted_tool_selected", Tool: picked, Jev: rec}
	case picked.Namespace != "":
		return decision{Mode: modePassthrough, Reason: "namespaced_tool_selected", Tool: picked, Jev: rec}
	case in.Steer == steerHint:
		return decision{Mode: modeHint, Tool: picked, Jev: rec}
	}
	return decision{Mode: modeForced, Tool: picked, Jev: rec}
}

// shortlist splits a large tool set into shards of at most maxTools, asks for
// the best tools of every shard in one request, and keeps the top of each.
func (r *router) shortlist(ctx context.Context, state jevState, tools []tool, rec *jevRecord) ([]tool, error) {
	shards := int(math.Ceil(float64(len(tools)) / maxTools))
	size := int(math.Ceil(float64(len(tools)) / float64(shards)))
	questions := typesafe.NewFields()
	var groups [][]tool
	for i := 0; i < len(tools); i += size {
		group := tools[i:min(len(tools), i+size)]
		groups = append(groups, group)
		criteria := toolCriteria(group).Set(noneOfThese, "None of the tools in this list fits the next step.")
		questions.Set(fmt.Sprintf("shard:%d", len(groups)-1), typesafe.Choice(shortlistInstruction, criteria))
	}
	resp, err := r.jev.SystemOne(ctx, typesafe.Request{State: state, Model: r.model, Questions: questions})
	if err != nil {
		return nil, err
	}
	rec.InputTokens += resp.Usage.InputTokens
	var out []tool
	for i, group := range groups {
		ans := resp.Answers[fmt.Sprintf("shard:%d", i)]
		byName := map[string]tool{}
		for _, t := range group {
			byName[t.Name] = t
		}
		probs := map[string]float64{}
		for name, p := range ans.Probabilities {
			if _, ok := byName[name]; ok {
				probs[name] = p
			}
		}
		for _, name := range sortedKeys(probs, shortlistPerShard) {
			out = append(out, byName[name])
		}
	}
	return out, nil
}

// sortedKeys returns up to n keys by descending value, ties by name.
func sortedKeys(m map[string]float64, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys[:min(n, len(keys))]
}

func top(m map[string]float64, n int) map[string]float64 {
	out := map[string]float64{}
	for _, k := range sortedKeys(m, n) {
		out[k] = m[k]
	}
	return out
}
