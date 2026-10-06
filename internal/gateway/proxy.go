package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
	"github.com/klauspost/compress/zstd"
)

// maxRequestBody bounds a request read into memory; agent requests with long
// histories and images stay well under it.
const maxRequestBody = 64 << 20

// Gateway routes and proxies requests for every listener of a Config.
type Gateway struct {
	cfg     Config
	router  *router
	events  *Events
	http    *http.Client
	dieter  *dieter
	routing atomic.Bool
	started time.Time
}

// New builds a gateway that asks jev (with model, "" for the API default) and records to events.
func New(cfg Config, jev jevClient, model string, events *Events) *Gateway {
	g := &Gateway{
		cfg: cfg,
		router: &router{
			jev:             jev,
			model:           model,
			minConfidence:   cfg.MinConfidence,
			forceNone:       cfg.ForceNone,
			verify:          cfg.Verify,
			budget:          cfg.Budget,
			maxStateChars:   cfg.MaxStateChars,
			maxMessageChars: cfg.MaxMessageChars,
		},
		events: events,
		// No client timeout: responses stream for minutes. The request context
		// ends the upstream call when the agent hangs up.
		http:    &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		started: time.Now(),
	}
	if cfg.Diet.Mode != DietOff && cfg.Diet.Mode != "" && jev != nil {
		g.dieter = newDieter(cfg.Diet, jev, model)
	}
	g.routing.Store(cfg.Routing)
	return g
}

// Config returns the gateway's settings.
func (g *Gateway) Config() Config { return g.cfg }

// Events returns the event store.
func (g *Gateway) Events() *Events { return g.events }

// Started is when the gateway started.
func (g *Gateway) Started() time.Time { return g.started }

// Routing reports whether requests are routed or only proxied.
func (g *Gateway) Routing() bool { return g.routing.Load() }

// SetRouting turns routing on or off for every listener until restart.
func (g *Gateway) SetRouting(on bool) { g.routing.Store(on) }

var (
	dropRequestHeaders  = []string{"Host", "Connection", "Content-Length", "Accept-Encoding", "Transfer-Encoding"}
	dropResponseHeaders = []string{"Connection", "Content-Length", "Content-Encoding", "Transfer-Encoding"}
)

// Handler serves one listener: /health, and /v1/* proxied to its upstream.
func (g *Gateway) Handler(l Listener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "pid": os.Getpid(), "client": l.Client,
				"upstream": l.Upstream, "routing": g.Routing(), "jev": g.router.model,
			})
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			g.serve(w, r, l)
		default:
			http.NotFound(w, r)
		}
	})
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, l Listener) {
	start := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "jev-gateway could not read the request: "+err.Error(), "unreadable_request")
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != l.Route {
		resp, err := g.forward(r, l, body, false) //nolint:bodyclose // relay closes it
		if err != nil {
			g.upstreamError(w, r, err)
			return
		}
		relay(w, resp, nil, nil)
		return
	}

	ev := Event{Time: start, Client: l.Client, Path: r.URL.Path}
	encoding := r.Header.Get("Content-Encoding")
	dieted := g.diet(r, l, body, encoding, &ev)
	routeBody := body
	if dieted != nil {
		routeBody, encoding = dieted, ""
	}
	d, rewritten := g.route(r, l, routeBody, encoding, &ev)
	send, plain := body, false
	switch {
	case rewritten != nil:
		send, plain = rewritten, true
	case dieted != nil:
		send, plain = dieted, true
	}
	resp, err := g.forward(r, l, send, plain) //nolint:bodyclose // closed on replay, else by relay
	if err == nil && plain && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
		// The upstream refused the rewrite; the agent gets its own request answered.
		_ = resp.Body.Close()
		if rewritten != nil {
			d = decision{Mode: modePassthrough, Reason: "upstream_rejected_" + d.Mode, Tool: d.Tool, Jev: d.Jev}
		}
		if dieted != nil {
			ev.Diet.Rejected = true
		}
		resp, err = g.forward(r, l, body, false) //nolint:bodyclose // relay closes it
	}
	ev.Mode, ev.Reason, ev.Jev = d.Mode, d.Reason, d.Jev
	if d.Tool != nil {
		ev.Tool = d.Tool.Name
	}
	if d.Jev != nil {
		ev.Confidence = d.Jev.Confidence
	}
	defer func() {
		ev.DurationMs = time.Since(start).Milliseconds()
		g.events.Add(ev)
	}()
	if err != nil {
		ev.Status = g.upstreamError(w, r, err)
		return
	}
	ev.Status = resp.StatusCode
	tap := &usageTap{}
	relay(w, resp, d.headers(), tap)
	ev.Usage = tap.usage()
}

