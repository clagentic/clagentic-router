// cmd/clagentic-router/version_test.go — regression test for lr-92ee18 B1:
// version must be a linker-settable var, not a const. -ldflags -X can only
// overwrite a package-level string VARIABLE's initial value — it silently
// no-ops against a const, which is exactly what happened before this fix:
// the Makefile's -X main.version flag looked correct and the binary still
// reported the hardcoded default, with no main.version symbol present at
// all, making build staleness undetectable.
package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersion_LdflagsInjectionTakesEffect builds the actual binary with
// -ldflags -X main.version=<rev> (mirroring the Makefile's LDFLAGS) and
// asserts the built binary's "version" subcommand reports that exact
// revision — not the hardcoded default. This is the acceptance test named
// in lr-92ee18's task description ("build-at-R prints R and symbol
// exists"), reproduced as an automated regression rather than only a manual
// verification step.
func TestVersion_LdflagsInjectionTakesEffect(t *testing.T) {
	const wantRev = "lr92ee18-test-rev-abc123"

	binPath := filepath.Join(t.TempDir(), "clagentic-router-test-bin")
	buildCmd := exec.Command("go", "build",
		"-ldflags", "-X main.version="+wantRev,
		"-o", binPath,
		".",
	)
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build with -ldflags -X main.version=%s failed: %v\noutput: %s", wantRev, err, buildOut)
	}

	runCmd := exec.Command(binPath, "version")
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running built binary's version subcommand failed: %v\noutput: %s", err, out)
	}

	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, wantRev) {
		t.Errorf("version subcommand output = %q, want it to contain injected revision %q — "+
			"if this fails, 'version' has regressed back to a const (ldflags -X silently no-ops "+
			"against a const; see main.go's version var doc)", got, wantRev)
	}
}

// TestVersion_NoLdflags_ReportsSaneDefault verifies a build invoked without
// -X (e.g. a bare `go build`, no Makefile) still reports a sane, non-empty
// default rather than an empty string or a build failure — the other half
// of B1's acceptance criterion.
func TestVersion_NoLdflags_ReportsSaneDefault(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "clagentic-router-test-bin-nold")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build (no ldflags) failed: %v\noutput: %s", err, buildOut)
	}

	runCmd := exec.Command(binPath, "version")
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running built binary's version subcommand failed: %v\noutput: %s", err, out)
	}

	got := strings.TrimSpace(string(out))
	if got == "" || !strings.Contains(got, version) {
		t.Errorf("version subcommand output = %q, want it to contain the package default %q", got, version)
	}
}
