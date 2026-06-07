// internal/backend/env_test.go — tests for buildCLIEnv and cliEnvAllowed.
package backend

import (
	"os"
	"testing"
)

// TestBuildCLIEnv_BlocksSecrets verifies that sensitive daemon env vars are not
// inherited by CLI subprocesses.
func TestBuildCLIEnv_BlocksSecrets(t *testing.T) {
	// Set secret-like vars in the process environment.
	os.Setenv("CLAGENTIC_ROUTER_TOKEN", "super-secret-token")
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	os.Setenv("OPENAI_API_KEY", "sk-openai-test")
	defer func() {
		os.Unsetenv("CLAGENTIC_ROUTER_TOKEN")
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	env := buildCLIEnv(nil)

	blocked := []string{
		"CLAGENTIC_ROUTER_TOKEN=super-secret-token",
		"ANTHROPIC_API_KEY=sk-ant-test",
		"OPENAI_API_KEY=sk-openai-test",
	}
	for _, secret := range blocked {
		for _, kv := range env {
			if kv == secret {
				t.Errorf("secret var leaked into CLI env: %q", secret)
			}
		}
	}
}

// TestBuildCLIEnv_AllowsRequiredVars verifies that PATH, HOME, and adapter-specific
// vars survive the filter.
func TestBuildCLIEnv_AllowsRequiredVars(t *testing.T) {
	// Ensure PATH and HOME are set.
	origPath := os.Getenv("PATH")
	origHome := os.Getenv("HOME")
	if origPath == "" {
		os.Setenv("PATH", "/usr/bin:/bin")
		defer os.Setenv("PATH", origPath)
	}
	if origHome == "" {
		os.Setenv("HOME", "/root")
		defer os.Setenv("HOME", origHome)
	}

	// Set a CLAUDE_ var.
	os.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude-test")
	defer os.Unsetenv("CLAUDE_CONFIG_DIR")

	env := buildCLIEnv([]string{"CLAGENTIC_DISABLE_RECALL=1"})

	present := func(prefix string) bool {
		for _, kv := range env {
			if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
				return true
			}
		}
		return false
	}

	if !present("PATH=") {
		t.Error("PATH missing from CLI env")
	}
	if !present("HOME=") {
		t.Error("HOME missing from CLI env")
	}
	if !present("CLAUDE_CONFIG_DIR=") {
		t.Error("CLAUDE_CONFIG_DIR missing from CLI env")
	}

	// Extra vars are always appended.
	found := false
	for _, kv := range env {
		if kv == "CLAGENTIC_DISABLE_RECALL=1" {
			found = true
		}
	}
	if !found {
		t.Error("extra var CLAGENTIC_DISABLE_RECALL=1 missing from CLI env")
	}
}

// TestCLIEnvAllowed_KeyMatchesExactly tests exact-key matching (e.g. PATH vs PATHEXT).
func TestCLIEnvAllowed_KeyMatchesExactly(t *testing.T) {
	if !cliEnvAllowed("PATH=/usr/bin") {
		t.Error("PATH should be allowed")
	}
	// ANTHROPIC_API_KEY should be blocked — not in the allowlist
	if cliEnvAllowed("ANTHROPIC_API_KEY=secret") {
		t.Error("ANTHROPIC_API_KEY should be blocked")
	}
}

// TestCLIEnvAllowed_PrefixMatch tests prefix matching (e.g. LC_ALL, CLAUDE_BIN).
func TestCLIEnvAllowed_PrefixMatch(t *testing.T) {
	cases := []struct {
		kv      string
		allowed bool
	}{
		{"LC_ALL=en_US.UTF-8", true},
		{"CLAUDE_BIN=/usr/local/bin/claude", true},
		{"GEMINI_API_KEY=token", true}, // GEMINI_ prefix is allowed
		{"CODEX_HOME=/home/user/.codex", true},
		{"CLAGENTIC_DISABLE_RECALL=1", true},
		{"CLAGENTIC_CODEX_TIER=flagship", true},
		{"CLAGENTIC_ROUTER_TOKEN=secret", false},   // not in allowlist
		{"CLAGENTIC_ROUTER_ADMIN_TOKEN=s", false},  // not in allowlist
		{"SECRET_KEY=abc", false},
		{"DATABASE_URL=postgres://...", false},
	}
	for _, tc := range cases {
		got := cliEnvAllowed(tc.kv)
		if got != tc.allowed {
			t.Errorf("cliEnvAllowed(%q) = %v, want %v", tc.kv, got, tc.allowed)
		}
	}
}
