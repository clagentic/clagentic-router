// internal/backend/claude_cli.go — adapter for the claude CLI (OAuth auth).
//
// Invokes: claude --print --verbose --output-format stream-json --model <model> [--system-prompt <s>]
// Input: user prompt via stdin.
// Output: newline-delimited JSON stream. The final "result" line carries the
// response text and cost. Intermediate lines include rate_limit_event lines
// which are parsed and attached to the Response.
//
// The CLI must be installed and authenticated via OAuth (claude auth login)
// before this adapter can be used. No API key is required or supported here —
// for API key auth use anthropic_api adapter instead.
//
// Model alias behavior: the model string from BackendConfig is passed directly
// to the --model flag without transformation. The claude CLI resolves family
// aliases at invocation time — "claude-sonnet" resolves to the current default
// Sonnet version, "claude-haiku" to the current Haiku, "fable" to the current
// Fable (the top tier, above opus), etc. This means:
//   - Pinned version (claude-sonnet-4-6): always uses that exact release.
//   - Family alias (claude-sonnet): tracks the CLI's current default, which
//     updates with CLI upgrades. No restart or config change needed.
//
// The router does not parse, cache, or expand model strings. Resolution is
// entirely delegated to the claude CLI on each invocation.
package backend

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
)

// claudeSubprocessHome is the HOME directory injected into every claude CLI subprocess.
// It must contain a ~/.claude directory with credentials but NO settings.json hooks —
// this prevents the operator's SessionStart/UserPromptSubmit hooks from firing inside
// router-spawned sessions, which would pollute stdout with hook telemetry and cause
// parseStreamJSON to fail with auth misclassification.
//
// Resolution order:
//  1. CLAGENTIC_ROUTER_SUBPROCESS_HOME env var (operator override)
//  2. {state_dir}/claude-home — created at package init if absent
//
// The credentials must be present at {home}/.claude/.credentials.json.
// At init time the subprocess home directory and a stub settings.json are created.
// Credential freshness is maintained by syncSubprocessCreds, called on each Invoke.
//
// This isolation also means {home}/.claude.json starts with an empty
// projects map, so Claude Code's per-project trust dialog has never been
// accepted for any directory a subprocess is invoked against — and "claude
// --print" (non-interactive) has no flag to accept it. syncProjectTrust
// (trust_sync.go), called from Invoke immediately before cmd.Run(), closes
// that gap by pre-accepting trust for exactly the directory the subprocess
// was already told to run in, bounded by the adapter's TrustAllowlist (see
// trust_allowlist.go) — a dir not on that operator-controlled list gets no
// write and the subprocess fails exactly as it did before lr-4abfe9.
// codex_subagent.go shares this same HOME and the same claude binary, so it
// calls syncProjectTrust too; codex_cli.go and gemini_cli.go invoke
// different binaries with no HOME override and are unaffected — see
// trust_sync.go package doc for the full breadth accounting and
// write-concurrency discipline (lr-4abfe9).
var claudeSubprocessHome = func() string {
	// Operator override takes precedence.
	if v := os.Getenv("CLAGENTIC_ROUTER_SUBPROCESS_HOME"); v != "" {
		return v
	}

	// Default: state dir sibling, which persists across restarts.
	stateDir := os.Getenv("CLAGENTIC_ROUTER_STATE_DIR")
	if stateDir == "" {
		stateDir = "/var/lib/clagentic-router"
	}
	home := filepath.Join(stateDir, "claude-home")
	claudeDir := filepath.Join(home, ".claude")

	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		slog.Warn("claude_cli: failed to create subprocess home, hooks may fire",
			"path", home, "err", err)
		return ""
	}

	// Write an empty settings.json to suppress hook loading.
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		// Minimal valid settings: empty object — no hooks, no MCP servers.
		if err2 := os.WriteFile(settingsPath, []byte("{}\n"), 0600); err2 != nil {
			slog.Warn("claude_cli: failed to write subprocess settings.json, hooks may fire",
				"path", settingsPath, "err", err2)
		}
	}

	return home
}()

