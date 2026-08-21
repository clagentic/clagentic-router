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
//
// READBACK (lr-c69197): a PASS from this command used to mean only "every
// exec.Command exited 0" — installBinary's os.Rename and
// restartSystemdService's `systemctl restart` were both taken at their exit
// code alone, with nothing re-reading the result. runUpdate now verifies
// each mutating step after it runs: verifyInstalledBinary re-stats
// install_path and compares it against the freshly built artifact;
// restartAndVerifySystemdService compares the unit's ActiveEnterTimestamp
// and MainPID before and after the restart call, so a restart that did not
// actually restart the unit is a hard error, not a silent pass. The final
// report line names the hostname, the resolved install_path, and the
// resolved unit+scope actually acted on — see runUpdate's own doc for why
// this makes a PASS falsifiable rather than a pre-action echo of intent.
package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

	// hostname is resolved once, up front, purely for the report line —
	// naming the machine an action actually landed on is the core fix this
	// change makes (lr-c69197 MILLER item: "PASS naming neither host nor
	// verified outcome is unfalsifiable by construction"). A resolution
	// failure (rare — a container with no /etc/hostname) must not block the
	// update itself; it degrades to a literal "(unknown)" in the report
	// rather than failing an otherwise-successful deploy over a cosmetic
	// field.
	hostname, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		hostname = "(unknown)"
	}

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

	stagedInfo, err := os.Stat(stagedPath)
	if err != nil {
		return fmt.Errorf("install: stat freshly built binary %s: %w", stagedPath, err)
	}

	if err := installAndVerifyWithRollback(stagedPath, installPath, stagedInfo, hostname, out); err != nil {
		return err
	}

	switch serviceManager {
	case "systemd":
		serviceName := deploy.ResolvedServiceName()
		fmt.Fprintf(out, "update: restarting systemd unit %s.service (system scope) on %s\n", serviceName, hostname)
		if err := restartAndVerifySystemdService(serviceName, systemdScopeSystem, out); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
	case "systemd-user":
		serviceName := deploy.ResolvedServiceName()
		fmt.Fprintf(out, "update: restarting systemd unit %s.service (user scope) on %s\n", serviceName, hostname)
		if err := restartAndVerifySystemdService(serviceName, systemdScopeUser, out); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
	case "none":
		fmt.Fprintln(out, "update: service_manager=none, skipping restart")
	default:
		return fmt.Errorf("deploy.service_manager: unknown value %q (want \"systemd\", \"systemd-user\", or \"none\")", serviceManager)
	}

	// Falsifiable summary line (lr-c69197): names the host, the resolved
	// install path, and the resolved unit+scope actually acted on — the
	// three facts MILLER's diagnosis found the pre-existing "installed X,
	// restarted Y" pre-action echo never carried. Emitted only after every
	// readback above has already returned nil, so its mere presence in a
	// captured log is itself evidence of a verified outcome, not a
	// pre-action intent statement.
	unitDesc := "none (service_manager=none)"
	if serviceManager == "systemd" || serviceManager == "systemd-user" {
		scopeDesc := "system"
		if serviceManager == "systemd-user" {
			scopeDesc = "user"
		}
		unitDesc = fmt.Sprintf("%s.service (%s scope)", deploy.ResolvedServiceName(), scopeDesc)
	}
	fmt.Fprintf(out, "update: done — host=%s install_path=%s unit=%s\n", hostname, installPath, unitDesc)
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
//
// IDENTITY CHECK (lr-720e91 fold-in, PEACHES comment 5360190075 / BOBBIE
// comment 5360192680): a present managed checkout used to be pulled purely
// on the strength of having a ".git" directory — proof it is *a* repo, not
// proof it is *the* repo this deploy is supposed to run. A pre-seeded,
// symlinked, or accidentally-repointed checkout at the managed path would
// be pulled, built, and installed as the running (often root-run) systemd
// daemon with no check at all. verifyOriginMatchesRepoURL below reads the
// checkout's own `origin` remote and compares it against repoURL — on both
// the present-and-pull branch AND (new) is consulted here on every call,
// closing the gap BOBBIE also flagged: repoURL used to be read only on the
// missing-directory clone branch and never consulted again for the life of
// the checkout.
//
// RESIDUAL (named, not fixed here — out of scope for this fold-in per
// dispatch instructions): this check verifies which remote the checkout
// *claims* to track, not who was able to create or write the managed path
// in the first place. An unprivileged local user able to pre-create or
// symlink the managed checkout directory before the daemon's first `update`
// run could still seed a checkout whose origin happens to equal
// deploy.repo_url (a fork with a rewritten origin, or a MITM'd clone), and
// this check would pass it. Closing that requires filesystem-permission
// hardening of the managed path itself (e.g. requiring it live under a
// directory only the service user/root can create), which is a distinct,
// larger change — not folded in here. TODO(lr-720e91): evaluate locking
// down DefaultManagedSourceDir's parent directory ownership/permissions as
// a follow-up.
func ensureSourceCheckout(sourceDir, repoURL string, out *os.File) error {
	info, statErr := os.Stat(sourceDir)
	switch {
	case statErr == nil && info.IsDir():
		if _, err := os.Stat(filepath.Join(sourceDir, ".git")); err != nil {
			return fmt.Errorf("deploy.source_dir %q exists but is not a git checkout (no .git). "+
				"Remove it and let update clone into it (deploy.repo_url required), or point "+
				"deploy.source_dir/--source-dir at an existing checkout", sourceDir)
		}
		if err := verifyOriginMatchesRepoURL(sourceDir, repoURL, out); err != nil {
			return err
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

// verifyOriginMatchesRepoURL is the identity check documented on
// ensureSourceCheckout above. It reads the checkout's own `origin` remote
// and compares it against repoURL (normalized — see normalizeGitURL),
// refusing to pull a managed checkout that does not track the configured
// repo. Three explicit decisions, all made here rather than left implicit:
//
//  1. repoURL == "" (no deploy.repo_url configured) with an existing managed
//     checkout: WARN, do not fail. There is nothing to compare against — an
//     operator who pre-cloned the checkout by hand without ever setting
//     repo_url stated no expectation for this check to enforce, and failing
//     would regress that (pre-existing, still-supported) setup. The warning
//     names the gap explicitly rather than silently saying nothing, so an
//     operator who *should* have set repo_url notices.
//  2. Multiple remotes: only `origin` is consulted — by design. `origin` is
//     the sole remote this checkout's own clone path (gitClone) ever
//     configures, so it is the only one with a defined meaning here; other
//     remotes an operator may have added by hand for their own purposes are
//     not this check's business.
//  3. No `origin` remote configured at all: hard error. A managed checkout
//     with no origin cannot be identity-checked, and "cannot verify" is not
//     "assume it's fine" for a path that gets compiled and installed as the
//     running daemon.
func verifyOriginMatchesRepoURL(sourceDir, repoURL string, out *os.File) error {
	origin, err := gitOriginURL(sourceDir)
	if err != nil {
		if repoURL == "" {
			fmt.Fprintf(out, "update: warning: deploy.source_dir %q has no \"origin\" remote and "+
				"deploy.repo_url is unset, so its identity cannot be verified before building from "+
				"it (proceeding — set deploy.repo_url to enable this check)\n", sourceDir)
			return nil
		}
		return fmt.Errorf("deploy.source_dir %q has no \"origin\" remote, so it cannot be verified "+
			"against deploy.repo_url %q before building from it. Add an origin remote pointing at "+
			"deploy.repo_url (git -C %s remote add origin %s), or point deploy.source_dir/--source-dir "+
			"at a different checkout: %w", sourceDir, repoURL, sourceDir, repoURL, err)
	}

	if repoURL == "" {
		fmt.Fprintf(out, "update: warning: deploy.repo_url is unset, so deploy.source_dir %q's origin "+
			"(%s) cannot be verified against an expected value before building from it (proceeding — "+
			"set deploy.repo_url to enable this check)\n", sourceDir, origin)
		return nil
	}

	if !gitURLsEquivalent(origin, repoURL) {
		return fmt.Errorf("deploy.source_dir %q is a checkout of a DIFFERENT repository than "+
			"deploy.repo_url configures: checkout origin is %q, deploy.repo_url is %q. Refusing to "+
			"pull and build from it — this directory would be compiled and installed as the running "+
			"service. Remove %q and let update re-clone it from deploy.repo_url, or point "+
			"deploy.source_dir/--source-dir at the correct checkout, or correct deploy.repo_url if it "+
			"is the one that is wrong", sourceDir, origin, repoURL, sourceDir)
	}
	return nil
}

// gitOriginURL returns the URL configured for the "origin" remote in dir.
func gitOriginURL(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// gitURLsEquivalent reports whether a and b identify the same git remote,
// tolerating the equivalent forms of the same URL that are NOT the same
// string: a ".git" suffix, a trailing slash, and scp-style SSH syntax
// (git@host:owner/repo) vs. an explicit scheme (https://host/owner/repo,
// ssh://git@host/owner/repo). It does not attempt any equivalence beyond
// that — two URLs with different hosts or different paths are always
// treated as different repos, deliberately, so this never masks a genuine
// mismatch by over-normalizing.
func gitURLsEquivalent(a, b string) bool {
	return normalizeGitURL(a) == normalizeGitURL(b)
}

// normalizeGitURL reduces a git remote URL to a scheme/host/path-lowercased
// comparison key: strips a trailing ".git", strips a trailing "/", and
// rewrites scp-style SSH ("git@host:owner/repo", "user@host:path") to the
// same "host/path" shape an explicit-scheme URL normalizes to, so
// "https://github.com/o/r", "https://github.com/o/r.git",
// "git@github.com:o/r.git", and "https://github.com/o/r/" all compare
// equal. Host is lowercased (DNS is case-insensitive); path case is left
// as-is (many git hosts, including self-hosted Forgejo, are
// case-sensitive on the path).
func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		// Explicit-scheme form (https://, ssh://, git://, ...): scheme itself
		// is not part of the identity key — https vs. ssh vs. git transport
		// to the same host/path is still the same repository.
		return strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/")
	}

	// scp-style SSH: [user@]host:path — url.Parse does not recognize this
	// (no scheme), so rewrite it by hand into the same "host/path" shape.
	if at := strings.Index(s, "@"); at != -1 {
		rest := s[at+1:]
		if colon := strings.Index(rest, ":"); colon != -1 {
			host := rest[:colon]
			path := rest[colon+1:]
			return strings.ToLower(host) + "/" + strings.TrimSuffix(path, "/")
		}
	}

	// Unrecognized shape (e.g. a bare local filesystem path) — fall back to
	// the trimmed string itself rather than guessing further.
	return s
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

// backupInstalledBinary preserves the current contents of installPath at
// installPath+".bak" (same directory, so the later restore is a same-
// filesystem rename — atomic, no partial-write window) before it is
// replaced. This is the rollback fold-in (lr-c69197, PEACHES comment
// 5373517397 / BOBBIE comment 5373549900): both the update.user.service unit
// comment and OPERATOR-GUIDE.md previously claimed a failed update leaves
// the previous binary "untouched", which was false — verifyInstalledBinary
// runs AFTER installBinary's os.Rename has already replaced installPath.
// This closes that gap for real rather than only correcting the claim.
//
// Returns ("", nil) ONLY for genuine first-ever install: neither
// installPath nor installPath+".bak" exists — there is nothing to back up
// and nothing to roll back to, and that is not an error; a caller checks
// for the empty return to know rollback-on-failure has nothing to restore.
//
// STALE .bak HANDLING (lr-c69197 second fold-in, PEACHES nit 1 / BOBBIE;
// crash-window fix, lr-c69197 fourth fold-in, PEACHES comment 5373781420):
// installPath+".bak" can already exist on entry — a prior update run was
// interrupted (killed, host rebooted, OOM) between this rename and the
// later cleanup/restore rename that would have consumed it. There are three
// distinct states to disambiguate, not two, and each wants different
// behavior:
//
//  1. Neither installPath nor .bak present: genuine first-ever install.
//     Nothing to back up, nothing to roll back to. Returns ("", nil).
//  2. installPath present, .bak present: a stale backup from an interrupted
//     run that crashed AFTER a working install had already completed and
//     been backed up again (or, more commonly, an operator/process placed a
//     .bak there by hand). Two wrong answers were considered and rejected:
//       - Silently overwrite it: the stale .bak may be the ONLY good binary
//         on the box (the interrupted run may have failed AFTER this backup
//         but BEFORE a working install ever completed) — clobbering it
//         destroys the one known-good artifact left to roll back to.
//       - Fail forever until an operator intervenes: a single interrupted
//         run would then permanently wedge every future update, which is
//         worse than the problem this rollback mechanism exists to solve.
//     The chosen behavior: refuse to proceed with THIS backup, but do not
//     destroy either file — hard error naming both installPath and the
//     stale backupPath, so the update fails loudly (same as any other
//     pre-install error: install_path itself is untouched) and an operator
//     resolves the ambiguity by hand.
//  3. installPath ABSENT, .bak present: the narrow crash window documented
//     in OPERATOR-GUIDE.md — a prior run's backup rename (installPath ->
//     .bak) completed but the crash landed before the replacing rename
//     (staged -> installPath) ever put a new binary at installPath. Unlike
//     case 2, there is no ambiguity about which file is "the current one"
//     to protect: installPath is empty, so .bak is unambiguously the only
//     candidate good binary on the box, and refusing here (treating this as
//     "first-ever install", the bug this fix closes) would (a) make a
//     SUBSEQUENT install failure claim "no previous binary existed to roll
//     back to" despite .bak sitting right there, restorable, and (b) on
//     install SUCCESS leave the stale .bak behind to wedge the very next
//     update against case 2's refusal — a single crash permanently wedging
//     every future update, exactly the outcome the case-2 guard exists to
//     prevent. The chosen behavior: treat .bak as the existing backup
//     in-place — return its path directly (no rename needed; installPath is
//     already empty, there is nothing to move aside) so the caller's
//     rollback-on-failure path can restore it, and a successful install
//     naturally consumes/removes it via the same cleanup as any other run.
//     This self-recovers the crash window without an operator having to
//     intervene, while never fabricating a rollback target that isn't
//     genuinely there.
func backupInstalledBinary(installPath string) (string, error) {
	backupPath := installPath + ".bak"
	_, backupStatErr := os.Stat(backupPath)
	if backupStatErr != nil && !os.IsNotExist(backupStatErr) {
		return "", fmt.Errorf("stat existing backup at %s: %w", backupPath, backupStatErr)
	}
	backupExists := backupStatErr == nil

	if _, err := os.Stat(installPath); err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat existing binary at %s: %w", installPath, err)
		}
		// installPath is absent. Case 3 (backupExists) vs. case 1 (it doesn't).
		if backupExists {
			return backupPath, nil
		}
		return "", nil
	}

	// installPath exists — case 2 if .bak also exists.
	if backupExists {
		return "", fmt.Errorf("refusing to back up %s: a stale backup already exists at %s from a "+
			"previous interrupted update — it may be the only known-good binary left to roll back to, "+
			"so it is never silently overwritten. Inspect %s by hand: if it is safe to discard, remove "+
			"it and re-run update; if it looks like the good binary and %s does not, restore it "+
			"manually (mv %s %s)", installPath, backupPath, backupPath, installPath, backupPath, installPath)
	}
	if err := os.Rename(installPath, backupPath); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w (backup and target must be on the same filesystem)", installPath, backupPath, err)
	}
	return backupPath, nil
}

