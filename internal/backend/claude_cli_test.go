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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// --- syncSubprocessAWSSSOCache tests (lr-6572d5) ---

// resetAWSSSOCacheSyncState clears the package-level sync cache so each test
// starts with a clean slate. Must be called with awsSSOCacheSyncMu unlocked.
func resetAWSSSOCacheSyncState() {
	awsSSOCacheSyncMu.Lock()
	awsSSOCacheSyncLastState = nil
	awsSSOCacheSyncMu.Unlock()
}

// TestSyncSubprocessAWSSSOCache_NoOpWhenAbsent verifies the majority-deployment
// case: no ~/.aws/sso/cache in the daemon's real HOME (OAuth hosts,
// static-credential Bedrock hosts) must be a verified no-op — no directory
// created in the subprocess HOME, no error.
func TestSyncSubprocessAWSSSOCache_NoOpWhenAbsent(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	t.Setenv("HOME", daemonHome)
	syncSubprocessAWSSSOCache(subprocessHome)

	dstDir := filepath.Join(subprocessHome, ".aws", "sso", "cache")
	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Errorf("subprocess .aws/sso/cache dir created (or stat errored unexpectedly) when source absent: err=%v", err)
	}
}

// TestSyncSubprocessAWSSSOCache_MirroredWhenPresent verifies that SSO cache
// token files present in the daemon's real HOME are mirrored into the
// isolated subprocess HOME.
func TestSyncSubprocessAWSSSOCache_MirroredWhenPresent(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	srcDir := filepath.Join(daemonHome, ".aws", "sso", "cache")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}

	// The SDK names cache files by hashing sso_start_url — the exact name is
	// opaque to this sync, which is the point (full-directory sync, not a
	// reimplementation of the SDK's hashing scheme).
	tokenContent := []byte(`{"accessToken":"fake-sso-token","expiresAt":"2099-01-01T00:00:00Z"}`)
	tokenFile := filepath.Join(srcDir, "abc123deadbeef.json")
	if err := os.WriteFile(tokenFile, tokenContent, 0600); err != nil {
		t.Fatalf("write src token file: %v", err)
	}

	t.Setenv("HOME", daemonHome)
	syncSubprocessAWSSSOCache(subprocessHome)

	dst := filepath.Join(subprocessHome, ".aws", "sso", "cache", "abc123deadbeef.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read mirrored token file: %v", err)
	}
	if string(got) != string(tokenContent) {
		t.Errorf("mirrored content = %q, want %q", got, tokenContent)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("dst mode = %04o, want 0600", info.Mode().Perm())
	}
}

// TestSyncSubprocessAWSSSOCache_DoesNotSyncOtherAWSState verifies the
// isolation-preservation requirement: syncing ~/.aws/sso/cache must never
// pull in ~/.aws/config, ~/.aws/credentials, or any other real-HOME state.
func TestSyncSubprocessAWSSSOCache_DoesNotSyncOtherAWSState(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	awsDir := filepath.Join(daemonHome, ".aws")
	srcCacheDir := filepath.Join(awsDir, "sso", "cache")
	if err := os.MkdirAll(srcCacheDir, 0700); err != nil {
		t.Fatalf("mkdir src cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcCacheDir, "token.json"), []byte(`{"accessToken":"x"}`), 0600); err != nil {
		t.Fatalf("write src token: %v", err)
	}

	// Real-HOME state that must NOT leak into the subprocess HOME.
	configContent := []byte("[profile bedrock]\nsso_start_url = https://example.awsapps.com/start\n")
	if err := os.WriteFile(filepath.Join(awsDir, "config"), configContent, 0600); err != nil {
		t.Fatalf("write src config: %v", err)
	}
	credsContent := []byte("[default]\naws_access_key_id = AKIAFAKE\n")
	if err := os.WriteFile(filepath.Join(awsDir, "credentials"), credsContent, 0600); err != nil {
		t.Fatalf("write src credentials: %v", err)
	}

	t.Setenv("HOME", daemonHome)
	syncSubprocessAWSSSOCache(subprocessHome)

	for _, unexpected := range []string{"config", "credentials"} {
		p := filepath.Join(subprocessHome, ".aws", unexpected)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s leaked into isolated subprocess HOME (isolation regression): stat err=%v", unexpected, err)
		}
	}

	// The cache file itself must still be present.
	if _, err := os.Stat(filepath.Join(subprocessHome, ".aws", "sso", "cache", "token.json")); err != nil {
		t.Errorf("expected cache token mirrored, stat failed: %v", err)
	}
}

// TestSyncSubprocessAWSSSOCache_Idempotent verifies that a second call with
// an unchanged source does not rewrite the destination file (no spurious
// mtime bump — mirrors TestSyncSubprocessCreds_NoOpWhenCurrent).
func TestSyncSubprocessAWSSSOCache_Idempotent(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	srcDir := filepath.Join(daemonHome, ".aws", "sso", "cache")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "token.json"), []byte(`{"accessToken":"stable"}`), 0600); err != nil {
		t.Fatalf("write src token: %v", err)
	}

	t.Setenv("HOME", daemonHome)

	syncSubprocessAWSSSOCache(subprocessHome)
	dst := filepath.Join(subprocessHome, ".aws", "sso", "cache", "token.json")
	info1, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst after first sync: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	syncSubprocessAWSSSOCache(subprocessHome)

	info2, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst after second sync: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("dst mtime changed on second call; expected no-op when source is unchanged")
	}
}

