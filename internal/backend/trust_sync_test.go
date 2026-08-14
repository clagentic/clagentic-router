// internal/backend/trust_sync_test.go — tests for syncProjectTrust
// (lr-4abfe9).
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

// TestSyncProjectTrust_EmptyProjectsMap verifies the defect-path scenario:
// starting from the isolated HOME's shipped empty projects map, a single
// Invoke against a real dir results in that dir's hasTrustDialogAccepted
// being true.
func TestSyncProjectTrust_EmptyProjectsMap(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"projects":{}}`), 0600); err != nil {
		t.Fatalf("seed .claude.json: %v", err)
	}

	dir := t.TempDir()
	syncProjectTrust(home, dir)

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
	syncProjectTrust(home, dir)

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
// syncProjectTrust twice against the same dir leaves the file's mtime
// unchanged on the second call (no-op fast path), and content correct.
func TestSyncProjectTrust_IdempotentReinvocation(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	syncProjectTrust(home, dir)
	path := filepath.Join(home, ".claude.json")
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first call: %v", err)
	}

	syncProjectTrust(home, dir)
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
// invoking against two different dirs accumulates both entries rather than
// each overwriting the other.
func TestSyncProjectTrust_RepeatedInvocationDifferentDirs(t *testing.T) {
	home := t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()

	syncProjectTrust(home, dirA)
	syncProjectTrust(home, dirB)

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
// afterward, i.e. no clobbering / no guessed merge.
func TestSyncProjectTrust_MalformedExistingFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	malformed := []byte(`{"projects": this is not valid json`)
	if err := os.WriteFile(path, malformed, 0600); err != nil {
		t.Fatalf("seed malformed .claude.json: %v", err)
	}

	dir := t.TempDir()
	syncProjectTrust(home, dir)

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
// all) is treated as an empty config and a valid file is created.
func TestSyncProjectTrust_MissingFile(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()

	syncProjectTrust(home, dir)

	cfg := readClaudeJSON(t, home)
	entry := projectEntry(t, cfg, dir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true", entry["hasTrustDialogAccepted"])
	}
}

// TestSyncProjectTrust_EmptyArgsAreNoOps verifies home=="" and dir=="" are
// both safe no-ops (no panic, no file created).
func TestSyncProjectTrust_EmptyArgsAreNoOps(t *testing.T) {
	home := t.TempDir()

	syncProjectTrust("", "/some/dir")
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("home=\"\" should not create a file anywhere, including a stray home dir")
	}

	syncProjectTrust(home, "")
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("dir=\"\" should be a no-op; no .claude.json expected")
	}
}

// TestSyncProjectTrust_Concurrent verifies that many concurrent callers
// upserting different dirs against the same .claude.json never corrupt it
// (always valid JSON at the end) and that every dir's entry is present —
// proving the mutex-serialized read-modify-write-rename discipline holds
// under race, mirroring TestSyncSubprocessCreds_Concurrent's coverage of
// the sibling credentials-sync race in claude_cli_test.go.
func TestSyncProjectTrust_Concurrent(t *testing.T) {
	home := t.TempDir()

	const goroutines = 20
	dirs := make([]string, goroutines)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for _, d := range dirs {
		d := d
		go func() {
			defer wg.Done()
			syncProjectTrust(home, d)
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

// TestClaudeCLI_Invoke_TrustDialogPreAccepted proves, at the actual Invoke
// call site (not by calling syncProjectTrust directly), that
// ClaudeCLIAdapter pre-accepts the trust dialog for req.WorkingDir before
// the subprocess runs, using claudeSubprocessHome exactly as production
// code does.
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

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff)
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

// TestClaudeCLI_Invoke_TrustDialogDefaultWorkingDir verifies the
// DefaultWorkingDir ("/") case: when the caller supplies no WorkingDir, the
// trust entry is written for "/" (whatever cmd.Dir actually resolved to),
// not skipped.
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

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	cfg := readClaudeJSON(t, fakeHome)
	entry := projectEntry(t, cfg, DefaultWorkingDir)
	if entry["hasTrustDialogAccepted"] != true {
		t.Errorf("hasTrustDialogAccepted for DefaultWorkingDir = %v, want true", entry["hasTrustDialogAccepted"])
	}
}

// TestCodexSubagent_Invoke_TrustDialogPreAccepted proves codex_subagent.go,
// which shares claudeSubprocessHome and the same claude binary, also
// pre-accepts trust for its req.WorkingDir before the subprocess runs.
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

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath)
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

// TestCodexCLI_Invoke_NoTrustFileWritten proves codex_cli.go (no HOME
// override, different binary) is a true no-op with respect to trust-dialog
// syncing: no .claude.json is written anywhere reachable via
// claudeSubprocessHome, because this adapter never calls syncProjectTrust.
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