// restoreBackupOrReport is the ONE restore path used by every post-backup
// failure mode in installAndVerifyWithRollback (lr-c69197 second fold-in,
// PEACHES nit 1): both an installBinary failure (the rename that would
// replace installPath never completed, or completed only partially) and a
// verifyInstalledBinary failure (the rename completed, but the result is
// wrong) need the exact same recovery — rename backupPath back onto
// installPath — and previously only the verification-failure branch did
// this, leaving installBinary's own failure branch to return the raw error
// with installPath potentially stranded (old binary at .bak, nothing or a
// half-written file at installPath). Routing both callers through this one
// function means there is a single place that can drift, not two.
//
// backupPath == "" (first-ever install, nothing to roll back to) always
// returns originalErr unchanged — see backupInstalledBinary's own doc. A
// restore failure is reported ALONGSIDE originalErr, never in place of it:
// an operator needs to know both that the step failed AND whether the
// restore itself worked.
func restoreBackupOrReport(originalErr error, backupPath, installPath string) error {
	if backupPath == "" {
		return fmt.Errorf("%w (no previous binary existed to roll back to — this was a first-ever "+
			"install at %s)", originalErr, installPath)
	}
	if restoreErr := os.Rename(backupPath, installPath); restoreErr != nil {
		return fmt.Errorf("%w; additionally, restoring the previous binary from %s FAILED: %v — %s "+
			"may now be missing or in an inconsistent state, check it by hand",
			originalErr, backupPath, restoreErr, installPath)
	}
	return fmt.Errorf("%w (previous binary restored from %s — the running service's on-disk binary "+
		"is back to its pre-update state)", originalErr, backupPath)
}

