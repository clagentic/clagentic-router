// internal/config/removed_keys_test.go — verifies a removed top-level config
// key (e.g. trusted_working_dirs, removed once the workspace-trust dialog
// it gated was found to never fire in this daemon's non-interactive call
// path) never becomes a hard startup error for an operator upgrading with
// that key still present in their router.yaml. This is the safe default:
// Load must succeed, not refuse to start over an ignorable key.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_RemovedKeyDoesNotFailStartup verifies that a router.yaml
// containing the removed trusted_working_dirs key still loads successfully
// — the key is ignored (with a Warn log, not asserted here) rather than
// causing Load to return an error.
func TestLoad_RemovedKeyDoesNotFailStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.yaml")
	yamlContent := `
backends:
  test-backend:
    adapter: ollama_http
    url: http://localhost:11434

trusted_working_dirs:
  - /home/router/projects/some-repo
`
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write router.yaml: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error for a removed-but-present key; removal must be non-fatal on upgrade: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load returned a nil config with no error")
	}
}

// TestLoad_NoRemovedKeysPresent verifies the normal case — a router.yaml
// with no removed keys loads cleanly, same as before this test file existed.
func TestLoad_NoRemovedKeysPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.yaml")
	yamlContent := `
backends:
  test-backend:
    adapter: ollama_http
    url: http://localhost:11434
`
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write router.yaml: %v", err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
}