// diet trims the skill listing and memory index of Claude requests; it
// returns the plain new body, or nil to keep the original.
func (g *Gateway) diet(r *http.Request, l Listener, body []byte, encoding string, ev *Event) []byte {
	if g.dieter == nil || l.Client != "claude" || strings.EqualFold(r.Header.Get("X-Jev-Gateway"), "off") {
		return nil
	}
	plain, err := decodeBody(body, encoding)
	if err != nil {
		return nil
	}
	rec := &dietRecord{}
	out := g.dieter.apply(r.Context(), plain, rec)
	if rec.Arm != "" {
		ev.Diet = rec
	}
	return out
}

// route decides one request and returns the rewritten body, or nil to send
// the original. ev gets the model and tool count.
func (g *Gateway) route(r *http.Request, l Listener, body []byte, encoding string, ev *Event) (decision, []byte) {
	if strings.EqualFold(r.Header.Get("X-Jev-Gateway"), "off") {
		return passthrough("disabled_by_header", nil), nil
	}
	if !g.Routing() {
		return passthrough("routing_disabled", nil), nil
	}
	plain, err := decodeBody(body, encoding)
	if err != nil {
		return passthrough("unsupported_encoding", nil), nil
	}
	in, model, skip, err := l.adapter.toInput(plain, g.cfg.MaxMessageChars)
	ev.Model, ev.Tools = model, len(in.Tools)
	if err != nil {
		return passthrough("unparseable_body", nil), nil
	}
	if skip != "" {
		return passthrough(skip, nil), nil
	}
	d := g.router.decide(r.Context(), in)
	if d.Mode == modePassthrough {
		return d, nil
	}
	root, err := typesafe.DecodeOrdered(plain)
	fields, ok := root.(*typesafe.Fields)
	if err != nil || !ok {
		return passthrough("router_error", d.Jev), nil
	}
	if !l.adapter.apply(fields, d) {
		reason := "router_error"
		if d.Mode == modeHint {
			reason = "hint_no_user_turn"
		}
		return decision{Mode: modePassthrough, Reason: reason, Tool: d.Tool, Jev: d.Jev}, nil
	}
	// MarshalJSON directly: json.Marshal would HTML-escape the agent's text.
	out, err := fields.MarshalJSON()
	if err != nil {
		return passthrough("router_error", d.Jev), nil
	}
	return d, out
}

func (d decision) headers() http.Header {
	h := http.Header{}
	h.Set("X-Jev-Gateway-Mode", d.Mode)
	if d.Reason != "" {
		h.Set("X-Jev-Gateway-Reason", d.Reason)
	}
	if d.Tool != nil {
		h.Set("X-Jev-Gateway-Tool", d.Tool.Name)
	}
	if d.Jev != nil {
		if d.Jev.Confidence > 0 {
			h.Set("X-Jev-Gateway-Confidence", strconv.FormatFloat(d.Jev.Confidence, 'f', 3, 64))
		}
		h.Set("X-Jev-Gateway-Latency-Ms", strconv.FormatInt(d.Jev.LatencyMs, 10))
	}
	return h
}

func decodeBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(zr, maxRequestBody))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderMaxMemory(maxRequestBody))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, maxRequestBody))
	}
	return nil, fmt.Errorf("unsupported content encoding %q", encoding)
}

// forward sends body to the listener's upstream. A rewritten (plain) body
// drops the client's Content-Encoding. The Go transport asks for gzip itself
// and decompresses, so the agent always gets an identity body.
func (g *Gateway) forward(r *http.Request, l Listener, body []byte, plain bool) (*http.Response, error) {
	target := strings.TrimSuffix(l.Upstream, "/") + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body)) //nolint:gosec // G704: the upstream is fixed by config; only the path comes from the agent
	if err != nil {
		return nil, err
	}
	for k, v := range r.Header {
		if !strings.HasPrefix(strings.ToLower(k), "x-jev-") {
			req.Header[k] = v
		}
	}
	for _, k := range dropRequestHeaders {
		req.Header.Del(k)
	}
	if plain {
		req.Header.Del("Content-Encoding")
	}
	return g.http.Do(req) //nolint:gosec // G704: see above
}

func (g *Gateway) upstreamError(w http.ResponseWriter, r *http.Request, err error) int {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return 499 // the agent hung up; nobody to answer
	}
	writeError(w, http.StatusBadGateway, "jev-gateway could not reach upstream: "+err.Error(), "upstream_unreachable")
	return http.StatusBadGateway
}

func writeError(w http.ResponseWriter, status int, msg, kind string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": kind}})
}

// relay streams resp to the client unchanged, flushing as data arrives so SSE
// stays live, and copies it into tap.
func relay(w http.ResponseWriter, resp *http.Response, extra http.Header, tap io.Writer) {
	defer func() { _ = resp.Body.Close() }()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	for _, k := range dropResponseHeaders {
		w.Header().Del(k)
	}
	for k, v := range extra {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // the agent hung up
			}
			if tap != nil {
				_, _ = tap.Write(buf[:n])
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