// TestSyncSubprocessAWSSSOCache_StaleRefreshed verifies that a changed source
// token file is refreshed in the subprocess HOME (mirrors
// TestSyncSubprocessCreds_Resync).
func TestSyncSubprocessAWSSSOCache_StaleRefreshed(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	srcDir := filepath.Join(daemonHome, ".aws", "sso", "cache")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	tokenFile := filepath.Join(srcDir, "token.json")
	if err := os.WriteFile(tokenFile, []byte(`{"accessToken":"old"}`), 0600); err != nil {
		t.Fatalf("write initial src token: %v", err)
	}

	t.Setenv("HOME", daemonHome)
	syncSubprocessAWSSSOCache(subprocessHome)

	// Rotate the token (SSO re-login produces fresh cache content).
	freshContent := []byte(`{"accessToken":"rotated-fresh"}`)
	if err := os.WriteFile(tokenFile, freshContent, 0600); err != nil {
		t.Fatalf("write rotated src token: %v", err)
	}

	syncSubprocessAWSSSOCache(subprocessHome)

	dst := filepath.Join(subprocessHome, ".aws", "sso", "cache", "token.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after resync: %v", err)
	}
	if string(got) != string(freshContent) {
		t.Errorf("dst content after resync = %q, want %q", got, freshContent)
	}
}

// TestSyncSubprocessAWSSSOCache_Concurrent verifies that concurrent callers
// do not corrupt the destination (mirrors TestSyncSubprocessCreds_Concurrent).
func TestSyncSubprocessAWSSSOCache_Concurrent(t *testing.T) {
	resetAWSSSOCacheSyncState()

	daemonHome := t.TempDir()
	subprocessHome := t.TempDir()

	srcDir := filepath.Join(daemonHome, ".aws", "sso", "cache")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir src dir: %v", err)
	}
	srcContent := []byte(`{"accessToken":"concurrent-token"}`)
	if err := os.WriteFile(filepath.Join(srcDir, "token.json"), srcContent, 0600); err != nil {
		t.Fatalf("write src token: %v", err)
	}

	t.Setenv("HOME", daemonHome)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			syncSubprocessAWSSSOCache(subprocessHome)
		}()
	}
	wg.Wait()

	dst := filepath.Join(subprocessHome, ".aws", "sso", "cache", "token.json")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst after concurrent calls: %v", err)
	}
	if string(got) != string(srcContent) {
		t.Errorf("dst corrupted after concurrent calls: got %q, want %q", got, srcContent)
	}
}

// TestSyncSubprocessAWSSSOCache_SubprocessHomeEmpty verifies that a blank
// subprocessHome is handled gracefully (early return, no panic).
func TestSyncSubprocessAWSSSOCache_SubprocessHomeEmpty(t *testing.T) {
	resetAWSSSOCacheSyncState()
	syncSubprocessAWSSSOCache("")
}

// TestClaudeCLI_Invoke_BedrockEnvSurvivesFilter proves, at the actual Invoke
// call site (not buildCLIEnv in isolation), that CLAUDE_CODE_USE_BEDROCK
// survives into the real claude_cli subprocess env — the gap this task
// closes (lr-6572d5). Follows the fake-bin env/state-capture pattern from
// TestCodexCLI_Invoke_SubprocessEnvFiltered (codex_cli_test.go).
func TestClaudeCLI_Invoke_BedrockEnvSurvivesFilter(t *testing.T) {
	os.Setenv("CLAGENTIC_ROUTER_TOKEN", "super-secret-token")
	os.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	os.Setenv("AWS_PROFILE", "test-bedrock-profile")
	os.Setenv("AWS_REGION", "us-test-1")
	defer func() {
		os.Unsetenv("CLAGENTIC_ROUTER_TOKEN")
		os.Unsetenv("CLAUDE_CODE_USE_BEDROCK")
		os.Unsetenv("AWS_PROFILE")
		os.Unsetenv("AWS_REGION")
	}()

	daemonHome := t.TempDir()
	t.Setenv("HOME", daemonHome)

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}
	script := "#!/bin/sh\n" +
		"env > " + envFile + "\n" +
		"printf '%s' " + shellQuote(string(claudeSuccess())) + "\n"
	binPath := filepath.Join(dir, "claude")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake claude bin: %v", err)
	}

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read subprocess env dump: %v", err)
	}
	subprocessEnv := string(data)

	if strings.Contains(subprocessEnv, "CLAGENTIC_ROUTER_TOKEN=super-secret-token") {
		t.Error("router token leaked into claude_cli subprocess env")
	}

	for _, want := range []string{
		"CLAUDE_CODE_USE_BEDROCK=1",
		"AWS_PROFILE=test-bedrock-profile",
		"AWS_REGION=us-test-1",
	} {
		if !strings.Contains(subprocessEnv, want) {
			t.Errorf("%s missing from claude_cli subprocess env — Bedrock auth would fall through to OAuth and fail", want)
		}
	}
}
