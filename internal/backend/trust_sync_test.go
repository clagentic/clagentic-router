// internal/backend/trust_sync_test.go — tests for syncProjectTrust
// (lr-4abfe9) and its TrustAllowlist containment (bobbie.uncat.1 follow-up).
//
// Follows the fake-bin state-capture pattern used elsewhere in this package
// (see codex_cli_test.go's TestCodexCLI_Invoke_SubprocessEnvFiltered): the
// Invoke-level tests assert on the actual .claude.json content a real
// subprocess would observe, using a temp HOME passed through
// NewClaudeCLIAdapter/NewCodexSubagentAdapter's normal call path — not on
// syncProjectTrust's internal function calls directly, except where a
// scenario (concurrency, malformed input) is easier to isolate at the
// function level.
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// readClaudeJSON reads and json-decodes the .claude.json at home into a
// generic map, for assertions that don't need the typed shape.
func readClaudeJSON(t *testing.T, home string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse .claude.json: %v", err)
	}
	return m
}

// projectEntry extracts projects[dir] as a map, failing the test if absent
// or the wrong shape.
func projectEntry(t *testing.T, cfg map[string]interface{}, dir string) map[string]interface{} {
	t.Helper()
	projects, ok := cfg["projects"].(map[string]interface{})
	if !ok {
		t.Fatalf("projects is not an object: %#v", cfg["projects"])
	}
	entry, ok := projects[dir].(map[string]interface{})
	if !ok {
		t.Fatalf("projects[%q] missing or not an object: %#v", dir, projects[dir])
	}
	return entry
}

// allowAll builds a TrustAllowlist containing exactly the given dirs
// (canonicalized the same way NewTrustAllowlist does), for tests that need
// syncProjectTrust to actually write.
func allowAll(t *testing.T, dirs ...string) *TrustAllowlist {
	t.Helper()
	return NewTrustAllowlist(dirs)
}

// TestSyncProjectTrust_EmptyProjectsMap verifies the defect-path scenario:
// starting from the isolated HOME's shipped empty projects map, a single
// Invoke against a real dir results in that dir's hasTrustDialogAccepted
// being true — when that dir is on the allowlist.
func TestSyncProjectTrust_EmptyProjectsMap(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0600); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}

	dir := t.TempDir()
	syncProjectTrust(home, dir, allowAll(t, dir))

	cfg := readClaudeJSON(t, home)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true", entry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_PreservesUnrelatedEntries verifies that pre-existing
