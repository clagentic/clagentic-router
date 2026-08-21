// internal/backend/binpath_test.go — regression tests for lr-92ee18 PEACHES
// re-review (comment 5371343493, finding 2): a configured bin_path must be
// validated (exists + executable) the same way a PATH-resolved binary
// already is, so an operator typo or stale bin_path shows up as unresolved
// on /health and /doctor instead of being accepted verbatim.
package backend

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveBinPath_ConfiguredNonexistent_ReturnsEmpty verifies a
// configured bin_path pointing at a file that does not exist is rejected —
// before this fix, ResolveBinPath accepted any non-empty configured string
// verbatim and BinaryResolved() would incorrectly report true.
func TestResolveBinPath_ConfiguredNonexistent_ReturnsEmpty(t *testing.T) {
	got := ResolveBinPath("claude", "/nonexistent/path/does/not/exist/claude", "CLAUDE_BIN")
	if got != "" {
		t.Errorf("ResolveBinPath with nonexistent configured path = %q, want empty", got)
	}
}

// TestResolveBinPath_ConfiguredDirectory_ReturnsEmpty verifies a configured
// bin_path pointing at a directory (not a file) is rejected.
func TestResolveBinPath_ConfiguredDirectory_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got := ResolveBinPath("claude", dir, "CLAUDE_BIN")
	if got != "" {
		t.Errorf("ResolveBinPath with directory as configured path = %q, want empty", got)
	}
}

// TestResolveBinPath_ConfiguredNotExecutable_ReturnsEmpty verifies a
// configured bin_path pointing at a real, existing file that lacks the
// executable bit is rejected — this is the exact defect B2 was meant to
// close: a bin_path that "exists" as a string but cannot actually serve a
// request must not be accepted as resolved.
func TestResolveBinPath_ConfiguredNotExecutable_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0644); err != nil {
		t.Fatalf("write non-executable file: %v", err)
	}

	got := ResolveBinPath("claude", path, "CLAUDE_BIN")
	if got != "" {
		t.Errorf("ResolveBinPath with non-executable configured path = %q, want empty", got)
	}
}

// TestResolveBinPath_ConfiguredExecutable_ReturnsAbsPath verifies a valid
// configured bin_path (exists, executable, not a directory) is still
// accepted and returned as an absolute path — the fix must not regress the
// working case.
func TestResolveBinPath_ConfiguredExecutable_ReturnsAbsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatalf("write executable file: %v", err)
	}

	got := ResolveBinPath("claude", path, "CLAUDE_BIN")
	if got == "" {
		t.Fatal("ResolveBinPath with a valid executable configured path returned empty, want the resolved path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveBinPath returned %q, want an absolute path", got)
	}
}

// TestNewClaudeCLIAdapter_InvalidConfiguredBinPath_BinaryResolvedFalse is an
// end-to-end regression test at the adapter level, mirroring the exact
// scenario the PEACHES finding describes: an operator-configured bin_path
// pointing at a nonexistent file must make BinaryResolved() report false —
// the same signal /health's unresolved_binaries reads (see
// internal/server/health_unresolved_binary_test.go) — not silently succeed
// because the configured string was merely non-empty.
func TestNewClaudeCLIAdapter_InvalidConfiguredBinPath_BinaryResolvedFalse(t *testing.T) {
	a := NewClaudeCLIAdapter("test", "", "/nonexistent/path/does/not/exist/claude", EffortLevel(""), ThinkingOff, 0)
	if a.BinaryResolved() {
		t.Error("BinaryResolved() = true for a configured bin_path that does not exist, want false")
	}
}

// TestNewClaudeCLIAdapter_ValidConfiguredBinPath_BinaryResolvedTrue verifies
// the positive case at the adapter level: a real, executable configured
// bin_path still resolves and BinaryResolved() reports true.
func TestNewClaudeCLIAdapter_ValidConfiguredBinPath_BinaryResolvedTrue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho hi\n"), 0755); err != nil {
		t.Fatalf("write executable file: %v", err)
	}

	a := NewClaudeCLIAdapter("test", "", path, EffortLevel(""), ThinkingOff, 0)
	if !a.BinaryResolved() {
		t.Error("BinaryResolved() = false for a valid, executable configured bin_path, want true")
	}
}
