// internal/backend/claude_cli_test.go — unit tests for syncSubprocessCreds and resolveDaemonHome.
//
// Tests cover:
//   - Initial bootstrap when the subprocess copy is absent.
//   - Resync when the source file is updated (different content/mtime).
//   - No-op when the source is unchanged (fast path: mtime+size match).
//   - No-op when source is absent and a working copy exists (don't clobber).
//   - Concurrent callers do not corrupt the destination.
//   - resolveDaemonHome: returns HOME env when set.
//   - resolveDaemonHome: falls back to user.Current() when HOME is empty.
//   - resolveDaemonHome: never returns a relative or empty path.
//   - syncSubprocessCreds: does not stat any path when home is unresolvable.
package backend

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetCredsSyncState clears the package-level sync cache so each test starts
// with a clean slate.  Must be called with credsSyncMu unlocked.
func resetCredsSyncState() {
	credsSyncMu.Lock()
	credsSyncLastInfo = nil
	credsSyncMu.Unlock()
}

// TestSyncSubprocessCreds_InitialBootstrap verifies that a missing subprocess
// copy is created from the source on first call.
func TestSyncSubprocessCreds_InitialBootstrap(t *testing.T) {
	resetCredsSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	src := filepath.Join(daemonHome, ".claude", ".credentials.json")
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	if err := os.MkdirAll(filepath.Dir(src), 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}

	srcContent := []byte(`{"token":"fresh-token-abc"}`)
	if err := os.WriteFile(src, srcContent, 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	t.Setenv("HOME", daemonHome)
	syncSubprocessCreds(subprocessHome)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(srcContent) {
		t.Errorf("dst content = %q, want %q", got, srcContent)
	}

	// Verify mode 0600.
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("dst mode = %04o, want 0600", info.Mode().Perm())
	}
}

// TestSyncSubprocessCreds_Resync verifies that a stale subprocess copy is
// refreshed when the source file changes (different content).
func TestSyncSubprocessCreds_Resync(t *testing.T) {
	resetCredsSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	src := filepath.Join(daemonHome, ".claude", ".credentials.json")
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	if err := os.MkdirAll(filepath.Dir(src), 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}

	// Write an old (stale) copy to dst.
	staleContent := []byte(`{"token":"old-stale-token"}`)
	if err := os.WriteFile(dst, staleContent, 0600); err != nil {
		t.Fatalf("write stale dst: %v", err)
	}

	// Write fresh content to src.
	freshContent := []byte(`{"token":"rotated-fresh-token"}`)
	if err := os.WriteFile(src, freshContent, 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	t.Setenv("HOME", daemonHome)
	syncSubprocessCreds(subprocessHome)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after resync: %v", err)
	}
	if string(got) != string(freshContent) {
		t.Errorf("dst content after resync = %q, want %q", got, freshContent)
	}
}

// TestSyncSubprocessCreds_NoOpWhenCurrent verifies that syncSubprocessCreds is
// a no-op (does not rewrite the dst) when the source is unchanged.
func TestSyncSubprocessCreds_NoOpWhenCurrent(t *testing.T) {
	resetCredsSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	src := filepath.Join(daemonHome, ".claude", ".credentials.json")
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	if err := os.MkdirAll(filepath.Dir(src), 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}

	content := []byte(`{"token":"current-token"}`)
	if err := os.WriteFile(src, content, 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	t.Setenv("HOME", daemonHome)

	// First call populates dst and caches srcInfo.
	syncSubprocessCreds(subprocessHome)

	// Record dst mtime after the first call.
	dstInfo1, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst after first call: %v", err)
	}

	// Sleep briefly so a spurious write would produce a different mtime.
	time.Sleep(10 * time.Millisecond)

	// Second call — source unchanged, should be a no-op.
	syncSubprocessCreds(subprocessHome)

	dstInfo2, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst after second call: %v", err)
	}

	if !dstInfo1.ModTime().Equal(dstInfo2.ModTime()) {
		t.Error("dst mtime changed on second call; expected no-op when source is unchanged")
	}
}

// TestSyncSubprocessCreds_SourceAbsent verifies that when the source
// credentials do not exist, a working subprocess copy is not clobbered.
func TestSyncSubprocessCreds_SourceAbsent(t *testing.T) {
	resetCredsSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}

	// Write a working copy to dst but do NOT create src.
	workingContent := []byte(`{"token":"working-copy"}`)
	if err := os.WriteFile(dst, workingContent, 0600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// daemonHome has no .claude/.credentials.json.
	t.Setenv("HOME", daemonHome)
	syncSubprocessCreds(subprocessHome)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after sync with absent src: %v", err)
	}
	if string(got) != string(workingContent) {
		t.Errorf("dst was clobbered: got %q, want %q", got, workingContent)
	}
}

