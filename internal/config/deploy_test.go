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
	// ResolvedSourceDir() is NOT "." (lr-720e91): a deployed host's update
	// subcommand has no reason to have a source tree in its own cwd. Default
	// is the managed checkout path, derived from XDG_DATA_HOME/HOME — see
	// TestResolvedSourceDir_ManagedDefault for the exact value assertion.
	if got := d.ResolvedSourceDir(); got == "." {
		t.Errorf("ResolvedSourceDir() = %q, want anything other than \".\" (lr-720e91: cwd is never a safe default on a deployed host)", got)
	}
	if !d.SourceDirIsManaged() {
		t.Error("SourceDirIsManaged() = false with SourceDir unset, want true")
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

// TestResolvedSourceDir_ManagedDefault pins down the exact managed-checkout
// default path derived from XDG_DATA_HOME (and the HOME fallback), so a
// regression changing the resolution shape is caught precisely, not just
// "not dot".
func TestResolvedSourceDir_ManagedDefault(t *testing.T) {
	t.Run("XDG_DATA_HOME set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg/data")
		var d DeployConfig
		if got, want := d.ResolvedSourceDir(), "/xdg/data/clagentic-router/src"; got != want {
			t.Errorf("ResolvedSourceDir() = %q, want %q", got, want)
		}
	})
	t.Run("XDG_DATA_HOME unset, HOME fallback", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "/home/op")
		var d DeployConfig
		if got, want := d.ResolvedSourceDir(), "/home/op/.local/share/clagentic-router/src"; got != want {
			t.Errorf("ResolvedSourceDir() = %q, want %q", got, want)
		}
	})
	t.Run("neither set", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("HOME", "")
		var d DeployConfig
		if got := d.ResolvedSourceDir(); got != "" {
			t.Errorf("ResolvedSourceDir() = %q, want empty (unresolvable — caller must fail loudly, never fall back to cwd)", got)
		}
	})
}

// TestSourceDirIsManaged_ExplicitValue verifies an explicit SourceDir
// (including explicitly ".") is reported as NOT managed — update must never
// touch the git state of a directory the operator pointed at themselves.
func TestSourceDirIsManaged_ExplicitValue(t *testing.T) {
	d := DeployConfig{SourceDir: "."}
	if d.SourceDirIsManaged() {
		t.Error("SourceDirIsManaged() = true with explicit SourceDir \".\", want false")
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

// TestDeployConfig_ServiceManager_SystemdUser verifies "systemd-user"
// round-trips as-is through ResolvedServiceManager, and that "systemd" and
// "none" remain byte-identical to their pre-lr-574334 values — the
// compatibility guarantee A1 requires.
func TestDeployConfig_ServiceManager_SystemdUser(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", "systemd"},        // default, unchanged
		{"systemd", "systemd"}, // unchanged
		{"none", "none"},       // unchanged
		{"systemd-user", "systemd-user"},
	}
	for _, tc := range cases {
		d := DeployConfig{ServiceManager: tc.configured}
		if got := d.ResolvedServiceManager(); got != tc.want {
			t.Errorf("ResolvedServiceManager() with ServiceManager=%q = %q, want %q", tc.configured, got, tc.want)
		}
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

// TestLoad_DeployBlock_Unmarshal_SystemdUser verifies "systemd-user" round-trips
// through config.Load the same way "systemd" and "none" already do (lr-574334 A1).
func TestLoad_DeployBlock_Unmarshal_SystemdUser(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/router.yaml"
	raw := `
backends:
  local:
    adapter: ollama_http
    url: http://localhost:8080
    model: phi4-mini

deploy:
  install_path: /home/operator/.local/bin/clagentic-router
  service_manager: systemd-user
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Deploy.ResolvedServiceManager(), "systemd-user"; got != want {
		t.Errorf("Deploy.ResolvedServiceManager() = %q, want %q", got, want)
	}
}
