package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bragamat/jevkit/internal/typesafe"
	"github.com/klauspost/compress/zstd"
)

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	long := strings.Repeat("a", 50) + strings.Repeat("z", 50)
	got := truncate(long, 40)
	if utf8.RuneCountInString(got) != 40 || !strings.Contains(got, truncMarker) {
		t.Fatalf("truncate(100→40) = %q (%d runes)", got, utf8.RuneCountInString(got))
	}
	// 40 - 15 marker runes = 25 kept: 15 head, 10 tail.
	if !strings.HasPrefix(got, strings.Repeat("a", 15)+truncMarker) || !strings.HasSuffix(got, truncMarker+strings.Repeat("z", 10)) {
		t.Fatalf("head/tail split wrong: %q", got)
	}
	multi := strings.Repeat("ação🙂", 40)
	if got := truncate(multi, 30); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 30 {
		t.Fatalf("multibyte truncate broke the text: %q", got)
	}
}

func TestTextOf(t *testing.T) {
	cases := map[string]string{
		`"plain"`: "plain",
		`[{"type":"text","text":"a"},{"type":"image"},{"source":{}}]`: "a\n[image]\n[attachment]",
		`null`:    "",
		`{"x":1}`: `{"x":1}`,
	}
	for in, want := range cases {
		if got := textOf(json.RawMessage(in)); got != want {
			t.Errorf("textOf(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildStateKeepsNewestTurns(t *testing.T) {
	var turns []turn
	for i := range 10 {
		turns = append(turns, textTurn("user", fmt.Sprintf("%02d %s", i, strings.Repeat("x", 80))))
	}
	s := buildState(input{System: "sys", Turns: turns}, 400, 4000)
	if s.EarlierTurnsOmitted == 0 || len(s.Conversation)+s.EarlierTurnsOmitted != 10 {
		t.Fatalf("omitted %d, kept %d", s.EarlierTurnsOmitted, len(s.Conversation))
	}
	if !strings.HasPrefix(*s.Conversation[len(s.Conversation)-1].Text, "09") {
		t.Fatalf("newest turn not kept last")
	}
	// The newest turn survives even when it alone exceeds the budget.
	s = buildState(input{Turns: turns}, 10, 4000)
	if len(s.Conversation) != 1 || s.EarlierTurnsOmitted != 9 {
		t.Fatalf("over-budget: kept %d", len(s.Conversation))
	}
}

const claudeBody = `{"model":"claude-opus-5-5","max_tokens":100,"system":[{"type":"text","text":"You are Claude Code."}],` +
	`"messages":[{"role":"user","content":"list the files"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"secret"},{"type":"text","text":"Sure."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.go\nb.go"},{"type":"text","text":"now read a.go <b>"},{"cache_control":{"type":"ephemeral"}}]}],` +
	`"tools":[{"name":"Bash","description":"Run a shell command","input_schema":{"type":"object","properties":{"command":{},"timeout":{}}}},` +
	`{"name":"Read","description":"","input_schema":{"properties":{"file_path":{},"offset":{}}}},` +
	`{"type":"web_search_20250305","name":"web_search"}],"stream":true}`

func TestMessagesToInput(t *testing.T) {
	in, model, skip, err := messagesAdapter{}.toInput([]byte(claudeBody), 4000)
	if err != nil || skip != "" || model != "claude-opus-5-5" {
		t.Fatalf("toInput: model=%q skip=%q err=%v", model, skip, err)
	}
	if in.System != "You are Claude Code." || in.Steer != steerHint || in.ToolChoice != choiceAuto {
		t.Fatalf("system/steer/choice = %q %q %q", in.System, in.Steer, in.ToolChoice)
	}
	if len(in.Tools) != 3 || in.Tools[1].Params[0] != "file_path" || in.Tools[2].Kind != kindHosted {
		t.Fatalf("tools = %+v", in.Tools)
	}
	got, _ := json.Marshal(in.Turns)
	want := `[{"role":"user","text":"list the files"},{"role":"assistant","text":"Sure."},` +
		`{"role":"assistant","tool_calls":[{"tool":"Bash","arguments":"{\"command\":\"ls\"}"}]},` +
		`{"role":"tool_result","tool":"Bash","content":"a.go\nb.go"},{"role":"user","text":"now read a.go \u003cb\u003e\n[attachment]"}]`
	if string(got) != want {
		t.Fatalf("turns\n got %s\nwant %s", got, want)
	}
}

func TestMessagesBlockWithoutTypeDoesNotCrash(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{},{"type":null},{"text":"hi"}]}],"tools":[{"name":"x"}]}`
	in, _, _, err := messagesAdapter{}.toInput([]byte(body), 100)
	if err != nil || *in.Turns[0].Text != "[attachment]\n[attachment]\nhi" {
		t.Fatalf("got %+v, %v", in.Turns, err)
	}
}

func applyTo(t *testing.T, a adapter, body string, d decision) string {
	t.Helper()
	v, err := typesafe.DecodeOrdered([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	f := v.(*typesafe.Fields)
	if !a.apply(f, d) {
		return ""
	}
	out, err := f.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestMessagesApply(t *testing.T) {
	bash := &tool{Name: "Bash", Kind: kindFunction}
	got := applyTo(t, messagesAdapter{}, `{"model":"m","tool_choice":{"type":"auto","disable_parallel_tool_use":true},"n":1.50,"messages":[]}`,
		decision{Mode: modeForced, Tool: bash})
	want := `{"model":"m","tool_choice":{"type":"tool","name":"Bash","disable_parallel_tool_use":true},"n":1.50,"messages":[]}`
	if got != want {
		t.Fatalf("forced\n got %s\nwant %s", got, want)
	}
	got = applyTo(t, messagesAdapter{}, `{"messages":[{"role":"user","content":"a <b>"}]}`, decision{Mode: modeHint, Tool: bash})
	if !strings.Contains(got, `[{"type":"text","text":"a <b>"},{"type":"text","text":"<system-reminder>A tool-routing model suggests the \"Bash\" tool`) {
		t.Fatalf("hint on string content: %s", got)
	}
	got = applyTo(t, messagesAdapter{}, `{"messages":[{"role":"user","content":"a"},{"role":"system","content":[{"type":"text","text":"ctx"}]}]}`, decision{Mode: modeHint, Tool: bash})
	if !strings.Contains(got, `{"role":"system","content":[{"type":"text","text":"ctx"},{"type":"text","text":"<system-reminder>`) {
		t.Fatalf("hint on trailing system message: %s", got)
	}
	if applyTo(t, messagesAdapter{}, `{"messages":[{"role":"assistant","content":"x"}]}`, decision{Mode: modeHint, Tool: bash}) != "" {
		t.Fatalf("hint applied without a user turn last")
	}
}

