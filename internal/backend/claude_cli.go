// internal/backend/claude_cli.go — adapter for the claude CLI (OAuth auth).
//
// Invokes: claude --print --model <model> --output-format json [--system-prompt <s>]
// Input: user prompt via stdin.
// Output: JSON {"result": "...", "cost_usd": ...}
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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// claudeOutput is the JSON shape of claude --output-format json stdout.
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
	if binPathOverride != "" {
		a.binPath = binPathOverride
	}
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

	args := []string{
		"--print",
		"--output-format", "json",
		"--max-turns", "1",
	}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	// Prevent recursive hook firing when called from within a Claude session.
	env := append(os.Environ(), "CLAGENTIC_DISABLE_RECALL=1")

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = env

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

	// Parse JSON output
	var out claudeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		// Non-JSON output — try treating stdout as plain text
		content := strings.TrimSpace(stdout.String())
		if content == "" {
			return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from claude CLI"}
		}
		return &Response{
			Content:             content,
			PromptTokensEst:     EstimateTokens(req.Messages),
			CompletionTokensEst: len(content) / 4,
		}, nil
	}

	// Handle error type in JSON response
	if out.Type == "error" || out.Error != "" {
		raw := out.Error
		if raw == "" {
			raw = out.Message
		}
		errType := ClassifyError(raw, 0)
		return nil, &InvokeError{Type: errType, Raw: truncate(raw, 500)}
	}

	content := strings.TrimSpace(out.Result)
	if content == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("empty result in claude output: %s", truncate(stdout.String(), 200))}
	}

	promptEst := EstimateTokens(req.Messages)
	completionEst := len(content) / 4

	slog.Debug("claude_cli invoke ok", "backend", a.id, "content_len", len(content), "cost_usd", out.CostUSD,
		"request_id", RequestIDFromCtx(ctx))

	return &Response{
		Content:             content,
		PromptTokensEst:     promptEst,
		CompletionTokensEst: completionEst,
		CostUSD:             out.CostUSD,
	}, nil
}
