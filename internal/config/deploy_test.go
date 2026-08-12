// internal/config/deploy_test.go — tests for DeployConfig defaults, the
// optional [deploy] block backing the "clagentic-router update" subcommand.
package config

import (
	"os"
	"testing"
)

// TestDeployConfig_Defaults verifies every Resolved* accessor falls back to
// a stock systemd install when the [deploy] block is entirely absent — the
// "clean third-party install works unconfigured" acceptance criterion.
func TestDeployConfig_Defaults(t *testing.T) {
	var d DeployConfig
	if got, want := d.ResolvedSourceDir(), "."; got != want {
		t.Errorf("ResolvedSourceDir() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedInstallPath(), "/usr/local/bin/clagentic-router"; got != want {
		t.Errorf("ResolvedInstallPath() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedServiceName(), "clagentic-router"; got != want {
		t.Errorf("ResolvedServiceName() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedServiceManager(), "systemd"; got != want {
		t.Errorf("ResolvedServiceManager() = %q, want %q", got, want)
	}
}

// TestDeployConfig_Overrides verifies explicit values win over defaults.
func TestDeployConfig_Overrides(t *testing.T) {
	d := DeployConfig{
		SourceDir:      "/srv/clagentic-router",
		InstallPath:    "/opt/clagentic/bin/clagentic-router",
		ServiceName:    "clagentic-router-staging",
		ServiceManager: "none",
	}
	if got, want := d.ResolvedSourceDir(), "/srv/clagentic-router"; got != want {
		t.Errorf("ResolvedSourceDir() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedInstallPath(), "/opt/clagentic/bin/clagentic-router"; got != want {
		t.Errorf("ResolvedInstallPath() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedServiceName(), "clagentic-router-staging"; got != want {
		t.Errorf("ResolvedServiceName() = %q, want %q", got, want)
	}
	if got, want := d.ResolvedServiceManager(), "none"; got != want {
		t.Errorf("ResolvedServiceManager() = %q, want %q", got, want)
	}
}

// TestLoad_DeployBlock_Unmarshal verifies the deploy block round-trips
// through the same config.Load path "serve" uses — no second config surface.
func TestLoad_DeployBlock_Unmarshal(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/router.yaml"
	raw := `
backends:
  local:
    adapter: ollama_http
    url: http://localhost:8080
    model: phi4-mini

deploy:
  install_path: /custom/bin/clagentic-router
  service_name: clagentic-router-custom
  service_manager: none
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Deploy.ResolvedInstallPath(), "/custom/bin/clagentic-router"; got != want {
		t.Errorf("Deploy.ResolvedInstallPath() = %q, want %q", got, want)
	}
	if got, want := cfg.Deploy.ResolvedServiceName(), "clagentic-router-custom"; got != want {
		t.Errorf("Deploy.ResolvedServiceName() = %q, want %q", got, want)
	}
	if got, want := cfg.Deploy.ResolvedServiceManager(), "none"; got != want {
		t.Errorf("Deploy.ResolvedServiceManager() = %q, want %q", got, want)
	}
}