// resolveDaemonHomeFunc is the resolver used by syncSubprocessCreds to locate the daemon's
// home directory. It is a package-level variable so tests can inject a replacement
// without requiring OS-level env manipulation (e.g. to simulate a missing /etc/passwd).
// Production code always uses the real resolveDaemonHome implementation.
var resolveDaemonHomeFunc = resolveDaemonHome

// resolveDaemonHome returns the daemon's home directory for locating source credentials.
//
// Resolution order:
//  1. HOME environment variable (standard POSIX; set by most service managers)
//  2. os/user.Current().HomeDir (NSS / passwd lookup; works when HOME is unset)
//
// Returns an error if neither source yields a non-empty absolute path, so callers can
// emit a clear diagnostic rather than proceeding with a relative path that silently
// resolves against the daemon's cwd (typically "/" in a systemd unit).
func resolveDaemonHome() (string, error) {
	if h := os.Getenv("HOME"); h != "" {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("HOME env var is unset and os/user lookup failed: %w; "+
			"set HOME in the service environment (e.g. Environment=HOME=/home/router in the systemd unit)", err)
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("HOME env var is unset and os/user returned an empty HomeDir; " +
			"set HOME in the service environment (e.g. Environment=HOME=/home/router in the systemd unit)")
	}
	return u.HomeDir, nil
}

// credsSyncMu guards concurrent credential resync calls from concurrent Invoke calls.
var credsSyncMu sync.Mutex

// credsSyncLastInfo caches the os.FileInfo of the source credentials file from the
// most recent successful sync so we can skip the copy when nothing has changed.
// Guarded by credsSyncMu.
var credsSyncLastInfo os.FileInfo

// syncSubprocessCreds ensures that the subprocess copy of .credentials.json is
// current with respect to the daemon's own HOME credentials.  It is called on
// every Invoke so that OAuth token rotations (which happen on the host, not in
// the subprocess home) propagate before the next request is dispatched.
//
// Algorithm:
//  1. Stat the source (daemon HOME).  If absent, log and return — do not clobber
//     a working copy with nothing.
//  2. Compare source mtime+size against the cached last-sync info.  If unchanged,
//     return immediately (hot path: a single Stat call per Invoke).
//  3. Compare SHA-256 of source vs copy.  This guards against clock skew and
//     filesystem timestamp granularity edge cases.
//  4. If different, write to a temp file then rename atomically so concurrent
//     reads of the destination never see a partial write.
//
// The function is safe for concurrent callers: credsSyncMu serialises the
// stat+copy path.  The hot-path stat (step 2) is also inside the lock because
// credsSyncLastInfo is shared state.
func syncSubprocessCreds(subprocessHome string) {
	if subprocessHome == "" {
		return
	}

	// Acquire the lock before calling resolveDaemonHomeFunc so that test
	// goroutines swapping the func variable cannot race with a concurrent
	// Invoke that is mid-resolution.  The resolver only reads env vars and
	// calls a syscall; holding the mutex across it is fine.
	credsSyncMu.Lock()
	defer credsSyncMu.Unlock()

	daemonHome, err := resolveDaemonHomeFunc()
	if err != nil {
		// Hard misconfiguration: HOME is unresolvable. Log at ERROR so this is
		// impossible to miss in the service journal, and return without touching
		// the subprocess copy. All claude_cli backends will fail to authenticate
		// until the operator sets HOME in the service environment.
		slog.Error("claude_cli: cannot resolve daemon home directory; credential sync disabled — "+
			"set HOME in the service environment (e.g. Environment=HOME=/home/router in the systemd unit)",
			"err", err)
		return
	}

	// Guard against a non-absolute home (e.g. empty string from a buggy lookup).
	// filepath.Join("", ...) collapses to a relative path that resolves against
	// the daemon's cwd ("/") — never what we want.
	if !filepath.IsAbs(daemonHome) {
		slog.Error("claude_cli: resolved home directory is not an absolute path; credential sync disabled",
			"home", daemonHome)
		return
	}

	src := filepath.Join(daemonHome, ".claude", ".credentials.json")
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	srcInfo, statErr := os.Stat(src)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// Source credentials absent — do not clobber an existing subprocess copy.
			// This is the legitimate case where HOME resolves but the file doesn't exist
			// (e.g. the operator has not run "claude auth login" yet).
			slog.Warn("claude_cli: source credentials not found; subprocess copy unchanged",
				"src", src)
		} else {
			slog.Warn("claude_cli: could not stat source credentials; subprocess copy unchanged",
				"src", src, "err", statErr)
		}
		return
	}

	// Fast path: source stat unchanged since last sync.
	if credsSyncLastInfo != nil &&
		srcInfo.ModTime().Equal(credsSyncLastInfo.ModTime()) &&
		srcInfo.Size() == credsSyncLastInfo.Size() {
		return
	}

	// Slow path: read both files and compare content to handle clock skew.
	srcData, err := os.ReadFile(src)
	if err != nil {
		slog.Warn("claude_cli: could not read source credentials; subprocess copy unchanged",
			"src", src, "err", err)
		return
	}

	dstData, _ := os.ReadFile(dst) // missing dst is fine — we will create it

	srcHash := sha256.Sum256(srcData)
	dstHash := sha256.Sum256(dstData)

	if srcHash == dstHash {
		// Content identical; update cached info so future calls take the fast path.
		credsSyncLastInfo = srcInfo
		return
	}

	// Content differs — write atomically via temp file + rename.
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, srcData, 0600); err != nil {
		slog.Warn("claude_cli: could not write temp credentials; subprocess copy unchanged",
			"tmp", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		slog.Warn("claude_cli: could not rename credentials into place; subprocess copy may be stale",
			"tmp", tmp, "dst", dst, "err", err)
		_ = os.Remove(tmp)
		return
	}

	credsSyncLastInfo = srcInfo
	slog.Info("claude_cli: refreshed subprocess credentials",
		"src", src, "dst", dst, "size", srcInfo.Size())
}

