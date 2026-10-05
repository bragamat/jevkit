// Package dashboard serves the gateway's live dashboard: routed requests from
// the gateway, and the decisions and usage jev logs from other processes.
package dashboard

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bragamat/jevkit/internal/gateway"
	"github.com/bragamat/jevkit/internal/typesafe"
)

//go:embed index.html
var indexHTML []byte

const (
	keepDecisions = 500
	keepUsage     = 5000
	// seedBytes is how much of each jev log is read at start.
	seedBytes = 2 << 20
	pollEvery = time.Second
)

// Server is the dashboard for one gateway; mount it on every listener.
type Server struct {
	gw        *gateway.Gateway
	decisions *tail
	usage     *tail

	mu   sync.Mutex
	subs map[chan message]struct{}
}

type message struct {
	kind string
	data any
}

// New starts tailing jev's logs until ctx ends.
func New(ctx context.Context, gw *gateway.Gateway) *Server {
	cfg := gw.Config()
	s := &Server{
		gw:        gw,
		decisions: &tail{path: cfg.DecisionLog, keep: keepDecisions},
		usage:     &tail{path: cfg.UsageLog, keep: keepUsage},
		subs:      map[chan message]struct{}{},
	}
	s.decisions.seed()
	s.usage.seed()
	go s.poll(ctx)
	go s.relayEvents(ctx)
	return s
}

func (s *Server) poll(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, rec := range s.decisions.read() {
				s.broadcast(message{"decision", rec})
			}
			for _, rec := range s.usage.read() {
				s.broadcast(message{"usage", rec})
			}
		}
	}
}

func (s *Server) relayEvents(ctx context.Context) {
	ch, stop := s.gw.Events().Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			s.broadcast(message{"route", ev})
		}
	}
}

func (s *Server) broadcast(m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

type router struct {
	Client   string `json:"client"`
	Port     int    `json:"port"`
	Upstream string `json:"upstream"`
	Route    string `json:"route"`
}

type snapshot struct {
	Started       time.Time         `json:"started"`
	Routing       bool              `json:"routing"`
	MinConfidence float64           `json:"minConfidence"`
	BudgetMs      int64             `json:"budgetMs"`
	USDPerMillion float64           `json:"usdPerMillion"`
	Routers       []router          `json:"routers"`
	Events        []gateway.Event   `json:"events"`
	Decisions     []json.RawMessage `json:"decisions"`
	Usage         []json.RawMessage `json:"usage"`
}

func (s *Server) snapshot() snapshot {
	cfg := s.gw.Config()
	snap := snapshot{
		Started:       s.gw.Started(),
		Routing:       s.gw.Routing(),
		MinConfidence: cfg.MinConfidence,
		BudgetMs:      cfg.Budget.Milliseconds(),
		USDPerMillion: typesafe.USDPerMillionInputTokens,
		Events:        s.gw.Events().Recent(2000),
		Decisions:     s.decisions.recent(),
		Usage:         s.usage.recent(),
	}
	for _, l := range cfg.Listeners {
		snap.Routers = append(snap.Routers, router{Client: l.Client, Port: l.Port, Upstream: l.Upstream, Route: l.Route})
	}
	return snap
}

// ServeHTTP handles /dashboard and its endpoints.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/dashboard":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		_, _ = w.Write(indexHTML)
	case "/dashboard/events":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.snapshot())
	case "/dashboard/stream":
		s.stream(w, r)
	case "/dashboard/routing":
		s.setRouting(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan message, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(kind string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			return true
		}
		if _, err := io.WriteString(w, "event: "+kind+"\ndata: "+string(b)+"\n\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("snapshot", s.snapshot()) {
		return
	}
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			if !send(m.kind, m.data) {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// setRouting flips routing. Only the dashboard page may do it: the request
// must carry X-Jev-Dashboard (a custom header, so another site's page cannot
// send it without a CORS preflight this server never grants) and, when the
// browser sends an Origin, it must be this host.
func (s *Server) setRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Jev-Dashboard") != "1" || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil || body.Enabled == nil {
		http.Error(w, `expected {"enabled": true|false}`, http.StatusBadRequest)
		return
	}
	s.gw.SetRouting(*body.Enabled)
	s.broadcast(message{"routing", map[string]bool{"routing": *body.Enabled}})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"routing": *body.Enabled})
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// tail follows a JSONL file another process appends to. It keeps the newest
// records and restarts from the top when the file shrinks (rotation).
type tail struct {
	path   string
	keep   int
	mu     sync.Mutex
	offset int64
	recs   []json.RawMessage
}

func (t *tail) seed() {
	st, err := os.Stat(t.path)
	if err != nil {
		return
	}
	// Starting mid-file may cut the first line; read skips it as invalid JSON.
	t.offset = max(0, st.Size()-seedBytes)
	t.read()
}

// read returns the records appended since the last read.
func (t *tail) read() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(t.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	if st.Size() < t.offset {
		t.offset = 0
	}
	if st.Size() == t.offset {
		return nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	var out []json.RawMessage
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		t.offset += int64(len(line))
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	t.recs = append(t.recs, out...)
	if len(t.recs) > t.keep {
		t.recs = append(t.recs[:0:0], t.recs[len(t.recs)-t.keep:]...)
	}
	return out
}

func (t *tail) recent() []json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]json.RawMessage(nil), t.recs...)
}
