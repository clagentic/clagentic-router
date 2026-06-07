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
	if binPathOverride != "" {
		a.binPath = binPathOverride
	}
	return a
}

func (a *GeminiCLIAdapter) ID() string { return a.id }

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