// verifyInstalledBinary is the readback half of the install step (lr-c69197
// MILLER item 2: "installBinary returns nil on exit 0 and that is the whole
// contract — nothing re-reads the result"). It re-stats installPath — the
// exact path a running service execs from, not the staged temp file — and
// compares size and permission bits against stagedInfo (captured from the
// freshly built artifact before the rename). A version-string comparison
// (running `<installPath> version` and matching the expected revision) was
// considered, since lr-92ee18 made the version linkable via -ldflags -X, but
// is deliberately NOT used here: this repo's build does not thread an
// expected revision string into runUpdate at all (the Makefile's -X flag is
// applied by `make build`, not by the `go build -o` this file invokes
// directly), so a version check here would either need a second,
// out-of-band way to learn the expected revision or would silently compare
// against the empty-string default on every unmodified build — neither is
// reliable enough to gate a deploy on. Size+mode is real, always available,
// and directly answers the question a PASS must be falsifiable against: did
// the artifact that was just built actually land at the path the service
// execs from.
func verifyInstalledBinary(installPath string, stagedInfo os.FileInfo) error {
	installedInfo, err := os.Stat(installPath)
	if err != nil {
		return fmt.Errorf("expected an installed binary at %s after install, but stat failed: %w "+
			"(a PASS that installed nothing to a path nothing runs from is exactly the defect this "+
			"check exists to catch)", installPath, err)
	}
	if installedInfo.Size() != stagedInfo.Size() {
		return fmt.Errorf("installed binary at %s has size %d bytes, want %d (the freshly built "+
			"artifact's size) — the file at install_path does not match what was just built",
			installPath, installedInfo.Size(), stagedInfo.Size())
	}
	if installedInfo.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("installed binary at %s is not executable (mode %s)", installPath, installedInfo.Mode().Perm())
	}
	return nil
}

