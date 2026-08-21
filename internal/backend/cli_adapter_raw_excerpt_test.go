// internal/backend/cli_adapter_raw_excerpt_test.go — regression coverage
// closing the gap PEACHES found in PR #63 (lr-151fa7 fold-in): the
// Raw-equals-classified-text window invariant (PR #61/lr-c1d353) was
// verified for ClassifiedTextExcerpt in isolation
// (failure_log_test.go::TestClassifiedTextExcerpt_WindowOnSameStringAsRaw)
// and exercised end-to-end for claude_cli
// (claude_cli_error_classification_test.go), but codex_subagent and
// gemini_cli had no adapter-level test driving their actual Invoke failure
// path — which is exactly how both silently classified against "combined"/
// "full" (stderr+stdout) while setting InvokeError.Raw from a
// separately-truncated stderr-only (or stdout-head) string, reintroducing
// the PR #61 defect. This file drives all four CLI adapters (claude_cli,
// codex_cli, codex_subagent, gemini_cli) through the same fake-binary
// nonzero-exit harness and asserts InvokeError.Raw is always a substring of
// (here: byte-identical to, since these fixtures are under the 500-char
// truncation cap) the text ClassifyErrorWithPattern classified — never a
// separately-sourced copy.
package backend

import (
	"context"
	"testing"
)

// TestCLIAdapters_RawMatchesClassifiedText_AllFour drives claude_cli,
// codex_cli, codex_subagent, and gemini_cli through a nonzero-exit fake
// binary with a genuine error string on stderr and EMPTY stdout, and
// confirms InvokeError.Raw reports that same text verbatim. Empty stdout is
// deliberate: it collapses each adapter's own in-band-vs-fallback text
// construction (which differs by adapter — claude_cli/codex_cli have
// JSONL-aware in-band extraction with their own fallback shapes;
// codex_subagent/gemini_cli use plain combined stderr+stdout) onto the same
// stderr-only text, so this test asserts the shared invariant (Raw ==
// classified text) without over-fitting to any one adapter's fallback
// shape. The property that silently broke for codex_subagent and
// gemini_cli (PEACHES, PR #63 review) is exactly this one, and a narrower
// unit test on ClassifiedTextExcerpt alone did not catch it.
func TestCLIAdapters_RawMatchesClassifiedText_AllFour(t *testing.T) {
	const stderrText = "rate limit exceeded, please slow down"

	t.Run("claude_cli", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "claude", "", stderrText, 1)
		adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		assertRawMatchesClassifiedText(t, err, stderrText, ErrTypeRateLimit)
	})

	t.Run("codex_cli", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "codex", "", stderrText, 1)
		adapter := NewCodexCLIAdapter("codex-high", "gpt-5", "", "", "", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		assertRawMatchesClassifiedText(t, err, stderrText, ErrTypeRateLimit)
	})

	t.Run("codex_subagent", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "claude", "", stderrText, 1)
		adapter := NewCodexSubagentAdapter("codex-subagent", "flagship", binPath, 0)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		assertRawMatchesClassifiedText(t, err, stderrText, ErrTypeRateLimit)
	})

	t.Run("gemini_cli", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "gemini", "", stderrText, 1)
		adapter := NewGeminiCLIAdapter("gemini-high", "gemini-2.5-pro", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		assertRawMatchesClassifiedText(t, err, stderrText, ErrTypeRateLimit)
	})
}

// TestCLIAdapters_RawIsWindowOnClassifiedText_StderrAndStdoutCombined covers
// the specific shape PEACHES flagged: an error signal split across BOTH
// stderr and stdout, present only when the two buffers are combined. If an
// adapter classifies on combined stderr+stdout but sets Raw from stderr
// alone (or a stdout head computed independently), Raw silently disagrees
// with error_type — this reproduces that exact split for codex_subagent and
// gemini_cli (the two adapters PEACHES found broken) plus claude_cli and
// codex_cli as the already-correct baseline.
func TestCLIAdapters_RawIsWindowOnClassifiedText_StderrAndStdoutCombined(t *testing.T) {
	const stderrPart = "preamble noise on stderr, nothing diagnostic here"
	const stdoutPart = "trailing detail: rate limit exceeded, please slow down"

	t.Run("codex_subagent", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "claude", stdoutPart, stderrPart, 1)
		adapter := NewCodexSubagentAdapter("codex-subagent", "flagship", binPath, 0)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		ie := requireInvokeError(t, err)
		if ie.Type != ErrTypeRateLimit {
			t.Fatalf("error_type = %q, want %q — classification must see the combined stderr+stdout text", ie.Type, ErrTypeRateLimit)
		}
		if ie.Raw != stderrPart+stdoutPart {
			t.Errorf("Raw = %q, want the combined stderr+stdout text verbatim (must be the SAME string that was classified)", ie.Raw)
		}
	})

	t.Run("gemini_cli", func(t *testing.T) {
		dir := t.TempDir()
		binPath := writeFakeBinExit(t, dir, "gemini", stdoutPart, stderrPart, 1)
		adapter := NewGeminiCLIAdapter("gemini-high", "gemini-2.5-pro", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		_, err := adapter.Invoke(context.Background(), req)
		ie := requireInvokeError(t, err)
		if ie.Type != ErrTypeRateLimit {
			t.Fatalf("error_type = %q, want %q — classification must see the combined stderr+stdout text", ie.Type, ErrTypeRateLimit)
		}
		if ie.Raw != stderrPart+stdoutPart {
			t.Errorf("Raw = %q, want the combined stderr+stdout text verbatim (must be the SAME string that was classified)", ie.Raw)
		}
	})
}

// requireInvokeError fails the test if err is nil or not an *InvokeError,
// else returns it.
func requireInvokeError(t *testing.T, err error) *InvokeError {
	t.Helper()
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure from the nonzero exit")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	return ie
}

// assertRawMatchesClassifiedText asserts err is an *InvokeError whose Type
// matches wantType and whose Raw field is exactly wantText — the
// Raw-equals-classified-text invariant.
func assertRawMatchesClassifiedText(t *testing.T, err error, wantText string, wantType ErrorType) {
	t.Helper()
	ie := requireInvokeError(t, err)
	if ie.Type != wantType {
		t.Errorf("error_type = %q, want %q", ie.Type, wantType)
	}
	if ie.Raw != wantText {
		t.Errorf("Raw = %q, want %q — Raw must be the exact text that was classified, never a separately-sourced copy", ie.Raw, wantText)
	}
}
