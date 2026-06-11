// internal/backend/codex_subagent.go — adapter for codex via the claude subagent.
//
// Invokes: claude -p --agent codex --output-format json
// with CLAGENTIC_CODEX_TIER=<tier> in the environment.
//
// The codex subagent (~/.claude/agents/codex.md) reads ~/.codex/models.json
// to resolve the tier to a concrete model. This is the "openai-via-codex-subagent"
// provider path from the relay registry — it avoids hardcoding model strings and
// lets the models.json file be the single source of truth for tier → model.
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

// CodexSubagentAdapter calls codex via the claude -p --agent codex path.
type CodexSubagentAdapter struct {
	id   string
	tier string // flagship | mini | spark

	mu      sync.Mutex
	binPath string
}

// NewCodexSubagentAdapter creates an adapter for the given tier.
// tier should be one of: flagship, mini, spark.
func NewCodexSubagentAdapter(id, tier, binPathOverride string) *CodexSubagentAdapter {
	a := &CodexSubagentAdapter{id: id, tier: tier}
	// Resolve and log the binary path at construction time so misconfigurations
	// surface in the startup log rather than silently at first invoke.
	// codex_subagent invokes the claude CLI (not codex directly).
	a.binPath = ResolveBinPath("claude", binPathOverride, "CLAUDE_BIN")
	return a
}

func (a *CodexSubagentAdapter) ID() string { return a.id }

func (a *CodexSubagentAdapter) resolveBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.binPath == "" {
		a.binPath = ResolveBinary("claude", "CLAUDE_BIN")
	}
	return a.binPath
}

func (a *CodexSubagentAdapter) refreshBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.binPath = ResolveBinary("claude", "CLAUDE_BIN")
	return a.binPath
}

// Invoke calls claude -p --agent codex with CLAGENTIC_CODEX_TIER set.
func (a *CodexSubagentAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	bin := a.resolveBin()
	if bin == "" {
		return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "claude binary not found (required for codex_subagent adapter)"}
	}

	prompt, system := FormatMessages(req.Messages)
	if prompt == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty prompt after message formatting"}
	}

	// Combine system + prompt
	var fullPrompt strings.Builder
	if system != "" {
		fullPrompt.WriteString(system)
		fullPrompt.WriteString("\n\n")
	}
	fullPrompt.WriteString(prompt)

	args := []string{
		"-p",
		"--agent", "codex",
		"--output-format", "json",
		"--max-turns", "1",
	}

	// buildCLIEnv filters the daemon environment to the allowlist — router tokens
	// and API keys are not passed to the subprocess. (lr-c7ac)
	extra := []string{"CLAGENTIC_DISABLE_RECALL=1"}
	if claudeSubprocessHome != "" {
		extra = append(extra, "HOME="+claudeSubprocessHome)
	}
	if a.tier != "" {
		extra = append(extra, fmt.Sprintf("CLAGENTIC_CODEX_TIER=%s", a.tier))
	}
	env := buildCLIEnv(extra)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(fullPrompt.String())
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
		combined := stderrStr + stdout.String()
		errType := ClassifyError(combined, exitCode)
		slog.Debug("codex_subagent invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType)
		raw := stderrStr
		if raw == "" {
			raw = truncate(stdout.String(), 500)
		}
		return nil, &InvokeError{Type: errType, Raw: raw}
	}

	// Try JSON parse (claude --output-format json)
	var out claudeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		content := strings.TrimSpace(stdout.String())
		if content == "" {
			return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from codex subagent"}
		}
		return &Response{
			Content:             content,
			PromptTokensEst:     EstimateTokens(req.Messages),
			CompletionTokensEst: len(content) / 4,
		}, nil
	}

	if out.Type == "error" || out.Error != "" {
		raw := out.Error
		if raw == "" {
			raw = out.Message
		}
		return nil, &InvokeError{Type: ClassifyError(raw, 0), Raw: truncate(raw, 500)}
	}

	content := strings.TrimSpace(out.Result)
	if content == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty result from codex subagent"}
	}

	slog.Debug("codex_subagent invoke ok", "backend", a.id, "tier", a.tier, "content_len", len(content))

	return &Response{
		Content:             content,
		PromptTokensEst:     EstimateTokens(req.Messages),
		CompletionTokensEst: len(content) / 4,
		CostUSD:             out.CostUSD,
	}, nil
}
