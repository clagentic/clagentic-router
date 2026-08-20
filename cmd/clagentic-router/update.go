// cmd/clagentic-router/update.go — "update" subcommand: maintain a source
// checkout, rebuild the router binary from it, and restart the running
// service in place.
//
// This is the target of NAOMI's post_merge_steps for this repo. The
// committed .crew/naomi.yaml step passes --source-dir . explicitly (see
// that file's own comment) so it keeps building from the merged tree
// (loadout-merge's cwd for every post-merge step) rather than the managed
// checkout — an explicit source_dir/--source-dir is honored byte-identically
// and update never touches that directory's git state, exactly as before
// lr-720e91. Every other host-specific detail (install path, systemd unit
// name) is resolved here from the SAME config chain the "serve" subcommand
// already uses (defaultConfigPath + config.Load), under the optional
// [deploy] block. No second config surface, no new gitignored file.
//
// DEPLOYED-HOST CASE (lr-720e91): on a host with only the installed binary
// and no source tree in cwd, deploy.source_dir is left unset and resolves
// to a managed checkout (config.DefaultManagedSourceDir) that this file
// owns: clone it (deploy.repo_url required) if it does not exist yet, else
// `git pull --ff-only` it before building. Non-fast-forwardable state and a
// missing Go toolchain both fail loudly with an actionable message instead
// of a raw `go build`/`git` error from an unexpected directory.
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
	sourceDir  string // "" = not overridden on the CLI; falls through to config
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

	deploy := cfg.Deploy
	if uf.sourceDir != "" {
		// --source-dir wins over config, mirroring --config's own precedence.
		// This is what lets a post-merge automation step stay a bare,
		// environment-agnostic verb plus one explicit flag rather than
		// requiring a repo-committed router.yaml with a deploy.source_dir.
		deploy.SourceDir = uf.sourceDir
	}

	return runUpdate(deploy, os.Stdout)
}

func parseUpdateFlags(args []string) (updateFlags, error) {
	configPath := defaultConfigPath()
	sourceDir := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return updateFlags{}, fmt.Errorf("--config requires a value")
			}
			i++
			configPath = args[i]
		case "--source-dir":
			if i+1 >= len(args) {
				return updateFlags{}, fmt.Errorf("--source-dir requires a value")
			}
			i++
			sourceDir = args[i]
		default:
			return updateFlags{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return updateFlags{configPath: configPath, sourceDir: sourceDir}, nil
}

// runUpdate ensures deploy.SourceDir holds the desired revision (managing
// its git state itself when it is the default managed checkout — see
// ensureSourceCheckout), rebuilds the binary from it, atomically installs
// the result at deploy.InstallPath, and restarts the configured service.
// Progress is written to out (stdout in normal operation, a buffer in
// tests).
func runUpdate(deploy config.DeployConfig, out *os.File) error {
	sourceDir := deploy.ResolvedSourceDir()
	installPath := deploy.ResolvedInstallPath()
	serviceManager := deploy.ResolvedServiceManager()

	if sourceDir == "" {
		return fmt.Errorf("deploy.source_dir could not be resolved: neither an explicit " +
			"deploy.source_dir/--source-dir nor $XDG_DATA_HOME nor $HOME is set, so there is no " +
			"safe default checkout location. Set deploy.source_dir explicitly (e.g. to an existing " +
			"checkout, or to \".\" to build from update's own working directory as before)")
	}

	if err := validateInstallPath(installPath); err != nil {
		return err
	}

	if err := checkGoToolchain(); err != nil {
		return err
	}

	if deploy.SourceDirIsManaged() {
		if err := ensureSourceCheckout(sourceDir, deploy.RepoURL, out); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "update: deploy.source_dir is explicitly set (%s) — building as-is, not managing its git state\n", sourceDir)
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

// checkGoToolchain reports an actionable error when the "go" binary is not
// on PATH, instead of letting buildBinary's exec.Command surface a bare
// "executable file not found in $PATH" that does not name which tool is
// missing or why update needs it. A deployed host running only the compiled
// binary has no reason to have a Go toolchain installed at all.
func checkGoToolchain() error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("update requires the Go toolchain to rebuild the binary, but \"go\" was " +
			"not found on PATH. Install Go (https://go.dev/doc/install) on this host, or use a host " +
			"that already has it (e.g. run update from the build/CI machine instead)")
	}
	return nil
}

// ensureSourceCheckout makes sourceDir hold a git checkout reflecting the
// latest available revision: clones it from repoURL when the directory does
// not exist yet, else runs `git pull --ff-only` in place. Only called for
// the DEFAULT managed checkout path (deploy.SourceDirIsManaged()) — an
// operator-supplied explicit source_dir is never touched here (see
// runUpdate). Non-fast-forwardable local state is a hard error, never a
// silent merge or reset, matching clagentic-lite's `update` convention
// (git pull --ff-only; divergence fails loudly rather than discarding
// history).
func ensureSourceCheckout(sourceDir, repoURL string, out *os.File) error {
	info, statErr := os.Stat(sourceDir)
	switch {
	case statErr == nil && info.IsDir():
		if _, err := os.Stat(filepath.Join(sourceDir, ".git")); err != nil {
			return fmt.Errorf("deploy.source_dir %q exists but is not a git checkout (no .git). "+
				"Remove it and let update clone into it (deploy.repo_url required), or point "+
				"deploy.source_dir/--source-dir at an existing checkout", sourceDir)
		}
		fmt.Fprintf(out, "update: pulling %s (git pull --ff-only)\n", sourceDir)
		if err := gitPullFFOnly(sourceDir); err != nil {
			return err
		}
		return nil

	case statErr == nil:
		return fmt.Errorf("deploy.source_dir %q exists but is not a directory", sourceDir)

	case os.IsNotExist(statErr):
		if repoURL == "" {
			return fmt.Errorf("deploy.source_dir %q does not exist and deploy.repo_url is not set. "+
				"Either set deploy.repo_url to the router's git remote so update can clone it there, "+
				"or pre-create the checkout yourself at that path (git clone <remote> %s), or set "+
				"deploy.source_dir/--source-dir to an existing checkout", sourceDir, sourceDir)
		}
		fmt.Fprintf(out, "update: cloning %s -> %s\n", repoURL, sourceDir)
		return gitClone(repoURL, sourceDir)

	default:
		return fmt.Errorf("stat deploy.source_dir %q: %w", sourceDir, statErr)
	}
}

// gitPullFFOnly runs `git pull --ff-only` in dir. A non-fast-forwardable
// checkout (local commits, diverged history) fails loudly here rather than
// merging or resetting — matching clagentic-lite's update convention.
func gitPullFFOnly(dir string) error {
	cmd := exec.Command("git", "-C", dir, "pull", "--ff-only")
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull --ff-only in %s failed (non-fast-forward divergence, local "+
			"changes, or no upstream configured) — resolve manually in that checkout, update never "+
			"merges or resets: %w\n%s", dir, err, output)
	}
	return nil
}

// gitClone clones repoURL into dir, creating dir's parent if needed.
func gitClone(repoURL, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create parent of deploy.source_dir %q: %w", dir, err)
	}
	cmd := exec.Command("git", "clone", repoURL, dir)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %s %s failed: %w\n%s", repoURL, dir, err, output)
	}
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