// installAndVerifyWithRollback is runUpdate's install step: back up any
// existing binary, atomically replace it with the freshly built one, verify
// the replacement, and — on EITHER an installBinary failure or a
// verification failure — restore the backup so installPath ends up in a
// real, falsifiable state rather than holding whatever installBinary's
// rename left there. Both failure modes are the same class ("installPath is
// not what it should be, and a good backup exists to restore from") and are
// routed through the single restoreBackupOrReport helper rather than two
// parallel restore implementations that could drift out of sync (lr-c69197
// second fold-in, PEACHES nit 1). Split out from runUpdate so it is
// independently unit-testable with synthetic staged/installed files, no
// real `go build` required (lr-c69197 fold-in regression test: "a failed
// verification restores the prior binary"; second fold-in regression test:
// "a failed installBinary restores the prior binary").
//
// hostname is used only for the success log line, matching runUpdate's own
// pre-fold-in wording.
func installAndVerifyWithRollback(stagedPath, installPath string, stagedInfo os.FileInfo, hostname string, out *os.File) error {
	// ROLLBACK (lr-c69197 fold-in, PEACHES comment 5373517397 / BOBBIE comment
	// 5373549900): the previously-installed binary is preserved BEFORE the
	// replace, at installPath+".bak", specifically so a verification failure
	// below has something real to restore — see backupInstalledBinary's own
	// doc for why a missing pre-existing binary (first-ever install) is not
	// an error here.
	backupPath, backupErr := backupInstalledBinary(installPath)
	if backupErr != nil {
		return fmt.Errorf("install: back up previous binary before replacing it: %w", backupErr)
	}

	fmt.Fprintf(out, "update: installing %s -> %s (atomic rename)\n", stagedPath, installPath)
	// ROLLBACK ON INSTALL FAILURE (lr-c69197 second fold-in, PEACHES nit 1):
	// installBinary itself can fail AFTER backupInstalledBinary has already
	// renamed the old binary away — os.Chmod on the staged file failing, or
	// the replacing os.Rename failing partway (cross-device, permissions
	// change mid-flight). Either leaves installPath stranded (absent, or
	// still holding whatever a partial rename left) with the good binary
	// sitting only at backupPath. This is the exact same "installPath is not
	// in a state matching what documentation claims" class the verification
	// rollback below closes — routed through the same restoreBackupOrReport
	// so there is one restore path, not two that can drift.
	if err := installBinary(stagedPath, installPath); err != nil {
		return fmt.Errorf("install: %w", restoreBackupOrReport(err, backupPath, installPath))
	}

	// READBACK (lr-c69197 MILLER item 2): installBinary's os.Rename returning
	// nil means "the syscall succeeded", not "the binary that is now running
	// is the one just staged". verifyInstalledBinary re-stats installPath —
	// the exact path a running service execs — and compares it against what
	// was staged, so a PASS here is falsifiable: if installPath does not
	// exist, or its size/mode don't match the staged artifact, this is a
	// hard error, not a silently-swallowed mismatch.
	//
	// ROLLBACK ON FAILURE (lr-c69197 fold-in): a verification failure here
	// used to leave installPath holding whatever installBinary's rename just
	// put there — the bad artifact, not the previously-working one, despite
	// documentation elsewhere claiming the old binary was "untouched". It is
	// restored from backupPath via the same restoreBackupOrReport used above
	// (best-effort: if the restore itself fails, that failure is reported
	// ALONGSIDE the verification failure, never in place of it — an operator
	// needs to know both that verification failed AND whether the restore
	// actually worked).
	if err := verifyInstalledBinary(installPath, stagedInfo); err != nil {
		return fmt.Errorf("install: post-install verification failed: %w",
			restoreBackupOrReport(err, backupPath, installPath))
	}

	// Re-stat installPath rather than reporting stagedInfo's pre-chmod mode
	// (lr-c69197 second fold-in, PEACHES nit 3): stagedInfo was captured
	// before installBinary's os.Chmod(0o755) ran, so it still carries the
	// staged artifact's original (often 0o644, from `go build`'s default
	// output mode) permission bits — logging it here claimed a mode that was
	// never actually on disk at installPath. installedInfo reflects what
	// verifyInstalledBinary itself just confirmed is really there.
	installedInfo, statErr := os.Stat(installPath)
	if statErr != nil {
		// Unreachable in practice — verifyInstalledBinary above just
		// successfully stat'd this exact path — but a stat failure here must
		// still not silently fall back to the misleading pre-chmod value.
		return fmt.Errorf("install: verified %s but a follow-up stat for the report line failed: %w",
			installPath, statErr)
	}
	fmt.Fprintf(out, "update: verified %s on %s (size=%d bytes, mode=%s)\n",
		installPath, hostname, installedInfo.Size(), installedInfo.Mode().Perm())

	// The backup has served its purpose once verification passes — remove it
	// rather than leaving a stale ".bak" artifact sitting next to installPath
	// forever. Best-effort: a removal failure here is a real problem (disk
	// full, permissions) but must not turn an otherwise-successful update
	// into a reported failure — it is logged, not returned.
	if backupPath != "" {
		if err := os.Remove(backupPath); err != nil {
			fmt.Fprintf(out, "update: warning: verified install succeeded, but removing the backup at %s failed: %v (harmless leftover file, safe to delete by hand)\n", backupPath, err)
		}
	}
	return nil
}

