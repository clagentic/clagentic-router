// internal/backend/codex_cli_json_test.go — tests for codex_cli's --json
// in-band failure surface and cache/token metrics wiring (lr-a40da5).
//
// Fixtures below are built from REAL captures against a live codex-cli
// 0.147.0 (an agent with a permitted `codex` execution path ran these —
// see codex_cli.go's package doc for the full transcripts), not invented
// shapes — same discipline cache_usage_test.go documents for the other
// adapters.
package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeCodexBinJSONL writes a fake "codex" binary that emits the given
// JSONL stdout and exits with the given code, mirroring
// writeFakeCodexBinFailing's shape (codex_cli_test.go) but for --json
// output rather than plain-text stderr.
func writeFakeCodexBinJSONL(t *testing.T, dir, stdout string, exitCode int) string {
	t.Helper()
	binPath := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' " + shellQuote(stdout) + "\n" +
		"exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake codex bin: %v", err)
	}
	return binPath
}

// codexSuccessJSONL is a real capture (codex-cli 0.147.0): a prompt "reply
// with exactly: ok" answered normally, exit 0.
const codexSuccessJSONL = `{"type":"thread.started","thread_id":"01a021b3-98a4-7b83-aadc-a943bf032c41"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ok"}}
{"type":"turn.completed","usage":{"input_tokens":16788,"cached_input_tokens":11008,"cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}}`

// codexInvalidReasoningEffortJSONL is a real capture: an invalid -c
// model_reasoning_effort value, exit 1. The top-level "error" event's
// message field is itself a JSON-encoded string (double-encoded), matching
// codexEventErrorText's expected nested shape.
const codexInvalidReasoningEffortJSONL = `{"type":"thread.started","thread_id":"01a021b3-b4c5-73a3-a538-5e4747cd722e"}
{"type":"turn.started"}
{"type":"error","message":"{\n  \"type\": \"error\",\n  \"error\": {\n    \"type\": \"invalid_request_error\",\n    \"code\": null,\n    \"message\": \"[ReasoningEffortParam] [reasoning.effort] [invalid_enum_value] Invalid value: 'bogus_invalid_value_xyz'. Supported values are: 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'.\",\n    \"param\": null\n  },\n  \"status\": 400\n}"}
{"type":"turn.failed","error":{"message":"{\n  \"type\": \"error\",\n  \"error\": {\n    \"type\": \"invalid_request_error\",\n    \"code\": null,\n    \"message\": \"[ReasoningEffortParam] [reasoning.effort] [invalid_enum_value] Invalid value: 'bogus_invalid_value_xyz'. Supported values are: 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'.\",\n    \"param\": null\n  },\n  \"status\": 400\n}"}}`

// codexInvalidModelJSONL is a real capture: an invalid --model value. Shows
// an item.completed error (non-fatal item-level warning) followed by the
// authoritative top-level error/turn.failed pair, exit 1.
const codexInvalidModelJSONL = `{"type":"thread.started","thread_id":"01a021b4-02a6-7dd0-95a5-7aa07e28a3ac"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Model metadata for ` + "`totally-bogus-model-xyz`" + ` not found. Defaulting to fallback metadata; this can degrade performance and cause issues."}}
{"type":"turn.started"}
{"type":"error","message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'totally-bogus-model-xyz' model is not supported when using Codex with a ChatGPT account.\"}}"}
{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'totally-bogus-model-xyz' model is not supported when using Codex with a ChatGPT account.\"}}"}}`

// TestCodexCLI_Invoke_CacheUsageWiredOnSuccess is the lr-718af0 payoff:
// turn.completed's usage object must populate Response.CacheUsage with the
// exact verified field mapping (input_tokens/cached_input_tokens/
// cache_write_input_tokens -> InputTokens/CacheReadTokens/CacheWriteTokens).
func TestCodexCLI_Invoke_CacheUsageWiredOnSuccess(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodexBinJSONL(t, dir, codexSuccessJSONL, 0)

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want %q", resp.Content, "ok")
	}
	if resp.CacheUsage == nil {
		t.Fatal("expected non-nil CacheUsage — turn.completed carried a real usage object")
	}
	if resp.CacheUsage.InputTokens != 16788 {
		t.Errorf("InputTokens = %d, want 16788", resp.CacheUsage.InputTokens)
	}
	if resp.CacheUsage.CacheReadTokens != 11008 {
		t.Errorf("CacheReadTokens = %d, want 11008", resp.CacheUsage.CacheReadTokens)
	}
	if resp.CacheUsage.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (real reported zero, not absence)", resp.CacheUsage.CacheWriteTokens)
	}
}

