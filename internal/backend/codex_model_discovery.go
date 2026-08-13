// internal/backend/codex_model_discovery.go — automatic discovery of the
// codex_cli model string via `codex debug models` + `codex doctor --json`
// (lr-82e68e).
//
// lr-8dd85a (codex_discovery.go) removed the hand-typed provider and project
// ids for the OpenAI-Project header. This file removes the last hand-typed
// value: the model string itself.
//
// # Mechanism
//
// `codex debug models` returns the model catalog as JSON, already filtered
// to the CLI's active auth context, entirely offline (bundled install data,
// not a network call). The catalog shape and field names below are captured
// ground truth from a live run on the ChatGPT-Plus (ambient "openai"
// provider) path — see task lr-82e68e comment thread. Key facts that shape
// this parser:
//
//   - Top-level shape is an OBJECT with a "models" key wrapping the array,
//     not a bare array.
//   - Each entry carries slug, display_name, priority, visibility, and
//     supported_in_api.
//   - visibility is exactly "list" or "hide".
//   - priority is codex's own internal ranking for the active auth context
//     only: CONFIRMED non-comparable across contexts (sparse, non-contiguous
//     values observed; different absolute ranges reported for different
//     auth contexts). Rank MUST be resolved by sorted position within the
//     filtered set, never by matching a literal priority value or assuming
//     a 0 base or contiguous range.
//   - Entries have been observed pre-sorted by priority ascending, but this
//     package sorts explicitly rather than depending on input order.
//   - slug format (bare vs. provider-prefixed) is UNCONFIRMED to be uniform
//     across auth contexts — this package takes slug verbatim from the
//     catalog and never constructs, prefixes, strips, or normalizes it.
//
// `codex doctor --json` exposes the active provider at
// checks -> "config.load" -> details -> "model provider" (literal dot in
// "config.load", literal space in "model provider") — already used by
// codex_discovery.go's provider-id discovery, reused here only to fail
// loudly with a named provider if that path is ever needed for
// diagnostics. Model discovery itself does not need the provider name: the
// catalog `codex debug models` returns is already scoped to whatever auth
// context codex is running under, so this package only needs the model
// list, not the provider string.
//
// # Filtering, sorting, selection
//
// Filter to visibility == "list" AND supported_in_api == true, sort
// ascending by priority, then select by rank (0-indexed, best-first —
// matches how Config.Tiers/Config.Chains already express preference as
// ordered-list position, see config.go). An out-of-range rank is a
// construction-time error, not a clamp: silently substituting the
// nearest-available model would pick something the operator's rank did not
// actually express, which is exactly the "silently wrong model" outcome
// this feature must never produce. A catalog filtered down to only a
// handful of usable entries (observed: as few as three) makes an
// out-of-range rank a realistic operator mistake, not a theoretical edge
// case — it must be surfaced, not papered over.
//
// # Caching and failure handling
//
// Resolution runs once, at adapter-construction time (never per-Invoke),
// mirroring DiscoverCodexProjectHeader's caching contract in
// codex_discovery.go. Unlike that header discovery (which degrades to
// empty/feature-off on any failure because omission is always safe),
// MODEL discovery failure cannot degrade to an empty model string when the
// operator has asked for rank-based selection — an empty model isn't
// "feature off," it's "codex picks whatever its own default is," which is
// exactly the silent behavior this feature exists to remove. So:
//
//   - Model explicitly set in BackendConfig: discovery is never attempted.
//     Zero behavior change from before this feature existed.
//   - Model unset, discovery succeeds: the resolved slug is used.
//   - Model unset, discovery fails (command failure, malformed JSON, empty
//     catalog after filtering, rank out of range): ResolveCodexModel
//     returns a descriptive error. Callers (buildAdapter in main.go) must
//     treat this as a construction failure for that backend, not silently
//     fall back to an empty/default model — an actionable startup log
//     entry is the correct outcome, matching how buildAdapter already
//     treats other construction errors (e.g. ollama_http requires url).
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
)