const codexBody = `{"model":"gpt-5.5-codex","instructions":"You are Codex.","input":[` +
	`{"type":"message","role":"developer","content":[{"type":"input_text","text":"sandbox: workspace-write"}]},` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"run the tests"}]},` +
	`{"type":"reasoning","encrypted_content":"zzz"},` +
	`{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"go test\"}","call_id":"c1"},` +
	`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
	`{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp__docs","description":"Docs server","tools":[{"type":"function","name":"search","description":"Search docs"}]}]}],` +
	`"tools":[{"type":"function","name":"shell","description":"Runs a command","parameters":{"properties":{"cmd":{}}}},` +
	`{"type":"custom","name":"apply_patch","description":"Edit files"},{"type":"web_search"},` +
	`{"type":"namespace","name":"functions","tools":[{"type":"function","name":"update_plan"}]}],"tool_choice":"auto","stream":true}`

func TestResponsesToInput(t *testing.T) {
	in, model, skip, err := responsesAdapter{}.toInput([]byte(codexBody), 4000)
	if err != nil || skip != "" || model != "gpt-5.5-codex" {
		t.Fatalf("toInput: %q %q %v", model, skip, err)
	}
	if in.System != "You are Codex.\n\nsandbox: workspace-write" || in.Steer != steerToolChoice || in.ToolChoice != choiceAuto {
		t.Fatalf("system/steer/choice = %q %q %q", in.System, in.Steer, in.ToolChoice)
	}
	var names []string
	for _, tl := range in.Tools {
		names = append(names, tl.Kind+":"+tl.Name)
	}
	if got := strings.Join(names, ","); got != "function:shell,custom:apply_patch,hosted:web_search,function:update_plan,function:mcp__docs.search" {
		t.Fatalf("tools = %s", got)
	}
	if d := in.Tools[4].Description; d != "[Docs server] Search docs" || in.Tools[4].Namespace != "mcp__docs" {
		t.Fatalf("namespaced tool = %+v", in.Tools[4])
	}
	got, _ := json.Marshal(in.Turns)
	want := `[{"role":"user","text":"run the tests"},{"role":"assistant","tool_calls":[{"tool":"shell","arguments":"{\"cmd\":\"go test\"}"}]},{"role":"tool_result","tool":"shell","content":"ok"}]`
	if string(got) != want {
		t.Fatalf("turns\n got %s\nwant %s", got, want)
	}

	_, _, skip, _ = responsesAdapter{}.toInput([]byte(`{"previous_response_id":"r1","input":"hi"}`), 100)
	if skip != "previous_response_id" {
		t.Fatalf("skip = %q", skip)
	}
	in, _, _, _ = responsesAdapter{}.toInput([]byte(`{"input":"hi","tool_choice":{"type":"function","name":"x"}}`), 100)
	if in.ToolChoice != choiceDecided || *in.Turns[0].Text != "hi" {
		t.Fatalf("string input / decided choice: %+v", in)
	}
	got2 := applyTo(t, responsesAdapter{}, `{"model":"m","tool_choice":"auto"}`, decision{Mode: modeForced, Tool: &tool{Name: "apply_patch", Kind: kindCustom}})
	if got2 != `{"model":"m","tool_choice":{"type":"custom","name":"apply_patch"}}` {
		t.Fatalf("forced custom: %s", got2)
	}
}

