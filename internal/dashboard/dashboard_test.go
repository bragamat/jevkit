package dashboard

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bragamat/jevkit/internal/gateway"
)

func newServer(t *testing.T) (*Server, *gateway.Gateway, string) {
	t.Helper()
	dir := t.TempDir()
	events, err := gateway.OpenEvents(filepath.Join(dir, "gw.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	cfg := gateway.Config{Routing: true, DecisionLog: filepath.Join(dir, "decisions.jsonl"), UsageLog: filepath.Join(dir, "usage.jsonl")}
	gw := gateway.New(cfg, nil, "", events)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, gw), gw, dir
}

func TestRoutingToggleNeedsDashboardHeaderAndSameOrigin(t *testing.T) {
	s, gw, _ := newServer(t)
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		body    string
		want    int
	}{
		{"get", http.MethodGet, nil, "", http.StatusMethodNotAllowed},
		{"no header", http.MethodPost, nil, `{"enabled":false}`, http.StatusForbidden},
		{"cross site", http.MethodPost, map[string]string{"X-Jev-Dashboard": "1", "Sec-Fetch-Site": "cross-site"}, `{"enabled":false}`, http.StatusForbidden},
		{"other origin", http.MethodPost, map[string]string{"X-Jev-Dashboard": "1", "Origin": "https://evil.example"}, `{"enabled":false}`, http.StatusForbidden},
		{"bad body", http.MethodPost, map[string]string{"X-Jev-Dashboard": "1"}, `{}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		req := httptest.NewRequestWithContext(t.Context(), c.method, "http://gw.local/dashboard/routing", strings.NewReader(c.body))
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != c.want || !gw.Routing() {
			t.Errorf("%s: status %d (want %d), routing %v", c.name, rec.Code, c.want, gw.Routing())
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://gw.local/dashboard/routing", strings.NewReader(`{"enabled":false}`))
	req.Header.Set("X-Jev-Dashboard", "1")
	req.Header.Set("Origin", "http://gw.local")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gw.Routing() {
		t.Fatalf("same-origin toggle: status %d, routing %v", rec.Code, gw.Routing())
	}
}

func TestStreamSendsSnapshotThenEvents(t *testing.T) {
	s, gw, dir := newServer(t)
	if err := os.WriteFile(filepath.Join(dir, "decisions.jsonl"), []byte(`{"command":"yesno"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/dashboard/stream", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	want := []string{"event: snapshot", "event: route", "event: decision"}
	gw.Events().Add(gateway.Event{Mode: "hint"})
	deadline := time.After(5 * time.Second)
	for len(want) > 0 {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("stream closed, still waiting for %v", want)
			}
			if l == want[0] {
				want = want[1:]
				if len(want) == 1 { // after the route event, let the tailer see the decision
					_ = appendLine(filepath.Join(dir, "decisions.jsonl"), `{"command":"decide"}`)
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %v", want)
		}
	}
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(line + "\n")
	return err
}

func TestTailFollowsAppendsAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	tl := &tail{path: path, keep: 3}
	tl.seed() // missing file is fine
	_ = os.WriteFile(path, []byte("{\"n\":1}\nnot json\n{\"n\":2}\n{\"n\":3}"), 0o600)
	if got := tl.read(); len(got) != 2 {
		t.Fatalf("first read %d records", len(got))
	}
	_ = appendLine(path, "")
	if got := tl.read(); len(got) != 1 || string(got[0]) != `{"n":3}` {
		t.Fatalf("completed partial line: %s", got)
	}
	_ = os.WriteFile(path, []byte("{\"n\":4}\n"), 0o600) // shrank: rotated
	if got := tl.read(); len(got) != 1 || string(got[0]) != `{"n":4}` {
		t.Fatalf("after rotation: %s", got)
	}
	if r := tl.recent(); len(r) != 3 || string(r[0]) != `{"n":2}` {
		t.Fatalf("kept %s", r)
	}
}
