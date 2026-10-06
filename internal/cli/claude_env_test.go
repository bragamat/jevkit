package cli

import (
	"slices"
	"testing"
)

func TestClaudeEnvTurnsToolSearchOn(t *testing.T) {
	env := claudeEnv([]string{"HOME=/h"}, "http://127.0.0.1:8789")
	if !slices.Contains(env, "ANTHROPIC_BASE_URL=http://127.0.0.1:8789") || !slices.Contains(env, "ENABLE_TOOL_SEARCH=true") {
		t.Fatalf("env %v", env)
	}
	env = claudeEnv([]string{"ENABLE_TOOL_SEARCH=false"}, "http://x")
	if slices.Contains(env, "ENABLE_TOOL_SEARCH=true") {
		t.Fatalf("overrode the user's choice: %v", env)
	}
}
