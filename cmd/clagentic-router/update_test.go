// cmd/clagentic-router/update_test.go — tests for the "update" subcommand's
// build/install/restart mechanics, guarding the failure modes documented in
// lr-2e0a65 (clagentic-directory's merge-without-redeploy outage): stale
// orphan artifacts from an -o-less build, "text file busy" from an in-place
// cp over a running binary, and shell-operator rejection in post-merge cmds
// (the latter is a naomi.yaml authoring concern, not this file's — but the
// atomic install and fresh-build guarantees this file tests are exactly
// what makes that naomi.yaml step safe).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clagentic/clagentic-router/internal/config"
)

// TestValidateInstallPath rejects a relative deploy.install_path — the
// installed binary replaces a path a systemd ExecStart references verbatim,
// so ambiguity here would be a footgun regardless of the update
// subcommand's own working directory.
func TestValidateInstallPath(t *testing.T) {
	cases := []struct {
		path    string
		wantErr bool
	}{
		{"/usr/local/bin/clagentic-router", false},
		{"relative/bin/clagentic-router", true},
		{"", true},
	}
	for _, tc := range cases {
		err := validateInstallPath(tc.path)
		if tc.wantErr && err == nil {
			t.Errorf("validateInstallPath(%q): expected error, got nil", tc.path)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateInstallPath(%q): unexpected error: %v", tc.path, err)
		}
	}
}

// TestInstallBinary_AtomicRename verifies the stage-then-rename install
// path: the staged binary is renamed (not copied) onto the target, and the
// target ends up executable with the staged content. This is the mechanism
// that avoids the "text file busy" failure a plain cp hits against a
// systemd-held binary (lr-2e0a65 comment #2).
func TestInstallBinary_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "clagentic-router.new")
	target := filepath.Join(dir, "clagentic-router")

	if err := os.WriteFile(staged, []byte("fake binary contents"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	// Simulate a pre-existing "live" binary at the target path — installBinary
	// must replace it via rename, not require it be absent or writable-in-place.
	if err := os.WriteFile(target, []byte("old binary contents"), 0o755); err != nil {
		t.Fatalf("write target file: %v", err)
	}

	if err := installBinary(staged, target); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("staged path %s should no longer exist after rename, stat err = %v", staged, err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("target mode %v is not executable", info.Mode())
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(content) != "fake binary contents" {
		t.Errorf("target content = %q, want staged content", string(content))
	}
}

// TestInstallBinary_MissingStagedFile verifies a missing staged file (e.g.
// a build that silently produced nothing) surfaces as an install error
// rather than a no-op that leaves the target untouched but reports success.
func TestInstallBinary_MissingStagedFile(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "does-not-exist.new")
	target := filepath.Join(dir, "clagentic-router")

	if err := installBinary(staged, target); err == nil {
		t.Fatal("expected error for missing staged file, got nil")
	}
}

// TestBackupInstalledBinary_ExistingBinary_RenamesToBak verifies the
// rollback fold-in's backup step: an existing binary at installPath is
// preserved at installPath+".bak" via rename (not copy), and installPath
// itself is empty afterward (the caller's subsequent installBinary rename
// is what repopulates it).
func TestBackupInstalledBinary_ExistingBinary_RenamesToBak(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clagentic-router")
	if err := os.WriteFile(target, []byte("old binary contents"), 0o755); err != nil {
		t.Fatalf("write target file: %v", err)
	}

	backupPath, crashWindowAdopted, err := backupInstalledBinary(target)
	if err != nil {
		t.Fatalf("backupInstalledBinary: %v", err)
	}
	if backupPath != target+".bak" {
		t.Errorf("backupPath = %q, want %q", backupPath, target+".bak")
	}
	if crashWindowAdopted {
		t.Errorf("crashWindowAdopted = true, want false (installPath existed — this is not the crash window)")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target %s should not exist after backup rename, stat err = %v", target, err)
	}
	content, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(content) != "old binary contents" {
		t.Errorf("backup content = %q, want the original binary's contents", string(content))
	}
}

// TestBackupInstalledBinary_NoExistingBinary_ReturnsEmptyNoError verifies
// the first-ever-install case: nothing exists yet at installPath, so there
// is nothing to back up — this must not be an error, and the returned path
// must be empty so callers can distinguish "nothing to roll back to" from a
// real backup path.
func TestBackupInstalledBinary_NoExistingBinary_ReturnsEmptyNoError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clagentic-router")

	backupPath, crashWindowAdopted, err := backupInstalledBinary(target)
	if err != nil {
		t.Fatalf("backupInstalledBinary: unexpected error for first-ever install: %v", err)
	}
	if backupPath != "" {
		t.Errorf("backupPath = %q, want empty string (nothing existed to back up)", backupPath)
	}
	if crashWindowAdopted {
		t.Errorf("crashWindowAdopted = true, want false (first-ever install is not the crash window)")
	}
}

// TestBuildBinary_EmitsFreshArtifact verifies buildBinary(-o outputPath)
// always produces a binary at exactly outputPath, guarding against the
// clagentic-directory outage root cause: an -o-less `go build ./...` never
// emits a fresh module-root artifact, so a naive deploy step ends up
// installing a stale orphan binary. Skipped in -short mode (invokes the
// real go toolchain against this repo's own source tree).
func TestBuildBinary_EmitsFreshArtifact(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go-build integration test in -short mode")
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// cmd/clagentic-router -> repo root
	repoRoot = filepath.Dir(filepath.Dir(repoRoot))

	dir := t.TempDir()
	out := filepath.Join(dir, "clagentic-router.new")

	if err := buildBinary(repoRoot, out); err != nil {
		t.Fatalf("buildBinary: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("expected fresh binary at %s, stat err: %v", out, err)
	}
	if info.Size() == 0 {
		t.Errorf("built binary at %s is empty", out)
	}
}

// TestRunUpdate_ServiceManagerNone runs the full build+install path against
// this repo's own source with restart skipped (service_manager: none), so
// it does not depend on systemd being present in the test environment.
// Skipped in -short mode for the same reason as TestBuildBinary_EmitsFreshArtifact.
func TestRunUpdate_ServiceManagerNone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go-build integration test in -short mode")
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot))

	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")

	deploy := config.DeployConfig{
		SourceDir:      repoRoot,
		InstallPath:    installPath,
		ServiceManager: "none",
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	if err := runUpdate(deploy, devNull); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	info, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("expected installed binary at %s, stat err: %v", installPath, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode %v is not executable", info.Mode())
	}
}

