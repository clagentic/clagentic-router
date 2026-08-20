// internal/backend/gemini_cli.go — adapter for the gemini CLI (OAuth auth).
//
// Invokes: gemini -p "<prompt>" -m <model> --output-format json
// Input: user prompt via -p flag (NOT stdin — gemini takes prompt as a flag).
// Output: JSON {"session_id": "...", "response": "<text>", "stats": {...}}
//
// The CLI must be installed and authenticated via OAuth (gemini auth login)
// before this adapter can be used. No API key is required for OAuth auth;
// for API key auth set GEMINI_API_KEY in the environment instead.
//
// Model alias behavior: the model string from BackendConfig is passed directly
// to the -m flag without transformation. The gemini CLI resolves aliases at
// invocation time. Short aliases (flash, pro, flash-lite) and full names
// (gemini-2.5-flash, gemini-2.5-pro) are both accepted by the CLI.
//
// System prompt: the gemini CLI has no --system-prompt flag. When a system
// message is present it is prepended to the user prompt as:
//
//	"System: <system>\n\n<user>"
//
// Stderr noise: the gemini CLI always emits keychain / credential lines on
// stderr regardless of success. Do NOT treat non-empty stderr as an error —
// check exit code only.
//
// Proactive quota signal investigation (lr-c98c Slice E): the geminiOutput/
// geminiOutputStats/geminiModelStats/geminiTokenCounts types below are this
// package's only verified capture of `gemini --output-format json` shape —
// they were derived from this repo's own prior live use of the CLI (this
// adapter's Invoke path), not from documentation or from another CLI's
// shape (CLAUDE.md's "verify per-provider assumptions against the live
// source" applies here directly). That verified shape contains a
// session_id, a response string, and a per-model token-count stats block —
// no quota, rate_limit, or usage field of any kind, proactive or otherwise.
//
// This task's own instruction was to run the CLI fresh with verbose/JSON
// output and inspect all emitted event types. That step could NOT be
// completed for this PR: AMoS's Bash tool is allowlisted by guard-bash.py
// and does not permit invoking the `gemini` binary at all (not even
// `gemini --help`) — only git/lore/build-and-test tooling and a small set
// of scoped scripts are reachable. No other tool available to this agent
// (Read/Write/Edit/Glob/Grep/Agent) can execute a subprocess either. So this
// is a genuine, stated gap, not a documented certainty: the finding above
// is confirmed for the JSON output shape this adapter has already parsed in
// production, but a fresh verbose/streaming invocation (which might emit
// additional event types beyond the single JSON object this adapter reads)
// was never actually run for this task. Per this task's own framing, a
// documented negative is a first-class outcome — gemini_cli remains
// reactive-only (quota known only from error text via ParseResetTime) until
// someone with a permitted execution path re-verifies against a live,
// verbose run.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// geminiOutput is the JSON shape of gemini --output-format json stdout.
type geminiOutput struct {
	SessionID string            `json:"session_id"`
	Response  string            `json:"response"`
	Stats     geminiOutputStats `json:"stats"`
}

// geminiOutputStats mirrors the stats.models map in gemini JSON output.
type geminiOutputStats struct {
	// Models is keyed by the model name (e.g. "gemini-2.5-flash").
	// The key is dynamic so we unmarshal into a map.
	Models map[string]geminiModelStats `json:"models"`
}

// geminiModelStats holds token counts for one model entry.
type geminiModelStats struct {
	Tokens geminiTokenCounts `json:"tokens"`
}

// geminiTokenCounts holds the per-model token usage from the stats block.
type geminiTokenCounts struct {
	Input      int `json:"input"`
	Candidates int `json:"candidates"`
	Total      int `json:"total"`
}

// GeminiCLIAdapter calls the gemini CLI subprocess.
type GeminiCLIAdapter struct {
	id    string
	model string

	mu      sync.Mutex
	binPath string
}

// NewGeminiCLIAdapter creates a new adapter for the given backend ID and model.
// binPathOverride is the explicit path to the binary (empty = auto-resolve via
// GEMINI_BIN env var and PATH).
func NewGeminiCLIAdapter(id, model, binPathOverride string) *GeminiCLIAdapter {
	a := &GeminiCLIAdapter{id: id, model: model}
	// Resolve and log the binary path at construction time so misconfigurations
	// surface in the startup log rather than silently at first invoke.
	a.binPath = ResolveBinPath("gemini", binPathOverride, "GEMINI_BIN")
	return a
}

func (a *GeminiCLIAdapter) ID() string { return a.id }

// Capabilities reports the gemini CLI adapter's wire protocol support.
// The subprocess path passes a flat prompt via the -p flag — no tool,
// streaming, or image passthrough. Invoke never reads Request.Tools and
// never populates Response.ToolUses (lr-add405); see ClaudeCLIAdapter's
// Capabilities doc for the shared "no structured wire field" rationale.
func (a *GeminiCLIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: false, SupportsStreaming: false, SupportsImages: false}
}

