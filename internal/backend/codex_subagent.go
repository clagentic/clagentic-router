// internal/backend/codex_subagent.go — adapter for codex via the claude subagent.
//
// Invokes: claude -p --agent codex --setting-sources user --output-format json
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

// Capabilities reports the codex-subagent adapter's wire protocol support.
// Like the direct codex CLI path, this pipes a flat prompt through the
// claude -p --agent codex invocation — no tool, streaming, or image
// passthrough.
func (a *CodexSubagentAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: false, SupportsStreaming: false, SupportsImages: false}
}

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
	// Verified, not assumed: this adapter's cmd (below) invokes bin — the
	// same claude binary resolved the same way as claude_cli.go — via
	// "-p --agent codex", passed through the identical buildCLIEnv(extra)
	// call and the identical claudeSubprocessHome HOME override that
	// claude_cli.go uses. There is no separate codex_subagent-specific auth
	// path to check: whatever env/HOME claude_cli's subprocess sees, this
	// adapter's subprocess sees too, because it is the same binary invoked
	// through the same env-construction code. So CLAUDE_CODE_USE_BEDROCK
	// must survive buildCLIEnv's filter (env.go, shared by both adapters),
	// and the isolated HOME needs the same AWS SSO cache mirror
	// claude_cli.go provides, for the identical reason (lr-6572d5). No
	// adapter-specific logic needed here beyond calling the shared sync
	// functions.
	syncSubprocessCreds(claudeSubprocessHome)
	syncSubprocessAWSSSOCache(claudeSubprocessHome)

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

	// --setting-sources user restricts the CLI to user-scope settings only,
	// excluding a caller-supplied working_dir's .claude/settings.json
	// (hooks, permissions.allow) and project CLAUDE.md entirely — see
	// claude_cli.go's Invoke for the full rationale, including why --bare
	// (auth-breaking under OAuth) was rejected and why --safe-mode was
	// tried first and found insufficient (it left a permissions.allow gap
	// open that --setting-sources user closes). This adapter invokes the
	// same claude binary via the --agent codex path, so the same fix
	// applies identically. scripts/verify-safe-mode-permissions.sh (`make
	// verify-safe-mode`) is the reproducible harness for that claim;
	// docs/setting-sources-verification-run.txt is the committed evidence from a
	// real run — see claude_cli.go's Invoke for the fuller note.
	args := []string{
		"-p",
		"--agent", "codex",
		"--setting-sources", "user",
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
	// Neutral working directory so the subprocess does not inherit the
	// daemon's cwd (which may be a project directory carrying its own
	// ./CLAUDE.md or ./.claude/settings.json the agent picks up unexpectedly
	// via the claude -p --agent codex path). Defaults to DefaultWorkingDir
	// ("/") when the caller does not supply req.WorkingDir; a validated,
	// caller-supplied directory (see ResolveWorkingDir at the wire boundary)
	// is honored instead. Mirrors claude_cli.go's cmd.Dir handling — see its
	// Invoke for the fuller two-layer rationale (HOME override + cmd.Dir).
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