// fakeJev answers System One requests from a function and records them.
type fakeJev struct {
	mu    sync.Mutex
	calls []typesafe.Request
	fn    func(req typesafe.Request) (*typesafe.Response, error)
}

func (f *fakeJev) SystemOne(_ context.Context, req typesafe.Request) (*typesafe.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.fn(req)
}

func answer(choice string, conf, needs float64) func(typesafe.Request) (*typesafe.Response, error) {
	return func(typesafe.Request) (*typesafe.Response, error) {
		return &typesafe.Response{Model: "jev-test", Usage: typesafe.Usage{InputTokens: 100}, Answers: map[string]typesafe.Answer{
			keyTool:         {Type: "choice", Choice: choice, Confidence: conf, Probabilities: map[string]float64{choice: conf}},
			keyToolReversed: {Type: "choice", Choice: choice, Confidence: conf, Probabilities: map[string]float64{choice: conf}},
			keyNeedsTool:    {Type: "noul", Noul: needs},
		}}, nil
	}
}

func newRouter(fn func(typesafe.Request) (*typesafe.Response, error)) (*router, *fakeJev) {
	jev := &fakeJev{fn: fn}
	return &router{jev: jev, minConfidence: 0.7, forceNone: true, budget: time.Second, maxStateChars: 60000, maxMessageChars: 4000}, jev
}

func TestDecide(t *testing.T) {
	tools := []tool{{Kind: kindFunction, Name: "Bash"}, {Kind: kindHosted, Name: "web_search"}, {Kind: kindFunction, Name: "mcp.x", Namespace: "mcp"}}
	base := input{Turns: []turn{textTurn("user", "hi")}, Tools: tools, ToolChoice: choiceAuto, Steer: steerToolChoice}
	hint := base
	hint.Steer = steerHint
	cases := []struct {
		name      string
		in        input
		fn        func(typesafe.Request) (*typesafe.Response, error)
		mode, why string
	}{
		{"forced", base, answer("Bash", 0.9, 0.9), modeForced, ""},
		{"hint", hint, answer("Bash", 0.9, 0.9), modeHint, ""},
		{"low confidence", base, answer("Bash", 0.5, 0.9), modePassthrough, "low_confidence"},
		{"disagree", base, answer("Bash", 0.9, 0.1), modePassthrough, "jev_answers_disagree"},
		{"none forced", base, answer(noTool, 0.9, 0.1), modeNone, ""},
		{"none with hint keeps cache", hint, answer(noTool, 0.9, 0.1), modePassthrough, noTool},
		{"unknown", base, answer("Nope", 0.9, 0.9), modePassthrough, "jev_unknown_tool"},
		{"hosted", base, answer("web_search", 0.9, 0.9), modePassthrough, "hosted_tool_selected"},
		{"namespaced", base, answer("mcp.x", 0.9, 0.9), modePassthrough, "namespaced_tool_selected"},
		{"error", base, func(typesafe.Request) (*typesafe.Response, error) { return nil, errors.New("boom") }, modePassthrough, "jev_error"},
		{"no tools", input{Turns: base.Turns}, nil, modePassthrough, "no_tools"},
		{"decided", input{Turns: base.Turns, Tools: tools, ToolChoice: choiceDecided}, nil, modePassthrough, "tool_choice_already_decided"},
		{"reserved", input{Turns: base.Turns, Tools: []tool{{Name: noTool}}}, nil, modePassthrough, "reserved_tool_name"},
		{"unsafe", input{Turns: base.Turns, Tools: []tool{{Name: "a b"}}}, nil, modePassthrough, "unsafe_tool_name"},
	}
	for _, c := range cases {
		r, _ := newRouter(c.fn)
		d := r.decide(context.Background(), c.in)
		if d.Mode != c.mode || d.Reason != c.why {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, d.Mode, d.Reason, c.mode, c.why)
		}
	}
}

