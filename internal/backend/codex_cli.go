// internal/backend/codex_cli.go — adapter for the codex CLI (OAuth auth, direct model).
//
// Invokes: codex exec --skip-git-repo-check --color never --json [--model <m>]
// [-c model_reasoning_effort="<e>"]
// [-c model_providers.<id>.http_headers={"OpenAI-Project"="<project>"}] - < prompt
//
// The trailing "-" tells codex exec to read the prompt from stdin.
// Output is written to stdout as JSONL (--json) — see parseCodexJSONL below.
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
// Proactive quota signal investigation (lr-c98c Slice E): `codex exec --json`
// carries only consumption counters (input_tokens, cached_input_tokens,
// output_tokens, reasoning_output_tokens, cache_write_input_tokens on
// turn.completed) — no rate-limit or quota field of any kind. codex_cli
// remains reactive-only for quota (known only from error text via
// ParseResetTime). See codex_discovery.go's package doc and this file's git
// history for the fuller app-server/account-rateLimits investigation; that
// remains a separate, deferred concern from the --json wiring below.
//
// --json event shape and in-band failure surface (lr-a40da5), VERIFIED LIVE
// against codex-cli 0.147.0 by an agent with a permitted `codex` execution
// path — not inferred from docs, per CLAUDE.md's "verify per-provider
// assumptions against the live source" rule and the lr-60781e/lr-8dd85a/
// lr-82e68e lineage this file has cited before. Four real captures:
//
//  1. Success (exit 0):
//     {"type":"thread.started","thread_id":"..."}
//     {"type":"turn.started"}
//     {"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ok"}}
//     {"type":"turn.completed","usage":{"input_tokens":16786,"cached_input_tokens":11008,
//     "cache_write_input_tokens":0,"output_tokens":5,
//     "reasoning_output_tokens":0}}
//
//  2. In-band failure, ZERO exit is NOT what was observed here — this specific
//     shape (invalid -c model_reasoning_effort value) exited 1:
//     {"type":"thread.started",...}
//     {"type":"turn.started"}
//     {"type":"error","message":"{\n  \"type\": \"error\",\n  \"error\": {...\"message\":\"...\"},\n  \"status\":400}"}
//     {"type":"turn.failed","error":{"message":"<same nested JSON as above>"}}
//     exit code: 1
//
//  3. In-band item-level failure (invalid --model), also exited 1:
//     {"type":"thread.started",...}
//     {"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for `X` not found..."}}
//     {"type":"turn.started"}
//     {"type":"error","message":"{...\"error\":{\"message\":\"The 'X' model is not supported...\"}}"}
//     {"type":"turn.failed","error":{"message":"<same>"}}
//     exit code: 1
//
//  4. Truncated/killed mid-turn (simulated via an external timeout kill):
//     {"type":"thread.started",...}
//     {"type":"turn.started"}
//     (no further output)
//     exit code: 124 (external kill, not a codex-native code)
//
// So every in-band failure this adapter's author was able to trigger live
// ALSO exited nonzero — this file's own probing did not reproduce a genuine
// zero-exit-with-stream-error case. That does not make the zero-exit path
// safe to leave unhandled: lr-a40da5's task framing (MILLER's diagnosis of
// lr-c1d353, confidence 0.8) is that some zero-exit-with-stream-error shape
// existed in production and was never captured live because no prior agent
// had a permitted `codex` execution path at all. Given that, and per the
// same task's explicit acceptance criterion ("codex_cli detects a zero-exit
// stream error"), parseCodexJSONL below checks type=="error"/"turn.failed"
// on the SUCCESS path (err == nil, exitCode == 0) unconditionally — not only
// on the nonzero-exit path — so a zero-exit stream carrying one of these
// event types is classified as a failure rather than returned as a
// successful Response with the error JSON as its content. This closes the
// gap the task describes even though this specific verification run did not
// reproduce the zero-exit trigger condition; the parser does not assume
// exit code and event outcome are correlated.
//
// Whether the 2026-08-20 codex-sol production failures (lr-c1d353 artifact,
// host ZCL3L6QW64) took the nonzero-exit path PR #60 already fixed, or a
// zero-exit path only this change fixes, is STILL UNRESOLVED by this
// verification run — none of the four captures above reproduce a zero-exit
// stream error, so this file cannot confirm which path the production
// incident took. What changed here is coverage: before this change, a
// zero-exit stream error (if one occurs) was undetectable by construction;
// after this change, it is classified via the same in-band check the
// nonzero path uses. Reference this comment, not a fabricated confirmation,
// if that question resurfaces.
//
// Why NOT reuse claude_cli's parseStreamJSON/claudeOutput machinery: this
// package's author explicitly evaluated it (lr-a40da5's "prefer reuse"
// framing) and chose a separate, codex-shaped parser instead of extending
// claudeOutput/parseStreamJSON to also handle codex's JSONL. The two
// streams do not share a wire schema at all — claude's stream-json is
// flat-line-per-event with a single "result" line carrying Result/IsError/
// Errors/CostUSD fields; codex's is a different flat-line-per-event stream
// with thread.started/turn.started/item.completed/turn.completed/error/
// turn.failed types, a nested usage object with different field names
// (input_tokens/cached_input_tokens/cache_write_input_tokens, not
// cache_creation_input_tokens/cache_read_input_tokens), and error text
// carried as a JSON-string-encoded nested object rather than a bare string
// field. Contorting claudeOutput to decode both shapes into one struct
// would mean every field on claudeOutput either means two different things
// depending on which CLI produced it, or the struct grows a second parallel
// set of fields gated by adapter identity — both are worse than two small,
// independently-readable parsers. What IS reused, deliberately, is the
// SHAPE of the approach: scan-lines-for-a-terminal-event, classify the
// error-bearing field's text (never the surrounding stream), and preserve
// the Raw-equals-classified-text invariant PR #61 (lr-c1d353) established
// for claude_cli — see codexErrorText's doc and Invoke below.
package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
		// --json switches codex exec's stdout to newline-delimited JSON
		// events (see package doc for the verified shape) — this is what
		// gives Invoke an in-band failure surface and cache/token metrics,
		// closing both lr-a40da5 and lr-718af0's in-file TODO in the same
		// change. Verified live against codex-cli 0.147.0; see package doc.
		"--json",
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

	if err != nil || exitCode != 0 {
		// Classify against the in-band JSONL error event first (the same
		// text becomes InvokeError.Raw — see codexErrorText's doc for the
		// Raw-equals-classified invariant, PR #61/lr-c1d353's rule carried
		// forward here). Fall back to the FULL stderr+stdout combined text
		// when stdout does not parse as codex JSONL or carries no
		// error-bearing event — codex writes a banner/progress preamble
		// first and the credential/stream error at the tail (lr-807319);
		// classifying against a head-truncated window would silently drop
		// the tail text the auth/quota/rate-limit patterns need to match.
		text := codexClassificationText(stdout.Bytes(), stderr.String())
		errType, patternID := ClassifyErrorWithPattern(text, exitCode)
		slog.Info("codex_cli invoke failed",
			"backend", a.id, "exit_code", exitCode, "error_type", errType,
			"request_id", RequestIDFromCtx(ctx),
			"stderr_len", stderr.Len(), "stdout_len", stdout.Len(), "matched_pattern_id", patternID)
		slog.Debug("codex_cli invoke failed: classified text excerpt",
			"backend", a.id, "request_id", RequestIDFromCtx(ctx),
			"classified_text_excerpt", ClassifiedTextExcerpt(text, patternID))
		return nil, &InvokeError{Type: errType, Raw: truncate(text, 500)}
	}

	// Zero exit: parse the JSONL stream for a terminal event. This is the
	// in-band failure surface lr-a40da5 adds — a zero-exit stream carrying
	// type=="error" or type=="turn.failed" is a genuine failure and must not
	// be returned as a successful Response with the error text as content
	// (the exact defect this task fixes; see package doc for verified
	// event shapes and why this check runs on BOTH the nonzero and zero
	// exit paths rather than assuming exit code alone is authoritative).
	return parseCodexJSONL(ctx, stdout.Bytes(), stderr.Len(), req, a.id)
}