// claudeOutput is the JSON shape of one line in claude --output-format stream-json stdout,
// and also of the single JSON object from --output-format json (used by codex_subagent).
// The "result" type carries the final response; "error" carries failure details.
// Other event types (e.g. rate_limit_event) are handled separately in ratelimit.go.
type claudeOutput struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	Result  string  `json:"result"`
	CostUSD float64 `json:"cost_usd"`
	// Error fields (when type="error")
	Error   string `json:"error"`
	Message string `json:"message"`
}

// ClaudeCLIAdapter calls the claude CLI subprocess.
type ClaudeCLIAdapter struct {
	id           string
	model        string
	effort       EffortLevel
	thinkingMode ThinkingMode
	timeout      func() interface{} // unused; timeout via context

	// trustedDirs bounds syncProjectTrust's write to operator-opted-in
	// directories (config.Config.TrustedWorkingDirs). nil (a call site that
	// never set it — e.g. an existing test built with a struct literal
	// rather than the constructor) is treated as an empty allowlist by
	// TrustAllowlist.Allows, which is nil-safe and fails closed: no write,
	// ever, until an operator explicitly configures trusted_working_dirs.
	// See trust_allowlist.go for the full trust model.
	trustedDirs *TrustAllowlist

	mu      sync.Mutex
	binPath string
}

// NewClaudeCLIAdapter creates a new adapter for the given backend ID and model.
// binPathOverride is the explicit path to the binary (empty = auto-resolve).
// effort and thinkingMode are noted at construction; see Invoke for current support status.
// trustedDirs bounds which working directories syncProjectTrust is allowed
// to mark trusted (nil is safe and trusts nothing — see TrustAllowlist).
func NewClaudeCLIAdapter(id, model, binPathOverride string, effort EffortLevel, thinkingMode ThinkingMode, trustedDirs *TrustAllowlist) *ClaudeCLIAdapter {
	a := &ClaudeCLIAdapter{id: id, model: model, effort: effort, thinkingMode: thinkingMode, trustedDirs: trustedDirs}
	// Resolve and log the binary path at construction time so misconfigurations
	// surface in the startup log rather than silently at first invoke.
	a.binPath = ResolveBinPath("claude", binPathOverride, "CLAUDE_BIN")
	// TODO(lr-7d2f): wire effort and thinkingMode to claude CLI flags once the CLI
	// exposes --effort / --thinking flags. At time of writing (2026-05-28) the claude
	// CLI has no stable --effort flag. effort/thinkingMode config values on claude_cli
	// backends are accepted but have no effect; a warning is logged below.
	if effort != "" || thinkingMode != ThinkingOff {
		slog.Warn("claude_cli: effort/thinking_mode configured but not yet supported by the CLI; fields are ignored",
			"backend", id, "effort", effort, "thinking_mode", thinkingMode)
	}
	return a
}