// codexModelEntry is one entry from `codex debug models`' "models" array.
// Only the fields this package uses are declared; unknown fields are
// ignored by encoding/json without error.
type codexModelEntry struct {
	Slug           string `json:"slug"`
	DisplayName    string `json:"display_name"`
	Priority       int    `json:"priority"`
	Visibility     string `json:"visibility"`
	SupportedInAPI bool   `json:"supported_in_api"`
}

// codexModelsResponse is the top-level shape of `codex debug models`
// output: an object wrapping the entry array under "models", not a bare
// array (confirmed live; a bare-array parser fails outright against this
// shape).
type codexModelsResponse struct {
	Models []codexModelEntry `json:"models"`
}

const (
	codexModelVisibilityList = "list"
)

// runCodexDebugModels invokes `codex debug models` and returns its parsed
// catalog. bin is the resolved codex binary path (empty falls back to bare
// "codex" via exec.Command's PATH lookup, matching other adapters'
// resolution fallback shape).
func runCodexDebugModels(ctx context.Context, bin string) ([]codexModelEntry, error) {
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.CommandContext(ctx, bin, "debug", "models")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("codex_model_discovery: %s debug models: %w (stderr: %s)",
			bin, err, truncate(stderr.String(), 300))
	}

	var parsed codexModelsResponse
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("codex_model_discovery: parse debug models output: %w", err)
	}
	return parsed.Models, nil
}

// filterAndSortCodexModels filters entries to visibility=="list" AND
// supported_in_api==true, then returns them sorted ascending by priority.
// Selection by rank must always operate on this filtered+sorted slice's
// POSITION, never on the priority value itself (see package doc: priority
// ranges are not comparable across codex auth contexts and were observed
// sparse/non-contiguous even within one context).
func filterAndSortCodexModels(entries []codexModelEntry) []codexModelEntry {
	var usable []codexModelEntry
	for _, e := range entries {
		if e.Visibility == codexModelVisibilityList && e.SupportedInAPI {
			usable = append(usable, e)
		}
	}
	sort.SliceStable(usable, func(i, j int) bool {
		return usable[i].Priority < usable[j].Priority
	})
	return usable
}

// selectCodexModelByRank returns the slug at sorted position rank (0 =
// best) within usable. Returns a descriptive error if rank is out of range
// or usable is empty — never a silent clamp to the nearest available
// entry, per package doc.
func selectCodexModelByRank(usable []codexModelEntry, rank int) (string, error) {
	if len(usable) == 0 {
		return "", fmt.Errorf("codex_model_discovery: no usable models in catalog (visibility=%q, supported_in_api=true) — cannot resolve rank %d",
			codexModelVisibilityList, rank)
	}
	if rank < 0 || rank >= len(usable) {
		return "", fmt.Errorf("codex_model_discovery: rank %d out of range — catalog has %d usable model(s) (ranks 0..%d) after filtering",
			rank, len(usable), len(usable)-1)
	}
	// slug is passed through verbatim — never constructed, prefixed,
	// stripped, or normalized (see package doc: slug format is unconfirmed
	// to be uniform across auth contexts).
	return usable[rank].Slug, nil
}

// ResolveCodexModel resolves the codex_cli model string for one backend at
// adapter-construction time. bin is the resolved codex binary path (may be
// empty; see runCodexDebugModels). rank is the 0-indexed best-first
// preference (BackendConfig.ResolvedModelRank()).
//
// Discovery is only ever invoked by callers when BackendConfig.Model is
// unset — this function does not itself check that, so callers (buildAdapter
// in main.go) MUST short-circuit on an explicit Model before calling this,
// exactly as DiscoverCodexProjectHeader's callers already short-circuit on
// explicit CodexProviderID/OpenAIProjectID overrides.
//
// Returns a descriptive error on any failure (command failure, malformed
// JSON, empty catalog after filtering, rank out of range) — this must never
// be treated as "feature off": an unresolved model for a backend that asked
// for rank-based selection is a construction failure for that backend, not
// a silently-empty model string. See package doc.
func ResolveCodexModel(ctx context.Context, bin string, rank int) (string, error) {
	entries, err := runCodexDebugModels(ctx, bin)
	if err != nil {
		return "", err
	}
	usable := filterAndSortCodexModels(entries)
	return selectCodexModelByRank(usable, rank)
}