func TestDecideOrderCheck(t *testing.T) {
	r, jev := newRouter(func(req typesafe.Request) (*typesafe.Response, error) {
		resp, _ := answer("Bash", 0.9, 0.9)(req)
		resp.Answers[keyToolReversed] = typesafe.Answer{Type: "choice", Choice: "Read", Confidence: 0.9}
		return resp, nil
	})
	d := r.decide(context.Background(), input{Turns: []turn{textTurn("user", "x")}, Tools: []tool{{Name: "Bash"}, {Name: "Read"}}})
	if d.Reason != "jev_order_disagrees" {
		t.Fatalf("got %s/%s", d.Mode, d.Reason)
	}
	q, _ := jev.calls[0].Questions.Get(keyTool)
	qr, _ := jev.calls[0].Questions.Get(keyToolReversed)
	keys := q.(typesafe.Question).Criteria.(*typesafe.Fields).Keys()
	rev := qr.(typesafe.Question).Criteria.(*typesafe.Fields).Keys()
	if strings.Join(keys, ",") != noTool+",Bash,Read" || strings.Join(rev, ",") != "Read,Bash,"+noTool {
		t.Fatalf("orders %v / %v", keys, rev)
	}
}

func TestDecideVerify(t *testing.T) {
	tools := []tool{{Name: "Bash", Description: "run a command"}, {Name: "Read"}, {Name: "Edit"}, {Name: "Grep"}}
	fits := func(f map[string]float64) func(typesafe.Request) (*typesafe.Response, error) {
		return func(req typesafe.Request) (*typesafe.Response, error) {
			if _, ok := req.Questions.Get(keyTool); ok {
				resp, _ := answer("Bash", 0.9, 0.9)(req)
				p := map[string]float64{"Bash": 0.6, "Read": 0.2, "Edit": 0.1, "Grep": 0.05}
				resp.Answers[keyTool] = typesafe.Answer{Type: "choice", Choice: "Bash", Confidence: 0.9, Probabilities: p}
				resp.Answers[keyToolReversed] = resp.Answers[keyTool]
				return resp, nil
			}
			ans := map[string]typesafe.Answer{}
			for _, k := range req.Questions.Keys() {
				ans[k] = typesafe.Answer{Type: "noul", Noul: f[strings.TrimPrefix(k, keyFitsPrefix)]}
			}
			return &typesafe.Response{Answers: ans, Usage: typesafe.Usage{InputTokens: 7}}, nil
		}
	}
	in := input{Turns: []turn{textTurn("user", "x")}, Tools: tools, ToolChoice: choiceAuto}
	r, jev := newRouter(fits(map[string]float64{"Bash": 0.4, "Read": 0.8, "Edit": 0.1}))
	r.verify = true
	d := r.decide(context.Background(), in)
	if d.Mode != modeForced || d.Tool.Name != "Read" || len(jev.calls) != 2 || d.Jev.InputTokens != 107 {
		t.Fatalf("got %s %+v", d.Mode, d.Jev)
	}
	if keys := jev.calls[1].Questions.Keys(); strings.Join(keys, ",") != "fits::Bash,fits::Read,fits::Edit" {
		t.Fatalf("verify questions %v", keys)
	}
	if q, _ := jev.calls[1].Questions.Get("fits::Bash"); !strings.Contains(fmt.Sprint(q.(typesafe.Question).Instructions), "run a command") {
		t.Fatal("verify question lacks the description")
	}
	r, _ = newRouter(fits(map[string]float64{"Bash": 0.2, "Read": 0.1, "Edit": 0.1}))
	r.verify = true
	if d := r.decide(context.Background(), in); d.Reason != "jev_verify_rejected" {
		t.Fatalf("rejected: %s/%s", d.Mode, d.Reason)
	}
}