// TestCodexCLI_Invoke_ZeroExitInBandErrorNotReturnedAsSuccess is the core
// lr-a40da5 regression test: a zero-exit stream carrying a top-level
// type=="error"/"turn.failed" pair must be classified as a genuine failure,
// never returned as a successful Response with the error JSON as content.
// This specific fixture was captured live exiting 1, not 0 — this test
// synthesizes the zero-exit variant of the same payload (exitCode
// overridden to 0 in the fake binary) because parseCodexJSONL's check must
// not depend on exit code at all; see codex_cli.go's package doc for why
// that independence is the fix this task requires even though no live
// zero-exit trigger was reproduced.
func TestCodexCLI_Invoke_ZeroExitInBandErrorNotReturnedAsSuccess(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodexBinJSONL(t, dir, codexInvalidReasoningEffortJSONL, 0)

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected the in-band error event to be classified as a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
	wantText := "[ReasoningEffortParam] [reasoning.effort] [invalid_enum_value] Invalid value: 'bogus_invalid_value_xyz'. Supported values are: 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', and 'max'."
	if ie.Raw != wantText {
		t.Errorf("Raw = %q, want the unwrapped nested error message %q", ie.Raw, wantText)
	}
}

// TestCodexCLI_Invoke_NonzeroExitInBandError_RawMatchesClassifiedText
// verifies the Raw-equals-classified-text invariant (PR #61/lr-c1d353,
// carried forward to codex_cli by this task) on the nonzero-exit path,
// using the real invalid-model capture.
func TestCodexCLI_Invoke_NonzeroExitInBandError_RawMatchesClassifiedText(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodexBinJSONL(t, dir, codexInvalidModelJSONL, 1)

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure from the nonzero exit")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
	wantText := "The 'totally-bogus-model-xyz' model is not supported when using Codex with a ChatGPT account."
	if ie.Raw != wantText {
		t.Errorf("Raw = %q, want the top-level error event's unwrapped message %q "+
			"(the authoritative event — item.completed's error is not double-classified)", ie.Raw, wantText)
	}
}

// TestCodexCLI_Invoke_NoErrorEventFallsBackToStderrPlusStdout verifies that
// a nonzero exit with no parseable codex JSONL error event still falls back
// to the pre-lr-a40da5 classification input (combined stderr+stdout),
// preserving the lr-807319 fix (classify the FULL text, not a truncated
// window) for outputs --json cannot explain in-band — e.g. a crash before
// any JSON line, or a kill mid-turn (package doc capture #4, exit 124).
func TestCodexCLI_Invoke_NoErrorEventFallsBackToStderrPlusStdout(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCodexBinFailing(t, dir, 800)

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected Invoke to return an error, got nil")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
	if ie.Type != ErrTypeAuth {
		t.Errorf("Type = %q, want %q (credential text past the 500-char head window must still classify as auth)", ie.Type, ErrTypeAuth)
	}
}

// TestCodexCLI_Invoke_TopLevelErrorEventIsAuthoritative verifies that an
// item.completed error alone (no top-level error/turn.failed following it)
// does not fail the call — only the top-level event is authoritative (see
// codexEvent.Item's doc). This guards against a future change accidentally
// making the item-level warning trigger failure independently.
func TestCodexCLI_Invoke_TopLevelErrorEventIsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	// item.completed error, but the turn actually completes normally after
	// it — a synthetic case (not the live invalid-model capture, which does
	// go on to fail) exercised specifically to prove item-level errors alone
	// are not fatal.
	stream := `{"type":"thread.started","thread_id":"test"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"a transient warning"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"final answer"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":2,"reasoning_output_tokens":0}}`
	bin := writeFakeCodexBinJSONL(t, dir, stream, 0)

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v (item-level error alone must not fail the call)", err)
	}
	if resp.Content != "final answer" {
		t.Errorf("Content = %q, want %q", resp.Content, "final answer")
	}
}
