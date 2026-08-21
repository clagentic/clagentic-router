// internal/backend/context_deadline_classification_test.go — regression
// tests for lr-2f35bd: a subprocess killed by its own invocation context's
// deadline must classify as error_type=timeout, for every CLI adapter, not
// error_type=unknown.
//
// Before this fix, exec.CommandContext's SIGKILL on a deadline normalized to
// a bare nonzero exit code (never 124 — that code is specific to the
// external `timeout` command, which none of these adapters invoke), and the
// "context deadline exceeded" text lives only in Go's err value, which never
// reaches the classifier (classification input is subprocess stdout/stderr
// only). So a deadline kill fell through to ClassifyErrorWithPattern's
// generic no-match path and reported error_type=unknown.
package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSleepBin writes a shell script that ignores its arguments and sleeps
// far longer than any test's context deadline, so exec.CommandContext is
// guaranteed to kill it before it exits on its own. It writes nothing to
// stdout/stderr — the whole point of this regression is that the CLI's
// output buffers carry no "context deadline exceeded" text for the
// classifier to find; the signal must come from the ctx/err pair alone.
func writeSleepBin(t *testing.T, dir, name string) string {
	t.Helper()
	// "exec" replaces the shell process with sleep(1) rather than forking a
	// child, so exec.CommandContext's SIGKILL (sent to the shell's own PID)
	// terminates the actual sleeping process immediately instead of leaving
	// an orphaned "sleep 10" running to completion under a different PID.
	script := "#!/bin/sh\nexec sleep 10\n"
	binPath := filepath.Join(dir, name)
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write sleep bin: %v", err)
	}
	return binPath
}

// shortDeadlineCtx returns a context whose deadline elapses well before
// writeSleepBin's 10-second sleep, so every adapter under test is killed by
// its own ctx, not by any test-suite-level timeout.
func shortDeadlineCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

