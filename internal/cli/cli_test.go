package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
)

// apiRequest is what the fake server sees, decoded loosely.
type apiRequest struct {
	State     any                       `json:"state"`
	Questions map[string]map[string]any `json:"questions"`
	rawState  string
}

// answerFunc answers one question of a request.
type answerFunc func(req apiRequest, id string, q map[string]any) map[string]any

type fakeAPI struct {
	mu       sync.Mutex
	requests []apiRequest
}

func newFake(t *testing.T, answer answerFunc) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			State     json.RawMessage           `json:"state"`
			Questions map[string]map[string]any `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		req := apiRequest{Questions: raw.Questions, rawState: string(raw.State)}
		_ = json.Unmarshal(raw.State, &req.State) // an absent state stays nil
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		answers := map[string]any{}
		for id, q := range raw.Questions {
			answers[id] = answer(req, id, q)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-test", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 1},
		}); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testApp(t *testing.T, srv *httptest.Server) (*App, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	return &App{
		Stdout: &out,
		Stdin:  strings.NewReader(""),
		NewClient: func() (*typesafe.Client, error) {
			c := typesafe.New("test-key", 5*time.Second)
			c.BaseURL = srv.URL
			c.MaxRetries = 0
			return c, nil
		},
		UsageLog:    filepath.Join(dir, "usage.jsonl"),
		DecisionLog: filepath.Join(dir, "decisions.jsonl"),
		Now:         func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) },
	}, &out
}

func run(t *testing.T, a *App, args ...string) error {
	t.Helper()
	root := NewRoot(a, "test")
	root.SetArgs(args)
	root.SetOut(a.Stdout)
	root.SetErr(a.Stdout)
	return root.Execute()
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func choice(probs map[string]float64) map[string]any {
	best, bestP := "", -1.0
	for k, p := range probs {
		if p > bestP || (p == bestP && k < best) {
			best, bestP = k, p
		}
	}
	return map[string]any{"type": "choice", "choice": best, "confidence": bestP, "probabilities": probs}
}

func TestFindRanksAcrossWindows(t *testing.T) {
	var doc strings.Builder
	for i := 1; i <= 300; i++ {
		switch {
		case i == 270:
			doc.WriteString("the deploy key lives in vault\n")
		case i%7 == 0:
			doc.WriteString("\n")
		default:
			fmt.Fprintf(&doc, "filler line %d\n", i)
		}
	}
	path := writeFile(t, "doc.txt", doc.String())
	fake, srv := newFake(t, func(req apiRequest, id string, q map[string]any) map[string]any {
		state := req.State.(string)
		if id == "exists" {
			if strings.Contains(state, "vault") {
				return map[string]any{"type": "noul", "noul": 0.9}
			}
			return map[string]any{"type": "noul", "noul": 0.1}
		}
		probs := map[string]float64{}
		for k := range q["criteria"].(map[string]any) {
			probs[k] = 0.001
		}
		if strings.Contains(state, "L270: the deploy key") {
			probs["L270"] = 0.95
		} else {
			// A confident pick in a window that does not answer.
			probs[strings.SplitN(state, ":", 2)[0]] = 0.9
		}
		return choice(probs)
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "find", path, "where is the deploy key?", "--top", "2"); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want 2 windows", len(fake.requests))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.HasPrefix(lines[0], "exists=0.90 (answers)") || !strings.Contains(lines[1], "L270: the deploy key lives in vault") {
		t.Fatalf("output:\n%s", out.String())
	}
	if !strings.HasPrefix(lines[2], "  0.09  L") || !strings.Contains(lines[2], "filler line") {
		t.Fatalf("non-answering window not down-weighted:\n%s", out.String())
	}
	if len(lines) != 3 {
		t.Fatalf("--top 2 printed %d lines", len(lines)-1)
	}

	out.Reset()
	if err := run(t, a, "--json", "find", path, "where is the deploy key?", "--top", "1"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Exists float64 `json:"exists"`
		Lines  []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Exists != 0.9 || got.Lines[0].Line != 270 {
		t.Fatalf("json = %s (%v)", out.String(), err)
	}
}

func TestPickTwoRounds(t *testing.T) {
	var opts strings.Builder
	for i := range 600 {
		fmt.Fprintf(&opts, "opt-%03d\n", i)
	}
	path := writeFile(t, "options.txt", opts.String())
	fake, srv := newFake(t, func(_ apiRequest, _ string, q map[string]any) map[string]any {
		probs := map[string]float64{}
		for k := range q["criteria"].(map[string]any) {
			probs[k] = 0.0
		}
		if _, ok := probs["opt-555"]; ok {
			probs["opt-555"] = 0.97
		}
		return choice(probs)
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "pick", "Which one?", path, "--top", "1", "--json"); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 4 {
		t.Fatalf("requests = %d, want 3 groups + 1 final", len(fake.requests))
	}
	final := fake.requests[len(fake.requests)-1]
	for _, r := range fake.requests {
		if len(r.Questions["q"]["criteria"].(map[string]any)) == 9 {
			final = r
		}
	}
	if len(final.Questions["q"]["criteria"].(map[string]any)) != 9 {
		t.Fatal("final round should have 3 finalists from each of 3 groups")
	}
	if got := strings.TrimSpace(out.String()); got != `[{"option":"opt-555","p":0.97}]` {
		t.Fatalf("output = %s", got)
	}
}

func TestPickKeepsOptionOrder(t *testing.T) {
	path := writeFile(t, "options.json", `{"zeta":"last letter","alpha":"","mid":null}`)
	fake, srv := newFake(t, func(_ apiRequest, _ string, q map[string]any) map[string]any {
		return choice(map[string]float64{"zeta": 0.2, "alpha": 0.7, "mid": 0.1})
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "pick", "Which?", path); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests[0].rawState; got != `{"task":"Which?"}` {
		t.Fatalf("default state = %s", got)
	}
	if !strings.HasPrefix(out.String(), "  0.70  alpha\n  0.20  zeta\n") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestTriageBatchedRewritesItemReferences(t *testing.T) {
	items := writeFile(t, "items.jsonl", `{"id":"a","msg":"server down"}`+"\n\n"+`{"id":"b","msg":"typo"}`+"\n"+`{"id":"c","msg":"slow page"}`)
	spec := writeFile(t, "spec.json", `{
		"urgent": {"type": "noul", "instructions": "Does `+"`item.msg`"+` need action today?"},
		"area": {"type": "choice", "instructions": "Which team owns it?", "criteria": ["infra", "docs"]}
	}`)
	ctxFile := writeFile(t, "ctx.json", `{"oncall":"infra"}`)
	fake, srv := newFake(t, func(req apiRequest, id string, q map[string]any) map[string]any {
		if strings.HasPrefix(id, "urgent::") {
			if id == "urgent::0" && len(req.State.(map[string]any)["items"].([]any)) == 2 {
				return map[string]any{"type": "noul", "noul": 0.95}
			}
			return map[string]any{"type": "noul", "noul": 0.2}
		}
		return choice(map[string]float64{"infra": 0.8, "docs": 0.2})
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "triage", items, spec, "--context", ctxFile, "--batch", "2", "--label", "id", "--sort", "urgent"); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want 2 batches", len(fake.requests))
	}
	for _, r := range fake.requests {
		state := r.State.(map[string]any)
		if state["context"].(map[string]any)["oncall"] != "infra" {
			t.Fatalf("state = %s", r.rawState)
		}
	}
	first := fake.requests[0]
	if first.Questions["urgent::1"] == nil {
		first = fake.requests[1]
	}
	if got := first.Questions["urgent::1"]["instructions"]; got != "Does `items[1].msg` need action today?" {
		t.Fatalf("rewritten instructions = %v", got)
	}
	if got := first.Questions["area::0"]["instructions"]; got != "About `items[0]`: Which team owns it?" {
		t.Fatalf("prefixed instructions = %v", got)
	}
	want := "a | urgent=0.95 | area=infra | area_conf=0.8\n" +
		"b | urgent=0.2 | area=infra | area_conf=0.8\n" +
		"c | urgent=0.2 | area=infra | area_conf=0.8\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestTriageEachItemIsTheState(t *testing.T) {
	items := writeFile(t, "items.json", `["first", "second"]`)
	spec := writeFile(t, "spec.json", `{"effort": {"type": "score", "instructions": "How big?", "criteria": ["small", "large"]}}`)
	fake, srv := newFake(t, func(req apiRequest, _ string, _ map[string]any) map[string]any {
		s := 0.0
		if req.State.(map[string]any)["item"] == "second" {
			s = 1.0
		}
		return map[string]any{"type": "score", "score": s, "confidence": 0.9, "probabilities": map[string]float64{}}
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "triage", items, spec, "--json"); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d", len(fake.requests))
	}
	if got := strings.Split(out.String(), "\n")[0]; got != `{"_":"first","effort":0,"effort_conf":0.9}` {
		t.Fatalf("row = %s", got)
	}
}

func TestCheck(t *testing.T) {
	path := writeFile(t, "notes.md", "intro\nthe cache TTL is 10 minutes\nunrelated\n")
	fake, srv := newFake(t, func(req apiRequest, id string, q map[string]any) map[string]any {
		switch id {
		case "exists":
			return map[string]any{"type": "noul", "noul": 0.8}
		case "where":
			return choice(map[string]float64{"L1": 0.1, "L2": 0.85, "L3": 0.05})
		}
		return choice(map[string]float64{"supports": 0.1, "partially": 0.1, "contradicts": 0.75, "not_addressed": 0.05})
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "check", "The cache TTL is 1 hour", path); err != nil {
		t.Fatal(err)
	}
	last := fake.requests[len(fake.requests)-1]
	if got := last.rawState; got != `{"claim":"The cache TTL is 1 hour","section":"L1: intro\nL2: the cache TTL is 10 minutes\nL3: unrelated"}` {
		t.Fatalf("check state = %s", got)
	}
	if !strings.HasPrefix(out.String(), "contradicts (conf 0.75, exists 0.80)\n  L2: the cache TTL is 10 minutes\n") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestDecideVerdicts(t *testing.T) {
	cases := []struct {
		conf float64
		risk string
		want string
	}{
		{0.8, "low", "ACT"},
		{0.8, "high", "CONFIRM"},
		{0.92, "high", "ACT"},
		{0.45, "low", "REPHRASE"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%.2f/%s", c.conf, c.risk), func(t *testing.T) {
			_, srv := newFake(t, func(_ apiRequest, _ string, _ map[string]any) map[string]any {
				return map[string]any{"type": "choice", "choice": "merge", "confidence": c.conf,
					"probabilities": map[string]float64{"merge": c.conf, "wait": 1 - c.conf}}
			})
			a, out := testApp(t, srv)
			if err := run(t, a, "decide", "Merge now?", "merge=CI is green", "wait", "--risk", c.risk, "--text", "CI green"); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out.String(), c.want+": merge") {
				t.Errorf("got %s", out.String())
			}
		})
	}
}

func TestDecideLogsAndReadsStdin(t *testing.T) {
	fake, srv := newFake(t, func(_ apiRequest, _ string, q map[string]any) map[string]any {
		return map[string]any{"type": "choice", "choice": "b", "confidence": 0.9,
			"probabilities": map[string]float64{"a": 0.1, "b": 0.9}}
	})
	a, out := testApp(t, srv)
	a.Stdin = strings.NewReader("evidence from a pipe")
	evidence := writeFile(t, "e.txt", "evidence from a file")
	if err := run(t, a, "decide", "Which?", "a", "b=the second", "--ctx", evidence, "--ctx", "-", "--json"); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests[0].rawState; got != `{"context":"evidence from a file\n\nevidence from a pipe"}` {
		t.Fatalf("state = %s", got)
	}
	crit := fake.requests[0].Questions["q"]["criteria"].(map[string]any)
	if crit["a"] != nil || crit["b"] != "the second" {
		t.Fatalf("criteria = %v", crit)
	}
	if got := strings.TrimSpace(out.String()); got != `{"choice":"b","confidence":0.9,"verdict":"ACT","probabilities":{"b":0.9,"a":0.1},"model":"jev-test"}` {
		t.Fatalf("json = %s", got)
	}
	log, _ := os.ReadFile(a.DecisionLog)
	if !strings.Contains(string(log), `"kind":"decide"`) || !strings.Contains(string(log), `"verdict":"ACT"`) {
		t.Fatalf("decision log = %s", log)
	}
}

func TestYesNoAndScore(t *testing.T) {
	_, srv := newFake(t, func(_ apiRequest, _ string, q map[string]any) map[string]any {
		if q["type"] == "noul" {
			crit := q["criteria"].(map[string]any)
			if crit["true"] != "it is broken" || crit["false"] != nil {
				t.Errorf("noul criteria = %v", crit)
			}
			return map[string]any{"type": "noul", "noul": 0.5}
		}
		return map[string]any{"type": "score", "score": 1.5, "confidence": 0.75, "probabilities": map[string]float64{}}
	})
	a, out := testApp(t, srv)
	if err := run(t, a, "yesno", "Is it broken?", "--yes", "it is broken"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "UNSURE  (p_yes 0.50, risk low, jev-test)") {
		t.Fatalf("yesno: %s", out.String())
	}
	out.Reset()
	if err := run(t, a, "score", "How urgent?", "later", "this week", "today"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "ACT: today  (score 1.50 of 0–2, conf 0.75, jev-test)") {
		t.Fatalf("score: %s", out.String())
	}
}

func TestInputErrorsAreOneLine(t *testing.T) {
	_, srv := newFake(t, func(apiRequest, string, map[string]any) map[string]any { return nil })
	a, _ := testApp(t, srv)
	dir := t.TempDir()
	bad := writeFile(t, "spec.json", "{\n  \"a\": {\"type\": \"noul\",,}\n}")
	items := writeFile(t, "items.json", `["x"]`)
	cases := map[string][]string{
		"file not found: " + filepath.Join(dir, "nope.txt"): {"find", filepath.Join(dir, "nope.txt"), "q"},
		"is a directory, not a file: " + dir:                {"find", dir, "q"},
		"give at least two distinct options":                {"decide", "q", "a", "a"},
		"--risk must be low or high":                        {"yesno", "q", "--risk", "medium"},
	}
	for want, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := run(t, a, args...); err == nil || err.Error() != want {
				t.Errorf("got %v, want %q", err, want)
			}
		})
	}
	if err := run(t, a, "triage", items, bad); err == nil || !strings.HasPrefix(err.Error(), "invalid JSON in "+bad+" (line 2, column ") {
		t.Errorf("bad spec: %v", err)
	}
}

func TestContextTooLarge(t *testing.T) {
	_, srv := newFake(t, func(apiRequest, string, map[string]any) map[string]any { return nil })
	a, _ := testApp(t, srv)
	big := writeFile(t, "big.txt", strings.Repeat("é", maxContextChars+1))
	if err := run(t, a, "yesno", "q", "--ctx", big); err == nil || !strings.Contains(err.Error(), "context too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageSummary(t *testing.T) {
	_, srv := newFake(t, func(_ apiRequest, _ string, _ map[string]any) map[string]any {
		return map[string]any{"type": "noul", "noul": 0.9}
	})
	a, out := testApp(t, srv)
	for range 3 {
		if err := run(t, a, "yesno", "q"); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(a.UsageLog, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2026-10-01T09:00:00","cmd":"find","input_tokens":2500000}` + "\nnot json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(t, a, "usage"); err != nil {
		t.Fatal(err)
	}
	want := "  find         1 call    2,500,000 tokens read by Jev\n" +
		"  yesno        3 calls         300 tokens read by Jev\n" +
		"  total        4 calls   2,500,300 tokens  ≈ US$ 0.1050\n"
	if out.String() != want {
		t.Fatalf("usage:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	if err := run(t, a, "usage", "--today", "--json"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"commands":[{"command":"yesno","calls":3,"input_tokens":300}],"calls":3,"input_tokens":300,"usd_estimate":0}` {
		t.Fatalf("usage json = %s", got)
	}
}

