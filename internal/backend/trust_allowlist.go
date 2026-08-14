// internal/backend/trust_allowlist.go — operator-controlled containment for
// syncProjectTrust (lr-4abfe9, BOBBIE finding bobbie.uncat.1).
//
// # Why this exists
//
// syncProjectTrust (trust_sync.go) upserts
// projects[dir].hasTrustDialogAccepted = true into the isolated subprocess
// HOME's .claude.json. That bit is not cosmetic: once set, the claude CLI
// stops refusing to honor that directory's .claude/settings.json
// permissions.allow entries, hooks, and project CLAUDE.md memory. Before
// this file existed, syncProjectTrust granted that trust to ANY dir that
// passed backend.ResolveWorkingDir — which validates only
// absolute/exists/is-a-directory and is explicitly documented (see this
// repo's CLAUDE.md) as NOT a containment control. A caller-supplied
// working_dir wire value therefore silently widened from "choose a cwd" to
// "choose whose hooks execute" the moment lr-4abfe9 shipped.
//
// TrustAllowlist closes that gap: syncProjectTrust now only writes the trust
// bit for a dir that matches an entry on this operator-controlled list.
// Every other dir gets no write, and the subprocess fails exactly as it did
// before lr-4abfe9 (the pre-existing, known "workspace has not been
// trusted" error) — that failure is the safe, expected outcome for an
// unlisted dir, not a regression.
//
// # What trust grants, restated (breadth doctrine)
//
// Adding a path to this allowlist is an operator opting the router daemon
// into running that directory's claude CLI hooks, hooks-adjacent
// permissions.allow entries, and project CLAUDE.md memory inside every
// subprocess invocation against that directory — for every caller able to
// reach this router's /v1/chat/completions endpoint with that working_dir
// value, not just a trusted local operator. Treat each entry as "this
// directory's repo owner can run arbitrary hook code inside my router
// daemon's subprocess," because that is what accepting the trust dialog
// means to the claude CLI itself.
//
// # Fail-closed default
//
// An empty or absent allowlist trusts nothing. This is a deliberate
// deviation from "no config = old behavior": the old behavior (unconditional
// trust of any resolvable dir) is exactly the defect this file exists to
// remove, so preserving it as the no-config default would silently
// reintroduce the widening for every deployment that upgrades without
// touching config. A deployment that wants the pre-fix convenience must now
// say so explicitly, directory by directory.
//
// # DefaultWorkingDir ("/")
//
// "/" receives no special-casing — it is just another candidate string
// checked against the allowlist like any other. In practice this means "/"
// is untrusted unless an operator explicitly lists it, which is correct:
// "/" is the fallback every subprocess adapter uses when a caller supplies
// no working_dir at all (see adapter.go's DefaultWorkingDir doc), so it is
// the single most likely value to be hit by a caller that supplies nothing
// or a caller that supplies garbage upstream of ResolveWorkingDir's
// validation. Trusting it by default would mean "every caller who doesn't
// bother to pass a working_dir gets root trusted," which is the opposite of
// this file's purpose. An operator who genuinely wants to run
// hooks/settings out of "/" can add it explicitly — that is their opt-in
// like any other path.
//
// # Canonicalization and the TOCTOU window
//
// membership is checked against the allowlist entries themselves resolved
// once at construction (NewTrustAllowlist), and the candidate dir resolved
// fresh on every Allows call via filepath.EvalSymlinks — so a symlink or
// ".."-bearing path that resolves outside every allowlisted tree is
// refused, not matched by surface string comparison. This mirrors
// ResolveWorkingDir's own posture: it is not a containment control in the
// sense of closing every race. There remains a TOCTOU window between this
// resolve-and-check and the later exec.Start of the subprocess (the same
// window CLAUDE.md already documents for ResolveWorkingDir) — a directory
// could be replaced between the check here and the subprocess actually
// running. This file does not attempt to close that window; it only adds
// the missing opt-in boundary BOBBIE's finding required. Do not read the
// EvalSymlinks call as closing the TOCTOU gap — it only defeats a
// static symlink/traversal bypass at check time.
package backend