func TestShortlistSplitsRequestsByTokens(t *testing.T) {
	var tools []tool
	for i := range 2000 {
		tools = append(tools, tool{Name: fmt.Sprintf("t%04d", i), Description: strings.Repeat("d", 1000)})
	}
	r, jev := newRouter(func(req typesafe.Request) (*typesafe.Response, error) {
		if tok := typesafe.ApproxTokens(req); tok > typesafe.MaxRequestTokens {
			t.Errorf("request of %d tokens", tok)
		}
		if _, ok := req.Questions.Get(keyTool); ok {
			return answer("t0000", 0.9, 0.9)(req)
		}
		ans := map[string]typesafe.Answer{}
		for _, k := range req.Questions.Keys() {
			ans[k] = typesafe.Answer{Type: "choice", Probabilities: map[string]float64{}}
		}
		return &typesafe.Response{Answers: ans}, nil
	})
	r.decide(context.Background(), input{Turns: []turn{textTurn("user", "x")}, Tools: tools, ToolChoice: choiceAuto})
	if len(jev.calls) < 4 { // 17 shards of ~12k tokens
		t.Fatalf("%d calls: shards not split across requests", len(jev.calls))
	}
}

func TestDecideRequiredOmitsNoTool(t *testing.T) {
	r, jev := newRouter(answer("Bash", 0.9, 0.9))
	r.decide(context.Background(), input{Turns: []turn{textTurn("user", "x")}, Tools: []tool{{Name: "Bash", Description: "run"}}, ToolChoice: choiceRequired})
	q, _ := jev.calls[0].Questions.Get(keyTool)
	if _, ok := q.(typesafe.Question).Criteria.(*typesafe.Fields).Get(noTool); ok {
		t.Fatal("no_tool_needed offered although a tool is required")
	}
}

func TestDecideBudget(t *testing.T) {
	r, _ := newRouter(nil)
	r.jev = slowJev{}
	r.budget = 50 * time.Millisecond
	start := time.Now()
	d := r.decide(context.Background(), input{Turns: []turn{textTurn("user", "x")}, Tools: []tool{{Name: "Bash"}}})
	if d.Reason != "jev_error" || time.Since(start) > time.Second {
		t.Fatalf("budget not enforced: %s after %s", d.Reason, time.Since(start))
	}
}

type slowJev struct{}