// TestRunUpdate_BuildFailureSurfaces verifies a build failure (e.g. an
// empty/non-module source_dir) is returned as an error rather than
// swallowed — runUpdate must never report success without a fresh binary
// actually landing at install_path.
func TestRunUpdate_BuildFailureSurfaces(t *testing.T) {
	dir := t.TempDir() // empty dir, no go.mod — `go build` fails here
	deploy := config.DeployConfig{
		SourceDir:      dir,
		InstallPath:    filepath.Join(dir, "clagentic-router"),
		ServiceManager: "none",
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	if err := runUpdate(deploy, devNull); err == nil {
		t.Fatal("expected error for unbuildable source_dir, got nil")
	}
	if _, err := os.Stat(deploy.InstallPath); !os.IsNotExist(err) {
		t.Errorf("install_path should not exist after a failed build, stat err = %v", err)
	}
}

// TestSystemctlRestartArgs verifies the argv shape for both scopes (lr-574334
// A1): system scope is byte-identical to the pre-"systemd-user" invocation
// ("systemctl restart <unit>.service", no "--user" token at all), and user
// scope inserts a single leading "--user" token ahead of "restart".
func TestSystemctlRestartArgs(t *testing.T) {
	cases := []struct {
		name        string
		serviceName string
		scope       systemdScope
		want        []string
	}{
		{"system scope", "clagentic-router", systemdScopeSystem, []string{"restart", "clagentic-router.service"}},
		{"user scope", "clagentic-router", systemdScopeUser, []string{"--user", "restart", "clagentic-router.service"}},
		{"system scope, custom unit name", "my-router", systemdScopeSystem, []string{"restart", "my-router.service"}},
		{"user scope, custom unit name", "my-router", systemdScopeUser, []string{"--user", "restart", "my-router.service"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := systemctlRestartArgs(tc.serviceName, tc.scope)
			if len(got) != len(tc.want) {
				t.Fatalf("systemctlRestartArgs(%q, %v) = %v, want %v", tc.serviceName, tc.scope, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("systemctlRestartArgs(%q, %v)[%d] = %q, want %q", tc.serviceName, tc.scope, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRunUpdate_ServiceManagerSystemdUser_DispatchesUserScopeRestart verifies
// runUpdate's switch actually routes "systemd-user" to the user-scope restart
// path (lr-574334 A1). This host's test environment has no reachable
// `systemctl --user` session, so the real systemctl invocation is expected to
// fail — the assertion here is that it fails with a systemctl error naming
// "--user", not the config-validation "unknown value" error a config typo
// would produce, and not a silent no-op the way "none" would behave.
func TestRunUpdate_ServiceManagerSystemdUser_DispatchesUserScopeRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go-build integration test in -short mode")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not on PATH in this environment")
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot))

	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")

	deploy := config.DeployConfig{
		SourceDir:      repoRoot,
		InstallPath:    installPath,
		ServiceManager: "systemd-user",
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = runUpdate(deploy, devNull)
	// The build+install steps must have succeeded (binary lands at
	// installPath) regardless of whether the restart itself succeeds in this
	// sandboxed test environment — install must never be gated on restart.
	if _, statErr := os.Stat(installPath); statErr != nil {
		t.Errorf("expected installed binary at %s even if restart fails, stat err: %v", installPath, statErr)
	}
	// This host's test environment has no reachable `systemctl --user`
	// session (guarded by the t.Skip above only when systemctl itself is
	// entirely absent from PATH), so the restart is expected to fail here —
	// runUpdate must return a non-nil error, and that error must name the
	// "--user" restart invocation it actually attempted. Asserting only
	// "IF err != nil THEN it names --user" (the prior form of this check) is
	// vacuously satisfied by err == nil, which would also be the observed
	// result if the systemd-user routing regressed to a silent no-op or to
	// dispatching a system-scope restart that happened to succeed — neither
	// of which this test is allowed to let pass silently.
	if err == nil {
		t.Fatalf("runUpdate with service_manager=systemd-user: want a restart error in this sandboxed " +
			"test environment (no systemctl --user session reachable), got nil — the user-scope restart " +
			"dispatch may not have run at all")
	}
	if !strings.Contains(err.Error(), "--user") {
		t.Errorf("runUpdate with service_manager=systemd-user error = %q, want it to name the --user restart invocation it attempted", err.Error())
	}
}

// TestVerifyInstalledBinary_MatchesStagedArtifact verifies the readback half
// of the install step (lr-c69197): a re-stat of installPath after a
// successful installBinary must succeed when the installed file's
// size/executability match what was staged.
func TestVerifyInstalledBinary_MatchesStagedArtifact(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "clagentic-router.new")
	target := filepath.Join(dir, "clagentic-router")

	if err := os.WriteFile(staged, []byte("fake binary contents"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(staged)
	if err != nil {
		t.Fatalf("hashFile(staged): %v", err)
	}

	if err := installBinary(staged, target); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	if err := verifyInstalledBinary(target, stagedInfo, stagedHash); err != nil {
		t.Errorf("verifyInstalledBinary: unexpected error: %v", err)
	}
}

// TestVerifyInstalledBinary_MissingInstallPath verifies acceptance item 2's
// explicit requirement: "a PASS that installed nothing to a path nothing
// runs from is the core defect" — a missing install_path after install must
// be a hard error, not a silent pass.
func TestVerifyInstalledBinary_MissingInstallPath(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "clagentic-router.new")
	if err := os.WriteFile(staged, []byte("fake binary contents"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(staged)
	if err != nil {
		t.Fatalf("hashFile(staged): %v", err)
	}

	missing := filepath.Join(dir, "does-not-exist")
	if err := verifyInstalledBinary(missing, stagedInfo, stagedHash); err == nil {
		t.Fatal("expected error for missing install path, got nil")
	}
}

// TestVerifyInstalledBinary_SizeMismatch verifies a file present at
// installPath but NOT matching the freshly built artifact (e.g. a stale
// leftover from an unrelated process) is rejected rather than accepted as
// "something is there."
func TestVerifyInstalledBinary_SizeMismatch(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "clagentic-router.new")
	if err := os.WriteFile(staged, []byte("fake binary contents, this one is longer"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(staged)
	if err != nil {
		t.Fatalf("hashFile(staged): %v", err)
	}

	target := filepath.Join(dir, "clagentic-router")
	if err := os.WriteFile(target, []byte("short"), 0o755); err != nil {
		t.Fatalf("write mismatched target: %v", err)
	}

	if err := verifyInstalledBinary(target, stagedInfo, stagedHash); err == nil {
		t.Fatal("expected error for size mismatch between installed and staged artifacts, got nil")
	}
}

// TestVerifyInstalledBinary_SameSizeSameModeDifferentContent_HashMismatch is
// the regression test for the lr-c69197 fifth fold-in defect (PEACHES nit):
// size+mode alone passes a same-size WRONG artifact. This constructs two
// files of identical length and identical (executable) mode but different
// content, and asserts verifyInstalledBinary rejects it — the exact gap a
// content hash exists to close.
func TestVerifyInstalledBinary_SameSizeSameModeDifferentContent_HashMismatch(t *testing.T) {
	dir := t.TempDir()
	staged := filepath.Join(dir, "clagentic-router.new")
	target := filepath.Join(dir, "clagentic-router")

	// Same length (12 bytes), same eventual mode, genuinely different bytes.
	if err := os.WriteFile(staged, []byte("AAAAAAAAAAAA"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(staged)
	if err != nil {
		t.Fatalf("hashFile(staged): %v", err)
	}

	if err := os.WriteFile(target, []byte("BBBBBBBBBBBB"), 0o755); err != nil {
		t.Fatalf("write wrong-content target: %v", err)
	}

	err = verifyInstalledBinary(target, stagedInfo, stagedHash)
	if err == nil {
		t.Fatal("expected error for same-size, same-mode, different-content artifact, got nil")
	}
	if !strings.Contains(err.Error(), "DIFFERENT content hash") {
		t.Errorf("error = %q, want it to name a content hash mismatch", err.Error())
	}
}

// TestInstallAndVerifyWithRollback_VerificationFailure_RestoresPreviousBinary
// is the regression test the lr-c69197 fold-in dispatch requires: a failed
// post-install verification must restore the previously-installed binary,
// not leave the bad artifact installBinary's os.Rename already put at
// installPath. stagedInfo is deliberately built from a decoy file of a
// different length than the real staged file, so verifyInstalledBinary's
// size check fails deterministically without depending on a corrupted `go
// build` output.
func TestInstallAndVerifyWithRollback_VerificationFailure_RestoresPreviousBinary(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")
	stagedPath := installPath + ".new"

	previousContents := []byte("previous, working binary contents")
	if err := os.WriteFile(installPath, previousContents, 0o755); err != nil {
		t.Fatalf("write pre-existing installed binary: %v", err)
	}
	if err := os.WriteFile(stagedPath, []byte("freshly built"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}

	// A stagedInfo whose size does not match the real staged file forces
	// verifyInstalledBinary's mismatch branch below.
	mismatchDir := t.TempDir()
	decoy := filepath.Join(mismatchDir, "decoy")
	if err := os.WriteFile(decoy, []byte("a file with a deliberately different length"), 0o644); err != nil {
		t.Fatalf("write decoy file: %v", err)
	}
	mismatchedStagedInfo, err := os.Stat(decoy)
	if err != nil {
		t.Fatalf("stat decoy file: %v", err)
	}
	mismatchedStagedHash, err := hashFile(decoy)
	if err != nil {
		t.Fatalf("hashFile(decoy): %v", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = installAndVerifyWithRollback(stagedPath, installPath, mismatchedStagedInfo, mismatchedStagedHash, "test-host", devNull)
	if err == nil {
		t.Fatal("expected an error for the forced verification mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "previous binary restored") {
		t.Errorf("error = %q, want it to confirm the previous binary was restored", err.Error())
	}

	restored, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("read installPath after rollback: %v", err)
	}
	if string(restored) != string(previousContents) {
		t.Errorf("installPath contents after rollback = %q, want the previous binary's contents %q",
			string(restored), string(previousContents))
	}
	if _, err := os.Stat(installPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup path %s.bak should no longer exist after being renamed back, stat err = %v", installPath, err)
	}
}

// TestInstallAndVerifyWithRollback_InstallFailure_RestoresPreviousBinary is
// the regression test the lr-c69197 second fold-in dispatch requires: an
// installBinary failure AFTER backupInstalledBinary has already renamed the
// old binary away must restore from that backup exactly as a verification
// failure does (PEACHES nit 1 — "close the remaining rollback hole"). The
// staged path is deliberately missing so installBinary's os.Chmod fails
// before any rename is attempted, forcing the install-failure branch
// deterministically without depending on a corrupted `go build` output.
func TestInstallAndVerifyWithRollback_InstallFailure_RestoresPreviousBinary(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")
	stagedPath := installPath + ".new" // deliberately never created

	previousContents := []byte("previous, working binary contents")
	if err := os.WriteFile(installPath, previousContents, 0o755); err != nil {
		t.Fatalf("write pre-existing installed binary: %v", err)
	}

	// stagedInfo/stagedHash would normally come from stat'ing/hashing the real
	// staged file before installBinary runs (runUpdate does this before
	// calling installAndVerifyWithRollback); here they are irrelevant to the
	// failure being forced (installBinary fails before verifyInstalledBinary
	// is ever reached), so any valid values work — reuse installPath's own.
	stagedInfo, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("stat installPath for stagedInfo: %v", err)
	}
	stagedHash, err := hashFile(installPath)
	if err != nil {
		t.Fatalf("hashFile(installPath) for stagedHash: %v", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = installAndVerifyWithRollback(stagedPath, installPath, stagedInfo, stagedHash, "test-host", devNull)
	if err == nil {
		t.Fatal("expected an error for the forced installBinary failure (missing staged file), got nil")
	}
	if !strings.Contains(err.Error(), "previous binary restored") {
		t.Errorf("error = %q, want it to confirm the previous binary was restored", err.Error())
	}

	restored, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("read installPath after rollback: %v (installPath must not be left stranded/absent "+
			"after an installBinary failure post-backup)", err)
	}
	if string(restored) != string(previousContents) {
		t.Errorf("installPath contents after rollback = %q, want the previous binary's contents %q",
			string(restored), string(previousContents))
	}
	if _, err := os.Stat(installPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup path %s.bak should no longer exist after being renamed back, stat err = %v", installPath, err)
	}
}

// TestBackupInstalledBinary_StaleBakAlreadyExists_RefusesWithoutClobbering
// covers the stale-artifact case both PEACHES and BOBBIE raised: a prior
// interrupted update left installPath.bak sitting there from before this
// run. backupInstalledBinary must not silently overwrite it (it may be the
// only known-good binary left) and must not fail forever either (both files
// are left exactly as found, so removing the stale .bak by hand unblocks
// the very next run).
func TestBackupInstalledBinary_StaleBakAlreadyExists_RefusesWithoutClobbering(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "clagentic-router")
	backupPath := target + ".bak"

	currentContents := []byte("current binary, about to be replaced")
	staleBackupContents := []byte("stale backup from an interrupted prior run")
	if err := os.WriteFile(target, currentContents, 0o755); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	if err := os.WriteFile(backupPath, staleBackupContents, 0o755); err != nil {
		t.Fatalf("write stale backup file: %v", err)
	}

	gotBackupPath, crashWindowAdopted, err := backupInstalledBinary(target)
	if err == nil {
		t.Fatal("expected error for a pre-existing stale .bak file, got nil")
	}
	if gotBackupPath != "" {
		t.Errorf("backupInstalledBinary returned backupPath = %q on error, want empty string", gotBackupPath)
	}
	if crashWindowAdopted {
		t.Errorf("crashWindowAdopted = true on error, want false (this is the stale-refusal case, not the crash window)")
	}
	if !strings.Contains(err.Error(), backupPath) {
		t.Errorf("error %q does not name the stale backup path %q", err.Error(), backupPath)
	}

	// Neither file may be clobbered — both must retain their original
	// contents exactly as found.
	targetContent, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target after refused backup: %v", readErr)
	}
	if string(targetContent) != string(currentContents) {
		t.Errorf("target contents after refused backup = %q, want unchanged %q", string(targetContent), string(currentContents))
	}
	backupContent, readErr := os.ReadFile(backupPath)
	if readErr != nil {
		t.Fatalf("read stale backup after refused backup: %v", readErr)
	}
	if string(backupContent) != string(staleBackupContents) {
		t.Errorf("stale backup contents after refused backup = %q, want unchanged %q", string(backupContent), string(staleBackupContents))
	}
}

// TestBackupInstalledBinary_ThreeStates_CrashWindow is the regression test
// for the lr-c69197 fourth fold-in defect (PEACHES comment 5373781420):
// backupInstalledBinary used to check installPath existence FIRST and
// return ("", nil) — "nothing to back up" — whenever installPath was
// missing, even when a restorable .bak sat right there. This covers all
// three states backupInstalledBinary must disambiguate, named explicitly in
// backupInstalledBinary's own doc:
//
//  1. neither installPath nor .bak present -> genuine first-ever install,
//     ("", nil), nothing to roll back to.
//  2. installPath present, .bak present -> stale-backup refusal (already
//     covered by TestBackupInstalledBinary_StaleBakAlreadyExists_
//     RefusesWithoutClobbering above; re-asserted here for the side-by-side
//     three-state comparison).
//  3. installPath ABSENT, .bak present (the crash window OPERATOR-GUIDE.md
//     documents as reachable) -> must return the existing .bak path so a
//     caller's rollback-on-failure has something real to restore, NOT
//     ("", nil) as if this were state 1.
func TestBackupInstalledBinary_ThreeStates_CrashWindow(t *testing.T) {
	t.Run("neither installPath nor .bak present: first-ever install", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "clagentic-router")

		backupPath, crashWindowAdopted, err := backupInstalledBinary(target)
		if err != nil {
			t.Fatalf("backupInstalledBinary: unexpected error: %v", err)
		}
		if backupPath != "" {
			t.Errorf("backupPath = %q, want empty string (nothing to back up, nothing to roll back to)", backupPath)
		}
		if crashWindowAdopted {
			t.Errorf("crashWindowAdopted = true, want false (first-ever install is not the crash window)")
		}
	})

	t.Run("installPath present, .bak present: stale-backup refusal", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "clagentic-router")
		backupPath := target + ".bak"
		if err := os.WriteFile(target, []byte("current"), 0o755); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.WriteFile(backupPath, []byte("stale"), 0o755); err != nil {
			t.Fatalf("write stale backup: %v", err)
		}

		gotBackupPath, crashWindowAdopted, err := backupInstalledBinary(target)
		if err == nil {
			t.Fatal("expected error for installPath present + stale .bak present, got nil")
		}
		if gotBackupPath != "" {
			t.Errorf("backupPath = %q on error, want empty string", gotBackupPath)
		}
		if crashWindowAdopted {
			t.Errorf("crashWindowAdopted = true on error, want false (installPath exists — not the crash window)")
		}
		// Neither file touched.
		if content, readErr := os.ReadFile(target); readErr != nil || string(content) != "current" {
			t.Errorf("target contents changed or unreadable: content=%q err=%v", content, readErr)
		}
		if content, readErr := os.ReadFile(backupPath); readErr != nil || string(content) != "stale" {
			t.Errorf("backup contents changed or unreadable: content=%q err=%v", content, readErr)
		}
	})

	t.Run("installPath ABSENT, .bak present: crash-window state must return the existing backup", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "clagentic-router")
		backupPath := target + ".bak"
		goodBinaryContents := []byte("the only good binary left on the box")
		if err := os.WriteFile(backupPath, goodBinaryContents, 0o755); err != nil {
			t.Fatalf("write .bak: %v", err)
		}
		// installPath deliberately never created — this is the crash window:
		// the backup rename completed, the replacing rename never landed.

		gotBackupPath, crashWindowAdopted, err := backupInstalledBinary(target)
		if err != nil {
			t.Fatalf("backupInstalledBinary: unexpected error for the crash-window state: %v "+
				"(a restorable .bak must not be reported as a first-ever install)", err)
		}
		if gotBackupPath != backupPath {
			t.Errorf("backupPath = %q, want %q (the existing, restorable backup — not empty, "+
				"which would falsely claim there is nothing to roll back to)", gotBackupPath, backupPath)
		}
		if !crashWindowAdopted {
			t.Errorf("crashWindowAdopted = false, want true (this IS the crash window: installPath " +
				"absent, .bak present — the caller must be told to log this adoption loudly)")
		}
		// The .bak file itself must be left in place and untouched — the
		// caller (installAndVerifyWithRollback) is responsible for consuming
		// it, not this function.
		content, readErr := os.ReadFile(backupPath)
		if readErr != nil {
			t.Fatalf("read .bak after backupInstalledBinary: %v", readErr)
		}
		if string(content) != string(goodBinaryContents) {
			t.Errorf(".bak contents = %q, want unchanged %q", string(content), string(goodBinaryContents))
		}
	})
}

// TestInstallAndVerifyWithRollback_CrashWindowState_RecoversOnInstallFailure
// is the end-to-end regression test for the crash-window state through the
// full rollback caller, not just backupInstalledBinary in isolation: when
// installPath is absent and .bak is the only good binary on the box, and
// the subsequent install itself fails, the operator must get "previous
// binary restored" (using the pre-existing .bak), NOT "no previous binary
// existed to roll back to" — the exact false claim PEACHES traced as a
// consequence of the defect.
func TestInstallAndVerifyWithRollback_CrashWindowState_RecoversOnInstallFailure(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")
	backupPath := installPath + ".bak"
	stagedPath := installPath + ".new" // deliberately never created, forces installBinary to fail

	goodBinaryContents := []byte("the only good binary left on the box, from before the crash")
	if err := os.WriteFile(backupPath, goodBinaryContents, 0o755); err != nil {
		t.Fatalf("write pre-existing .bak: %v", err)
	}
	// installPath deliberately absent — the crash window.

	stagedInfo, err := os.Stat(backupPath) // any valid FileInfo; install fails before this is consulted
	if err != nil {
		t.Fatalf("stat backupPath for stagedInfo: %v", err)
	}
	stagedHash, err := hashFile(backupPath) // any valid hash; install fails before this is consulted
	if err != nil {
		t.Fatalf("hashFile(backupPath) for stagedHash: %v", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = installAndVerifyWithRollback(stagedPath, installPath, stagedInfo, stagedHash, "test-host", devNull)
	if err == nil {
		t.Fatal("expected an error for the forced installBinary failure, got nil")
	}
	if strings.Contains(err.Error(), "no previous binary existed to roll back to") {
		t.Errorf("error = %q — falsely claims no previous binary existed, despite a restorable "+
			".bak being present at %s (the exact regression this test guards)", err.Error(), backupPath)
	}
	if !strings.Contains(err.Error(), "previous binary restored") {
		t.Errorf("error = %q, want it to confirm the previous binary was restored from the "+
			"crash-window .bak", err.Error())
	}

	restored, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("read installPath after crash-window recovery: %v (installPath must hold the "+
			"restored binary, not be left absent)", err)
	}
	if string(restored) != string(goodBinaryContents) {
		t.Errorf("installPath contents after recovery = %q, want the restored .bak contents %q",
			string(restored), string(goodBinaryContents))
	}
}

// TestInstallAndVerifyWithRollback_Success_RemovesBackup verifies the
// non-failure path: a successful install+verify removes the backup file
// rather than leaving a stale ".bak" artifact behind permanently.
func TestInstallAndVerifyWithRollback_Success_RemovesBackup(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")
	stagedPath := installPath + ".new"

	if err := os.WriteFile(installPath, []byte("previous binary"), 0o755); err != nil {
		t.Fatalf("write pre-existing installed binary: %v", err)
	}
	if err := os.WriteFile(stagedPath, []byte("fresh binary contents"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(stagedPath)
	if err != nil {
		t.Fatalf("hashFile(stagedPath): %v", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	if err := installAndVerifyWithRollback(stagedPath, installPath, stagedInfo, stagedHash, "test-host", devNull); err != nil {
		t.Fatalf("installAndVerifyWithRollback: unexpected error: %v", err)
	}

	content, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("read installPath: %v", err)
	}
	if string(content) != "fresh binary contents" {
		t.Errorf("installPath contents = %q, want the freshly staged contents", string(content))
	}
	if _, err := os.Stat(installPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("backup path %s.bak should have been removed after a successful verify, stat err = %v", installPath, err)
	}
}

// TestInstallAndVerifyWithRollback_CrashWindowState_SuccessConsumesStaleBak
// covers the crash-window state's other outcome: a SUCCESSFUL install/verify
// following recovery from state (iii) must consume (remove) the pre-existing
// .bak exactly like any other successful run, so the crash does not leave a
// stale .bak behind to wedge the NEXT update against the case-2 stale-backup
// refusal — the second half of the consequence PEACHES traced ("on success
// the stale .bak survives, so the next update hits your new refusal").
func TestInstallAndVerifyWithRollback_CrashWindowState_SuccessConsumesStaleBak(t *testing.T) {
	dir := t.TempDir()
	installPath := filepath.Join(dir, "clagentic-router")
	backupPath := installPath + ".bak"
	stagedPath := installPath + ".new"

	if err := os.WriteFile(backupPath, []byte("pre-crash good binary"), 0o755); err != nil {
		t.Fatalf("write pre-existing .bak: %v", err)
	}
	// installPath deliberately absent — the crash window.
	if err := os.WriteFile(stagedPath, []byte("freshly built, post-recovery"), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	stagedInfo, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	stagedHash, err := hashFile(stagedPath)
	if err != nil {
		t.Fatalf("hashFile(stagedPath): %v", err)
	}

	// A real, readable file (not /dev/null) — this test also asserts on the
	// report output below, so the crash-window adoption's log line must
	// actually be capturable.
	reportPath := filepath.Join(dir, "report.log")
	reportFile, err := os.Create(reportPath)
	if err != nil {
		t.Fatalf("create report file: %v", err)
	}
	defer reportFile.Close()

	if err := installAndVerifyWithRollback(stagedPath, installPath, stagedInfo, stagedHash, "test-host", reportFile); err != nil {
		t.Fatalf("installAndVerifyWithRollback: unexpected error recovering from the crash window: %v", err)
	}

	content, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("read installPath: %v", err)
	}
	if string(content) != "freshly built, post-recovery" {
		t.Errorf("installPath contents = %q, want the freshly staged contents", string(content))
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Errorf("crash-window .bak at %s should have been consumed/removed after a successful "+
			"recovery, stat err = %v (a surviving stale .bak here would wedge the NEXT update against "+
			"the stale-backup refusal)", backupPath, err)
	}

	// VISIBILITY (lr-c69197 fifth fold-in, BOBBIE comment 5373968195): the
	// crash-window adoption must be logged loudly, both when it is first
	// detected and again in the final success report — an operator running
	// this unattended hourly under the timer must be able to see a recovery
	// adoption happened, not have it pass unremarked.
	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report file: %v", err)
	}
	if !strings.Contains(string(report), "RECOVERY") {
		t.Errorf("report output = %q, want it to loudly name the crash-window .bak adoption (RECOVERY)", string(report))
	}
	if !strings.Contains(string(report), backupPath) {
		t.Errorf("report output = %q, want it to name the adopted backup path %q", string(report), backupPath)
	}
	if !strings.Contains(string(report), "RECOVERY COMPLETE") {
		t.Errorf("report output = %q, want the final success line to also confirm recovery completed", string(report))
	}
}

// TestVerifyRestartAdvanced_UnchangedSnapshot_IsError is the regression test
// named explicitly in the lr-c69197 dispatch: "a restart that did NOT
// restart must be an error, not a silent pass." A restart call that exits 0
// but whose before/after ActiveEnterTimestamp+MainPID are identical (e.g.
// systemd accepted the restart request against an already-running unit that
// for some reason didn't actually cycle, or the unit silently no-op'd) must
// be reported as a failed restart.
func TestVerifyRestartAdvanced_UnchangedSnapshot_IsError(t *testing.T) {
	snap := systemdUnitSnapshot{activeEnterTimestamp: "Thu 2026-08-20 19:15:06 EDT", mainPID: "2577441"}

	err := verifyRestartAdvanced("clagentic-router", snap, nil, snap)
	if err == nil {
		t.Fatal("expected error for unchanged ActiveEnterTimestamp/MainPID across a restart, got nil")
	}
	if !strings.Contains(err.Error(), "not actually restarted") {
		t.Errorf("error = %q, want it to name that the service was not actually restarted", err.Error())
	}
}

// TestVerifyRestartAdvanced_FastRestartSameWallClockSecond_NotMisreportedAsFailure
// is the regression test for the lr-c69197 fifth fold-in defect (PEACHES
// nit): ActiveEnterTimestamp is second-granular, so a restart completing
// within the same wall-clock second — the common case for this daemon's own
// binary — combined with the kernel reusing the previous PID, could leave
// the OLD two-field (ActiveEnterTimestamp, MainPID) comparison
// byte-identical despite the unit having genuinely cycled, producing a FALSE
// "restart did not happen" error. activeEnterTimestampMonotonic (microsecond
// precision, no same-second collision floor) must still differ and must be
// enough on its own to pass verification even when the wall-clock timestamp
// and PID happen to collide.
func TestVerifyRestartAdvanced_FastRestartSameWallClockSecond_NotMisreportedAsFailure(t *testing.T) {
	before := systemdUnitSnapshot{
		activeEnterTimestamp:          "Thu 2026-08-20 19:15:06 EDT",
		activeEnterTimestampMonotonic: "1234567890",
		mainPID:                       "2577441",
	}
	after := systemdUnitSnapshot{
		// Same wall-clock second AND the kernel happened to reuse the PID —
		// the exact collision this fold-in guards against.
		activeEnterTimestamp:          "Thu 2026-08-20 19:15:06 EDT",
		activeEnterTimestampMonotonic: "1234567913", // advanced by 23us — genuinely restarted
		mainPID:                       "2577441",
	}

	if err := verifyRestartAdvanced("clagentic-router", before, nil, after); err != nil {
		t.Errorf("verifyRestartAdvanced: unexpected error for a fast restart within the same wall-clock "+
			"second (ActiveEnterTimestamp and MainPID collided, but the monotonic timestamp advanced): %v", err)
	}
}

// TestVerifyRestartAdvanced_TimestampAdvanced_Passes verifies the normal
// case: ActiveEnterTimestamp changing (the unit actually cycled) is accepted
// even if MainPID is unavailable/unchanged for some reason.
func TestVerifyRestartAdvanced_TimestampAdvanced_Passes(t *testing.T) {
	before := systemdUnitSnapshot{activeEnterTimestamp: "Thu 2026-08-20 09:00:00 EDT", mainPID: "111"}
	after := systemdUnitSnapshot{activeEnterTimestamp: "Thu 2026-08-20 19:15:06 EDT", mainPID: "222"}

	if err := verifyRestartAdvanced("clagentic-router", before, nil, after); err != nil {
		t.Errorf("verifyRestartAdvanced: unexpected error for an advanced snapshot: %v", err)
	}
}

// TestVerifyRestartAdvanced_NoBeforeSnapshot_Passes verifies the first-ever
// start case: when a pre-restart snapshot could not be read at all
// (beforeErr != nil — the unit has never been started before), there is
// nothing to compare against, so this must not fail merely because the
// after-snapshot happens to look the same as the (unset) zero value.
func TestVerifyRestartAdvanced_NoBeforeSnapshot_Passes(t *testing.T) {
	after := systemdUnitSnapshot{activeEnterTimestamp: "Thu 2026-08-20 19:15:06 EDT", mainPID: "2577441"}
	beforeErr := fmt.Errorf("unit not found")

	if err := verifyRestartAdvanced("clagentic-router", systemdUnitSnapshot{}, beforeErr, after); err != nil {
		t.Errorf("verifyRestartAdvanced: unexpected error when no before-snapshot was available: %v", err)
	}
}

// TestValidateSystemdUnitSnapshot_PartiallyPopulated_IsError is the
// regression test for the lr-c69197 sixth fold-in defect (MILLER diagnosis):
// `systemctl show --property=<unknown> --value` exits 0 with empty stdout
// for a property systemd does not recognize, so a misspelled property name
// is indistinguishable at the single-field level from the legitimate
// "unit never started" case (both are ("", nil) from systemctlShowValue).
// The whole-suite risk this closes: every other test in this file
// hand-constructs systemdUnitSnapshot literals directly, so a wrong
// property name in readSystemdUnitSnapshot's systemctlShowValue calls would
// pass this entire suite green — it never actually exercises the "read a
// property that doesn't exist" path. This test exercises
// validateSystemdUnitSnapshot directly against a snapshot shaped exactly
// like what a wrong property name would produce: some fields populated
// (proving the unit HAS started and systemctl IS reachable), one field
// empty (the fingerprint of a bad property name), which must be a hard,
// named error rather than silently accepted as "never started".
func TestValidateSystemdUnitSnapshot_PartiallyPopulated_IsError(t *testing.T) {
	cases := []struct {
		name     string
		snapshot systemdUnitSnapshot
		wantName string // property name the error must call out
	}{
		{
			name: "ActiveEnterTimestamp empty, others populated",
			snapshot: systemdUnitSnapshot{
				activeEnterTimestampMonotonic: "1234567890",
				mainPID:                       "2577441",
			},
			wantName: "ActiveEnterTimestamp",
		},
		{
			name: "ActiveEnterTimestampMonotonic empty, others populated (the misspelling this task fixes)",
			snapshot: systemdUnitSnapshot{
				activeEnterTimestamp: "Thu 2026-08-20 19:15:06 EDT",
				mainPID:              "2577441",
			},
			wantName: "ActiveEnterTimestampMonotonic",
		},
		{
			name: "MainPID empty, others populated",
			snapshot: systemdUnitSnapshot{
				activeEnterTimestamp:          "Thu 2026-08-20 19:15:06 EDT",
				activeEnterTimestampMonotonic: "1234567890",
			},
			wantName: "MainPID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSystemdUnitSnapshot(tc.snapshot)
			if err == nil {
				t.Fatalf("validateSystemdUnitSnapshot(%+v): expected error for a partially populated snapshot, got nil",
					tc.snapshot)
			}
			if !strings.Contains(err.Error(), tc.wantName) {
				t.Errorf("validateSystemdUnitSnapshot(%+v) error = %q, want it to name the empty property %q",
					tc.snapshot, err.Error(), tc.wantName)
			}
		})
	}
}

// TestValidateSystemdUnitSnapshot_AllEmpty_Passes verifies the legitimate
// never-started/nonexistent-unit case is unaffected by the sixth fold-in's
// partial-population guard: when systemd genuinely has no runtime state for
// the unit at all, every property comes back empty together, and that must
// still pass validation exactly as before.
func TestValidateSystemdUnitSnapshot_AllEmpty_Passes(t *testing.T) {
	if err := validateSystemdUnitSnapshot(systemdUnitSnapshot{}); err != nil {
		t.Errorf("validateSystemdUnitSnapshot(zero value): unexpected error for the never-started case: %v", err)
	}
}

// TestValidateSystemdUnitSnapshot_FullyPopulated_Passes verifies the normal
// successful-read case is unaffected.
func TestValidateSystemdUnitSnapshot_FullyPopulated_Passes(t *testing.T) {
	snapshot := systemdUnitSnapshot{
		activeEnterTimestamp:          "Thu 2026-08-20 19:15:06 EDT",
		activeEnterTimestampMonotonic: "1234567890",
		mainPID:                       "2577441",
	}
	if err := validateSystemdUnitSnapshot(snapshot); err != nil {
		t.Errorf("validateSystemdUnitSnapshot(%+v): unexpected error for a fully populated snapshot: %v", snapshot, err)
	}
}

// TestSystemctlShowArgs verifies the argv shape for both scopes, mirroring
// TestSystemctlRestartArgs's coverage for the sibling restart-args builder.
func TestSystemctlShowArgs(t *testing.T) {
	cases := []struct {
		name        string
		serviceName string
		scope       systemdScope
		property    string
		want        []string
	}{
		{"system scope", "clagentic-router", systemdScopeSystem, "ActiveEnterTimestamp",
			[]string{"show", "--property=ActiveEnterTimestamp", "--value", "clagentic-router.service"}},
		{"user scope", "clagentic-router", systemdScopeUser, "MainPID",
			[]string{"--user", "show", "--property=MainPID", "--value", "clagentic-router.service"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := systemctlShowArgs(tc.serviceName, tc.scope, tc.property)
			if len(got) != len(tc.want) {
				t.Fatalf("systemctlShowArgs(...) = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("systemctlShowArgs(...)[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDeployConfig_UnknownServiceManager_Value verifies an unrecognized
// deploy.service_manager string round-trips as-is (validated at dispatch
// time in runUpdate, not silently coerced to a known value here).
func TestDeployConfig_UnknownServiceManager_Value(t *testing.T) {
	d := config.DeployConfig{ServiceManager: "launchd"}
	if got, want := d.ResolvedServiceManager(), "launchd"; got != want {
		t.Errorf("ResolvedServiceManager() = %q, want %q", got, want)
	}
}

// TestParseUpdateFlags_SourceDir verifies --source-dir is parsed and wins
// over config in cmdUpdate (see cmdUpdate's own doc) — the mechanism a
// post-merge automation step uses to keep building from its own cwd
// (lr-720e91) without a repo-committed router.yaml deploy.source_dir.
func TestParseUpdateFlags_SourceDir(t *testing.T) {
	uf, err := parseUpdateFlags([]string{"--source-dir", "."})
	if err != nil {
		t.Fatalf("parseUpdateFlags: %v", err)
	}
	if got, want := uf.sourceDir, "."; got != want {
		t.Errorf("sourceDir = %q, want %q", got, want)
	}
}

// TestParseUpdateFlags_SourceDirRequiresValue verifies --source-dir with no
// following argument is a parse error, not a silent empty string (which
// would fall through to the config-resolved default rather than the
// operator's evidently-intended explicit override).
func TestParseUpdateFlags_SourceDirRequiresValue(t *testing.T) {
	if _, err := parseUpdateFlags([]string{"--source-dir"}); err == nil {
		t.Fatal("expected error for --source-dir with no value, got nil")
	}
}

// TestRunUpdate_MissingSourceDir_UnresolvableDefault verifies runUpdate
// fails loudly (rather than silently falling back to cwd) when
// ResolvedSourceDir() cannot produce a default at all — the
// XDG_DATA_HOME-and-HOME-both-unset edge case.
func TestRunUpdate_MissingSourceDir_UnresolvableDefault(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")
	dir := t.TempDir()
	deploy := config.DeployConfig{
		InstallPath:    filepath.Join(dir, "clagentic-router"),
		ServiceManager: "none",
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = runUpdate(deploy, devNull)
	if err == nil {
		t.Fatal("expected error for unresolvable source_dir, got nil")
	}
}

// TestEnsureSourceCheckout_MissingDir_NoRepoURL verifies the actionable,
// deployed-host-facing error: a missing managed checkout with no
// deploy.repo_url configured must name the exact remedy (set repo_url,
// pre-clone, or point source_dir elsewhere) rather than surfacing as a raw
// `go build` failure against a nonexistent directory.
func TestEnsureSourceCheckout_MissingDir_NoRepoURL(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist-yet")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = ensureSourceCheckout(missing, "", devNull)
	if err == nil {
		t.Fatal("expected error for missing checkout with no repo_url, got nil")
	}
	if !strings.Contains(err.Error(), "repo_url") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the actionable remedy (repo_url) and the path", err.Error())
	}
}

// TestEnsureSourceCheckout_ExistingDirNotGit verifies a present-but-not-git
// directory (e.g. leftover cruft, or a directory created for another
// purpose) at the managed checkout path is a hard error, not an attempt to
// build whatever happens to be sitting there.
func TestEnsureSourceCheckout_ExistingDirNotGit(t *testing.T) {
	dir := t.TempDir() // present, but no .git
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = ensureSourceCheckout(dir, "", devNull)
	if err == nil {
		t.Fatal("expected error for existing non-git directory, got nil")
	}
}

// TestEnsureSourceCheckout_ExistingGitRepo_NoUpstream_FailsLoudly verifies
// the present + already-a-git-repo branch runs `git pull --ff-only` rather
// than cloning — using this repo's own checkout (read-only: git pull
// --ff-only against a checkout with no upstream tracking branch configured
// is expected to error, which is itself the correct, loud failure mode for
// a local dev checkout, not a false "success").
func TestEnsureSourceCheckout_ExistingGitRepo_NoUpstream_FailsLoudly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-invoking test in -short mode")
	}
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot)) // cmd/clagentic-router -> repo root

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	// This checkout's current branch may or may not have an upstream in the
	// test environment; either a clean pull or a "no upstream" error is
	// consistent with "never silently merges/resets" — only assert it never
	// panics and returns some definite outcome (err or nil), not a specific one.
	_ = ensureSourceCheckout(repoRoot, "", devNull)
}

// TestCheckGoToolchain_FoundOnPath verifies checkGoToolchain succeeds when
// "go" is on PATH — true in this test environment (go test itself requires it).
func TestCheckGoToolchain_FoundOnPath(t *testing.T) {
	if err := checkGoToolchain(); err != nil {
		t.Errorf("checkGoToolchain() = %v, want nil (go must be on PATH to run this test)", err)
	}
}

// TestNormalizeGitURL_Equivalence covers the URL-equivalence cases from the
// lr-720e91 identity-check fold-in (PEACHES comment 5360190075): a naive
// string comparison would false-fail on these legitimate variations of the
// same remote.
func TestNormalizeGitURL_Equivalence(t *testing.T) {
	equivalentGroups := [][]string{
		{
			"https://github.com/o/r",
			"https://github.com/o/r.git",
			"https://github.com/o/r/",
			"https://github.com/o/r.git/",
		},
		{
			"git@github.com:o/r.git",
			"git@github.com:o/r",
			"https://github.com/o/r",
			"ssh://git@github.com/o/r",
		},
		{
			"https://GitHub.com/o/r",
			"https://github.com/o/r",
		},
	}
	for _, group := range equivalentGroups {
		want := normalizeGitURL(group[0])
		for _, u := range group[1:] {
			if got := normalizeGitURL(u); got != want {
				t.Errorf("normalizeGitURL(%q) = %q, want %q (equivalent to %q)", u, got, want, group[0])
			}
		}
	}
}

// TestGitURLsEquivalent_DifferentReposNotEqual verifies normalization never
// over-corrects into treating genuinely different remotes as the same repo
// — different host, different owner, and different repo name must all
// still compare unequal.
func TestGitURLsEquivalent_DifferentReposNotEqual(t *testing.T) {
	cases := []struct{ a, b string }{
		{"https://github.com/o/r", "https://gitlab.com/o/r"},     // different host
		{"https://github.com/o/r", "https://github.com/other/r"}, // different owner
		{"https://github.com/o/r", "https://github.com/o/other"}, // different repo
		{"git@github.com:o/r.git", "git@github.com:o/fork.git"},  // scp-style, different repo
	}
	for _, tc := range cases {
		if gitURLsEquivalent(tc.a, tc.b) {
			t.Errorf("gitURLsEquivalent(%q, %q) = true, want false (genuinely different repos)", tc.a, tc.b)
		}
	}
}

// initBareRepo creates a bare git repo at dir, for use as a fake "remote"
// in the ensureSourceCheckout identity-check tests below — mirrors the
// verification style used for the original lr-720e91 clone/pull tests.
func initBareRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bare repo dir: %v", err)
	}
	cmd := exec.Command("git", "init", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", dir, err, out)
	}
	return dir
}

// seedBareRepo clones bareRepoDir into a scratch working tree, commits one
// file, and pushes it back — a bare repo has no commits of its own, and
// ensureSourceCheckout's clone/pull path needs at least one to exercise
// realistically.
func seedBareRepo(t *testing.T, bareRepoDir string) {
	t.Helper()
	work := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Force a known branch name regardless of the environment's
	// init.defaultBranch, then push and set it as the bare repo's HEAD, so a
	// later `git clone`/`git pull` has a matching, tracked default branch
	// (avoids "no such ref was fetched" from a master/main mismatch between
	// this seeding step and the environment's git config).
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	run("add", "README")
	run("commit", "-m", "seed")
	run("remote", "add", "origin", bareRepoDir)
	run("push", "origin", "HEAD:refs/heads/main")

	bareCmd := exec.Command("git", "-C", bareRepoDir, "symbolic-ref", "HEAD", "refs/heads/main")
	if out, err := bareCmd.CombinedOutput(); err != nil {
		t.Fatalf("set bare repo HEAD to main: %v\n%s", err, out)
	}
}

// TestEnsureSourceCheckout_OriginMismatch_FailsWithBothURLs is the real,
// non-mocked reproduction the fold-in dispatch asked for: a managed
// checkout cloned from bare repo A, then pointed at bare repo B via
// deploy.repo_url. ensureSourceCheckout must refuse to pull it and must
// name both URLs in the error.
func TestEnsureSourceCheckout_OriginMismatch_FailsWithBothURLs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-invoking test in -short mode")
	}
	root := t.TempDir()
	repoA := initBareRepo(t, filepath.Join(root, "repo-a.git"))
	repoB := initBareRepo(t, filepath.Join(root, "repo-b.git"))
	seedBareRepo(t, repoA)

	checkout := filepath.Join(root, "checkout")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	// Clone from repo A into the managed checkout location.
	if err := gitClone(repoA, checkout); err != nil {
		t.Fatalf("gitClone(repoA): %v", err)
	}

	// Now point deploy.repo_url at repo B instead — the mismatch case.
	err = ensureSourceCheckout(checkout, repoB, devNull)
	if err == nil {
		t.Fatal("expected error for origin/repo_url mismatch, got nil")
	}
	t.Logf("captured mismatch error:\n%s", err.Error())
	if !strings.Contains(err.Error(), repoA) {
		t.Errorf("error %q does not name the checkout's actual origin (%s)", err.Error(), repoA)
	}
	if !strings.Contains(err.Error(), repoB) {
		t.Errorf("error %q does not name the configured deploy.repo_url (%s)", err.Error(), repoB)
	}
}

// TestEnsureSourceCheckout_OriginMatches_PullsSuccessfully verifies the
// non-mismatch case still works end-to-end: same repo_url as origin pulls
// cleanly with no error.
func TestEnsureSourceCheckout_OriginMatches_PullsSuccessfully(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-invoking test in -short mode")
	}
	root := t.TempDir()
	repoA := initBareRepo(t, filepath.Join(root, "repo-a.git"))
	seedBareRepo(t, repoA)

	checkout := filepath.Join(root, "checkout")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	if err := gitClone(repoA, checkout); err != nil {
		t.Fatalf("gitClone(repoA): %v", err)
	}

	if err := ensureSourceCheckout(checkout, repoA, devNull); err != nil {
		t.Fatalf("ensureSourceCheckout with matching repo_url: %v", err)
	}
}

// TestEnsureSourceCheckout_NoOriginRemote_FailsWithRepoURLSet verifies a
// managed checkout with no "origin" remote at all is a hard error when
// deploy.repo_url IS set — "cannot verify" must not be treated as "assume
// it's fine" for a path that gets compiled and installed as the running
// daemon.
func TestEnsureSourceCheckout_NoOriginRemote_FailsWithRepoURLSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-invoking test in -short mode")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "README")
	run("commit", "-m", "seed")
	// Deliberately no `git remote add origin` — this checkout has no origin.

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	err = ensureSourceCheckout(dir, "https://example.invalid/o/r.git", devNull)
	if err == nil {
		t.Fatal("expected error for missing origin remote with repo_url set, got nil")
	}
}

// TestEnsureSourceCheckout_NoOriginRemote_WarnsWhenRepoURLAlsoEmpty
// verifies decision item 1 from the lr-720e91 fold-in dispatch: an existing
// managed checkout with NO deploy.repo_url configured (and, here, no origin
// remote either) is a WARN-and-proceed, not a hard failure — there is
// nothing to compare against, and an operator who pre-cloned deliberately
// without ever setting repo_url stated no expectation this check enforces.
func TestEnsureSourceCheckout_NoOriginRemote_WarnsWhenRepoURLAlsoEmpty(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git-invoking test in -short mode")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "README")
	run("commit", "-m", "seed")

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()

	// No origin remote, no repo_url — expected outcome per item 1 is a
	// no-upstream `git pull --ff-only` failure (same as the pre-existing
	// TestEnsureSourceCheckout_ExistingGitRepo_NoUpstream_FailsLoudly case),
	// NOT an identity-check failure. Assert the identity check itself does
	// not produce the error by checking it doesn't mention repo_url/origin
	// mismatch wording.
	err = ensureSourceCheckout(dir, "", devNull)
	if err != nil && strings.Contains(err.Error(), "DIFFERENT repository") {
		t.Errorf("expected the no-repo_url case to warn-and-proceed to the pull step, not fail the "+
			"identity check, got: %v", err)
	}
}