// resolveBin returns the gemini binary path, resolving once and caching.
func (a *GeminiCLIAdapter) resolveBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.binPath == "" {
		a.binPath = ResolveBinary("gemini", "GEMINI_BIN")
	}
	return a.binPath
}

// refreshBin forces re-resolution (called after FileNotFoundError).
func (a *GeminiCLIAdapter) refreshBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.binPath = ResolveBinary("gemini", "GEMINI_BIN")
	return a.binPath
}

// Invoke calls the gemini CLI with the given request.
func (a *GeminiCLIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	bin := a.resolveBin()
	if bin == "" {
		return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "gemini binary not found on PATH"}
	}

	prompt, system := FormatMessages(req.Messages)
	if prompt == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty prompt after message formatting"}
	}

	// gemini CLI has no --system-prompt flag; prepend system message to prompt.
	if system != "" {
		prompt = "System: " + system + "\n\n" + prompt
	}

	args := []string{
		"-p", prompt,
		"--output-format", "json",
	}
	if a.model != "" {
		args = append(args, "-m", a.model)
	}

	// NO_COLOR suppresses ANSI escape codes that may appear in some output paths.
	// buildCLIEnv filters the daemon environment to the allowlist — router tokens
	// and API keys are not passed to the subprocess. (lr-c7ac)
	env := buildCLIEnv([]string{"NO_COLOR=1"})

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	// Neutral working directory so the subprocess does not inherit the
	// daemon's cwd. Defaults to DefaultWorkingDir ("/") when the caller does
	// not supply req.WorkingDir; a validated, caller-supplied directory (see
	// ResolveWorkingDir at the wire boundary) is honored instead. Previously
	// this adapter set no cmd.Dir at all and silently inherited whatever cwd
	// the daemon process happened to be started from — see claude_cli.go's
	// Invoke for the two-layer hook-suppression rationale used
	// there — that rationale does not transfer here: this adapter sets no
	// HOME override, so cmd.Dir is its only hook-suppression layer, not a
	// second layer on top of one. Defaulting it to "/" is still a strict
	// improvement over the prior no-cmd.Dir behavior regardless of that
	// asymmetry.
	cmd.Dir = req.WorkingDir
	if cmd.Dir == "" {
		cmd.Dir = DefaultWorkingDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
		// Re-resolve binary on FileNotFoundError.
		if strings.Contains(err.Error(), "no such file") {
			bin = a.refreshBin()
			if bin == "" {
				return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "gemini binary not found"}
			}
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}

	// Stderr always contains noise lines (keychain errors, credential messages).
	// Classify using stderr+stdout combined but do NOT fail on non-empty stderr alone.
	stderrStr := truncate(stderr.String(), 500)

	if err != nil || exitCode != 0 {
		// In --output-format json mode error JSON goes to stderr.
		errType := ClassifyError(stderrStr+stdout.String(), exitCode)
		resetAt := ParseResetTime(stderrStr + stdout.String())
		raw := stderrStr
		if raw == "" {
			raw = truncate(stdout.String(), 500)
		}
		slog.Debug("gemini_cli invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType, "reset_at", resetAt,
			"request_id", RequestIDFromCtx(ctx))
		return nil, &InvokeError{Type: errType, Raw: raw}
	}

	// Parse JSON output.
	var out geminiOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// Non-JSON output — treat stdout as plain text.
		content := strings.TrimSpace(stdout.String())
		if content == "" {
			return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from gemini CLI"}
		}
		return &Response{
			Content:             content,
			PromptTokensEst:     EstimateTokens(req.Messages),
			CompletionTokensEst: len(content) / 4,
		}, nil
	}

	content := strings.TrimSpace(out.Response)
	if content == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("empty response in gemini output: %s", truncate(stdout.String(), 200))}
	}

	// Sum token counts across all model entries in stats.models.
	promptTokens := 0
	completionTokens := 0
	for _, ms := range out.Stats.Models {
		promptTokens += ms.Tokens.Input
		completionTokens += ms.Tokens.Candidates
	}
	// Fall back to estimates when stats are absent.
	if promptTokens == 0 {
		promptTokens = EstimateTokens(req.Messages)
	}
	if completionTokens == 0 {
		completionTokens = len(content) / 4
	}

	slog.Debug("gemini_cli invoke ok", "backend", a.id, "content_len", len(content),
		"prompt_tokens", promptTokens, "completion_tokens", completionTokens,
		"request_id", RequestIDFromCtx(ctx))

	return &Response{
		Content:             content,
		PromptTokensEst:     promptTokens,
		CompletionTokensEst: completionTokens,
	}, nil
}
