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
//
// Proactive quota signal investigation (lr-c98c Slice E), UPDATED with live
// findings from a second agent that had a permitted execution path (this
// adapter's own author could not invoke `codex` at all — guard-bash denies
// it — and said so rather than guessing; that gap is what let the following
// get checked against a real run instead of shipping unverified):
//
//  1. `codex exec --json` IS a real, documented flag ("Print events to
//     stdout as JSONL" per `codex exec --help`, codex-cli 0.147.0, verified
//     live) — the "undocumented mode" open question this comment used to
//     pose is resolved. But its JSONL stream (thread.started/turn.started/
//     item.completed/turn.completed) carries only consumption counters
//     (input_tokens, cached_input_tokens, output_tokens,
//     reasoning_output_tokens) — no rate-limit or quota field of any kind.
//     It does not answer this slice's proactive-quota question by itself,
//     and this adapter still does not pass --json to `codex exec` (see
//     Invoke below) — no reason to change a load-bearing invocation for a
//     flag whose payload doesn't carry what this slice needs anyway.
//
//  2. A genuine proactive quota snapshot DOES exist, but on a different
//     transport: the codex app-server JSON-RPC protocol's
//     `account/rateLimits/read` method (schema:
//     v2/GetAccountRateLimitsResponse.json via
//     `codex app-server generate-json-schema --experimental`), returning
//     RateLimitWindow{usedPercent, resetsAt, windowDurationMins} plus a
//     CreditsSnapshot and planType — verified live against a real
//     ChatGPT-Plus account. This is marked EXPERIMENTAL by codex itself.
//
// `codex app-server` is not a one-shot subprocess call like `codex exec` or
// `codex debug models` (codex_model_discovery.go) — it is a long-running
// process that speaks JSON-RPC over stdio and (per every JSON-RPC/LSP-style
// server this shape resembles) expects an `initialize` handshake before any
// other method call. The live verification that produced the JSON above
// confirmed the method and its response shape, but not the full
// handshake/framing contract (e.g. whether messages are newline-delimited
// JSON, as `codex exec --json` uses, or Content-Length-framed like LSP) —
// getting that wrong in a bounded, timeout-guarded client would still be a
// silent construction-time or steady-state hang risk against codex_cli's
// load-bearing production path, and this repo's author has no live `codex`
// execution path to verify it further. Per CLAUDE.md's "verify per-provider
// assumptions against the live source, never generalize from one example"
// and this task's own framing (a documented, honest gap is a first-class
// outcome), wiring `account/rateLimits/read` into quota_snapshots is
// deferred rather than guessed at. TODO(lr-c98c): implement a bounded
// (timeout + response-size-capped, mirroring
// codex_model_discovery.go's runCodexDebugModelsWithLimits) app-server
// JSON-RPC client, cached at adapter-construction time or on a TTL — never
// per-Invoke — once the handshake/framing contract is verified against a
// live run by an agent with a permitted `codex` execution path.
//
// codex_cli remains reactive-only (quota known only from error text via
// ParseResetTime) until that follow-up lands.
//
// Cache token accounting (lr-718af0) — VERIFIED, not inferred: a live
// capture of `codex exec --json` (codex-cli 0.147.0, provided directly in
// this task's dispatch) shows the JSONL turn.completed event carries a
// usage object with EXACTLY the shape this feature needs:
//
//	"usage":{"input_tokens":16786,"cached_input_tokens":11008,
//	         "cache_write_input_tokens":0,"output_tokens":5,
//	         "reasoning_output_tokens":0}
//
// So codex_cli is NOT a documented no-op for cache accounting — the data
// exists and is reachable. The decision made here is deliberately NOT to
// wire it into this adapter's Invoke in this change:
//
//   - Invoke below does not pass --json today; it treats stdout as a flat
//     text string and classifies failures from that raw text (ClassifyError,
//     ParseResetTime). Adding --json would change the parse target, the
//     success/failure detection path, and the error-classification input for
//     every call this adapter makes — codex_cli is a load-bearing production
//     path (CLAUDE.md), not a place to bundle an invocation-shape change into
//     an additive observability feature.
//   - The safer precedent this repo already uses for "shell out and parse a
//     different codex output shape" (codex_discovery.go, codex_model_discovery.go)
//     is a SEPARATE one-shot subprocess call at adapter-construction time,
//     never touching the per-request Invoke path. That precedent does not
//     transfer here: cache usage is per-invocation data (it varies call to
//     call), not a one-time discovery value resolvable once at construction,
//     so there is no equivalent "call it once, cache the result" escape
//     hatch available.
//   - Switching Invoke's own stdout contract for every codex_cli call,
//     everywhere this adapter is deployed, to gain one new observability
//     field is a real behavior change to a production path and needs its own
//     scoped, reviewed change with its own risk assessment and test
//     coverage — not a rider on this task's diff.
//
// Response.CacheUsage is therefore left nil for codex_cli today: this is a
// genuine, temporary "not yet wired" gap, not a "cannot report" gap — the
// two are different facts and this comment states which one applies.
// TODO(lr-718af0): switch Invoke to `codex exec --json`, parse the
// turn.completed usage object above, and re-validate the existing
// stdout-based failure classification against the new JSONL shape (error
// events likely need their own line-type check, mirroring how claude_cli.go
// distinguishes a "result" line from an "error" line in parseStreamJSON)
// before this can land as its own change.
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