// projects entries for OTHER directories, and unrelated per-project fields,
// survive the upsert untouched — the fix must write exactly one key for
// exactly the invoked path, never merge/replace the whole map.
func TestSyncProjectTrust_PreservesUnrelatedEntries(t *testing.T) {
	home := t.TempDir()
	other := "/some/other/project"
	seed := `{
		"projects": {
			"` + other + `": {
				"hasTrustDialogAccepted": true,
				"someOtherField": "keep-me"
			}
		},
		"unrelatedTopLevelKey": "keep-me-too"
	}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(seed), 0600); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}

	dir := t.TempDir()
	syncProjectTrust(home, dir, allowAll(t, dir))

	cfg := readClaudeJSON(t, home)

	if cfg["unrelatedTopLevelKey"] != "keep-me-too" {
		t.Errorf("unrelatedTopLevelKey = %v, want preserved", cfg["unrelatedTopLevelKey"])
	}

	otherEntry := projectEntry(t, cfg, other)
	if otherEntry["hasTrustDialogAccepted"] != true {
		t.Errorf("other project's hasTrustDialogAccepted changed: %v", otherEntry["hasTrustDialogAccepted"])
	}
	if otherEntry["someOtherField"] != "keep-me" {
		t.Errorf("other project's someOtherField = %v, want preserved", otherEntry["someOtherField"])
	}

	newEntry := projectEntry(t, cfg, dir)
	if newEntry["hasTrustDialogAccepted"] != true {
		t.Errorf("new dir hasTrustDialogAccepted = %v, want true", newEntry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_IdempotentReinvocation verifies that calling
// syncProjectTrust twice against the same allowlisted dir leaves the file's
// mtime unchanged on the second call (no-op fast path), and content correct.
func TestSyncProjectTrust_IdempotentReinvocation(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	allow := allowAll(t, dir)

	syncProjectTrust(home, dir, allow)
	path := filepath.Join(home, ".claude.json")
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first call: %v", err)
	}

	syncProjectTrust(home, dir, allow)
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second call: %v", err)
	}

	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("mtime changed on idempotent re-invocation; expected no-op when already trusted")
	}

	cfg := readClaudeJSON(t, home)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true", entry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_RepeatedInvocationDifferentDirs verifies that
// invoking against two different allowlisted dirs accumulates both entries
// rather than each overwriting the other.
func TestSyncProjectTrust_RepeatedInvocationDifferentDirs(t *testing.T) {
	home := t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()
	allow := allowAll(t, dirA, dirB)

	syncProjectTrust(home, dirA, allow)
	syncProjectTrust(home, dirB, allow)

	cfg := readClaudeJSON(t, home)
	for _, d := range []string{dirA, dirB} {
		entry := projectEntry(t, cfg, d)
		if entry["hasTrustDialogAccepted"] != true {
			t.Errorf("dir %q hasTrustDialogAccepted = %v, want true", d, entry["hasTrustDialogAccepted"])
		}
	}
}

// TestSyncProjectTrust_MalformedExistingFile verifies that a pre-existing
// .claude.json that fails to parse as JSON is left completely untouched —
// the "degrade safely and loudly" requirement. We cannot assert on the log
// output directly, but we assert the file content is byte-identical
// afterward, i.e. no clobbering / no guessed merge. Uses an allowlisted dir
// so the malformed-file path, not the allowlist gate, is what's exercised.
func TestSyncProjectTrust_MalformedExistingFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	malformed := []byte(`{"projects": this is not valid json`)
	if err := os.WriteFile(path, malformed, 0600); err != nil {
		t.Fatalf("seed malformed .claude.json: %v", err)
	}

	dir := t.TempDir()
	syncProjectTrust(home, dir, allowAll(t, dir))

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .claude.json after sync attempt: %v", err)
	}
	if string(got) != string(malformed) {
		t.Errorf(".claude.json was modified despite being unparseable: got %q, want %q (unchanged)", got, malformed)
	}
}

// TestSyncProjectTrust_MissingFile verifies that a completely absent
// .claude.json (fresh isolated HOME, not yet touched by the claude CLI at
// all) is treated as an empty config and a valid file is created, for an
// allowlisted dir.
func TestSyncProjectTrust_MissingFile(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	syncProjectTrust(home, dir, allowAll(t, dir))

	cfg := readClaudeJSON(t, home)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true", entry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_EmptyArgsAreNoOps verifies home=="" and dir=="" are
// both safe no-ops (no panic, no file created), even with an allowlist that
// would otherwise admit the dir.
func TestSyncProjectTrust_EmptyArgsAreNoOps(t *testing.T) {
	home := t.TempDir()

	syncProjectTrust("", "/some/dir", allowAll(t, "/some/dir"))
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("home=\"\" should not create a file anywhere, including a stray home dir")
	}

	syncProjectTrust(home, "", allowAll(t))
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("dir=\"\" should be a no-op; no .claude.json expected")
	}
}

// TestSyncProjectTrust_Concurrent verifies that many concurrent callers
// upserting different allowlisted dirs against the same .claude.json never
// corrupt it (always valid JSON at the end) and that every dir's entry is
// present — proving the mutex-serialized read-modify-write-rename
// discipline holds under race, mirroring TestSyncSubprocessCreds_Concurrent's
// coverage of the sibling credentials-sync race in claude_cli_test.go.
func TestSyncProjectTrust_Concurrent(t *testing.T) {
	home := t.TempDir()

	const goroutines = 20
	dirs := make([]string, goroutines)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	allow := allowAll(t, dirs...)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for _, d := range dirs {
		d := d
		go func() {
			defer wg.Done()
			syncProjectTrust(home, d, allow)
		}()
	}
	wg.Wait()

	cfg := readClaudeJSON(t, home)
	for _, d := range dirs {
		entry := projectEntry(t, cfg, d)
		if entry["hasTrustDialogAccepted"] != true {
			t.Errorf("dir %q hasTrustDialogAccepted = %v, want true", d, entry["hasTrustDialogAccepted"])
		}
	}
}

// --- TrustAllowlist containment (bobbie.uncat.1) ---

// TestSyncProjectTrust_NotOnAllowlist_NoWrite verifies the core containment
// fix: a dir that is NOT on the allowlist gets no trust write at all — no
// file created, not even an empty-projects skeleton.
func TestSyncProjectTrust_NotOnAllowlist_NoWrite(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	other := t.TempDir()

	// Allowlist covers a different directory only.
	syncProjectTrust(home, dir, allowAll(t, other))

	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("dir not on allowlist must receive no write — .claude.json should not exist")
	}
}

// TestSyncProjectTrust_NotOnAllowlist_ExistingFileUntouched verifies that
// when .claude.json already exists (e.g. another allowlisted dir already
// wrote to it), a call for a non-allowlisted dir leaves it byte-identical —
// no accidental empty-projects-map upsert, no mtime change.
func TestSyncProjectTrust_NotOnAllowlist_ExistingFileUntouched(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	seed := []byte(`{"projects":{}}`)
	if err := os.WriteFile(path, seed, 0600); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	dir := t.TempDir()
	other := t.TempDir()
	syncProjectTrust(home, dir, allowAll(t, other))

	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Error("mtime changed for a dir not on the allowlist; expected a total no-op")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(got) != string(seed) {
		t.Errorf(".claude.json content changed for a dir not on the allowlist: got %q, want %q", got, seed)
	}
}

// TestSyncProjectTrust_EmptyAllowlist_TrustsNothing verifies the fail-closed
// default: an empty (non-nil) TrustAllowlist and a nil TrustAllowlist both
// refuse every dir.
func TestSyncProjectTrust_EmptyAllowlist_TrustsNothing(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty allowlist", func(t *testing.T) {
		home := t.TempDir()
		syncProjectTrust(home, dir, NewTrustAllowlist(nil))
		if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
			t.Error("empty allowlist must trust nothing")
		}
	})

	t.Run("nil allowlist", func(t *testing.T) {
		home := t.TempDir()
		syncProjectTrust(home, dir, nil)
		if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
			t.Error("nil allowlist must trust nothing (fail closed)")
		}
	})
}

// TestSyncProjectTrust_SymlinkEscapeRefused verifies that a dir which is a
// symlink resolving OUTSIDE every allowlisted tree is refused — containment
// is evaluated on the canonicalized path, not the surface string.
func TestSyncProjectTrust_SymlinkEscapeRefused(t *testing.T) {
	home := t.TempDir()

	allowedTarget := t.TempDir()
	outsideTarget := t.TempDir()

	// Symlink named to look like it's "inside" a plausible project tree, but
	// resolving to a directory that was never allowlisted.
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "escape-link")
	if err := os.Symlink(outsideTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	allow := allowAll(t, allowedTarget)
	syncProjectTrust(home, link, allow)

	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("symlink resolving outside the allowlist must be refused, not matched on surface path")
	}
}

// TestSyncProjectTrust_SymlinkIntoAllowlistAdmitted verifies the converse:
// a symlink that resolves TO an allowlisted directory is admitted — the
// allowlist check follows real paths in both directions, it does not merely
// reject all symlinks.
func TestSyncProjectTrust_SymlinkIntoAllowlistAdmitted(t *testing.T) {
	home := t.TempDir()

	allowedTarget := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "into-allowlist")
	if err := os.Symlink(allowedTarget, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	allow := allowAll(t, allowedTarget)
	syncProjectTrust(home, link, allow)

	cfg := readClaudeJSON(t, home)
	// The entry is keyed by the exact dir string passed in (mirrors what the
	// CLI itself would look up for its own cmd.Dir, which is the symlink
	// path if that's what was passed as WorkingDir) — syncProjectTrust never
	// rewrites the key to its canonical form, only the allowlist check
	// resolves symlinks.
	entry := projectEntry(t, cfg, link)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true for symlink resolving into the allowlist", entry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_DotDotEscapeRefused verifies that a path containing
// ".." which resolves outside the allowlist is refused. filepath.EvalSymlinks
// cleans ".." components as part of resolving the real path, so this is
// covered by the same canonicalization as the symlink case.
func TestSyncProjectTrust_DotDotEscapeRefused(t *testing.T) {
	home := t.TempDir()

	allowedParent := t.TempDir()
	allowedChild := filepath.Join(allowedParent, "child")
	if err := os.MkdirAll(allowedChild, 0700); err != nil {
		t.Fatalf("mkdir allowed child: %v", err)
	}
	// sibling escapes the allowed child via ".." back up to the parent,
	// which is NOT itself on the allowlist.
	escaping := filepath.Join(allowedChild, "..", "..")

	allow := allowAll(t, allowedChild)
	syncProjectTrust(home, escaping, allow)

	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("a \"..\"-escaping path resolving outside the allowlist must be refused")
	}
}

// --- TrustAllowlist unit tests ---

// TestTrustAllowlist_NilAndEmptyRefuseEverything verifies TrustAllowlist's
// own fail-closed contract independent of syncProjectTrust.
func TestTrustAllowlist_NilAndEmptyRefuseEverything(t *testing.T) {
	dir := t.TempDir()

	var nilAllow *TrustAllowlist
	if nilAllow.Allows(dir) {
		t.Error("nil TrustAllowlist must refuse every dir")
	}

	empty := NewTrustAllowlist(nil)
	if empty.Allows(dir) {
		t.Error("empty TrustAllowlist must refuse every dir")
	}

	emptySlice := NewTrustAllowlist([]string{})
	if emptySlice.Allows(dir) {
		t.Error("TrustAllowlist built from an empty slice must refuse every dir")
	}
}

// TestTrustAllowlist_UnresolvableEntryDropped verifies that an entry which
// does not exist on disk (cannot be resolved via EvalSymlinks) is dropped at
// construction rather than causing a panic or being treated as a literal
// string match.
func TestTrustAllowlist_UnresolvableEntryDropped(t *testing.T) {
	parent := t.TempDir()
	nonexistent := filepath.Join(parent, "does-not-exist")

	allow := NewTrustAllowlist([]string{nonexistent})
	if allow.Allows(nonexistent) {
		t.Error("an allowlist entry that does not exist on disk must never be admitted")
	}
}

// TestTrustAllowlist_DefaultWorkingDirNotSpecialCased verifies that "/" gets
// no implicit trust — it must be explicitly configured like any other path.
func TestTrustAllowlist_DefaultWorkingDirNotSpecialCased(t *testing.T) {
	allow := NewTrustAllowlist(nil)
	if allow.Allows(DefaultWorkingDir) {
		t.Error("DefaultWorkingDir (\"/\") must not be trusted by an empty allowlist")
	}

	dir := t.TempDir()
	allowOther := NewTrustAllowlist([]string{dir})
	if allowOther.Allows(DefaultWorkingDir) {
		t.Error("DefaultWorkingDir (\"/\") must not be trusted merely because some other dir is allowlisted")
	}
}

// --- Invoke-level proofs (claude_cli.go / codex_subagent.go call sites) ---

// TestClaudeCLI_Invoke_TrustDialogPreAccepted proves, at the actual Invoke
// call site (not by calling syncProjectTrust directly), that
// ClaudeCLIAdapter pre-accepts the trust dialog for req.WorkingDir before
// the subprocess runs, using claudeSubprocessHome exactly as production
// code does, when that dir is on the adapter's TrustAllowlist.
func TestClaudeCLI_Invoke_TrustDialogPreAccepted(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello"}
		data, _ := json.Marshal(out)
		return data
	}

	dir := t.TempDir()
	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "claude", string(claudeSuccess()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, allowAll(t, dir))
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: dir}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	cfg := readClaudeJSON(t, fakeHome)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true for dir %q", entry["hasTrustDialogAccepted"], dir)
	}
}

// TestClaudeCLI_Invoke_TrustDialogNotOnAllowlist_NoWrite proves the
// containment fix at the Invoke call site: when req.WorkingDir is a real,
// valid directory (passes ResolveWorkingDir) but is NOT on the adapter's
// TrustAllowlist, no .claude.json is written at all — the subprocess must
// fail exactly as it did before lr-4abfe9.
func TestClaudeCLI_Invoke_TrustDialogNotOnAllowlist_NoWrite(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello"}
		data, _ := json.Marshal(out)
		return data
	}

	dir := t.TempDir()
	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "claude", string(claudeSuccess()))

	// No allowlist at all (nil) — the safe default an operator gets by
	// doing nothing.
	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, nil)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: dir}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fakeHome, ".claude.json")); !os.IsNotExist(err) {
		t.Error("working_dir not on trusted_working_dirs must receive no trust write")
	}
}

// TestClaudeCLI_Invoke_TrustDialogDefaultWorkingDir verifies the
// DefaultWorkingDir ("/") case: when the caller supplies no WorkingDir and
// "/" is NOT on the allowlist (the default posture — see
// trust_allowlist.go), no trust write happens for it.
func TestClaudeCLI_Invoke_TrustDialogDefaultWorkingDir(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello"}
		data, _ := json.Marshal(out)
		return data
	}

	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "claude", string(claudeSuccess()))

	// Allowlist covers some unrelated dir, not "/" — proves "/" gets no
	// implicit trust just because the allowlist is non-empty.
	other := t.TempDir()
	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, allowAll(t, other))
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fakeHome, ".claude.json")); !os.IsNotExist(err) {
		t.Error("DefaultWorkingDir (\"/\") must not receive a trust write unless explicitly allowlisted")
	}
}

// TestCodexSubagent_Invoke_TrustDialogPreAccepted proves codex_subagent.go,
// which shares claudeSubprocessHome and the same claude binary, also
// pre-accepts trust for its req.WorkingDir before the subprocess runs, when
// that dir is on the adapter's TrustAllowlist.
func TestCodexSubagent_Invoke_TrustDialogPreAccepted(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}

	dir := t.TempDir()
	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, allowAll(t, dir))
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: dir}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	cfg := readClaudeJSON(t, fakeHome)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true for dir %q", entry["hasTrustDialogAccepted"], dir)
	}
}

// TestCodexSubagent_Invoke_TrustDialogNotOnAllowlist_NoWrite mirrors the
// claude_cli containment proof for codex_subagent.go.
func TestCodexSubagent_Invoke_TrustDialogNotOnAllowlist_NoWrite(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}

	dir := t.TempDir()
	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, nil)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: dir}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fakeHome, ".claude.json")); !os.IsNotExist(err) {
		t.Error("working_dir not on trusted_working_dirs must receive no trust write (codex_subagent)")
	}
}

// TestCodexCLI_Invoke_NoTrustFileWritten proves codex_cli.go (no HOME
// override, different binary) is a true no-op with respect to trust-dialog
// syncing: no .claude.json is written anywhere reachable via
// claudeSubprocessHome, because this adapter never calls syncProjectTrust —
// independent of any allowlist, since codex_cli takes no TrustAllowlist
// parameter at all.
func TestCodexCLI_Invoke_NoTrustFileWritten(t *testing.T) {
	origHome := claudeSubprocessHome
	fakeHome := t.TempDir()
	claudeSubprocessHome = fakeHome
	defer func() { claudeSubprocessHome = origHome }()

	dir := t.TempDir()
	binDir := t.TempDir()
	binPath := writeFakeBin(t, binDir, "codex", "response from codex")

	adapter := NewCodexCLIAdapter("test", "", "", "", "", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: dir}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if _, err := os.Stat(filepath.Join(fakeHome, ".claude.json")); !os.IsNotExist(err) {
		t.Error("codex_cli must never write claudeSubprocessHome's .claude.json — it sets no HOME override and has no trust-dialog gate of its own")
	}
}
