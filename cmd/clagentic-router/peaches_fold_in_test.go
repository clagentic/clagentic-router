// cmd/clagentic-router/peaches_fold_in_test.go — regression tests for the
// PEACHES blocking review on PR #64 (lr-92ee18 fold-in,
// https://github.com/clagentic/clagentic-router/pull/64#issuecomment-5371256262):
//
//  1. cmdGetText (used by the "metrics" subcommand) must not silently print
//     an error body and exit 0 on a non-2xx response — it must go through
//     the same status-checking + enrichAuthError diagnostics as apiGet.
//  2. An empty (or whitespace-only) --token-file must not set tokenSource:
//     doing so previously broke the documented "first non-empty value wins"
//     token resolution contract and produced a misleading "resolved but
//     rejected" diagnostic instead of the correct "no token found" one.
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdGetText_401_ReturnsEnrichedError verifies that a 401 from the
// server surfaces as an error (naming the checked token sources) instead of
// being printed to stdout with a nil (exit 0) result.
func TestCmdGetText_401_ReturnsEnrichedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized\n"))
	}))
	defer srv.Close()

	err := cmdGetText([]string{"--server", srv.URL}, "/metrics")
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error %q does not report HTTP 401", err.Error())
	}
	if !strings.Contains(err.Error(), "no token found") {
		t.Errorf("error %q missing enrichAuthError diagnostic (expected \"no token found\")", err.Error())
	}
}

// TestCmdGetText_200_WritesBodyVerbatim verifies the success path still
// prints the raw (non-JSON) response body, matching /metrics' Prometheus
// text exposition format — cmdGetText must remain a "print verbatim, not
// pretty-printed JSON" client, unlike cmdGet.
func TestCmdGetText_200_WritesBodyVerbatim(t *testing.T) {
	const want = "# HELP clagentic_router_requests_total\nclagentic_router_requests_total 42\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(want))
	}))
	defer srv.Close()

	// Redirect stdout to a pipe so we can capture cmdGetText's output.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	cmdErr := cmdGetText([]string{"--server", srv.URL}, "/metrics")
	w.Close()
	os.Stdout = origStdout

	if cmdErr != nil {
		t.Fatalf("cmdGetText: %v", cmdErr)
	}
	buf := make([]byte, len(want)+16)
	n, _ := r.Read(buf)
	got := string(buf[:n])
	if got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestParseClientFlags_EmptyTokenFile_FallsThrough verifies an empty (or
// whitespace-only) --token-file does not set tokenSource, and resolution
// falls through to the next step (CLAGENTIC_ROUTER_TOKEN in this case) per
// the documented "first non-empty value wins" contract.
func TestParseClientFlags_EmptyTokenFile_FallsThrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-token.txt")
	if err := os.WriteFile(path, []byte("   \n"), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Setenv("CLAGENTIC_ROUTER_TOKEN", "fallback-tok")
	t.Setenv("CLAGENTIC_ROUTER_ADMIN_TOKEN", "")

	f, _, err := parseClientFlags([]string{"--token-file", path})
	if err != nil {
		t.Fatalf("parseClientFlags: %v", err)
	}
	if f.token != "fallback-tok" {
		t.Errorf("token = %q, want %q (should fall through to env var)", f.token, "fallback-tok")
	}
	if f.tokenSource != "env:CLAGENTIC_ROUTER_TOKEN" {
		t.Errorf("tokenSource = %q, want %q", f.tokenSource, "env:CLAGENTIC_ROUTER_TOKEN")
	}
	if strings.HasPrefix(f.tokenSource, "--token-file") {
		t.Errorf("tokenSource must not report --token-file when the file was empty, got %q", f.tokenSource)
	}
}

// TestParseClientFlags_EmptyTokenFile_NoOtherSource_ReportsNoTokenFound
// covers the exact PEACHES-cited misdiagnosis: with no other source
// available, an empty --token-file must leave tokenSource unset so
// enrichAuthError reports "no token found," not "resolved from
// --token-file ... but rejected."
func TestParseClientFlags_EmptyTokenFile_NoOtherSource_ReportsNoTokenFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-token.txt")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	t.Setenv("CLAGENTIC_ROUTER_TOKEN", "")
	t.Setenv("CLAGENTIC_ROUTER_ADMIN_TOKEN", "")
	t.Setenv("CLAGENTIC_ROUTER_ENV_FILE", filepath.Join(dir, "does-not-exist"))

	f, _, err := parseClientFlags([]string{"--token-file", path})
	if err != nil {
		t.Fatalf("parseClientFlags: %v", err)
	}
	if f.token != "" {
		t.Errorf("token = %q, want empty", f.token)
	}
	if f.tokenSource != "" {
		t.Errorf("tokenSource = %q, want empty (no source should be reported)", f.tokenSource)
	}

	baseErr := &testHTTPError{msg: "HTTP 401: unauthorized"}
	got := enrichAuthError(baseErr, f)
	msg := got.Error()
	if !strings.Contains(msg, "no token found") {
		t.Errorf("enrichAuthError message %q does not report \"no token found\"", msg)
	}
	if strings.Contains(msg, "--token-file") && strings.Contains(msg, "resolved") {
		t.Errorf("enrichAuthError message %q incorrectly claims a token was resolved from --token-file", msg)
	}
}
