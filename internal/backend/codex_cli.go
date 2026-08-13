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
// rejects the same override against a reserved builtin provider. The router
// treats both values as opaque operator-supplied strings; it does not read,
// parse, or validate the local codex config.toml, and does not know or care
// what provider or endpoint sits behind providerID.
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
// the model_providers.<id> key in the operator's local codex config.toml;
// projectID is the header value. Both are opaque operator-supplied strings.
// binPathOverride is the explicit path to the codex binary (empty = auto-resolve).
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
// streaming, or image passthrough.
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

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(fullPrompt.String())

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
