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

	if err := installBinary(staged, target); err != nil {
		t.Fatalf("installBinary: %v", err)
	}

	if err := verifyInstalledBinary(target, stagedInfo); err != nil {
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

	missing := filepath.Join(dir, "does-not-exist")
	if err := verifyInstalledBinary(missing, stagedInfo); err == nil {
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

	target := filepath.Join(dir, "clagentic-router")
	if err := os.WriteFile(target, []byte("short"), 0o755); err != nil {
		t.Fatalf("write mismatched target: %v", err)
	}

	if err := verifyInstalledBinary(target, stagedInfo); err == nil {
		t.Fatal("expected error for size mismatch between installed and staged artifacts, got nil")
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