func (a *ClaudeCLIAdapter) ID() string { return a.id }

// Capabilities reports the claude CLI adapter's wire protocol support.
// The subprocess path formats messages into a flat prompt string (see
// FormatMessages) with no tool, streaming, or image passthrough.
func (a *ClaudeCLIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: false, SupportsStreaming: false, SupportsImages: false}
}

// resolveBin returns the claude binary path, resolving once and caching.
func (a *ClaudeCLIAdapter) resolveBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.binPath == "" {
		a.binPath = ResolveBinary("claude", "CLAUDE_BIN")
	}
	return a.binPath
}

// refreshBin forces re-resolution (called after FileNotFoundError).
func (a *ClaudeCLIAdapter) refreshBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.binPath = ResolveBinary("claude", "CLAUDE_BIN")
	return a.binPath
}

// Invoke calls the claude CLI with the given request.
func (a *ClaudeCLIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	// Refresh subprocess credentials before each invocation.  The host OAuth token
	// rotates over time; if the subprocess copy is stale the CLI returns an auth
	// error and the backend is marked offline.  syncSubprocessCreds is a no-op when
	// the source is unchanged (mtime+size fast path).
	syncSubprocessCreds(claudeSubprocessHome)

	bin := a.resolveBin()
	if bin == "" {
		return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "claude binary not found on PATH"}
	}

	prompt, system := FormatMessages(req.Messages)
	if prompt == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty prompt after message formatting"}
	}

	// --verbose is required when combining --print with --output-format stream-json;
	// the claude CLI (>=2.1.173) rejects the combination without it. The flag does
	// not change the stream-json output format — it only unlocks the mode. (lr-1994)
	args := []string{
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--max-turns", "1",
	}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	// Prevent recursive hook firing when called from within a Claude session.
	// buildCLIEnv filters the daemon environment to the allowlist — router tokens
	// and API keys are not passed to the subprocess. (lr-c7ac)
	//
	// The subprocess HOME is overridden to claudeSubprocessHome (see its doc
	// above), which points at a stub config dir with an empty settings.json —
	// this is what suppresses hooks, MCP servers, and memory loaded from
	// ~/.claude, not any CLAUDE_CONFIG_DIR setting. Without it, SessionStart
	// hooks (e.g. LORE) fire on every router invocation, polluting stdout with
	// hook telemetry and occasionally triggering auth misclassification in
	// parseStreamJSON.
	extra := []string{"CLAGENTIC_DISABLE_RECALL=1"}
	if claudeSubprocessHome != "" {
		extra = append(extra, "HOME="+claudeSubprocessHome)
	}
	env := buildCLIEnv(extra)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = env
	// Working directory: the HOME override above is the first of two hook-
	// suppression layers and covers ~/.claude-scoped hooks regardless of cwd.
	// cmd.Dir is the second, narrower layer — it covers project-scoped
	// ./CLAUDE.md and ./.claude/settings.json, which the HOME override does
	// not touch. Defaults to DefaultWorkingDir ("/") when the caller does not
	// supply req.WorkingDir; a validated, caller-supplied directory (see
	// ResolveWorkingDir at the wire boundary) is honored instead.
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = DefaultWorkingDir
	}

	// Pre-accept the Claude Code per-project trust dialog for this exact
	// directory inside the isolated subprocess HOME, before the subprocess
	// starts. Without this, "claude --print" against a real project
	// directory fails non-interactively — there is no flag to accept the
	// dialog, and the isolated HOME's .claude.json starts with an empty
	// projects map. Bounded by a.trustedDirs: a dir not on the operator's
	// trusted_working_dirs allowlist gets no write and the subprocess fails
	// exactly as it did before lr-4abfe9. See trust_sync.go and
	// trust_allowlist.go package docs for the write discipline, isolation
	// guarantees, and trust model (lr-4abfe9).
	syncProjectTrust(claudeSubprocessHome, cmd.Dir, a.trustedDirs)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
		// Re-resolve binary on FileNotFoundError
		if strings.Contains(err.Error(), "no such file") {
			bin = a.refreshBin()
			if bin == "" {
				return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "claude binary not found"}
			}
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}

	stderrStr := truncate(stderr.String(), 500)

	if err != nil || exitCode != 0 {
		errType := ClassifyError(stderrStr+stdout.String(), exitCode)
		resetAt := ParseResetTime(stderrStr + stdout.String())
		raw := stderrStr
		if raw == "" {
			raw = truncate(stdout.String(), 500)
		}
		slog.Debug("claude_cli invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType, "reset_at", resetAt,
			"request_id", RequestIDFromCtx(ctx))
		ie := &InvokeError{Type: errType, Raw: raw}
		return nil, ie
	}

	// Parse stream-json output: scan line by line.
	// The stream emits one JSON object per line. We look for the "result" line
	// (carries the final response) and rate_limit_event lines (quota telemetry).
	// All other line types are silently ignored for forward compatibility.
	return parseStreamJSON(stdout.Bytes(), req, a.id)
}

