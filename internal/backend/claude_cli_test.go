// internal/backend/claude_cli_test.go — unit tests for syncSubprocessCreds.
//
// Tests cover:
//   - Initial bootstrap when the subprocess copy is absent.
//   - Resync when the source file is updated (different content/mtime).
//   - No-op when the source is unchanged (fast path: mtime+size match).
//   - No-op when source is absent and a working copy exists (don't clobber).
//   - Concurrent callers do not corrupt the destination.
package backend

import (
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