func (slowJev) SystemOne(ctx context.Context, _ typesafe.Request) (*typesafe.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestShortlist(t *testing.T) {
	var tools []tool
	for i := range 250 {
		tools = append(tools, tool{Kind: kindFunction, Name: fmt.Sprintf("t%03d", i)})
	}
	r, jev := newRouter(func(req typesafe.Request) (*typesafe.Response, error) {
		if _, ok := req.Questions.Get("shard:0"); ok {
			ans := map[string]typesafe.Answer{}
			for i, k := range req.Questions.Keys() {
				first := i * 84 // shards of 84, 83, 83
				ans[k] = typesafe.Answer{Type: "choice", Probabilities: map[string]float64{
					fmt.Sprintf("t%03d", first): 0.5, fmt.Sprintf("t%03d", first+1): 0.3, fmt.Sprintf("t%03d", first+2): 0.1,
					fmt.Sprintf("t%03d", first+3): 0.05, noneOfThese: 0.9,
				}}
			}
			return &typesafe.Response{Answers: ans, Usage: typesafe.Usage{InputTokens: 10}}, nil
		}
		if n := req.Questions.Keys(); len(n) != 3 {
			t.Errorf("final questions = %v", n)
		}
		return answer("t084", 0.95, 0.95)(req)
	})
	d := r.decide(context.Background(), input{Turns: []turn{textTurn("user", "x")}, Tools: tools, ToolChoice: choiceAuto})
	if d.Mode != modeForced || d.Tool.Name != "t084" || len(d.Jev.Shortlist) != 9 || d.Jev.InputTokens != 110 || len(jev.calls) != 2 {
		t.Fatalf("got %s %+v calls=%d", d.Mode, d.Jev, len(jev.calls))
	}
	if strings.Join(d.Jev.Shortlist[:3], ",") != "t000,t001,t002" {
		t.Fatalf("shortlist order = %v", d.Jev.Shortlist)
	}
}

func TestUsageTap(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":900,\"cache_creation_input_tokens\":90,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n"
	tap := &usageTap{}
	for i := 0; i < len(sse); i += 7 { // split across writes, mid-line
		_, _ = tap.Write([]byte(sse[i:min(len(sse), i+7)]))
	}
	if u := tap.usage(); u == nil || *u != (llmUsage{Input: 1000, Output: 42, Cached: 900, CacheWrite: 90}) {
		t.Fatalf("anthropic usage = %+v", u)
	}
	tap = &usageTap{}
	_, _ = tap.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":500,"input_tokens_details":{"cached_tokens":400},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5}}}}` + "\n"))
	if u := tap.usage(); u == nil || *u != (llmUsage{Input: 500, Output: 20, Cached: 400, Reasoning: 5}) {
		t.Fatalf("responses usage = %+v", u)
	}
	tap = &usageTap{}
	_, _ = tap.Write([]byte(`{"usage":{"input_tokens":3,"output_tokens":4}}`))
	if u := tap.usage(); u == nil || u.Input != 3 || u.Output != 4 {
		t.Fatalf("json usage = %+v", u)
	}
}

func TestEventsRotateAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gw.jsonl")
	e, err := OpenEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	ch, stop := e.Subscribe()
	defer stop()
	e.Add(Event{Mode: modeHint})
	if got := <-ch; got.ID != 1 || got.Event != "route" {
		t.Fatalf("subscriber got %+v", got)
	}
	e.size = rotateSize // force the next write to rotate
	e.Add(Event{Mode: modeForced})
	_ = e.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	e, err = OpenEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if r := e.Recent(10); len(r) != 2 || r[1].Mode != modeForced {
		t.Fatalf("reloaded %+v", r)
	}
	if ev := e.Add(Event{}); ev.ID != 3 {
		t.Fatalf("id after reload = %d", ev.ID)
	}
}

// upstream records what the fake LLM received.
type upstream struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	header http.Header
}

