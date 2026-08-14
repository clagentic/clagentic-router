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

// TestBuildCLIEnv_CodexCLIBlocksSecrets verifies that codex_cli's subprocess
// env (buildCLIEnv(nil) — see codex_cli.go's Invoke, which passes no extra
// vars) filters daemon secrets the same as every other CLI adapter. codex_cli
// was previously the only CLI adapter that never called buildCLIEnv at all
// and inherited the daemon's entire environment (lr-bd5dc0).
func TestBuildCLIEnv_CodexCLIBlocksSecrets(t *testing.T) {
	os.Setenv("CLAGENTIC_ROUTER_TOKEN", "super-secret-token")
	os.Setenv("OPENAI_API_KEY", "sk-openai-test")
	defer func() {
		os.Unsetenv("CLAGENTIC_ROUTER_TOKEN")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	origHome := os.Getenv("HOME")
	if origHome == "" {
		os.Setenv("HOME", "/root")
		defer os.Setenv("HOME", origHome)
	}
	os.Setenv("CODEX_HOME", "/home/user/.codex")
	defer os.Unsetenv("CODEX_HOME")

	// codex_cli.go's Invoke calls buildCLIEnv(nil) — no extra vars, no HOME
	// override (unlike claude_cli/codex_subagent).
	env := buildCLIEnv(nil)

	for _, secret := range []string{
		"CLAGENTIC_ROUTER_TOKEN=super-secret-token",
		"OPENAI_API_KEY=sk-openai-test",
	} {
		for _, kv := range env {
			if kv == secret {
				t.Errorf("secret var leaked into codex_cli subprocess env: %q", secret)
			}
		}
	}

	present := func(prefix string) bool {
		for _, kv := range env {
			if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
				return true
			}
		}
		return false
	}
	// HOME and CODEX_ are both allowlisted — auth.json resolution for
	// ChatGPT-Plus OAuth (via HOME or CODEX_HOME) must survive the filter.
	if !present("HOME=") {
		t.Error("HOME missing from codex_cli subprocess env — would break auth.json resolution")
	}
	if !present("CODEX_HOME=") {
		t.Error("CODEX_HOME missing from codex_cli subprocess env")
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

// TestCLIEnvAllowed_PrefixMatch tests prefix matching (e.g. LC_ALL) and exact
// literal matching (e.g. CLAUDE_BIN, CODEX_HOME).
func TestCLIEnvAllowed_PrefixMatch(t *testing.T) {
	cases := []struct {
		kv      string
		allowed bool
	}{
		{"LC_ALL=en_US.UTF-8", true},
		{"CLAUDE_BIN=/usr/local/bin/claude", true},
		{"CODEX_HOME=/home/user/.codex", true},
		{"CLAGENTIC_DISABLE_RECALL=1", true},
		{"CLAGENTIC_CODEX_TIER=flagship", true},
		{"CLAGENTIC_ROUTER_TOKEN=secret", false},  // not in allowlist
		{"CLAGENTIC_ROUTER_ADMIN_TOKEN=s", false}, // not in allowlist
		{"SECRET_KEY=abc", false},
		{"DATABASE_URL=postgres://...", false},
		// Narrowing: bare CLAUDE_/CODEX_/GEMINI_ prefixes used to admit
		// anything with that stem, including secret-shaped vars.
		// Literal-only matching now blocks these.
		{"GEMINI_API_KEY=token", false},
		{"CODEX_API_KEY=token", false},
		{"CLAUDE_API_KEY=token", false},
		// Widening: AWS SDK standard credential/config vars are admitted
		// (Bedrock-fronted codex_cli), listed as literals — see env.go
		// package doc for why a suffix denylist was rejected (it would
		// also block AWS_SECRET_ACCESS_KEY/AWS_SESSION_TOKEN, which are
		// the exact vars this widening exists to admit).
		{"AWS_PROFILE=my-profile", true},
		{"AWS_REGION=us-east-1", true},
		{"AWS_DEFAULT_REGION=us-east-1", true},
		{"AWS_ACCESS_KEY_ID=AKIA...", true},
		{"AWS_SECRET_ACCESS_KEY=abc", true},
		{"AWS_SESSION_TOKEN=abc", true},
		{"AWS_ROLE_ARN=arn:aws:iam::x", true},
		{"AWS_WEB_IDENTITY_TOKEN_FILE=/p", true},
		{"AWS_SDK_LOAD_CONFIG=1", true},
		{"AWS_CONFIG_FILE=/p", true},
		{"AWS_SHARED_CREDENTIALS_FILE=/p", true},
		// An AWS var NOT in the enumerated literal set must still be
		// blocked — proves this is literal matching, not a disguised
		// AWS_ prefix.
		{"AWS_UNLISTED_MADE_UP_VAR=x", false},
		// Vertex AI / Azure OpenAI SDK credential vars, pre-empting the
		// identical gap AWS_ absence caused for Bedrock — not known to be
		// in live use by any adapter today.
		{"GOOGLE_APPLICATION_CREDENTIALS=/p", true},
		{"GOOGLE_CLOUD_PROJECT=my-project", true},
		{"CLOUDSDK_CORE_PROJECT=my-project", true},
		{"AZURE_TENANT_ID=abc", true},
		{"AZURE_CLIENT_SECRET=abc", true},
	}
	for _, tc := range cases {
		got := cliEnvAllowed(tc.kv)
		if got != tc.allowed {
			t.Errorf("cliEnvAllowed(%q) = %v, want %v", tc.kv, got, tc.allowed)
		}
	}
}