// systemdScope selects whether restartSystemdService targets the system
// systemd manager (PID 1, root-run units under /etc/systemd/system) or the
// per-user systemd manager (`systemctl --user`, units under
// ~/.config/systemd/user — see deploy/clagentic-router.user.service). This
// is deploy.service_manager's "systemd" vs. "systemd-user" distinction,
// carried as a typed value rather than re-branching on the raw config
// string past runUpdate, so systemctlRestartArgs stays a pure, easily
// tested function of (serviceName, scope).
type systemdScope int

const (
	systemdScopeSystem systemdScope = iota
	systemdScopeUser
)

// systemctlRestartArgs builds the argv for restarting serviceName at the
// given scope, without the leading "systemctl" — split out from
// restartSystemdService so the argument shape (in particular, that
// "--user" precedes "restart", and the unit name is always
// serviceName+".service") is unit-testable without invoking systemctl or
// requiring a systemd session in the test environment.
func systemctlRestartArgs(serviceName string, scope systemdScope) []string {
	unit := serviceName + ".service"
	if scope == systemdScopeUser {
		return []string{"--user", "restart", unit}
	}
	return []string{"restart", unit}
}

// restartSystemdService restarts the named systemd unit (without the
// .service suffix, which is appended here) at the given scope. systemd and
// none behavior is unchanged by the addition of the user-scope path: the
// system-scope branch below issues the exact same "systemctl restart
// <unit>.service" invocation as before "systemd-user" was added.
func restartSystemdService(serviceName string, scope systemdScope) error {
	args := systemctlRestartArgs(serviceName, scope)
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %w\n%s", strings.Join(args, " "), err, output)
	}
	return nil
}

