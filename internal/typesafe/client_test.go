package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestFieldsKeepOrder(t *testing.T) {
	f := NewFields().Set("L10", nil).Set("L2", "two").Set("L1", 1).Set("L2", "again")
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"L10":null,"L2":"again","L1":1}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestDecodeOrderedRoundTrip(t *testing.T) {
	in := `{"z":1,"a":{"y":[1,"<b>",null],"b":true},"m":2.50}`
	v, err := DecodeOrdered([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"z":1,"a":{"y":[1,"<b>",null],"b":true},"m":2.50}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if _, err := DecodeOrdered([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestSystemOneSendsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if body["model"] != DefaultModel {
			t.Errorf("model = %v", body["model"])
		}
		q := body["questions"].(map[string]any)["q"].(map[string]any)
		if q["type"] != "noul" || q["criteria"] != nil {
			t.Errorf("question = %v", q)
		}
		_, _ = w.Write([]byte(`{"model":"jev-1","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":12,"output_tokens":1}}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	r, err := c.SystemOne(context.Background(), Request{State: "x", Questions: NewFields().Set("q", Noul("Is it?", nil, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if r.Answers["q"].Noul != 0.8 || r.Usage.InputTokens != 12 || r.Model != "jev-1" {
		t.Fatalf("response = %+v", r)
	}
}

func TestRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"jev-latest","description":"d","release_date":"2026-09-15"}]}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	var waited time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(models) != 1 || waited != time.Millisecond {
		t.Fatalf("calls=%d models=%v waited=%v", calls.Load(), models, waited)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("x-typesafe-request-id", "req_1")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"` + strings.Repeat("x", 300) + `"}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	c.sleep = noSleep
	_, err := c.Models(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	if len([]rune(apiErr.Body)) != 301 || !strings.Contains(err.Error(), "req_1") {
		t.Fatalf("error = %q", err)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	c.sleep = noSleep
	if _, err := c.Models(context.Background()); err == nil || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := range 10 {
		if d := backoff(attempt); d <= 0 || d > 5*time.Second {
			t.Errorf("attempt %d: %v", attempt, d)
		}
	}
}

func TestLongRetryAfterFallsBackToBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	var waited time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waited = d; return nil }
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited <= 0 || waited > maxRetryAfter {
		t.Fatalf("waited %v, want the normal backoff", waited)
	}
}

func TestTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	refused := srv.URL
	srv.Close() // nothing listens there any more
	cases := []struct {
		name    string
		baseURL string
		retried bool
	}{
		{"connection refused is retried", refused, true},
		{"bad scheme is not retried", "ftp://example.invalid", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New("k", time.Second)
			c.BaseURL = tc.baseURL
			var sleeps int
			c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
			if _, err := c.Models(context.Background()); err == nil {
				t.Fatal("want an error")
			}
			if got := sleeps > 0; got != tc.retried {
				t.Fatalf("retried = %v (sleeps %d), want %v", got, sleeps, tc.retried)
			}
		})
	}
}

func TestFieldsZeroValueAndNil(t *testing.T) {
	var f Fields
	f.Set("a", 1)
	if b, err := json.Marshal(&f); err != nil || string(b) != `{"a":1}` {
		t.Fatalf("zero value: %s, %v", b, err)
	}
	var nilFields *Fields
	if b, err := json.Marshal(nilFields); err != nil || string(b) != "null" {
		t.Fatalf("nil: %s, %v", b, err)
	}
}

func FuzzDecodeOrdered(f *testing.F) {
	for _, seed := range []string{`{"z":1,"a":[1,{"b":null}]}`, `[]`, `"x"`, `{"a":`, `{1:2}`, `{} {}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := DecodeOrdered(data)
		if err != nil {
			return
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("re-encoding %q: %v", data, err)
		}
		if !json.Valid(out) {
			t.Fatalf("invalid output %q for %q", out, data)
		}
		if json.Valid(data) {
			var a, b any
			_ = json.Unmarshal(data, &a)
			_ = json.Unmarshal(out, &b)
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			if string(ja) != string(jb) {
				t.Fatalf("round trip changed the value: %s vs %s", ja, jb)
			}
		}
	})
}

func TestValidationErrorKeepsEveryLocation(t *testing.T) {
	long := strings.Repeat("y", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":[{"loc":["body","questions","q",0],"msg":"` + long + `"},{"loc":["body","state"],"msg":"too long"}]}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	_, err := c.Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "body.questions.q.0: "+long) || !strings.Contains(err.Error(), "body.state: too long") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewFromEnvRejectsMangledKeys(t *testing.T) {
	for key, want := range map[string]error{
		"  ts_abc123\n": nil,
		"":              ErrNoAPIKey,
		"ts_abc 123":    ErrBadAPIKey,
		`"ts_abc123"`:   ErrBadAPIKey,
		"ts_abcé":       ErrBadAPIKey,
	} {
		t.Setenv(APIKeyEnv, key)
		if _, err := NewFromEnv(time.Second); !errors.Is(err, want) {
			t.Errorf("%q: err = %v, want %v", key, err, want)
		}
	}
}

func TestApproxTokensIsConservative(t *testing.T) {
	if n := ApproxTokens(strings.Repeat("a", 2998)); n != 1000 { // 3000 runes with quotes
		t.Fatalf("tokens = %d", n)
	}
}