import (
	"log/slog"
	"path/filepath"
)

// TrustAllowlist is the operator-controlled set of directories syncProjectTrust
// is permitted to mark trusted. A nil or empty TrustAllowlist trusts nothing
// (fail-closed default — see package doc).
//
// Membership is EXACT-MATCH on the canonicalized path, never subtree/prefix
// matching: listing "/workspace" does NOT admit "/workspace/foo". Despite
// the plural "_dirs" in the config field name (trusted_working_dirs), each
// entry trusts only that one directory — every directory a subprocess may
// run in, including subdirectories of an already-listed one, needs its own
// entry. This is the safer of the two possible semantics (an operator who
// wants breadth must say so explicitly, one entry per directory, rather than
// a single top-level entry silently trusting an entire tree including
// directories that did not exist when the allowlist was written) and must
// be preserved — do not "helpfully" add prefix matching later without
// treating that as the security-relevant semantic change it would be.
type TrustAllowlist struct {
	// canon holds each configured entry's canonicalized (symlink-resolved,
	// absolute) form, keyed by itself for O(1) membership testing.
	canon map[string]struct{}
}

// NewTrustAllowlist builds a TrustAllowlist from operator-supplied directory
// strings (config.Config.TrustedWorkingDirs). Each entry is resolved via
// filepath.EvalSymlinks at construction time so allowlist membership is
// always evaluated against real, canonical paths — never a symlink alias of
// one. An entry that does not exist on disk, or otherwise fails to resolve,
// is dropped with a loud Warn log rather than silently admitted or treated
// as a fatal startup error: a typo'd or not-yet-created path should not be
// able to block the whole daemon from starting, but it must not silently
// grant trust to whatever EvalSymlinks would have failed to resolve either.
//
// entries == nil or empty returns a TrustAllowlist that trusts nothing —
// the safe default (see package doc "Fail-closed default").
func NewTrustAllowlist(entries []string) *TrustAllowlist {
	a := &TrustAllowlist{canon: make(map[string]struct{}, len(entries))}
	for _, raw := range entries {
		if raw == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(raw)
		if err != nil {
			slog.Warn("trust_allowlist: dropping trusted_working_dirs entry that could not be resolved; "+
				"it will never be trusted until the path exists and this entry is corrected",
				"path", raw, "err", err)
			continue
		}
		if !filepath.IsAbs(resolved) {
			slog.Warn("trust_allowlist: dropping trusted_working_dirs entry that did not resolve to an absolute path",
				"path", raw, "resolved", resolved)
			continue
		}
		a.canon[resolved] = struct{}{}
	}
	return a
}

// Allows reports whether dir is permitted to receive the trust write. dir is
// canonicalized via filepath.EvalSymlinks before the membership check, so a
// symlink or ".."-bearing path that resolves outside every allowlisted
// directory is refused rather than matched on surface string form. The
// check itself is EXACT-MATCH against the canonicalized entries (see the
// TrustAllowlist struct doc) — a dir that is a subdirectory of an
// allowlisted entry, but not itself an entry, is refused just like any
// other non-member. A dir that fails to resolve (e.g. does not exist) is
// refused — syncProjectTrust
// is only ever called with a path that has already passed
// backend.ResolveWorkingDir's existence check, so a resolve failure here
// would indicate the directory vanished between validation and this check
// (the documented TOCTOU window), and the safe response to that is refusal,
// not a guess.
//
// A nil receiver (uninitialized TrustAllowlist) always returns false —
// mirrors the fail-closed default for callers that construct their adapter
// without wiring an allowlist at all.
func (a *TrustAllowlist) Allows(dir string) bool {
	if a == nil || len(a.canon) == 0 || dir == "" {
		return false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	_, ok := a.canon[resolved]
	return ok
}
