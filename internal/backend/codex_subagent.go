// internal/backend/codex_subagent.go — adapter for codex via the claude subagent.
//
// Invokes: claude -p --agent codex --setting-sources user --output-format json
// with CLAGENTIC_CODEX_TIER=<tier> in the environment.
//
// The codex subagent (~/.claude/agents/codex.md) reads ~/.codex/models.json
// to resolve the tier to a concrete model. This is the "openai-via-codex-subagent"
// provider path from the relay registry — it avoids hardcoding model strings and
// lets the models.json file be the single source of truth for tier → model.
//
// Cache token accounting (lr-718af0): this adapter invokes the claude CLI
// (--output-format json, parsed into the same claudeOutput type
// claude_cli.go uses) via the codex subagent, so it inherits the identical
// unverified-usage-shape gap documented in claude_cli.go's package doc — no
// permitted `claude` execution path exists for this repo's author to
// confirm whether/how a usage/cache object appears in this specific
// --agent codex output path. Response.CacheUsage is left nil here for the
// same reason: an honest unverified gap, not a guessed field or a
// fabricated zero. TODO(lr-718af0): resolve identically to claude_cli.go's
// TODO — the same live-verification step should cover both adapters, since
// they parse the same claudeOutput shape.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// CodexSubagentAdapter calls codex via the claude -p --agent codex path.
type CodexSubagentAdapter struct {
	id       string
	tier     string // flagship | mini | spark
	maxTurns int

	mu      sync.Mutex
	binPath string
}

// NewCodexSubagentAdapter creates an adapter for the given tier.
// tier should be one of: flagship, mini, spark. maxTurns is the --max-turns
// ceiling; <= 0 resolves to DefaultMaxTurns at Invoke time via
// resolveMaxTurns — same resolution claude_cli.go uses, since this adapter
// invokes the identical claude binary (lr-39ed6b: this backend had the same
// hardcoded --max-turns 1 defect as claude_cli.go, fixed identically here).
func NewCodexSubagentAdapter(id, tier, binPathOverride string, maxTurns int) *CodexSubagentAdapter {
	a := &CodexSubagentAdapter{id: id, tier: tier, maxTurns: maxTurns}
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
// passthrough. Invoke never reads Request.Tools and never populates
// Response.ToolUses (lr-add405); see ClaudeCLIAdapter's Capabilities doc
// for the shared "no structured wire field" rationale.
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
		// See claude_cli.go's DefaultMaxTurns doc for why this is not "1"
		// and why it is not unbounded (lr-39ed6b) — this adapter invokes
		// the identical claude binary and shares the identical resolution.
		"--max-turns", strconv.Itoa(resolveMaxTurns(a.maxTurns)),
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

	if err != nil || exitCode != 0 {
		// classificationText and Raw are now the SAME string (lr-151fa7,
		// following PR #61/lr-c1d353's invariant): classify against the FULL
		// stderr+stdout, not a separately-computed stderr-only (or
		// stdout-head) string — see claude_cli.go's Invoke for the full
		// rationale (this adapter shares claude_cli's exec-and-scan shape).
		// truncate() is applied to the SAME text used for classification so
		// Raw is never a window on a different buffer than what
		// ClassifyErrorWithPattern/ClassifiedTextExcerpt saw.
		combined := stderr.String() + stdout.String()
		errType, patternID := ClassifyErrorWithPattern(combined, exitCode)
		slog.Info("codex_subagent invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType,
			"request_id", RequestIDFromCtx(ctx),
			"stderr_len", stderr.Len(), "stdout_len", stdout.Len(), "matched_pattern_id", patternID)
		slog.Debug("codex_subagent invoke failed: classified text excerpt",
			"backend", a.id, "request_id", RequestIDFromCtx(ctx),
			"classified_text_excerpt", ClassifiedTextExcerpt(combined, patternID))
		return nil, &InvokeError{Type: errType, Raw: truncate(combined, 500)}
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

	// max_turns budget exhaustion (lr-39ed6b) — same shape and same
	// --max-turns flag as claude_cli.go, since this adapter invokes the
	// identical claude binary via "-p --agent codex". See
	// isMaxTurnsTermination and claude_cli.go's parseStreamJSON for the
	// full rationale; checked before the generic error branch below for the
	// same reason (a max_turns exit is not necessarily type="error").
	if isMaxTurnsTermination(&out) {
		raw := strings.Join(out.Errors, "; ")
		if raw == "" {
			raw = fmt.Sprintf("codex subagent (claude CLI) terminated: terminal_reason=max_turns num_turns=%d", out.NumTurns)
		}
		return nil, &InvokeError{Type: ErrTypeMaxTurns, Raw: truncate(raw, 500)}
	}

	if out.Type == "error" || out.Error != "" || out.IsError {
		raw := out.Error
		if raw == "" {
			raw = out.Message
		}
		if raw == "" && len(out.Errors) > 0 {
			raw = strings.Join(out.Errors, "; ")
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