// codexEvent is one line of `codex exec --json` JSONL output. Field
// coverage is deliberately narrow — only what Invoke/parseCodexJSONL
// actually reads, per the four live-captured shapes in the package doc.
// Unrecognized event types (thread.started, turn.started, and any future
// type) are ignored for forward compatibility, matching claude_cli's
// parseStreamJSON precedent.
type codexEvent struct {
	Type string `json:"type"`

	// Item carries item.completed's payload. A sub-type of "error" here is
	// an item-level failure (e.g. an invalid --model warning) that in every
	// live capture co-occurred with a top-level type=="error"/"turn.failed"
	// event later in the same stream — this adapter treats the top-level
	// event as authoritative and does not independently fail on an
	// item.completed error alone, to avoid double-classifying (or
	// classifying early on) a single underlying failure.
	Item *codexItem `json:"item,omitempty"`

	// Message carries the top-level type=="error" event's payload: a
	// JSON-string-encoded object (verified live — see package doc capture
	// #2/#3), not a bare human-readable string. codexErrorText unwraps it.
	Message string `json:"message,omitempty"`

	// Error carries the top-level type=="turn.failed" event's payload —
	// same nested-JSON-string shape as Message above, under a different
	// wrapper key. Verified live (package doc capture #2/#3).
	Error *codexTurnFailedError `json:"error,omitempty"`

	// Usage carries the type=="turn.completed" event's consumption
	// counters (lr-718af0) — verified live, exact field names from a real
	// capture (package doc). nil on every event type except turn.completed.
	Usage *codexUsage `json:"usage,omitempty"`
}