// TestClaudeCLI_ContextDeadline_ClassifiesAsTimeout covers AC1/AC2 for
// claude_cli: a deadline-killed subprocess must classify as ErrTypeTimeout,
// not ErrTypeUnknown, and InvokeError must never be nil (must not be
// silently treated as success).
func TestClaudeCLI_ContextDeadline_ClassifiesAsTimeout(t *testing.T) {
	dir := t.TempDir()
	binPath := writeSleepBin(t, dir, "claude")

	adapter := NewClaudeCLIAdapter("claude-test", "claude-sonnet-4-6", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(shortDeadlineCtx(t), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a timeout failure from the killed subprocess")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeTimeout {
		t.Errorf("error_type = %q, want %q — context-deadline kill must classify as timeout, not unknown (lr-2f35bd)", ie.Type, ErrTypeTimeout)
	}
}

// TestCodexCLI_ContextDeadline_ClassifiesAsTimeout is the codex_cli analogue
// of the claude_cli test above.
func TestCodexCLI_ContextDeadline_ClassifiesAsTimeout(t *testing.T) {
	dir := t.TempDir()
	binPath := writeSleepBin(t, dir, "codex")

	adapter := NewCodexCLIAdapter("codex-test", "o4-mini", "", "", "", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(shortDeadlineCtx(t), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a timeout failure from the killed subprocess")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeTimeout {
		t.Errorf("error_type = %q, want %q — context-deadline kill must classify as timeout, not unknown (lr-2f35bd)", ie.Type, ErrTypeTimeout)
	}
}

// TestCodexSubagent_ContextDeadline_ClassifiesAsTimeout is the
// codex_subagent analogue.
func TestCodexSubagent_ContextDeadline_ClassifiesAsTimeout(t *testing.T) {
	dir := t.TempDir()
	binPath := writeSleepBin(t, dir, "claude")

	adapter := NewCodexSubagentAdapter("codex-subagent-test", "flagship", binPath, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(shortDeadlineCtx(t), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a timeout failure from the killed subprocess")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeTimeout {
		t.Errorf("error_type = %q, want %q — context-deadline kill must classify as timeout, not unknown (lr-2f35bd)", ie.Type, ErrTypeTimeout)
	}
}

// TestGeminiCLI_ContextDeadline_ClassifiesAsTimeout is the gemini_cli
// analogue.
func TestGeminiCLI_ContextDeadline_ClassifiesAsTimeout(t *testing.T) {
	dir := t.TempDir()
	binPath := writeSleepBin(t, dir, "gemini")

	adapter := NewGeminiCLIAdapter("gemini-test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(shortDeadlineCtx(t), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a timeout failure from the killed subprocess")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeTimeout {
		t.Errorf("error_type = %q, want %q — context-deadline kill must classify as timeout, not unknown (lr-2f35bd)", ie.Type, ErrTypeTimeout)
	}
}

// TestClaudeCLI_NonzeroExit_DeadlinePending_ClassifiesUnchanged covers AC3:
// a subprocess that fails for a NON-timeout reason (a real nonzero exit)
// with a deadline still pending (far in the future, never actually
// elapsed) must classify exactly as it did before this fix — no regression
// on the existing exit-code/output-text classification path. Uses the same
// nonzero-exit + auth-error shape as
// TestClaudeCLI_NonzeroExit_RawMatchesClassifiedText
// (claude_cli_error_classification_test.go) but with a real (generous,
// never-elapsing) context deadline attached, so IsContextDeadlineKill must
// return false and fall through to the pre-existing classification path.
func TestClaudeCLI_NonzeroExit_DeadlinePending_ClassifiesUnchanged(t *testing.T) {
	dir := t.TempDir()
	stream := `{"type":"system","subtype":"init","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","model":"claude-opus-4-7"}` + "\n" +
		`{"type":"error","error":"authentication_error: invalid api key"}`
	binPath := writeFakeBinExit(t, dir, "claude", stream, "", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	// Deadline far in the future — never elapses during this test, so the
	// subprocess exits on its own (nonzero, not killed) well before ctx
	// would ever fire.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	_, err := adapter.Invoke(ctx, req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeAuth {
		t.Errorf("error_type = %q, want %q — a non-timeout failure with a pending deadline must classify unchanged (lr-2f35bd AC3)", ie.Type, ErrTypeAuth)
	}
	if ie.Raw != "authentication_error: invalid api key" {
		t.Errorf("Raw = %q, want the classified error text verbatim", ie.Raw)
	}
}

// TestIsContextDeadlineKill_NoRegressionOnNonTimeoutFailure covers AC3: a
// subprocess that fails for a NON-timeout reason (a real nonzero exit, no
// deadline involved) must not be misclassified as a timeout — the ctx used
// has no deadline at all, matching real production usage for a
// non-timed-out call, and IsContextDeadlineKill must return false so the
// existing exit-code/output-text classification path (unchanged) still
// drives the result.
func TestIsContextDeadlineKill_NoRegressionOnNonTimeoutFailure(t *testing.T) {
	ctx := context.Background() // no deadline
	if IsContextDeadlineKill(ctx, nil) {
		t.Error("IsContextDeadlineKill(no-deadline ctx, nil err) = true, want false (err is nil, no failure occurred)")
	}
}

// TestIsContextDeadlineKill_ErrNilAlwaysFalse covers the doc'd invariant
// directly: a nil err (the call actually succeeded) must never be reported
// as a deadline kill, even if the ctx passed happens to be expired (e.g. a
// narrow race where the deadline elapsed in the window between the
// subprocess exiting and Run() returning).
func TestIsContextDeadlineKill_ErrNilAlwaysFalse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // ensure the deadline has elapsed
	if IsContextDeadlineKill(ctx, nil) {
		t.Error("IsContextDeadlineKill(expired ctx, nil err) = true, want false — a nil err means the call succeeded regardless of ctx state")
	}
}
