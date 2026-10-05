// Package gateway is a local LLM proxy that asks Jev which tool a coding agent
// should call next and steers the request toward it. It serves Claude Code
// (Anthropic Messages) and Codex (OpenAI Responses), each on its own port, and
// a live dashboard of what it routed.
package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Listener is one client-facing port and the upstream it forwards to.
type Listener struct {
	// Client names the agent behind the port ("claude", "codex") in events.
	Client   string
	Port     int
	Upstream string
	// Route is the POST path whose requests are routed; anything else under /v1 is proxied untouched.
	Route   string
	adapter adapter
}

// Config holds the gateway settings; FromEnv reads them.
type Config struct {
	Host      string
	Listeners []Listener

	MinConfidence   float64
	ForceNone       bool
	Routing         bool
	Budget          time.Duration
	MaxStateChars   int
	MaxMessageChars int

	// EventLog keeps one JSON line per routed request; DecisionLog and UsageLog
	// are jev-cli's own logs, shown on the dashboard.
	EventLog    string
	DecisionLog string
	UsageLog    string
}

// Default ports match the npm jev-gateway, so running sessions keep their base URLs.
const (
	DefaultAnthropicPort = 8789
	DefaultOpenAIPort    = 8790
)

// FromEnv reads the configuration. stateDir is where jev-cli keeps its logs.
func FromEnv(stateDir string) Config {
	return Config{
		Host: envString("JEV_GATEWAY_HOST", "127.0.0.1"),
		Listeners: []Listener{
			{
				Client:   "claude",
				Port:     envInt("JEV_CLAUDE_PORT", DefaultAnthropicPort),
				Upstream: envString("JEV_CLAUDE_UPSTREAM_BASE_URL", "https://api.anthropic.com/v1"),
				Route:    "/v1/messages",
				adapter:  messagesAdapter{},
			},
			{
				Client:   "codex",
				Port:     envInt("JEV_CODEX_PORT", DefaultOpenAIPort),
				Upstream: envString("JEV_CODEX_UPSTREAM_BASE_URL", CodexUpstream()),
				Route:    "/v1/responses",
				adapter:  responsesAdapter{},
			},
		},
		MinConfidence:   envFloat("JEV_MIN_CONFIDENCE", 0.7),
		ForceNone:       envString("JEV_ON_NONE", "force_none") == "force_none",
		Routing:         envString("JEV_ROUTING", "true") != "false",
		Budget:          time.Duration(envInt("JEV_BUDGET_MS", 2500)) * time.Millisecond,
		MaxStateChars:   envInt("JEV_MAX_STATE_CHARS", 60000),
		MaxMessageChars: envInt("JEV_MAX_MESSAGE_CHARS", 4000),
		EventLog:        envString("JEV_GATEWAY_LOG", filepath.Join(stateDir, "gateway.jsonl")),
		DecisionLog:     envString("JEV_DECISION_LOG", filepath.Join(stateDir, "decisions.jsonl")),
		UsageLog:        envString("JEV_USAGE_LOG", filepath.Join(stateDir, "usage.jsonl")),
	}
}

// CodexUpstream is the ChatGPT backend when Codex is logged in with ChatGPT,
// and the OpenAI API otherwise.
func CodexUpstream() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".codex")
	}
	var auth struct {
		AuthMode     string          `json:"auth_mode"`
		Tokens       json.RawMessage `json:"tokens"`
		OpenAIAPIKey *string         `json:"OPENAI_API_KEY"`
	}
	if b, err := os.ReadFile(filepath.Join(home, "auth.json")); err == nil && json.Unmarshal(b, &auth) == nil { //nolint:gosec // G703: CODEX_HOME is the user's own Codex config
		hasTokens := len(auth.Tokens) > 0 && string(auth.Tokens) != "null"
		hasKey := auth.OpenAIAPIKey != nil && *auth.OpenAIAPIKey != ""
		if auth.AuthMode == "chatgpt" || (hasTokens && !hasKey) {
			return "https://chatgpt.com/backend-api/codex"
		}
	}
	return "https://api.openai.com/v1"
}

func envString(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v >= 0 && v <= 1 {
		return v
	}
	return def
}
