package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != DefaultModel {
			t.Errorf("model = %v", body["model"])
		}
		q := body["questions"].(map[string]any)["q"].(map[string]any)
		if q["type"] != "noul" || q["criteria"] != nil {
			t.Errorf("question = %v", q)
		}
		w.Write([]byte(`{"model":"jev-1","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":12,"output_tokens":1}}`))
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
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.Header.Get("X-TypeSafe-Retry-Count") != "1" {
			t.Errorf("retry count header = %q", r.Header.Get("X-TypeSafe-Retry-Count"))
		}
		w.Write([]byte(`{"models":[{"name":"jev-latest","description":"d","release_date":"2026-09-15"}]}`))
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
	if calls != 2 || len(models) != 1 || waited != time.Millisecond {
		t.Fatalf("calls=%d models=%v waited=%v", calls, models, waited)
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("x-typesafe-request-id", "req_1")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"detail":"` + strings.Repeat("x", 300) + `"}`))
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	c.sleep = noSleep
	_, err := c.Models(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if len([]rune(apiErr.Body)) != 201 || !strings.Contains(err.Error(), "req_1") {
		t.Fatalf("error = %q", err)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New("k", time.Second)
	c.BaseURL = srv.URL
	c.sleep = noSleep
	if _, err := c.Models(context.Background()); err == nil || calls != 4 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		d := backoff(attempt)
		if d <= 0 || d > 5*time.Second {
			t.Fatalf("attempt %d: %v", attempt, d)
		}
	}
}
