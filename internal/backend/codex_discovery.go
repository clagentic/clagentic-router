// internal/backend/codex_discovery.go — automatic discovery of the codex_cli
// OpenAI-Project header's provider-id input (lr-8dd85a).
//
// PR #35 (lr-60781e) required the operator to hand-type codex_provider_id in
// router.yaml. That is the thing this file removes for the common case: the
// provider id is PULLED from local state, mirroring the discovery pattern
// clagentic-console uses for model lists (paginated GET with a static
// fallback only on failure — see generate-model-catalog.js /
// yoke/adapters/codex.js's model/list RPC).
//
// # Provider discovery
//
// The provider id is read from the operator's local codex CLI config (the
// [model_providers.<id>] tables in config.toml, resolved via CODEX_HOME or
// the default ~/.codex/config.toml — same file the codex binary itself
// reads). Reserved builtin provider ids (see reservedCodexProviderIDs) are
// excluded because codex hard-rejects an http_headers override against a
// builtin. Exactly one non-reserved entry is used automatically; zero means
// the feature is off; more than one is genuinely ambiguous and requires the
// operator to set codex_provider_id explicitly.
//
// This file does not vendor a TOML library (no allow_new_deps in
// .crew/amos.yaml): it parses only the two constructs it needs — top-level
// [section] / [section.sub] table headers and a same-line base_url = "..."
// string assignment inside a table. Anything else in config.toml (arrays,
// nested inline tables, multiline strings, comments mid-value, etc.) is
// irrelevant to discovery and is not a claim this parser is a general TOML
// reader.
//
// # Project id is override-only
//
// The OpenAI-Project header value (openai_project_id) has no discovery path
// in this file. There is no verified, callable endpoint for enumerating
// Bedrock mantle projects; a prior version of this file asserted one as
// fact without ever calling it (lr-698965 reverted that code). An operator
// who wants the header injected sets openai_project_id explicitly; unset
// means the header is simply not injected — no live call, no credential
// needed on this path. A non-empty value is validated the same way
// providerID is (validateOpenAIProjectID mirrors validateCodexProviderID)
// before it is interpolated into codex's own -c http_headers override
// string; rejection means the header is not injected and is logged at Warn,
// since — unlike provider discovery — a malformed value here can only be an
// operator typo, never a discovery-path artifact.
//
// # Caching and failure handling
//
// Discovery is not free (reads a file) and must not run on every Invoke.
// Callers run it once (e.g. at adapter construction, mirroring
// ResolveBinPath's construction-time binary resolution) and treat any
// failure as feature-off: an empty providerID/projectID pair, which
// codex_cli.go's existing empty-value check already treats as "no header
// injection" with zero behavior change. Discovery must never return an
// error that blocks constructing the adapter or invoking it.
package backend

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxCodexProviderIDLen bounds an accepted codex model_providers.<id> key
// before it is interpolated into codex's own -c TOML-override syntax. Real
// provider ids are short slugs; this exists only to reject a pathological
// value, not to accommodate any observed real-world id.
const maxCodexProviderIDLen = 64

// maxOpenAIProjectIDLen bounds an accepted openai_project_id value before it
// is interpolated into codex's own -c
// model_providers.<id>.http_headers={"OpenAI-Project"="<id>"} override
// string (codex_cli.go). Real OpenAI project ids are short slugs; this
// exists only to reject a pathological value, not to accommodate any
// observed real-world id.
const maxOpenAIProjectIDLen = 128

// isCodexProviderIDChar reports whether r is valid within a codex
// model_providers.<id> key. codex's own TOML table-header parsing already
// constrains what parseModelProviders extracts (see the header-parsing loop
// above: no "." tolerated, quote-stripped), but the id is re-validated here,
// independently, at the point it is about to cross into codex's own -c
// override syntax (codex_cli.go) — a value crossing into another tool's
// config-override parser should not rely on an upstream parser's side
// effects to stay safe. Lowercase/uppercase alnum, hyphen, and underscore
// covers every realistic provider id shape without accepting characters that
// have any special meaning to TOML or shell.
func isCodexProviderIDChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
}

