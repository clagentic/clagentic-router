// internal/backend/adapter_test.go — unit tests for ResolveWorkingDir.
//
// Covers the fail-loud validation contract: absent field falls through to
// DefaultWorkingDir with no inference; a supplied value must be absolute,
// must exist, and must be a directory, or the call is rejected.
package backend

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWorkingDir_Absent verifies that an empty raw value returns
// DefaultWorkingDir with no error and no filesystem inference attempted.
func TestResolveWorkingDir_Absent(t *testing.T) {
	got, err := ResolveWorkingDir("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DefaultWorkingDir {
		t.Errorf("got %q, want DefaultWorkingDir %q", got, DefaultWorkingDir)
	}
}

// TestResolveWorkingDir_ValidAbsoluteDir verifies that a valid absolute
// directory is returned unchanged.
func TestResolveWorkingDir_ValidAbsoluteDir(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolveWorkingDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

// TestResolveWorkingDir_RelativePathRejected verifies that a relative path
// is rejected rather than silently resolved against some ambient cwd.
func TestResolveWorkingDir_RelativePathRejected(t *testing.T) {
	_, err := ResolveWorkingDir("relative/path")
	if err == nil {
		t.Fatal("expected error for relative path, got nil")
	}
}

// TestResolveWorkingDir_NonexistentPathRejected verifies that an absolute
// path which does not exist on disk is rejected.
func TestResolveWorkingDir_NonexistentPathRejected(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	_, err := ResolveWorkingDir(missing)
	if err == nil {
		t.Fatal("expected error for nonexistent path, got nil")
	}
}

// TestResolveWorkingDir_FileNotDirectoryRejected verifies that an absolute
// path pointing at a regular file (not a directory) is rejected.
func TestResolveWorkingDir_FileNotDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err := ResolveWorkingDir(filePath)
	if err == nil {
		t.Fatal("expected error for file-not-directory, got nil")
	}
}
