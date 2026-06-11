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
// Sonnet version, "claude-haiku" to the current Haiku, etc. This means:
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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
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
// At init time, if the target home lacks credentials but the daemon's own HOME has
// them, they are copied automatically (convenience bootstrap — not repeated after that).
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

	// Bootstrap credentials from the daemon's own HOME if not already present.
	credsTarget := filepath.Join(claudeDir, ".credentials.json")
	if _, err := os.Stat(credsTarget); os.IsNotExist(err) {
		daemonHome := os.Getenv("HOME")
		credsSrc := filepath.Join(daemonHome, ".claude", ".credentials.json")
		if data, err2 := os.ReadFile(credsSrc); err2 == nil {
			if err3 := os.WriteFile(credsTarget, data, 0600); err3 == nil {
				slog.Info("claude_cli: bootstrapped subprocess credentials",
					"src", credsSrc, "dst", credsTarget)
			}
		}
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

	mu      sync.Mutex
	binPath string
}

// NewClaudeCLIAdapter creates a new adapter for the given backend ID and model.
// binPathOverride is the explicit path to the binary (empty = auto-resolve).
// effort and thinkingMode are noted at construction; see Invoke for current support status.
func NewClaudeCLIAdapter(id, model, binPathOverride string, effort EffortLevel, thinkingMode ThinkingMode) *ClaudeCLIAdapter {
	a := &ClaudeCLIAdapter{id: id, model: model, effort: effort, thinkingMode: thinkingMode}
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
	// CLAUDE_CONFIG_DIR is set to an empty directory so the subprocess does not
	// load any hooks, MCP servers, or memory from the operator's ~/.claude config.
	// Without this, SessionStart hooks (e.g. LORE) fire on every router invocation,
	// polluting stdout with hook telemetry and occasionally triggering auth
	// misclassification in parseStreamJSON.
	extra := []string{"CLAGENTIC_DISABLE_RECALL=1"}
	if claudeSubprocessHome != "" {
		extra = append(extra, "HOME="+claudeSubprocessHome)
	}
	env := buildCLIEnv(extra)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = env
	// Set a neutral working directory so the subprocess does not inherit the
	// daemon's cwd (which may be a project directory with CLAUDE.md hooks).
	cmd.Dir = "/"

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