// parseStreamJSON scans the stream-json output lines, extracts the result and any
// rate_limit_event, and returns a populated Response. Exported for testing.
func parseStreamJSON(data []byte, req *Request, backendID string) (*Response, error) {
	var (
		resultLine   *claudeOutput
		rateLimitEvt *RateLimitEvent
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Try rate_limit_event first — these lines have a distinctive type field.
		if evt := parseRateLimitEvent(line); evt != nil {
			rateLimitEvt = evt
			continue
		}

		// Decode the line to check its type field.
		var sl claudeOutput
		if err := json.Unmarshal(line, &sl); err != nil {
			// Ignore unparseable lines — forward-compat with new event types.
			continue
		}

		switch sl.Type {
		case "result":
			resultLine = &sl
		case "error":
			raw := sl.Error
			if raw == "" {
				raw = sl.Message
			}
			errType := ClassifyError(raw, 0)
			return nil, &InvokeError{Type: errType, Raw: truncate(raw, 500)}
		}
		// All other event types are ignored.
	}

	if resultLine == nil {
		// No result line found — fall back to treating stdout as plain text if non-empty.
		content := strings.TrimSpace(string(data))
		if content == "" {
			return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from claude CLI"}
		}
		return &Response{
			Content:             content,
			PromptTokensEst:     EstimateTokens(req.Messages),
			CompletionTokensEst: len(content) / 4,
			RateLimitEvent:      rateLimitEvt,
		}, nil
	}

	// Handle error type carried in the result line itself
	if resultLine.Type == "error" || resultLine.Error != "" {
		raw := resultLine.Error
		if raw == "" {
			raw = resultLine.Message
		}
		errType := ClassifyError(raw, 0)
		return nil, &InvokeError{Type: errType, Raw: truncate(raw, 500)}
	}

	content := strings.TrimSpace(resultLine.Result)
	if content == "" {
		return nil, &InvokeError{
			Type: ErrTypeSchema,
			Raw:  fmt.Sprintf("empty result in claude output: %s", truncate(string(data), 200)),
		}
	}

	promptEst := EstimateTokens(req.Messages)
	completionEst := len(content) / 4

	slog.Debug("claude_cli invoke ok",
		"backend", backendID,
		"content_len", len(content),
		"cost_usd", resultLine.CostUSD,
		"has_rate_limit_event", rateLimitEvt != nil,
	)

	return &Response{
		Content:             content,
		PromptTokensEst:     promptEst,
		CompletionTokensEst: completionEst,
		CostUSD:             resultLine.CostUSD,
		RateLimitEvent:      rateLimitEvt,
	}, nil
}
