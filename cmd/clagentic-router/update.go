// cmd/clagentic-router/update.go — "update" subcommand: rebuild the router
// binary from source and restart the running service in place.
//
// This is the target of NAOMI's post_merge_steps for this repo. The
// committed .crew/naomi.yaml step is a bare, environment-agnostic verb
// ("clagentic-router update") — every host-specific detail (install path,
// systemd unit name, source checkout location) is resolved here from the
// SAME config chain the "serve" subcommand already uses
// (defaultConfigPath + config.Load), under the optional [deploy] block.
// No second config surface, no new gitignored file.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/clagentic/clagentic-router/internal/config"
)

// updateFlags holds parsed flags for the update subcommand.
type updateFlags struct {
	configPath string
}

func cmdUpdate(args []string) error {
	uf, err := parseUpdateFlags(args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(uf.configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", uf.configPath, err)
	}

	return runUpdate(cfg.Deploy, os.Stdout)
}

func parseUpdateFlags(args []string) (updateFlags, error) {
	configPath := defaultConfigPath()
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return updateFlags{}, fmt.Errorf("--config requires a value")
			}
			i++
			configPath = args[i]
		default:
			return updateFlags{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return updateFlags{configPath: configPath}, nil
}

// runUpdate rebuilds the binary from deploy.SourceDir, atomically installs
// it at deploy.InstallPath, and restarts the configured service. Progress
// is written to out (stdout in normal operation, a buffer in tests).
func runUpdate(deploy config.DeployConfig, out *os.File) error {
	sourceDir := deploy.ResolvedSourceDir()
	installPath := deploy.ResolvedInstallPath()
	serviceManager := deploy.ResolvedServiceManager()

	if err := validateInstallPath(installPath); err != nil {
		return err
	}

	stagedPath := installPath + ".new"

	fmt.Fprintf(out, "update: building from %s\n", sourceDir)
	if err := buildBinary(sourceDir, stagedPath); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	fmt.Fprintf(out, "update: installing %s -> %s (atomic rename)\n", stagedPath, installPath)
	if err := installBinary(stagedPath, installPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}

	switch serviceManager {
	case "systemd":
		serviceName := deploy.ResolvedServiceName()
		fmt.Fprintf(out, "update: restarting systemd unit %s.service\n", serviceName)
		if err := restartSystemdService(serviceName); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
	case "none":
		fmt.Fprintln(out, "update: service_manager=none, skipping restart")
	default:
		return fmt.Errorf("deploy.service_manager: unknown value %q (want \"systemd\" or \"none\")", serviceManager)
	}

	fmt.Fprintln(out, "update: done")
	return nil
}

// validateInstallPath rejects a relative install path. The binary replaces
// a path a running systemd unit's ExecStart references, so it must be
// unambiguous regardless of the update subcommand's own working directory.
func validateInstallPath(installPath string) error {
	if !filepath.IsAbs(installPath) {
		return fmt.Errorf("deploy.install_path must be an absolute path, got %q", installPath)
	}
	return nil
}

// buildBinary compiles the router binary from sourceDir, emitting it
// directly at outputPath. Using `go build -o` (not `go install`) guarantees
// a fresh artifact lands at exactly the staged path — the failure mode that
// crash-looped a sibling service (stale orphan binary from an -o-less
// `go build ./...`) is structurally not possible here.
func buildBinary(sourceDir, outputPath string) error {
	cmd := exec.Command("go", "build", "-o", outputPath, "./cmd/clagentic-router")
	cmd.Dir = sourceDir
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build failed: %w\n%s", err, output)
	}
	return nil
}

// installBinary atomically replaces installPath with stagedPath via
// os.Rename. A plain copy over a running systemd-held binary fails with
// "text file busy"; rename on the same filesystem is atomic and succeeds
// even while the old inode is held open by the running process — the
// service keeps serving the old inode until it is restarted.
func installBinary(stagedPath, installPath string) error {
	if err := os.Chmod(stagedPath, 0o755); err != nil {
		return fmt.Errorf("chmod staged binary: %w", err)
	}
	if err := os.Rename(stagedPath, installPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w (stage and target must be on the same filesystem)", stagedPath, installPath, err)
	}
	return nil
}

// restartSystemdService restarts the named systemd unit (without the
// .service suffix, which is appended here).
func restartSystemdService(serviceName string) error {
	cmd := exec.Command("systemctl", "restart", serviceName+".service")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s.service failed: %w\n%s", serviceName, err, output)
	}
	return nil
}
