// internal/backend/trust_sync.go — pre-accepts the Claude Code per-project
// trust dialog inside the isolated subprocess HOME (lr-4abfe9).
//
// # The gap this closes
//
// claudeSubprocessHome (see claude_cli.go) gives every claude CLI subprocess
// an isolated HOME so it never inherits the operator's real ~/.claude
// config, hooks, or memory. That isolation is correct. But Claude Code also
// gates tool use on a per-project trust dialog recorded at
// projects["<path>"].hasTrustDialogAccepted in <home>/.claude.json, and the
// isolated HOME's .claude.json ships with an empty projects map that
// nothing ever populates. "claude -p" (the invocation both claude_cli.go
// and codex_subagent.go use) is non-interactive by construction and has no
// flag to pre-accept a project's trust dialog — so every subprocess
// invocation against a real project directory fails before it can produce
// output.
//
// syncProjectTrust closes this by upserting exactly one key — the trust bit
// for the single directory the subprocess was already told to run in — into
// the isolated HOME's .claude.json, immediately before that subprocess is
// started. It never touches the operator's real .claude.json and never
// copies, merges, or reads any of its content; the two files are unrelated
// beyond sharing a schema.
//
// # Containment (BOBBIE finding bobbie.uncat.1, lr-4abfe9 follow-up)
//
// The trust bit this function sets is not cosmetic: once true, the claude
// CLI starts honoring that directory's .claude/settings.json
// permissions.allow entries, hooks, and project CLAUDE.md memory for every
// subprocess invocation against it. backend.ResolveWorkingDir validates only
// absolute/exists/is-a-directory — it is documented (this repo's CLAUDE.md)
// as NOT a containment control — so a caller-supplied working_dir passing
// ResolveWorkingDir must never be sufficient, on its own, to grant that
// trust. syncProjectTrust therefore takes a *TrustAllowlist and refuses
// (no-op, no write) for any dir not on it. See trust_allowlist.go for the
// full trust model, the fail-closed empty-allowlist default, and the
// DefaultWorkingDir ("/") posture.
//
// # Write discipline (concurrency)
//
// Multiple Invoke calls (claude_cli and codex_subagent both call this, and
// each can have several in-flight requests) can race to upsert the same
// .claude.json. This is a read-modify-write on a single shared file, so:
//
//  1. trustSyncMu serializes the entire read-modify-write-rename sequence
//     across all callers in this process. This is the same pattern
//     syncSubprocessCreds already uses for the sibling .credentials.json
//     race (claude_cli.go), reused here rather than reinvented.
//  2. The write itself is temp-file-then-rename, never an in-place write —
//     a crash or concurrent reader mid-write can only ever observe the old
//     complete file or the new complete file, never a truncated/partial one.
//     This is what keeps a crash from corrupting JSON the CLI can't parse.
//  3. The mutex is process-local, not cross-process. This process is the
//     only writer of this file (nothing else in the router, or on the host,
//     writes the isolated subprocess HOME's .claude.json), so a process-local
//     mutex is sufficient — there is no multi-process writer scenario to
//     guard against here, unlike credentials sync which races against the
//     operator's own `claude auth login` refreshing the source file.
//
// # Malformed pre-existing .claude.json / schema drift
//
// If the existing file fails to parse as JSON, or parses but "projects" is
// present with a non-object type, syncProjectTrust logs at Error and leaves
// the file untouched rather than guessing at a merge or overwriting content
// it cannot safely interpret — this is the "degrade safely and loudly"
// requirement: a future Claude Code release changing the .claude.json shape
// in an incompatible way surfaces as a loud log line and a continued (known,
// pre-existing) trust-dialog failure, never a silent corruption of operator
// state or a silently-ignored write.
package backend

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// trustSyncMu serializes read-modify-write access to the isolated HOME's
// .claude.json across all concurrent Invoke callers (claude_cli and
// codex_subagent both go through syncProjectTrust). See package doc.
var trustSyncMu sync.Mutex

// claudeUserConfig models only the fields syncProjectTrust reads or writes
// in .claude.json. Unknown top-level fields are preserved via rawExtra so a
// round-trip never drops content this code does not understand.
type claudeUserConfig struct {
	Projects map[string]claudeProjectConfig `json:"projects"`
	// rawExtra holds every top-level key this struct does not model,
	// preserved byte-for-byte across the read-modify-write.
	rawExtra map[string]json.RawMessage `json:"-"`
}

// claudeProjectConfig models only the trust field syncProjectTrust needs.
// Unknown per-project fields are preserved via rawExtra for the same reason
// as claudeUserConfig.rawExtra.
type claudeProjectConfig struct {
	HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
	rawExtra               map[string]json.RawMessage
}