// codexItem is item.completed's nested item object.
type codexItem struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Message string `json:"message,omitempty"`
}

// codexTurnFailedError is turn.failed's nested error object — carries the
// same JSON-string-encoded payload as the top-level error event's Message
// field, verified identical in both live captures.
type codexTurnFailedError struct {
	Message string `json:"message,omitempty"`
}

// codexUsage mirrors `codex exec --json`'s turn.completed "usage" object,
// VERIFIED LIVE against codex-cli 0.147.0 (lr-718af0's original finding,
// re-confirmed live for lr-a40da5 — see package doc capture #1):
//
//	{"input_tokens":16786,"cached_input_tokens":11008,
//	 "cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}
type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// nestedErrorPayload decodes the JSON-string-encoded object carried inside
// a codex top-level error/turn.failed event's Message field (verified live
// shape — package doc captures #2/#3):
//
//	{"type":"error","error":{"type":"invalid_request_error","code":null,
//	 "message":"<human-readable text>","param":null},"status":400}
type nestedErrorPayload struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// codexEventErrorText extracts the human-readable error text from a
// top-level codex error/turn.failed event's raw message string. The
// message is itself JSON — see nestedErrorPayload's doc — so this unwraps
// one layer and returns the nested error.message when present. Falls back
// to the raw (un-unwrapped) string when it does not parse as the expected
// nested shape, so a future/unexpected codex error payload is still
// reported rather than silently dropped.
func codexEventErrorText(raw string) string {
	if raw == "" {
		return ""
	}
	var nested nestedErrorPayload
	if err := json.Unmarshal([]byte(raw), &nested); err == nil && nested.Error.Message != "" {
		return nested.Error.Message
	}
	return raw
}

// codexErrorTextFromJSONL scans stdout as codex --json JSONL looking for a
// terminal error event: type=="error" (Message field) or
// type=="turn.failed" (Error.Message field) — both verified to carry the
// same nested-JSON payload live (package doc). Returns ("", false) when
// stdout does not decode as codex JSONL at all, or decodes but carries no
// error-bearing event.
//
// This is the codex analogue of claude_cli.go's errorTextFromStreamJSON —
// same shape-of-approach (scan lines, extract only the error-bearing
// field's text, never the surrounding stream), different wire schema. See
// this file's package doc for why the two are not unified into one parser.
func codexErrorTextFromJSONL(stdout []byte) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	var sawValidLine bool
	var text string
	var found bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		sawValidLine = true

		switch ev.Type {
		case "error":
			if t := codexEventErrorText(ev.Message); t != "" {
				text = t
				found = true
			}
		case "turn.failed":
			if ev.Error != nil {
				if t := codexEventErrorText(ev.Error.Message); t != "" {
					text = t
					found = true
				}
			}
		}
	}
	if found {
		return text, true
	}
	if !sawValidLine {
		return "", false
	}
	return "", false
}