func TestMissingAPIKey(t *testing.T) {
	_, srv := newFake(t, func(apiRequest, string, map[string]any) map[string]any { return nil })
	a, _ := testApp(t, srv)
	a.NewClient = func() (*typesafe.Client, error) { return nil, typesafe.ErrNoAPIKey }
	if err := run(t, a, "yesno", "q"); err == nil || err.Error() != "TYPESAFE_API_KEY is not set" {
		t.Fatalf("err = %v", err)
	}
}

func TestThousands(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %s, want %s", in, got, want)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	_, srv := newFake(t, func(apiRequest, string, map[string]any) map[string]any { return nil })
	a, _ := testApp(t, srv)
	for name, args := range map[string][]string{
		"unknown command": {"nope"},
		"unknown flag":    {"yesno", "q", "--bogus"},
		"missing args":    {"find"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(t, a, args...); err == nil || !IsUsageError(err) {
				t.Fatalf("err = %v, want a usage error", err)
			}
		})
	}
	if err := run(t, a, "decide", "q", "a", "a"); err == nil || IsUsageError(err) {
		t.Fatalf("input error classified as usage: %v", err)
	}
}

func TestParallelStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := parallel(ctx, 5, 2, func(context.Context, int) (int, error) { return 1, nil })
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("out=%v err=%v", out, err)
	}
	out, err = parallel(context.Background(), 4, 2, func(_ context.Context, i int) (int, error) { return i * i, nil })
	if err != nil || fmt.Sprint(out) != "[0 1 4 9]" {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestServedURLFindsTailscaleServeForPort(t *testing.T) {
	status := []byte(`{"Web":{
		"box.tail.ts.net:8445":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8099"}}},
		"box.tail.ts.net:8789":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8789"}}},
		"box.tail.ts.net:443":{"Handlers":{"/":{"Proxy":"8790"}}}}}`)
	cases := map[int]string{8789: "https://box.tail.ts.net:8789", 8790: "https://box.tail.ts.net", 9999: ""}
	for port, want := range cases {
		if got := servedURL(status, port); got != want {
			t.Errorf("port %d: %q, want %q", port, got, want)
		}
	}
	if got := servedURL([]byte("not json"), 8789); got != "" {
		t.Errorf("bad json: %q", got)
	}
}

func TestDashboardURLPrefersEnv(t *testing.T) {
	t.Setenv("JEV_DASHBOARD_URL", "https://dash.example/dashboard/")
	if got := dashboardURL(t.Context(), "127.0.0.1", 8789); got != "https://dash.example/dashboard" {
		t.Fatalf("got %q", got)
	}
}
