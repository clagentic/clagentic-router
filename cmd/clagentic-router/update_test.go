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
	"os"
	"path/filepath"
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

// TestDeployConfig_UnknownServiceManager_Value verifies an unrecognized
// deploy.service_manager string round-trips as-is (validated at dispatch
// time in runUpdate, not silently coerced to a known value here).
func TestDeployConfig_UnknownServiceManager_Value(t *testing.T) {
	d := config.DeployConfig{ServiceManager: "launchd"}
	if got, want := d.ResolvedServiceManager(), "launchd"; got != want {
		t.Errorf("ResolvedServiceManager() = %q, want %q", got, want)
	}
}
