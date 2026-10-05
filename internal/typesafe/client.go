// Package typesafe is a small client for the TypeSafe System One API
// (POST /v1/systemone), the API behind the Jev model.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultBaseURL = "https://api.typesafe.ai"
	DefaultModel   = "jev-latest"

	APIKeyEnv       = "TYPESAFE_API_KEY" //nolint:gosec // the variable name, not a credential
	BaseURLEnv      = "TYPESAFE_BASE_URL"
	DefaultModelEnv = "TYPESAFE_DEFAULT_MODEL"

	// USDPerMillionInputTokens is Jev's published input price, used only for cost estimates.
	USDPerMillionInputTokens = 0.042
)

// maxRetryAfter caps how long a Retry-After header can make the client wait. Longer
// values fall back to the normal backoff, so a CLI never hangs for minutes.
const maxRetryAfter = 60 * time.Second

// ErrNoAPIKey is returned by NewFromEnv when TYPESAFE_API_KEY is empty.
var ErrNoAPIKey = errors.New(APIKeyEnv + " is not set")

// Question is one typed question: a noul (probability of true), a choice
// (one of N labels) or a score (position on an ordered scale).
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Choice asks the model to pick one key of criteria. A nil value means the
// key needs no description.
func Choice(instructions any, criteria *Fields) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

// Noul asks for the probability that instructions hold. yes and no describe
// what counts as true and false; when both are nil the model's default applies.
func Noul(instructions, yes, no any) Question {
	q := Question{Type: "noul", Instructions: instructions}
	if yes != nil || no != nil {
		q.Criteria = NewFields().Set("true", yes).Set("false", no)
	}
	return q
}

// Score asks where the state sits on levels, ordered from lowest to highest.
func Score(instructions any, levels []any) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Request is the body of POST /v1/systemone.
type Request struct {
	State     any     `json:"state"`
	Model     string  `json:"model"`
	Questions *Fields `json:"questions"`
}

// Answer holds whichever fields the answer type fills.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Legend        map[string]any     `json:"legend,omitempty"`
}

// Usage is what the API billed for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the body returned by POST /v1/systemone.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Model describes one model listed by GET /v1/models.
type Model struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReleaseDate string `json:"release_date"`
}

// APIError is a non-2xx response.
type APIError struct {
	Status     int
	Body       string
	RequestID  string
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("API returned %d", e.Status)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	if e.RequestID != "" {
		msg += " (request " + e.RequestID + ")"
	}
	return msg
}

// Client calls the TypeSafe API. The zero value is not usable; build it with
// New or NewFromEnv.
type Client struct {
	BaseURL      string
	APIKey       string
	DefaultModel string
	UserAgent    string
	HTTP         *http.Client
	// MaxRetries counts retries after the first attempt, for 408, 429, 5xx
	// and connection errors.
	MaxRetries int
	// sleep waits between retries; tests replace it.
	sleep func(context.Context, time.Duration) error
}

// New returns a client with the SDK's retry defaults and a per-attempt timeout.
func New(apiKey string, timeout time.Duration) *Client {
	return &Client{
		BaseURL:      DefaultBaseURL,
		APIKey:       apiKey,
		DefaultModel: DefaultModel,
		UserAgent:    "jevkit",
		HTTP:         &http.Client{Timeout: timeout},
		MaxRetries:   3,
		sleep:        sleepContext,
	}
}

// NewFromEnv reads TYPESAFE_API_KEY, TYPESAFE_BASE_URL and TYPESAFE_DEFAULT_MODEL.
func NewFromEnv(timeout time.Duration) (*Client, error) {
	key := strings.TrimSpace(os.Getenv(APIKeyEnv))
	if key == "" {
		return nil, ErrNoAPIKey
	}
	c := New(key, timeout)
	if v := os.Getenv(BaseURLEnv); v != "" {
		c.BaseURL = v
	}
	if v := os.Getenv(DefaultModelEnv); v != "" {
		c.DefaultModel = v
	}
	return c, nil
}

// SystemOne asks the questions about the state. An empty Model uses the
// client's default.
func (c *Client) SystemOne(ctx context.Context, req Request) (*Response, error) {
	if req.Model == "" {
		req.Model = c.DefaultModel
	}
	if req.Questions == nil || req.Questions.Len() == 0 {
		return nil, errors.New("at least one question is required")
	}
	body, err := marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	var out Response
	if err := c.call(ctx, http.MethodPost, "/v1/systemone", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Models lists the models the API serves.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/models", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, body, attempt, out)
		if err == nil || attempt >= c.MaxRetries || !retryable(ctx, err) {
			return err
		}
		delay := backoff(attempt)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.retryAfter > 0 && apiErr.retryAfter <= maxRetryAfter {
			delay = apiErr.retryAfter
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, attempt int, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if attempt > 0 {
		req.Header.Set("X-TypeSafe-Retry-Count", strconv.Itoa(attempt))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportError{err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return transportError{fmt.Errorf("reading response from %s: %w", path, err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text := strings.TrimSpace(string(data))
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200]) + "…"
		}
		return &APIError{
			Status:     resp.StatusCode,
			Body:       text,
			RequestID:  resp.Header.Get("x-typesafe-request-id"),
			retryAfter: parseRetryAfter(resp.Header),
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return decodeError{fmt.Errorf("unexpected response from %s: %w", path, err)}
	}
	return nil
}

func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		s := apiErr.Status
		return s == http.StatusRequestTimeout || s == http.StatusTooManyRequests || (s >= 500 && s <= 599)
	}
	var tErr transportError
	return errors.As(err, &tErr) && transient(tErr.error)
}

// transient reports whether a transport failure is worth retrying: a timeout or a
// refused or dropped connection. A bad URL or a certificate error fails the same way
// every time.
func transient(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET)
}

// transportError marks a failure to reach the API or to read its response.
type transportError struct{ error }

func (e transportError) Unwrap() error { return e.error }

// decodeError marks a 2xx response whose body did not parse; retrying will not help.
type decodeError struct{ error }

func (e decodeError) Unwrap() error { return e.error }

// backoff doubles from 0.5 s up to 5 s and subtracts up to 25% of jitter.
func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << attempt
	if d > 5*time.Second || d <= 0 {
		d = 5 * time.Second
	}
	return d - time.Duration(rand.Float64()*0.25*float64(d)) //nolint:gosec // jitter needs no crypto randomness
}

func parseRetryAfter(h http.Header) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
			return time.Duration(s * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
