package gateway

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
	"golang.org/x/sync/errgroup"
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
	// maxShards keeps the final question at most maxTools options (3 per shard).
	maxShards = maxTools / shortlistPerShard
	// verifyCount is how many top tools the second request re-checks, one noul each.
	verifyCount         = 3
	maxDescriptionChars = 1024
	// verifyDescriptionChars is the full-description cut for the verification nouls.
	verifyDescriptionChars = 4000
	// questionCharBudget is shared by the tool descriptions of one choice. With the
	// default state cap it keeps state plus the longest question under the API's
	// 32k tokens at typesafe.CharsPerToken.
	questionCharBudget   = 36000
	keyTool              = "tool"
	keyToolReversed      = "tool:reversed"
	keyNeedsTool         = "needs_tool"
	keyFitsPrefix        = "fits::"
	wantsToolFloor       = 0.3
	noToolCeiling        = 0.7
	shortlistInstruction = "Given the conversation, which of these tools would best advance the user's latest request if the assistant called it next?"
	toolInstruction      = "Given the conversation, what should the assistant do next? Pick the single tool whose call best advances the user's latest request."
	noToolCriterion      = "No tool call is needed right now: the assistant should reply to the user in plain text (answer directly, ask a clarifying question, or report results that tools already returned)."
	needsToolInstruction = "Does the assistant need to call one of its tools now, rather than reply to the user in plain text?"
	// fitsFloor drops a verified shortlist whose best tool fits less than this,
	// as in docs.typesafe.ai/cookbooks/skill_suggestion.
	fitsFloor = 0.3
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
	Fits             map[string]float64 `json:"fits,omitempty"`
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
	verify          bool
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
	case len(in.Tools) > maxTools*maxShards:
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
		f.Set(t.Name, describe(t, limit))
	}
	return f
}

// describe is a tool's description cut to limit runes, its parameter names
// when it has no description, or nil.
func describe(t tool, limit int) any {
	if d := strings.TrimSpace(t.Description); d != "" {
		r := []rune(d)
		return string(r[:min(len(r), limit)])
	}
	if len(t.Params) > 0 {
		return "Parameters: " + strings.Join(t.Params, ", ")
	}
	return nil
}

