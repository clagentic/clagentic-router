// internal/backend/codex_cli.go — adapter for the codex CLI (OAuth auth, direct model).
//
// Invokes: codex exec --skip-git-repo-check --color never [--model <m>]
// [-c model_reasoning_effort="<e>"]
// [-c model_providers.<id>.http_headers={"OpenAI-Project"="<project>"}] - < prompt
//
// The trailing "-" tells codex exec to read the prompt from stdin.
// Output is written to stdout.
//
// The http_headers override is additive and opt-in: it is only emitted when
// both providerID and projectID are non-empty. It exists to attribute
// per-caller Bedrock-mode traffic against a real OpenAI project registry
// entry when model_provider is a custom (non-reserved) provider id — codex
// rejects the same override against a reserved builtin provider.
//
// This adapter itself treats both values as opaque strings and never reads
// codex config.toml or makes network calls — it only appends the flag it is
// given. Both values are validated upstream, before this adapter ever sees
// them, by DiscoverCodexProjectHeader (codex_discovery.go), which is the
// single call site both cross through on their way into this adapter's
// constructor: providerID via discovery or override, projectID via
// override only (no discovery path — see codex_discovery.go package doc).
// router.yaml's codex_provider_id/openai_project_id remain available as
// explicit overrides for the ambiguous provider case and as the only way to
// set a project id at all.
//
// This is the "openai-via-codex" provider path from the relay registry.
// For the codex subagent (openai-via-codex-subagent), use CodexSubagentAdapter.
//
// Model alias behavior: the model string from BackendConfig is passed directly
// to the --model flag without transformation. The codex CLI does not expose a
// family-alias scheme comparable to Claude's — use pinned OpenAI model strings
// (o4-mini, o3, o3-pro) and update them explicitly when rolling forward.
//
// The router does not parse, cache, or expand model strings. Resolution (if any)
// is delegated to the codex CLI on each invocation.
package backend

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// CodexCLIAdapter calls the codex CLI directly (not via the claude subagent).
type CodexCLIAdapter struct {
	id              string
	model           string
	reasoningEffort string
	providerID      string
	projectID       string

	mu      sync.Mutex
	binPath string
}

// NewCodexCLIAdapter creates a new adapter for the given backend ID.
// model and reasoningEffort may be empty.
// providerID and projectID may be empty; the OpenAI-Project header override
// is only emitted when both are non-empty (see package doc). providerID is
// the model_providers.<id> key to patch in codex's own config.toml;
// projectID is the header value. Callers normally pass the values resolved
// by DiscoverCodexProjectHeader rather than a hand-set config field — see
// package doc. binPathOverride is the explicit path to the codex binary
// (empty = auto-resolve).
func NewCodexCLIAdapter(id, model, reasoningEffort, providerID, projectID, binPathOverride string) *CodexCLIAdapter {
	a := &CodexCLIAdapter{id: id, model: model, reasoningEffort: reasoningEffort, providerID: providerID, projectID: projectID}
	// Resolve and log the binary path at construction time so misconfigurations
	// surface in the startup log rather than silently at first invoke.
	a.binPath = ResolveBinPath("codex", binPathOverride, "CODEX_BIN")
	return a
}

func (a *CodexCLIAdapter) ID() string { return a.id }

// Capabilities reports the codex CLI adapter's wire protocol support.
// The subprocess path pipes a flat prompt string to stdin — no tool,
// streaming, or image passthrough. Invoke never reads Request.Tools and
// never populates Response.ToolUses (lr-add405); see ClaudeCLIAdapter's
// Capabilities doc for the shared "no structured wire field" rationale.
func (a *CodexCLIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: false, SupportsStreaming: false, SupportsImages: false}
}

func (a *CodexCLIAdapter) resolveBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.binPath == "" {
		a.binPath = ResolveBinary("codex", "CODEX_BIN")
	}
	return a.binPath
}

func (a *CodexCLIAdapter) refreshBin() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.binPath = ResolveBinary("codex", "CODEX_BIN")
	return a.binPath
}

// Invoke calls codex exec with the prompt via stdin.
func (a *CodexCLIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	bin := a.resolveBin()
	if bin == "" {
		return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "codex binary not found on PATH"}
	}

	prompt, system := FormatMessages(req.Messages)
	if prompt == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty prompt after message formatting"}
	}

	// Combine system + prompt for codex stdin
	var fullPrompt strings.Builder
	if system != "" {
		fullPrompt.WriteString(system)
		fullPrompt.WriteString("\n\n")
	}
	fullPrompt.WriteString(prompt)

	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--color", "never",
	}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	if a.reasoningEffort != "" {
		args = append(args, "-c", fmt.Sprintf(`model_reasoning_effort="%s"`, a.reasoningEffort))
	}
	if a.providerID != "" && a.projectID != "" {
		args = append(args, "-c", fmt.Sprintf(`model_providers.%s.http_headers={"OpenAI-Project"="%s"}`, a.providerID, a.projectID))
	}
	args = append(args, "-") // read from stdin

	// buildCLIEnv filters the daemon environment to the allowlist — router
	// tokens and API keys are not passed to the subprocess. (lr-c7ac). HOME
	// and CODEX_ are both on the allowlist (env.go), so ~/.codex/auth.json
	// (or CODEX_HOME-relative) resolution for ChatGPT-Plus OAuth is preserved
	// unchanged. Unlike claude_cli.go/codex_subagent.go, this adapter sets no
	// HOME override here — see cmd.Dir comment below for why that asymmetry
	// is fine; HOME curation is a separate, out-of-scope concern.
	env := buildCLIEnv(nil)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(fullPrompt.String())
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
		if strings.Contains(err.Error(), "no such file") {
			bin = a.refreshBin()
			if bin == "" {
				return nil, &InvokeError{Type: ErrTypeNotFound, Raw: "codex binary not found"}
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
		slog.Debug("codex_cli invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType,
			"request_id", RequestIDFromCtx(ctx))
		raw := stderrStr
		if raw == "" {
			raw = truncate(stdout.String(), 500)
		}
		return nil, &InvokeError{Type: errType, Raw: raw}
	}

	content := strings.TrimSpace(stdout.String())
	if content == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from codex CLI"}
	}

	promptEst := EstimateTokens(req.Messages)
	completionEst := len(content) / 4

	slog.Debug("codex_cli invoke ok", "backend", a.id, "content_len", len(content),
		"request_id", RequestIDFromCtx(ctx))

	return &Response{
		Content:             content,
		PromptTokensEst:     promptEst,
		CompletionTokensEst: completionEst,
	}, nil
}