// systemctlShowArgs builds the argv for reading a unit property via
// `systemctl show`, without the leading "systemctl" — mirrors
// systemctlRestartArgs's split so the argument shape ("--user" ahead of
// "show", "--property=" as one token, unit name always +".service") is
// unit-testable without invoking systemctl.
func systemctlShowArgs(serviceName string, scope systemdScope, property string) []string {
	unit := serviceName + ".service"
	args := []string{}
	if scope == systemdScopeUser {
		args = append(args, "--user")
	}
	args = append(args, "show", "--property="+property, "--value", unit)
	return args
}

// systemdUnitSnapshot is a restart's before/after fingerprint (lr-c69197
// MILLER item 2: "no systemctl is-active/ActiveEnterTimestamp comparison, no
// PID delta"). Both fields are read via `systemctl show`, not `systemctl
// status` — show's --value output is a single stable machine-readable line
// per property, not prose meant for a human terminal.
type systemdUnitSnapshot struct {
	activeEnterTimestamp string
	mainPID              string
}

// readSystemdUnitSnapshot reads ActiveEnterTimestamp and MainPID for the
// named unit at the given scope. A unit that does not exist yet (e.g. the
// very first restart before the unit was ever started) returns an error —
// callers use this only around a restart that is expected to succeed, so an
// unreadable unit here is itself informative, not a case to paper over.
func readSystemdUnitSnapshot(serviceName string, scope systemdScope) (systemdUnitSnapshot, error) {
	activeEnter, err := systemctlShowValue(serviceName, scope, "ActiveEnterTimestamp")
	if err != nil {
		return systemdUnitSnapshot{}, err
	}
	mainPID, err := systemctlShowValue(serviceName, scope, "MainPID")
	if err != nil {
		return systemdUnitSnapshot{}, err
	}
	return systemdUnitSnapshot{activeEnterTimestamp: activeEnter, mainPID: mainPID}, nil
}

