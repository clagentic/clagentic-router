// internal/backend/claude_cli_error_classification_test.go — regression
// tests for the Raw/classify split fix (lr-c1d353, item 3).
//
// MILLER's diagnosis: on a nonzero claude subprocess exit, the code used to
// classify against "full" (stderr + entire stdout) while reporting a
// SEPARATE "raw" string (stderr, or a stdout head when stderr was empty) as
// InvokeError.Raw — so the logged/reported error text was never guaranteed
// to be what actually drove error_type. This reproduces the exact
// production shape: claude exits nonzero with EMPTY stderr and a
// stream-json init event on stdout (a harmless success-shaped line, not an
// error), and asserts the fix's two guarantees:
//  1. error_type is NOT driven by incidental substrings elsewhere in the
//     init event (must not misclassify as rate_limit/quota/auth).
//  2. InvokeError.Raw is the SAME text that was classified — never a
//     different string, so the log is self-explaining.
package backend

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeBinExit writes a shell script that records argv, writes the
// given stdout/stderr, and exits with the given code — the nonzero-exit
// analogue of writeFakeBin (cli_model_passthrough_test.go), which always
// exits 0.
func writeFakeBinExit(t *testing.T, dir, name, stdout, stderr string, exitCode int) string {
	t.Helper()
	argsFile := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"printf '%s' " + shellQuote(stdout) + " >&1\n" +
		"printf '%s' " + shellQuote(stderr) + " >&2\n" +
		"exit " + itoa(exitCode) + "\n"
	binPath := filepath.Join(dir, name)
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return binPath
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestClaudeCLI_NonzeroExit_InitEventNotMisclassified is the production
// journal reproduction: a stream-json init event (well-formed, harmless
// success-shaped line, carrying a session_id/model/cwd) on stdout, empty
// stderr, nonzero exit. Must not classify as rate_limit/quota/auth on the
// strength of incidental substrings in the init event's fields.
func TestClaudeCLI_NonzeroExit_InitEventNotMisclassified(t *testing.T) {
	dir := t.TempDir()
	initEvent := `{"type":"system","subtype":"init","cwd":"/home/akuehner/code/project-coldest-tea/repo","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","tools":["Read","Write"],"mcp_servers":[],"model":"claude-opus-4-7","permissionMode":"default"}`
	binPath := writeFakeBinExit(t, dir, "claude", initEvent, "", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure from the nonzero exit")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type == ErrTypeRateLimit || ie.Type == ErrTypeQuota || ie.Type == ErrTypeAuth {
		t.Errorf("classified as %q from a harmless init event — misclassification not closed (lr-c1d353)", ie.Type)
	}
}

// TestClaudeCLI_NonzeroExit_RawMatchesClassifiedText asserts InvokeError.Raw
// is the SAME string ClassifyError saw — never a different string. Uses a
// stream-json "error" event so there is a genuine error-bearing field to
// extract, and confirms Raw reports exactly that field's text, not the
// surrounding stream or a stdout head.
func TestClaudeCLI_NonzeroExit_RawMatchesClassifiedText(t *testing.T) {
	dir := t.TempDir()
	// Two lines: a benign init event first (as in production), then a
	// genuine error event. Raw must reflect the error event's text, not the
	// init line, and error_type must derive from that same text.
	stream := `{"type":"system","subtype":"init","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","model":"claude-opus-4-7"}` + "\n" +
		`{"type":"error","error":"authentication_error: invalid api key"}`
	binPath := writeFakeBinExit(t, dir, "claude", stream, "", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeAuth {
		t.Errorf("error_type = %q, want %q — must classify from the actual error event, not the init line", ie.Type, ErrTypeAuth)
	}
	if ie.Raw != "authentication_error: invalid api key" {
		t.Errorf("Raw = %q, want the classified error text verbatim (Raw and classification input must be the same string)", ie.Raw)
	}
}

// TestClaudeCLI_NonzeroExit_FallsBackToStderrWhenStdoutUnparseable covers
// the case stdout is not stream-json at all (e.g. a shell/crash message) —
// the fix must still fall back to stderr, matching pre-existing behavior
// for non-stream-json failures.
func TestClaudeCLI_NonzeroExit_FallsBackToStderrWhenStdoutUnparseable(t *testing.T) {
	dir := t.TempDir()
	binPath := writeFakeBinExit(t, dir, "claude", "not json at all", "rate limit exceeded, please slow down", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeRateLimit {
		t.Errorf("error_type = %q, want %q (stderr fallback)", ie.Type, ErrTypeRateLimit)
	}
	if ie.Raw != "rate limit exceeded, please slow down" {
		t.Errorf("Raw = %q, want stderr text verbatim", ie.Raw)
	}
}

// TestClaudeCLI_NonzeroExit_ValidStreamNoErrorField_FallsBackToStderr
// covers stdout that IS valid stream-json throughout but carries no
// error-bearing field at all (e.g. only an init event, and the real reason
// for the nonzero exit was never described in-band) — this is the
// production journal's actual shape when stderr also happens to be
// non-empty. Falls back to stderr, matching the documented fallback order.
func TestClaudeCLI_NonzeroExit_ValidStreamNoErrorField_FallsBackToStderr(t *testing.T) {
	dir := t.TempDir()
	initEvent := `{"type":"system","subtype":"init","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","model":"claude-opus-4-7"}`
	binPath := writeFakeBinExit(t, dir, "claude", initEvent, "not logged in", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeAuth {
		t.Errorf("error_type = %q, want %q (stderr fallback when stream-json has no error field)", ie.Type, ErrTypeAuth)
	}
	if ie.Raw != "not logged in" {
		t.Errorf("Raw = %q, want stderr text %q — must not report the harmless init event as the error", ie.Raw, "not logged in")
	}
}