// validateCodexProviderID rejects a providerID that is empty, too long, or
// contains any character outside the alnum/hyphen/underscore class. Never
// rewrites or strips — an invalid providerID degrades the caller to
// feature-off, consistent with every other discovery failure path in this
// file (see package doc).
func validateCodexProviderID(id string) bool {
	if id == "" || len(id) > maxCodexProviderIDLen {
		return false
	}
	for _, r := range id {
		if !isCodexProviderIDChar(r) {
			return false
		}
	}
	return true
}

// isOpenAIProjectIDChar reports whether r is valid within an
// openai_project_id value. The value is interpolated into codex's own -c
// http_headers={"OpenAI-Project"="<id>"} TOML/JSON-ish override string
// (codex_cli.go) — a value crossing into another tool's config-override
// parser should not rely on that parser tolerating arbitrary bytes.
// Lowercase/uppercase alnum, hyphen, and underscore covers every realistic
// project id shape without accepting characters (quote, brace, equals,
// whitespace) that have special meaning to TOML or the surrounding
// http_headers JSON-ish literal — the same class used for providerID
// (isCodexProviderIDChar) since both values cross into the same override
// string.
func isOpenAIProjectIDChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
}

// validateOpenAIProjectID rejects a projectID that is empty, too long, or
// contains any character outside the alnum/hyphen/underscore class. Never
// rewrites or strips — an invalid projectID degrades the caller to
// feature-off (header not injected), consistent with validateCodexProviderID
// and every other discovery failure path in this file.
func validateOpenAIProjectID(id string) bool {
	if id == "" || len(id) > maxOpenAIProjectIDLen {
		return false
	}
	for _, r := range id {
		if !isOpenAIProjectIDChar(r) {
			return false
		}
	}
	return true
}

// reservedCodexProviderIDs are the codex CLI's built-in model_providers keys.
// These are never eligible for automatic selection: codex hard-rejects an
// http_headers override against a reserved/builtin provider id (confirmed
// live, see lr-60781e task history) regardless of what is written to
// config.toml for them.
var reservedCodexProviderIDs = map[string]struct{}{
	"openai": {},
}

// codexConfigPath returns the path to the codex CLI's config.toml, honoring
// CODEX_HOME the same way the codex binary itself does. Empty return means
// no usable path could be resolved (e.g. no HOME in the environment).
func codexConfigPath() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "config.toml")
	}
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// codexProviderCandidate is one non-reserved [model_providers.<id>] table
// found in config.toml.
type codexProviderCandidate struct {
	ID      string
	BaseURL string
}