// systemctlShowValue runs `systemctl [--user] show --property=<property>
// --value <unit>.service` and returns its trimmed stdout.
func systemctlShowValue(serviceName string, scope systemdScope, property string) (string, error) {
	args := systemctlShowArgs(serviceName, scope, property)
	cmd := exec.Command("systemctl", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("systemctl %s failed: %w\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

// restartAndVerifySystemdService is restartSystemdService plus the readback
// half of the restart step (lr-c69197 MILLER item 2): a `systemctl restart`
// that exits 0 only means systemd accepted the request, not that the unit
// actually cycled — restarting an already-stopped unit, or a unit whose
// ExecStart silently no-ops, can both exit 0 without the daemon actually
// having restarted. This captures the unit's ActiveEnterTimestamp and
// MainPID before and after the restart call and requires at least one of
// them to have changed; neither changing is treated as a restart that did
// not restart, per the task's explicit acceptance criterion ("a restart
// that did not restart must be an error, not a silent pass").
//
// The before-snapshot is best-effort: a unit that has never been started
// (first-ever deploy) has no ActiveEnterTimestamp/MainPID to read yet, and
// failing the whole restart over that would regress the pre-existing
// first-install case. Only the after-snapshot is required to succeed — if
// systemctl show cannot read the unit at all after a restart that itself
// reported success, that is a real error worth surfacing.
func restartAndVerifySystemdService(serviceName string, scope systemdScope, out *os.File) error {
	before, beforeErr := readSystemdUnitSnapshot(serviceName, scope)
	if beforeErr != nil {
		fmt.Fprintf(out, "update: could not read pre-restart unit state for %s.service (%v) — "+
			"proceeding, this is expected on a first-ever start\n", serviceName, beforeErr)
	}

	if err := restartSystemdService(serviceName, scope); err != nil {
		return err
	}

	after, err := readSystemdUnitSnapshot(serviceName, scope)
	if err != nil {
		return fmt.Errorf("restart reported success but post-restart unit state could not be read: %w "+
			"(a PASS that cannot confirm the unit is actually running is not a PASS)", err)
	}

	if err := verifyRestartAdvanced(serviceName, before, beforeErr, after); err != nil {
		return err
	}

	fmt.Fprintf(out, "update: verified restart of %s.service (ActiveEnterTimestamp=%s, MainPID=%s)\n",
		serviceName, after.activeEnterTimestamp, after.mainPID)
	return nil
}

// verifyRestartAdvanced is the pure comparison at the heart of the restart
// readback, split out from restartAndVerifySystemdService so it is
// unit-testable with synthetic before/after snapshots — no systemctl or
// systemd session required. Per the task's explicit acceptance criterion
// ("a restart that did not restart must be an error, not a silent pass"):
// when a pre-restart snapshot was successfully read (beforeErr == nil) and
// the ActiveEnterTimestamp/MainPID pair is byte-identical after the restart
// call reported success, that is treated as the unit never having actually
// cycled, regardless of the "systemctl restart" exit code. beforeErr != nil
// (no pre-restart snapshot available — first-ever start) always passes this
// check; there is nothing to compare against.
func verifyRestartAdvanced(serviceName string, before systemdUnitSnapshot, beforeErr error, after systemdUnitSnapshot) error {
	if beforeErr == nil && before == after {
		return fmt.Errorf("restart of %s.service reported success but the unit's ActiveEnterTimestamp (%s) "+
			"and MainPID (%s) are unchanged from before the restart — the service was not actually restarted",
			serviceName, after.activeEnterTimestamp, after.mainPID)
	}
	return nil
}
