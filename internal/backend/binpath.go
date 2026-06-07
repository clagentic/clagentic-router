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
)

// ResolveBinPath resolves a CLI binary to its absolute path at construction time.
// If configured is non-empty it is used directly (no PATH search). Otherwise
// ResolveBinary is called with envVar so that env overrides (e.g. CLAUDE_BIN)
// are honoured at construction time — matching the behaviour of the invoke-time
// resolveBin/refreshBin path.
//
// The resolved path is logged at Info. A warning is emitted if the binary's
// parent directory is world-writable (indicates a supply-chain risk).
//
// Returns the resolved absolute path, or an empty string when the binary is not
// found. An empty return does NOT block construction — adapters fall back to bare
// name resolution at invoke time, which produces a clear ErrTypeNotFound error.
func ResolveBinPath(name, configured, envVar string) string {
	var resolved string
	if configured != "" {
		resolved = configured
	} else {
		resolved = ResolveBinary(name, envVar)
	}

	if resolved == "" {
		slog.Warn("binary not found at startup", "name", name)
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