// discoverCodexProvider reads the codex CLI config at path and returns the
// single non-reserved model_providers entry to use automatically.
//
// Zero non-reserved entries: returns ("", "", nil) — feature off, not an
// error (the operator simply has no Bedrock-mode provider configured).
// Exactly one: returns it. More than one: returns an error naming the
// candidates — genuinely ambiguous, the operator must set codex_provider_id.
// A missing or unreadable config file is also feature-off, not an error —
// codex_cli works perfectly well with no config.toml at all (ChatGPT-Plus
// auth, the common case).
func discoverCodexProvider(path string) (codexProviderCandidate, error) {
	if path == "" {
		return codexProviderCandidate{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return codexProviderCandidate{}, nil // missing/unreadable = feature off, not a hard error
	}
	defer f.Close()

	candidates, err := parseModelProviders(f)
	if err != nil {
		return codexProviderCandidate{}, nil // malformed config = feature off, never blocks Invoke
	}

	var nonReserved []codexProviderCandidate
	for _, c := range candidates {
		if _, reserved := reservedCodexProviderIDs[c.ID]; reserved {
			continue
		}
		nonReserved = append(nonReserved, c)
	}

	switch len(nonReserved) {
	case 0:
		return codexProviderCandidate{}, nil
	case 1:
		return nonReserved[0], nil
	default:
		ids := make([]string, len(nonReserved))
		for i, c := range nonReserved {
			ids[i] = c.ID
		}
		return codexProviderCandidate{}, fmt.Errorf(
			"codex_discovery: multiple non-reserved model_providers entries found (%s) — set codex_provider_id explicitly to disambiguate",
			strings.Join(ids, ", "))
	}
}

// modelProvidersTableRe-free scan: parseModelProviders reads TOML-ish
// content looking only for "[model_providers.<id>]" table headers and a
// same-line "base_url = \"...\"" assignment within that table. This is
// intentionally narrow — see package doc for why no TOML library is used.
func parseModelProviders(r io.Reader) ([]codexProviderCandidate, error) {
	var candidates []codexProviderCandidate
	var current *codexProviderCandidate

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			// Entering a new table (or array-of-tables) header — flush any
			// in-progress model_providers entry first.
			if current != nil {
				candidates = append(candidates, *current)
				current = nil
			}
			header := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			header = strings.TrimPrefix(header, "[") // tolerate [[array.tables]]
			header = strings.TrimSuffix(header, "]")
			const prefix = "model_providers."
			if strings.HasPrefix(header, prefix) {
				id := strings.TrimPrefix(header, prefix)
				id = strings.Trim(id, `"'`)
				if id != "" && !strings.Contains(id, ".") {
					current = &codexProviderCandidate{ID: id}
				}
			}
			continue
		}

		if current == nil {
			continue
		}
		if key, val, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "base_url" {
			current.BaseURL = strings.Trim(strings.TrimSpace(val), `"'`)
		}
	}
	if current != nil {
		candidates = append(candidates, *current)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// DiscoverCodexProjectHeader resolves the provider id for the codex_cli
// OpenAI-Project header injection, applying an operator override where
// given and discovery otherwise. Called once at adapter construction time
// (never per-Invoke — see package doc on caching).
//
// projectID has no discovery path (see package doc, "Project id is
// override-only"): unset (empty) is returned unchanged and simply means no
// header injection — that is not a validation failure and is never logged.
// A non-empty overrideProjectID is validated the same way providerID is
// (see below) before it is returned, because it is interpolated into the
// same codex -c http_headers override string. Since this field has no
// discovery path, a non-empty value is always an operator's own
// router.yaml setting — so unlike a discovery failure (silent, expected in
// normal operation), a rejected explicit setting is logged at Warn: an
// operator who typed openai_project_id and had it silently dropped would
// have no signal that their header is not being injected. codex_cli.go
// only emits the header when both providerID and projectID are non-empty,
// so a rejected/unset projectID alone suppresses injection without
// blocking or delaying Invoke.
//
// Any provider-discovery failure (ambiguous provider, missing/malformed
// config) degrades to an empty providerID/projectID pair rather than
// propagating an error — codex_cli.go already treats an empty pair as "no
// header injection", so discovery failure can never break the request
// path. The failure reason is logged at Warn for operator visibility.
func DiscoverCodexProjectHeader(overrideProviderID, overrideProjectID string) (providerID, projectID string) {
	providerID = overrideProviderID
	projectID = overrideProjectID

	if providerID == "" {
		cand, err := discoverCodexProvider(codexConfigPath())
		if err != nil {
			logDiscoveryWarn("provider", err)
			return "", ""
		}
		if cand.ID == "" {
			return "", "" // zero non-reserved providers: feature off, not a warning
		}
		providerID = cand.ID
	}

	// providerID (whether an operator override or config.toml-discovered)
	// is about to be interpolated into codex's own -c
	// model_providers.<id>.http_headers TOML-override syntax (codex_cli.go)
	// — validate its character class before it ever leaves this function.
	// Rejection degrades to feature-off like every other discovery failure
	// path here, never a rewrite/strip.
	if !validateCodexProviderID(providerID) {
		slog.Warn("codex_cli discovery: providerID failed validation, feature disabled for this call",
			"provider_id_len", len(providerID))
		return "", ""
	}

	// projectID is override-only (see doc above) — an empty value is the
	// normal "operator hasn't set it" case and is not logged. A non-empty
	// value that fails validation, however, can only be an operator typo in
	// router.yaml (there is no discovery path to produce a malformed one),
	// so it is surfaced at Warn rather than silently dropped like the
	// zero-candidate provider-discovery case above.
	if projectID != "" && !validateOpenAIProjectID(projectID) {
		slog.Warn("codex_cli: openai_project_id failed validation, header injection disabled for this call",
			"project_id_len", len(projectID))
		return providerID, ""
	}

	return providerID, projectID
}

// logDiscoveryWarn logs a discovery failure at Warn. Discovery failures
// never block Invoke (see DiscoverCodexProjectHeader doc) but should remain
// visible to an operator diagnosing why the header isn't being injected.
func logDiscoveryWarn(what string, err error) {
	slog.Warn("codex_cli discovery failed, feature disabled for this call", "what", what, "err", err)
}