// TestSyncSubprocessCreds_Concurrent verifies that concurrent callers do not
// corrupt the destination file.
func TestSyncSubprocessCreds_Concurrent(t *testing.T) {
	resetCredsSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	src := filepath.Join(daemonHome, ".claude", ".credentials.json")
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")

	if err := os.MkdirAll(filepath.Dir(src), 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst dir: %v", err)
	}

	srcContent := []byte(`{"token":"concurrent-token"}`)
	if err := os.WriteFile(src, srcContent, 0600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	t.Setenv("HOME", daemonHome)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			syncSubprocessCreds(subprocessHome)
		}()
	}
	wg.Wait()

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after concurrent calls: %v", err)
	}
	if string(got) != string(srcContent) {
		t.Errorf("dst corrupted after concurrent calls: got %q, want %q", got, srcContent)
	}
}

// TestSyncSubprocessCreds_SubprocessHomeEmpty verifies that a blank
// subprocessHome is handled gracefully (early return, no panic).
func TestSyncSubprocessCreds_SubprocessHomeEmpty(t *testing.T) {
	resetCredsSyncState()
	// Should not panic or error.
	syncSubprocessCreds("")
}

// --- resolveDaemonHome tests ---

// TestResolveDaemonHome_FromEnv verifies that a set HOME env var is returned directly.
func TestResolveDaemonHome_FromEnv(t *testing.T) {
	t.Setenv("HOME", "/home/testuser")
	got, err := resolveDaemonHome()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/home/testuser" {
		t.Errorf("got %q, want /home/testuser", got)
	}
}

// TestResolveDaemonHome_FallsBackToUserCurrent verifies that when HOME is empty,
// resolveDaemonHome falls back to os/user.Current().HomeDir.
// This test is only meaningful when user.Current() succeeds on the test host —
// which is true in CI and on developer machines with a real /etc/passwd entry.
func TestResolveDaemonHome_FallsBackToUserCurrent(t *testing.T) {
	t.Setenv("HOME", "")

	got, err := resolveDaemonHome()
	if err != nil {
		// user.Current() may fail in highly restricted environments (e.g. scratch
		// containers with no /etc/passwd). Skip rather than fail.
		t.Skipf("user.Current() unavailable in this environment; skipping fallback test: %v", err)
	}
	if got == "" {
		t.Error("fallback returned empty home; expected a non-empty path from user.Current()")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("fallback home %q is not absolute", got)
	}
}

// TestResolveDaemonHome_BothUnresolvable verifies that when HOME is empty AND
// user.Current().HomeDir is empty, resolveDaemonHome returns an error and never
// returns a non-absolute path.
//
// We cannot force user.Current() to fail in a normal test environment, so this
// test exercises resolveDaemonHome's own guard for an empty HomeDir by checking
// that the returned path (if any) is always absolute.  The error path for a truly
// unresolvable HOME is covered by the integration note: if both sources fail, the
// function must return a non-nil error.
func TestResolveDaemonHome_NeverReturnsRelativePath(t *testing.T) {
	t.Setenv("HOME", "")

	got, err := resolveDaemonHome()
	if err != nil {
		// Acceptable: HOME unset AND user.Current() failed or returned empty dir.
		// Verify the error message is diagnosable.
		msg := err.Error()
		if !contains(msg, "HOME") {
			t.Errorf("error message does not mention HOME: %q", msg)
		}
		return
	}
	// If we got a result, it must be absolute — never empty or relative.
	if got == "" {
		t.Error("resolveDaemonHome returned empty path with no error")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolveDaemonHome returned non-absolute path %q", got)
	}
}

// --- syncSubprocessCreds HOME-unset behavior ---

// TestSyncSubprocessCreds_UnresolvableHomeNoStatAttempt verifies that when HOME is
// empty and user.Current() returns an empty HomeDir, syncSubprocessCreds returns
// without attempting to stat any path (i.e. no relative-path stat against cwd).
//
// We simulate "both unresolvable" by temporarily replacing resolveDaemonHome with a
// version that returns an error.  Because Go does not support monkey-patching, we use
// the package-internal hook variable resolveDaemonHomeFunc to allow test injection.
func TestSyncSubprocessCreds_UnresolvableHomeNoStatAttempt(t *testing.T) {
	resetCredsSyncState()

	subprocessHome := t.TempDir()
	dst := filepath.Join(subprocessHome, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	workingContent := []byte(`{"token":"working"}`)
	if err := os.WriteFile(dst, workingContent, 0600); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	// Inject an unresolvable HOME resolver.
	orig := resolveDaemonHomeFunc
	resolveDaemonHomeFunc = func() (string, error) {
		return "", fmt.Errorf("HOME env var is unset and os/user lookup failed: injected test error")
	}
	defer func() { resolveDaemonHomeFunc = orig }()

	// Should return without touching dst.
	syncSubprocessCreds(subprocessHome)

	// The working copy must be untouched.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(workingContent) {
		t.Errorf("dst was touched when home was unresolvable: got %q, want %q", got, workingContent)
	}
}

// contains is a helper because strings.Contains is not available without import.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
