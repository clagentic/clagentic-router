// internal/backend/binpath.go — startup binary resolution with logging and security checks.
//
// ResolveBinPath is called at adapter construction time to resolve a CLI binary,
// log its absolute path, and warn if it lives in a world-writable directory.
// Lazy re-resolution on FileNotFoundError is handled by each adapter's refreshBin().
package backend

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// ResolveBinPath resolves a CLI binary to its absolute path at construction time.
// If configured is non-empty it is validated (see isExecutableFile) and used
// directly on success — no PATH search. Otherwise ResolveBinary is called
// with envVar so that env overrides (e.g. CLAUDE_BIN) are honoured at
// construction time — matching the behaviour of the invoke-time
// resolveBin/refreshBin path.
//
// The resolved path is logged at Info. A warning is emitted if the binary's
// parent directory is world-writable (indicates a supply-chain risk).
//
// Returns the resolved absolute path, or an empty string when the binary is
// not found or not usable. An empty return does NOT block construction —
// adapters fall back to bare name resolution at invoke time, which produces
// a clear ErrTypeNotFound error.
//
// An unresolvable binary is logged at ERROR, not WARN (lr-92ee18 B2): this is
// a configured backend that cannot possibly serve a request until the
// operator fixes it, not a transient or cosmetic condition — WARN-level
// logging let this defect hide in a startup log an operator was not
// necessarily watching, and the backend still reported /health status:ok
// until the first real request failed. The adapter's BinaryChecker
// implementation (see adapter.go) surfaces the same fact on /health and
// /doctor so it is visible without grepping the startup log at all.
//
// A configured bin_path is validated the same way a PATH-resolved binary
// already is (lr-92ee18 PEACHES re-review, comment 5371343493 finding 2):
// before this fix, ResolveBinPath accepted any non-empty configured string
// verbatim, so an operator typo or a bin_path pointing at a moved/deleted
// file never appeared as unresolved on /health or /doctor — defect B2's
// stated purpose ("a router configured with an unresolvable CLI binary
// would report /health status:ok indefinitely" — see BinaryChecker's doc in
// adapter.go) was defeated for exactly the deployment shape (explicit
// bin_path override) most likely to have a stale or mistyped value. The
// auto-discovery path (ResolveBinary, both the exec.LookPath branch and the
// extraBinDirs branch) already only returns a path that passed an
// existence+non-directory check — exec.LookPath additionally requires the
// executable bit on Unix — so this brings the configured-path branch to the
// same standard rather than a stricter one.
func ResolveBinPath(name, configured, envVar string) string {
	var resolved string
	if configured != "" {
		if !isExecutableFile(configured) {
			slog.Error("configured bin_path is not an executable file — this backend cannot serve requests until fixed",
				"name", name, "bin_path", configured)
			return ""
		}
		resolved = configured
	} else {
		resolved = ResolveBinary(name, envVar)
	}

	if resolved == "" {
		slog.Error("binary not found at startup — this backend cannot serve requests until fixed",
			"name", name)
		return ""
	}

	abs, err := filepath.Abs(resolved)
	if err == nil {
		resolved = abs
	}

	slog.Info("binary resolved", "name", name, "path", resolved)
	warnIfWorldWritable(resolved)
	return resolved
}

// isExecutableFile reports whether path exists, is a regular (non-directory)
// file, and is executable by someone (any of the owner/group/other execute
// bits set). This mirrors what ResolveBinary's two search paths already
// guarantee for an auto-discovered binary: exec.LookPath enforces the
// executable bit as part of PATH resolution on Unix, and the extraBinDirs
// fallback at minimum enforces "exists and is not a directory" — a
// configured bin_path previously enforced neither.
//
// The permission-bit check is Unix-specific (os.FileMode's executable bits
// have no equivalent meaning on Windows, where executability is
// extension-based) — on windows this function only checks existence and
// non-directory, matching what the extraBinDirs branch already does
// cross-platform.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

// warnIfWorldWritable logs a warning if the directory containing bin is world-writable.
// A world-writable bin directory allows any local user to replace the binary.
func warnIfWorldWritable(bin string) {
	dir := filepath.Dir(bin)
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0002 != 0 {
		slog.Warn("binary is in a world-writable directory", "path", bin, "dir", dir)
	}
}
