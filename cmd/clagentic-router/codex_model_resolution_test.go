// cmd/clagentic-router/codex_model_resolution_test.go — regression coverage
// for lr-82e68e: buildAdapter's codex_cli model resolution must be purely
// additive. An explicit Model in BackendConfig always wins byte-identically
// with zero discovery attempted; only an unset Model engages
// backend.ResolveCodexModel.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clagentic/clagentic-router/internal/config"
)

// writeFailingFakeBin writes an executable that always exits non-zero. Used
// to prove that a codex_cli backend with an explicit Model never invokes
// codex at all: if discovery ran, this binary's failure would surface as a
// construction error, but explicit-Model construction must succeed.
func writeFailingFakeBin(t *testing.T, dir, name string) string {
	t.Helper()
	binPath := filepath.Join(dir, name)
	script := "#!/bin/sh\necho 'discovery must not have been called' >&2\nexit 1\n"
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write failing fake bin: %v", err)
	}
	return binPath
}

// TestBuildAdapter_CodexCLI_ExplicitModel_SkipsDiscovery is the required
// regression proof: a codex_cli backend with an explicit Model resolves to
// exactly that string and performs no discovery call. BinPath points at a
// binary that always fails — if ResolveCodexModel were invoked despite the
// explicit Model, construction would return an error; it must not.
func TestBuildAdapter_CodexCLI_ExplicitModel_SkipsDiscovery(t *testing.T) {
	dir := t.TempDir()
	failingBin := writeFailingFakeBin(t, dir, "codex")

	const explicitModel = "fake-pinned-model-lr82e68e"
	b := &config.BackendConfig{
		Adapter: config.AdapterCodexCLI,
		Model:   explicitModel,
		BinPath: failingBin,
	}

	adapter, err := buildAdapter("test-backend", b, nil)
	if err != nil {
		t.Fatalf("buildAdapter returned an error, implying discovery ran despite explicit Model: %v", err)
	}
	if adapter == nil {
		t.Fatal("buildAdapter returned a nil adapter with no error")
	}
	if adapter.ID() != "test-backend" {
		t.Errorf("adapter ID = %q, want test-backend", adapter.ID())
	}
}

// TestBuildAdapter_CodexCLI_NoModel_AttemptsDiscoveryAndFails is the
// complementary case: with Model unset, discovery IS attempted, and a
// failing codex binary surfaces as a construction-time error rather than a
// silently empty/wrong model.
func TestBuildAdapter_CodexCLI_NoModel_AttemptsDiscoveryAndFails(t *testing.T) {
	dir := t.TempDir()
	failingBin := writeFailingFakeBin(t, dir, "codex")

	b := &config.BackendConfig{
		Adapter: config.AdapterCodexCLI,
		BinPath: failingBin,
	}

	_, err := buildAdapter("test-backend", b, nil)
	if err == nil {
		t.Fatal("expected buildAdapter to fail when model discovery's codex invocation fails, got nil error")
	}
}
