// cmd/clagentic-router/no_subprocess_home_warning_test.go — regression test
// for lr-92ee18 B4: version/update/doctor/health must never create the
// claude_cli subprocess-home directory or warn about it — that directory is
// prepared lazily, on the first real claude_cli/codex_subagent Invoke, not
// at package init. Verified via a real subprocess run of the built binary
// (not an in-process call) because the underlying resolveClaudeSubprocessHome
// is guarded by a package-level sync.Once — the only way to observe "does
// this subcommand touch it AT ALL" for a truly fresh process is a fresh
// process.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionSubcommand_NoSubprocessHomeDirectoryCreated builds the real
// binary and runs its "version" subcommand (no server, no claude_cli
// Invoke) against an isolated CLAGENTIC_ROUTER_STATE_DIR, then asserts the
// claude-home subdirectory was never created and stderr carries no
// subprocess-home permissions warning — the B4 acceptance criterion:
// "version/update/doctor/health emit no subprocess-home warning and create
// no directories."
func TestVersionSubcommand_NoSubprocessHomeDirectoryCreated(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "clagentic-router-test-bin")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\noutput: %s", err, out)
	}

	stateDir := t.TempDir()
	runCmd := exec.Command(binPath, "version")
	runCmd.Env = append(os.Environ(), "CLAGENTIC_ROUTER_STATE_DIR="+stateDir)
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running version subcommand failed: %v\noutput: %s", err, out)
	}

	claudeHomeDir := filepath.Join(stateDir, "claude-home")
	if _, statErr := os.Stat(claudeHomeDir); statErr == nil {
		t.Errorf("version subcommand created %s — subprocess home must be resolved lazily, "+
			"only on a real claude_cli/codex_subagent Invoke", claudeHomeDir)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stating %s: %v", claudeHomeDir, statErr)
	}

	if strings.Contains(string(out), "subprocess home") {
		t.Errorf("version subcommand output unexpectedly mentions subprocess home:\n%s", out)
	}
}

// TestHelpSubcommand_NoSubprocessHomeDirectoryCreated is the same assertion
// for the "help" subcommand — another non-serve path that never invokes a
// claude_cli backend.
func TestHelpSubcommand_NoSubprocessHomeDirectoryCreated(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "clagentic-router-test-bin-help")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\noutput: %s", err, out)
	}

	stateDir := t.TempDir()
	runCmd := exec.Command(binPath, "help")
	runCmd.Env = append(os.Environ(), "CLAGENTIC_ROUTER_STATE_DIR="+stateDir)
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running help subcommand failed: %v\noutput: %s", err, out)
	}

	claudeHomeDir := filepath.Join(stateDir, "claude-home")
	if _, statErr := os.Stat(claudeHomeDir); statErr == nil {
		t.Errorf("help subcommand created %s — subprocess home must be resolved lazily", claudeHomeDir)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stating %s: %v", claudeHomeDir, statErr)
	}
}
