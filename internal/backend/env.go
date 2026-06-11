// internal/backend/env.go — filtered environment construction for CLI subprocess adapters.
//
// CLI adapters (claude_cli, codex_subagent, gemini_cli) inherit only a curated
// subset of the daemon's environment. This prevents API keys, router tokens, and
// deployment secrets from leaking into subprocess environments. (lr-c7ac)
package backend

import (
	"os"
	"strings"
)

// cliEnvAllowlist is the set of env var prefixes passed to CLI subprocess adapters.
// API keys, router tokens, and deployment secrets must not be inherited.
var cliEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"SHELL",
	"TERM",
	"LANG",
	"LC_",
	"XDG_",
	"TMPDIR",
	"TMP",
	"TEMP",
	// Claude CLI needs these
	"CLAUDE_",
	// Codex CLI needs these
	"CODEX_",
	// Gemini CLI needs these
	"GEMINI_",
	// Clagentic session vars that adapters intentionally propagate
	"CLAGENTIC_DISABLE_RECALL",
	"CLAGENTIC_CODEX_TIER",
}

// buildCLIEnv constructs a filtered environment for CLI subprocess adapters.
// Only variables matching cliEnvAllowlist prefixes are inherited from the daemon.
// extra is appended last and takes precedence — any key that appears in extra
// is excluded from the daemon environment to prevent duplicate/shadowed entries.
func buildCLIEnv(extra []string) []string {
	// Build set of keys overridden by extra so we can drop them from daemon env.
	override := make(map[string]struct{}, len(extra))
	for _, kv := range extra {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		override[key] = struct{}{}
	}

	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if idx := strings.IndexByte(kv, '='); idx >= 0 {
			key = kv[:idx]
		}
		if _, overridden := override[key]; overridden {
			continue // extra wins
		}
		if cliEnvAllowed(kv) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func cliEnvAllowed(kv string) bool {
	key := kv
	if idx := strings.IndexByte(kv, '='); idx >= 0 {
		key = kv[:idx]
	}
	for _, prefix := range cliEnvAllowlist {
		if key == prefix || strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