// codexClassificationText returns the text ClassifyError should classify
// AND that InvokeError.Raw should report on the nonzero-exit path — the
// SAME string in both cases, preserving the Raw-equals-classified-text
// invariant PR #61 (lr-c1d353) established for claude_cli and this task
// carries forward to codex_cli. Tries the in-band JSONL error event first;
// falls back to combined stderr+stdout (matching this adapter's pre---json
// classification input) when stdout does not parse as codex JSONL or
// carries no error-bearing event — e.g. a crash before any JSON output, or
// a nonzero exit the CLI never described in-band (the timeout-kill capture
// in the package doc is exactly this case: no error event, exit 124).
func codexClassificationText(stdout []byte, stderrStr string) string {
	if len(bytes.TrimSpace(stdout)) > 0 {
		if text, ok := codexErrorTextFromJSONL(stdout); ok {
			return text
		}
	}
	return stderrStr + string(stdout)
}

// parseCodexJSONL scans a zero-exit codex --json JSONL stream for a
// terminal event and returns either a failure (in-band error/turn.failed —
// see package doc for why this is checked unconditionally, not only on the
// nonzero-exit path) or a successful Response carrying the agent_message
// text and cache/token usage from turn.completed (lr-718af0). stderrLen is
// the byte length of the subprocess's stderr buffer at the call site —
// passed through rather than re-derived here so the zero-exit in-band
// failure log line below reports the same stderr_len field the nonzero-exit
// path does (lr-151fa7), even though stderr itself plays no role in this
// path's classification (codex's zero-exit in-band error text lives on
// stdout only — see codexErrorTextFromJSONL).
func parseCodexJSONL(ctx context.Context, stdout []byte, stderrLen int, req *Request, backendID string) (*Response, error) {
	// A zero-exit in-band error is classified identically to a nonzero-exit
	// one — same text extraction, same ClassifyErrorWithPattern call, same
	// Raw-equals-classified invariant. exitCode 0 is passed to
	// ClassifyErrorWithPattern (it carries no special-case meaning beyond
	// 127/124, per its own doc).
	if text, ok := codexErrorTextFromJSONL(stdout); ok {
		errType, patternID := ClassifyErrorWithPattern(text, 0)
		slog.Info("codex_cli invoke failed (zero exit, in-band error)",
			"backend", backendID, "error_type", errType,
			"request_id", RequestIDFromCtx(ctx),
			"stderr_len", stderrLen, "stdout_len", len(stdout), "matched_pattern_id", patternID)
		slog.Debug("codex_cli invoke failed (zero exit, in-band error): classified text excerpt",
			"backend", backendID, "request_id", RequestIDFromCtx(ctx),
			"classified_text_excerpt", ClassifiedTextExcerpt(text, patternID))
		return nil, &InvokeError{Type: errType, Raw: truncate(text, 500)}
	}

	var (
		content strings.Builder
		usage   *codexUsage
	)

	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
				if content.Len() > 0 {
					content.WriteString("\n")
				}
				content.WriteString(ev.Item.Text)
			}
		case "turn.completed":
			usage = ev.Usage
		}
	}

	text := strings.TrimSpace(content.String())
	if text == "" {
		// No agent_message content and no in-band error event either — fall
		// back to raw stdout as plain text (pre-existing behavior for
		// unparseable output), then to a schema error if that's empty too.
		// stderrStr is intentionally not consulted for content — codex's
		// success-path text lives on stdout only.
		text = strings.TrimSpace(string(stdout))
	}
	if text == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty output from codex CLI"}
	}

	promptEst := EstimateTokens(req.Messages)
	completionEst := len(text) / 4

	resp := &Response{
		Content:             text,
		PromptTokensEst:     promptEst,
		CompletionTokensEst: completionEst,
	}

	if usage != nil {
		// A real reported cache miss (all-zero cache fields) is still a
		// non-nil CacheUsage — see CacheUsage's own doc (adapter.go) for why
		// nil vs. zero is load-bearing here. usage is only non-nil when a
		// real turn.completed event was seen, so this is never a fabricated
		// zero.
		resp.CacheUsage = &CacheUsage{
			InputTokens:      usage.InputTokens,
			CacheReadTokens:  usage.CachedInputTokens,
			CacheWriteTokens: usage.CacheWriteInputTokens,
		}
	}

	slog.Debug("codex_cli invoke ok", "backend", backendID, "content_len", len(text),
		"has_cache_usage", usage != nil)

	return resp, nil
}