// syncProjectTrust upserts projects[dir].hasTrustDialogAccepted = true into
// home's .claude.json, preserving every other key (top-level and
// per-project) already present. dir must be the exact, already-resolved
// working directory the subprocess is about to be started with (see
// backend.ResolveWorkingDir) — never inferred or normalized differently
// here, so the trust entry always matches what the CLI itself will look up
// for its own (identical) cwd.
//
// allowlist bounds the write: dir must satisfy allowlist.Allows(dir) or this
// call is a no-op — no read, no write, no mutation of any kind. This is the
// containment BOBBIE's finding (bobbie.uncat.1) required: trust is a
// human-in-the-loop safety gate, and this function must never widen it to a
// directory the operator did not explicitly opt in via
// config.Config.TrustedWorkingDirs. See trust_allowlist.go's package doc for
// the full trust model, the fail-closed default, and the DefaultWorkingDir
// ("/") posture. A nil allowlist (e.g. a call site that never wired one)
// also refuses every dir — TrustAllowlist.Allows is nil-safe and fails
// closed.
//
// home == "" or dir == "" are no-ops: home=="" means the isolated-HOME
// feature itself is off (mirrors the claudeSubprocessHome=="" checks at the
// claude_cli.go/codex_subagent.go call sites), and dir=="" would upsert a
// meaningless key.
//
// Failures (unwritable directory, unparseable existing file, unexpected
// "projects" shape) are logged at Error/Warn and leave the file untouched;
// they are never returned as a hard error, matching the discovery-failure
// posture in codex_discovery.go for cases where degrading to "feature off"
// is safe. Here "feature off" means the trust dialog error persists exactly
// as it does today — a known, pre-existing failure mode, not a regression.
func syncProjectTrust(home, dir string, allowlist *TrustAllowlist) {
	if home == "" || dir == "" {
		return
	}

	if !allowlist.Allows(dir) {
		slog.Debug("claude_cli: working_dir is not on trusted_working_dirs allowlist; "+
			"trust dialog will not be pre-accepted for this invocation — "+
			"the underlying Claude Code CLI call will fail with a trust-dialog error "+
			"unless this directory is added to trusted_working_dirs in router config",
			"dir", dir)
		return
	}

	trustSyncMu.Lock()
	defer trustSyncMu.Unlock()

	path := filepath.Join(home, ".claude.json")

	cfg, err := readClaudeUserConfig(path)
	if err != nil {
		slog.Error("claude_cli: could not read/parse isolated HOME .claude.json; "+
			"trust dialog will not be pre-accepted for this invocation — "+
			"the underlying Claude Code CLI call may fail with a trust-dialog error",
			"path", path, "err", err)
		return
	}

	if cfg.Projects == nil {
		cfg.Projects = make(map[string]claudeProjectConfig)
	}

	entry := cfg.Projects[dir]
	if entry.HasTrustDialogAccepted {
		// Already set for this exact dir — nothing to write. Keeps repeated
		// invocations against the same dir a no-op past the first.
		return
	}
	entry.HasTrustDialogAccepted = true
	cfg.Projects[dir] = entry

	if err := writeClaudeUserConfig(path, cfg); err != nil {
		slog.Error("claude_cli: could not write isolated HOME .claude.json; "+
			"trust dialog will not be pre-accepted for this invocation",
			"path", path, "err", err)
		return
	}

	slog.Debug("claude_cli: pre-accepted trust dialog for project dir", "path", path, "dir", dir)
}

// readClaudeUserConfig reads and parses path. A missing file is treated as
// an empty config (first-ever write for a freshly created isolated HOME) —
// not an error. Any other read/parse failure is returned so the caller can
// degrade loudly rather than guess.
func readClaudeUserConfig(path string) (*claudeUserConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &claudeUserConfig{Projects: map[string]claudeProjectConfig{}}, nil
		}
		return nil, err
	}

	// Decode into a raw top-level map first so unknown keys survive the
	// round-trip untouched.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}

	cfg := &claudeUserConfig{
		Projects: map[string]claudeProjectConfig{},
		rawExtra: map[string]json.RawMessage{},
	}

	for k, v := range top {
		if k != "projects" {
			cfg.rawExtra[k] = v
			continue
		}
		var rawProjects map[string]json.RawMessage
		if err := json.Unmarshal(v, &rawProjects); err != nil {
			return nil, err
		}
		for projPath, projRaw := range rawProjects {
			var projTop map[string]json.RawMessage
			if err := json.Unmarshal(projRaw, &projTop); err != nil {
				return nil, err
			}
			var pc claudeProjectConfig
			if trustRaw, ok := projTop["hasTrustDialogAccepted"]; ok {
				if err := json.Unmarshal(trustRaw, &pc.HasTrustDialogAccepted); err != nil {
					return nil, err
				}
				delete(projTop, "hasTrustDialogAccepted")
			}
			pc.rawExtra = projTop
			cfg.Projects[projPath] = pc
		}
	}

	return cfg, nil
}

// writeClaudeUserConfig serializes cfg back to path via temp-file-then-
// rename so a crash mid-write, or a concurrent reader (the claude CLI
// itself, if invoked at the same instant by another in-flight request),
// never observes a partial file. Mirrors the atomic-write discipline
// syncSubprocessCreds already uses for .credentials.json in claude_cli.go.
func writeClaudeUserConfig(path string, cfg *claudeUserConfig) error {
	top := make(map[string]json.RawMessage, len(cfg.rawExtra)+1)
	for k, v := range cfg.rawExtra {
		top[k] = v
	}

	projects := make(map[string]json.RawMessage, len(cfg.Projects))
	for projPath, pc := range cfg.Projects {
		projTop := make(map[string]json.RawMessage, len(pc.rawExtra)+1)
		for k, v := range pc.rawExtra {
			projTop[k] = v
		}
		trustRaw, err := json.Marshal(pc.HasTrustDialogAccepted)
		if err != nil {
			return err
		}
		projTop["hasTrustDialogAccepted"] = trustRaw
		projRaw, err := json.Marshal(projTop)
		if err != nil {
			return err
		}
		projects[projPath] = projRaw
	}
	projectsRaw, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	top["projects"] = projectsRaw

	data, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