func newGateway(t *testing.T, jev jevClient, h func(u *upstream, w http.ResponseWriter, r *http.Request)) (*Gateway, *upstream, *httptest.Server) {
	t.Helper()
	u := &upstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(b))
		u.paths = append(u.paths, r.URL.RequestURI())
		u.header = r.Header.Clone()
		u.mu.Unlock()
		h(u, w, r)
	}))
	t.Cleanup(up.Close)
	events, err := OpenEvents(filepath.Join(t.TempDir(), "gw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	cfg := Config{MinConfidence: 0.7, ForceNone: true, Routing: true, Budget: time.Second, MaxStateChars: 60000, MaxMessageChars: 4000}
	g := New(cfg, jev, "", events)
	l := Listener{Client: "codex", Upstream: up.URL + "/base", Route: "/v1/responses", adapter: responsesAdapter{}}
	srv := httptest.NewServer(g.Handler(l))
	t.Cleanup(srv.Close)
	return g, u, srv
}

const sseReply = "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
	"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":70,\"output_tokens\":7}}}\n\n"

func TestProxyRoutesAndStreams(t *testing.T) {
	g, u, srv := newGateway(t, &fakeJev{fn: answer("shell", 0.9, 0.9)}, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseReply)
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/responses?x=1", strings.NewReader(codexBody))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("X-Jev-Debug", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != sseReply {
		t.Fatalf("stream changed:\n%q", got)
	}
	if resp.Header.Get("X-Jev-Gateway-Mode") != modeForced || resp.Header.Get("X-Jev-Gateway-Tool") != "shell" {
		t.Fatalf("headers = %v", resp.Header)
	}
	if u.paths[0] != "/base/responses?x=1" || u.header.Get("Authorization") != "Bearer k" || u.header.Get("X-Jev-Debug") != "" {
		t.Fatalf("upstream saw %s %v", u.paths[0], u.header)
	}
	if !strings.Contains(u.bodies[0], `"tool_choice":{"type":"function","name":"shell"}`) {
		t.Fatalf("body not rewritten: %s", u.bodies[0])
	}
	ev := waitEvents(t, g, 1)[0]
	if ev.Mode != modeForced || ev.Status != 200 || ev.Usage == nil || ev.Usage.Input != 70 || ev.Model != "gpt-5.5-codex" || ev.Tools != 5 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestProxyReplaysRejectedRewrite(t *testing.T) {
	g, u, srv := newGateway(t, &fakeJev{fn: answer("shell", 0.9, 0.9)}, func(u *upstream, w http.ResponseWriter, _ *http.Request) {
		if len(u.bodies) == 1 {
			http.Error(w, `{"error":"tool_choice"}`, http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	resp, err := post(t, srv.URL+"/v1/responses", strings.NewReader(codexBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(u.bodies) != 2 || u.bodies[1] != codexBody {
		t.Fatalf("status %d, %d upstream calls", resp.StatusCode, len(u.bodies))
	}
	if ev := waitEvents(t, g, 1)[0]; ev.Reason != "upstream_rejected_forced" {
		t.Fatalf("reason = %q", ev.Reason)
	}
}

func TestProxyZstdBody(t *testing.T) {
	_, u, srv := newGateway(t, &fakeJev{fn: answer("shell", 0.9, 0.9)}, func(_ *upstream, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	enc, _ := zstd.NewWriter(nil)
	body := enc.EncodeAll([]byte(codexBody), nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Encoding", "zstd")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if u.header.Get("Content-Encoding") != "" || !strings.Contains(u.bodies[0], `"name":"shell"}`) {
		t.Fatalf("zstd body not decoded and rewritten: %q / %.80s", u.header.Get("Content-Encoding"), u.bodies[0])
	}
}

func TestProxyPassthroughCases(t *testing.T) {
	jev := &fakeJev{fn: answer("shell", 0.9, 0.9)}
	g, u, srv := newGateway(t, jev, func(_ *upstream, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/responses", strings.NewReader(codexBody))
	req.Header.Set("X-Jev-Gateway", "off")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	g.SetRouting(false)
	resp, _ = post(t, srv.URL+"/v1/responses", strings.NewReader(codexBody))
	_ = resp.Body.Close()
	resp, _ = get(t, srv.URL+"/v1/models")
	_ = resp.Body.Close()

	if len(jev.calls) != 0 || u.bodies[0] != codexBody || u.bodies[1] != codexBody || u.paths[2] != "/base/models" {
		t.Fatalf("jev calls %d, paths %v", len(jev.calls), u.paths)
	}
	evs := waitEvents(t, g, 2)
	if len(evs) != 2 || evs[0].Reason != "disabled_by_header" || evs[1].Reason != "routing_disabled" {
		t.Fatalf("events = %+v", evs)
	}
	resp, _ = get(t, srv.URL+"/elsewhere")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-/v1 path status %d", resp.StatusCode)
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	g, _, srv := newGateway(t, &fakeJev{fn: answer(noTool, 0.9, 0.1)}, nil)
	g.cfg.Listeners = nil
	l := Listener{Client: "codex", Upstream: "http://127.0.0.1:1", Route: "/v1/responses", adapter: responsesAdapter{}}
	down := httptest.NewServer(g.Handler(l))
	defer down.Close()
	_ = srv
	resp, err := post(t, down.URL+"/v1/responses", strings.NewReader(codexBody))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "upstream_unreachable") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

func TestCodexUpstream(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if got := CodexUpstream(); got != "https://api.openai.com/v1" {
		t.Fatalf("no auth.json: %s", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"x"}}`), 0o600)
	if got := CodexUpstream(); got != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("chatgpt login: %s", got)
	}
}

func post(t *testing.T, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func get(t *testing.T, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// waitEvents waits for n events: the gateway records a request after its
// response has been sent, so the client can finish first.
func waitEvents(t *testing.T, g *Gateway, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := g.Events().Recent(100)
		if len(evs) >= n || time.Now().After(deadline) {
			if len(evs) < n {
				t.Fatalf("got %d events, want %d", len(evs), n)
			}
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}