// reversed returns criteria with the keys in the opposite order.
func reversed(f *typesafe.Fields) *typesafe.Fields {
	keys := slices.Clone(f.Keys())
	slices.Reverse(keys)
	out := typesafe.NewFields()
	for _, k := range keys {
		v, _ := f.Get(k)
		out.Set(k, v)
	}
	return out
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

	// jev-1.13 leans toward the first option (docs.typesafe.ai jaggedness), so the
	// same choice is asked in both orders and must agree. no_tool_needed goes first
	// in the main order, where any lean favors leaving the request alone.
	criteria := typesafe.NewFields()
	if in.ToolChoice != choiceRequired {
		criteria.Set(noTool, noToolCriterion)
	}
	criteria = withTools(criteria, candidates)
	questions := typesafe.NewFields().
		Set(keyTool, typesafe.Choice(toolInstruction, criteria)).
		Set(keyToolReversed, typesafe.Choice(toolInstruction, reversed(criteria))).
		Set(keyNeedsTool, typesafe.Noul(needsToolInstruction, nil, nil))
	resp, err := r.jev.SystemOne(ctx, typesafe.Request{State: state, Model: r.model, Questions: questions})
	if err != nil {
		return fail("jev_error", err)
	}
	rec.Model = resp.Model
	rec.InputTokens += resp.Usage.InputTokens
	rec.LatencyMs = time.Since(start).Milliseconds()
	toolAns, ok1 := resp.Answers[keyTool]
	revAns, ok2 := resp.Answers[keyToolReversed]
	needsAns, ok3 := resp.Answers[keyNeedsTool]
	if !ok1 || !ok2 || !ok3 || toolAns.Type != "choice" || revAns.Type != "choice" || needsAns.Type != "noul" || toolAns.Choice == "" {
		return passthrough("jev_unexpected_answer", rec)
	}
	probs := map[string]float64{}
	for _, k := range criteria.Keys() {
		probs[k] = (toolAns.Probabilities[k] + revAns.Probabilities[k]) / 2
	}
	rec.Choice, rec.Confidence, rec.NeedsTool = toolAns.Choice, min(toolAns.Confidence, revAns.Confidence), needsAns.Noul
	rec.TopProbabilities = top(probs, 3)

	wantsTool := toolAns.Choice != noTool
	switch {
	case revAns.Choice != toolAns.Choice:
		return passthrough("jev_order_disagrees", rec)
	case rec.Confidence < r.minConfidence:
		return passthrough("low_confidence", rec)
	case wantsTool && needsAns.Noul < wantsToolFloor, !wantsTool && needsAns.Noul > noToolCeiling:
		return passthrough("jev_answers_disagree", rec)
	case !wantsTool:
		// tool_choice none would also break the prompt cache, so hint requests keep theirs.
		// Before the first tool call the agent has not looked at anything yet, and
		// forcing none there made Codex answer without reading the workspace.
		if r.forceNone && in.Steer != steerHint && usedTools(in.Turns) {
			return decision{Mode: modeNone, Jev: rec}
		}
		return passthrough(noTool, rec)
	}
	if r.verify {
		winner, reason, err := r.verifyTop(ctx, state, candidates, probs, rec)
		rec.LatencyMs = time.Since(start).Milliseconds()
		switch {
		case err != nil:
			return fail("jev_error", err)
		case reason != "":
			return passthrough(reason, rec)
		}
		rec.Choice = winner
	}
	var picked *tool
	for i := range candidates {
		if candidates[i].Name == rec.Choice {
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

// withTools appends the tool descriptions to criteria.
func withTools(criteria *typesafe.Fields, tools []tool) *typesafe.Fields {
	tc := toolCriteria(tools)
	for _, k := range tc.Keys() {
		v, _ := tc.Get(k)
		criteria.Set(k, v)
	}
	return criteria
}

// verifyTop re-checks the best tools of the first answer, one noul each with
// the full description, in the two-step shape of
// docs.typesafe.ai/cookbooks/skill_suggestion. Nouls are answered on their own,
// so they carry no order bias and can all reject. It returns the tool that fits
// best, or a passthrough reason.
func (r *router) verifyTop(ctx context.Context, state jevState, candidates []tool, probs map[string]float64, rec *jevRecord) (string, string, error) {
	byName := map[string]tool{}
	for _, t := range candidates {
		byName[t.Name] = t
	}
	ranked := map[string]float64{}
	for k, p := range probs {
		if _, ok := byName[k]; ok {
			ranked[k] = p
		}
	}
	questions := typesafe.NewFields()
	for _, name := range sortedKeys(ranked, verifyCount) {
		instr := fmt.Sprintf("Would calling the tool %q now do the specific thing the user's latest request needs next?", name)
		if d := describe(byName[name], verifyDescriptionChars); d != nil {
			instr += " It is described as: " + fmt.Sprint(d)
		}
		questions.Set(keyFitsPrefix+name, typesafe.Noul(instr, nil, nil))
	}
	resp, err := r.jev.SystemOne(ctx, typesafe.Request{State: state, Model: r.model, Questions: questions})
	if err != nil {
		return "", "", err
	}
	rec.InputTokens += resp.Usage.InputTokens
	rec.Fits = map[string]float64{}
	for _, k := range questions.Keys() {
		ans, ok := resp.Answers[k]
		if !ok || ans.Type != "noul" {
			return "", "jev_unexpected_answer", nil
		}
		rec.Fits[strings.TrimPrefix(k, keyFitsPrefix)] = ans.Noul
	}
	best := sortedKeys(rec.Fits, 1)
	if len(best) == 0 || rec.Fits[best[0]] < fitsFloor {
		return "", "jev_verify_rejected", nil
	}
	return best[0], "", nil
}

// shortlist splits a large tool set into shards of at most maxTools and keeps
// the best tools of each. Shards are packed into as few requests as the API's
// token limit allows, and the requests run in parallel.
func (r *router) shortlist(ctx context.Context, state jevState, tools []tool, rec *jevRecord) ([]tool, error) {
	shards := int(math.Ceil(float64(len(tools)) / maxTools))
	size := int(math.Ceil(float64(len(tools)) / float64(shards)))
	var groups [][]tool
	for i := 0; i < len(tools); i += size {
		groups = append(groups, tools[i:min(len(tools), i+size)])
	}
	room := typesafe.MaxRequestTokens - typesafe.ApproxTokens(state)
	var batches []*typesafe.Fields
	used := 0
	for i, group := range groups {
		criteria := toolCriteria(group).Set(noneOfThese, "None of the tools in this list fits the next step.")
		q := typesafe.Choice(shortlistInstruction, criteria)
		cost := typesafe.ApproxTokens(q) + 16
		if len(batches) == 0 || used+cost > room {
			batches = append(batches, typesafe.NewFields())
			used = 0
		}
		batches[len(batches)-1].Set(fmt.Sprintf("shard:%d", i), q)
		used += cost
	}
	answers := make([]map[string]typesafe.Answer, len(batches))
	tokens := make([]int, len(batches))
	g, gctx := errgroup.WithContext(ctx)
	for i, questions := range batches {
		g.Go(func() error {
			resp, err := r.jev.SystemOne(gctx, typesafe.Request{State: state, Model: r.model, Questions: questions})
			if err != nil {
				return err
			}
			answers[i], tokens[i] = resp.Answers, resp.Usage.InputTokens
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	all := map[string]typesafe.Answer{}
	for i := range batches {
		rec.InputTokens += tokens[i]
		for k, v := range answers[i] {
			all[k] = v
		}
	}
	var out []tool
	for i, group := range groups {
		ans := all[fmt.Sprintf("shard:%d", i)]
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

// usedTools reports whether the conversation already holds a tool call or result.
func usedTools(turns []turn) bool {
	for _, t := range turns {
		if len(t.ToolCalls) > 0 || t.Role == "tool_result" {
			return true
		}
	}
	return false
}
