// Package cli implements the jev command: Jev recipes that let coding agents
// read less and delegate small judgment calls.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bragamat/jevkit/internal/typesafe"
)

const (
	// windowSize stays under the 255 options a choice accepts.
	windowSize = 250
	// concurrency caps parallel requests per command.
	concurrency = 16
	// requestTimeout applies to each HTTP attempt.
	requestTimeout = 120 * time.Second
	// usdPerMillionInputTokens is Jev's published input price, used only for the usage estimate.
	usdPerMillionInputTokens = 0.042
)

// App holds what every command shares. Tests build it directly.
type App struct {
	Stdout io.Writer
	Stdin  io.Reader
	// NewClient builds the API client the first time a command needs it.
	NewClient func() (*typesafe.Client, error)
	// UsageLog and DecisionLog are JSONL files; empty disables logging.
	UsageLog    string
	DecisionLog string
	Now         func() time.Time

	jsonOut bool
	model   string
	command string

	client     *typesafe.Client
	clientLock sync.Mutex
	logLock    sync.Mutex
}

// NewApp wires the real environment.
func NewApp() *App {
	return &App{
		Stdout:      os.Stdout,
		Stdin:       os.Stdin,
		NewClient:   func() (*typesafe.Client, error) { return typesafe.NewFromEnv(requestTimeout) },
		UsageLog:    stateFile("JEV_USAGE_LOG", "usage.jsonl"),
		DecisionLog: stateFile("JEV_DECISION_LOG", "decisions.jsonl"),
		Now:         time.Now,
	}
}

func stateFile(env, name string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "jev", name)
}

// getClient builds the API client on first use; commands call it from parallel goroutines.
func (a *App) getClient() (*typesafe.Client, error) {
	a.clientLock.Lock()
	defer a.clientLock.Unlock()
	if a.client == nil {
		c, err := a.NewClient()
		if err != nil {
			if errors.Is(err, typesafe.ErrNoAPIKey) {
				return nil, inputError{err.Error()}
			}
			return nil, err
		}
		a.client = c
	}
	return a.client, nil
}

// ask sends one System One request and records the tokens it was billed.
func (a *App) ask(ctx context.Context, state any, questions *typesafe.Fields) (*typesafe.Response, error) {
	client, err := a.getClient()
	if err != nil {
		return nil, err
	}
	r, err := client.SystemOne(ctx, typesafe.Request{State: state, Model: a.model, Questions: questions})
	if err != nil {
		return nil, err
	}
	for _, id := range questions.Keys() {
		if _, ok := r.Answers[id]; !ok {
			return nil, fmt.Errorf("the API returned no answer for %q", id)
		}
	}
	a.appendLog(a.UsageLog, map[string]any{
		"ts":           a.timestamp(),
		"cmd":          a.command,
		"cwd":          cwd(),
		"input_tokens": r.Usage.InputTokens,
		"model":        r.Model,
	})
	return r, nil
}

func (a *App) timestamp() string {
	return a.Now().Format("2006-01-02T15:04:05")
}

// appendLog writes one JSONL record. Logging is best effort: a read-only home
// must not break a decision.
func (a *App) appendLog(path string, record map[string]any) {
	if path == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	a.logLock.Lock()
	defer a.logLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// emit prints v as one line of JSON, without HTML escaping.
func (a *App) emit(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *App) printf(format string, args ...any) {
	fmt.Fprintf(a.Stdout, format, args...)
}

func cwd() string {
	d, _ := os.Getwd()
	return d
}

// parallel runs fn for 0..n-1 with at most limit in flight and stops at the first error.
func parallel[T any](ctx context.Context, n, limit int, fn func(context.Context, int) (T, error)) ([]T, error) {
	out := make([]T, n)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, limit)
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			v, err := fn(ctx, i)
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			out[i] = v
		}(i)
	}
	wg.Wait()
	return out, firstErr
}

func round(x float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(x*p) / p
}
