// internal/backend/codex_discovery.go — automatic discovery of the codex_cli
// OpenAI-Project header inputs (lr-8dd85a).
//
// PR #35 (lr-60781e) required the operator to hand-type codex_provider_id
// and openai_project_id in router.yaml. That is the thing this file removes
// for the common case: both values are now PULLED from local state and a
// live API call, mirroring the discovery pattern clagentic-console uses for
// model lists (paginated GET with a static fallback only on failure — see
// generate-model-catalog.js / yoke/adapters/codex.js's model/list RPC).
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
// # Project discovery
//
// Bedrock mantle exposes project enumeration only on the OpenAI-compatible
// HTTP surface (no aws CLI / boto3 equivalent — established fact, see task
// lr-8dd85a):
//
//	GET https://bedrock-mantle.{region}.api.aws/v1/organization/projects
//	Authorization: Bearer <api key>
//
// The region is never a literal in this file — it is parsed out of the
// discovered provider's own base_url (which already points at the regional
// mantle endpoint), so a host in any region resolves correctly with zero
// code change.
//
// Multiple projects is resolved deterministically: a project whose name is
// literally "default" (AWS's own convention for the auto-created default
// project in a Bedrock organization) is preferred. No other project list
// order or count is used to break a tie — anything else would be a silent
// arbitrary pick, which the task explicitly forbids.
//
// # Caching and failure handling
//
// Discovery is expensive (reads a file, makes an HTTP call) and must not
// run on every Invoke. Callers run it once (e.g. at adapter construction,
// mirroring ResolveBinPath's construction-time binary resolution) and treat
// any failure as feature-off: an empty providerID/projectID pair, which
// codex_cli.go's existing empty-value check already treats as "no header
// injection" with zero behavior change. Discovery must never return an
// error that blocks constructing the adapter or invoking it.
package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// reservedCodexProviderIDs are the codex CLI's built-in model_providers keys.
// These are never eligible for automatic selection: codex hard-rejects an
// http_headers override against a reserved/builtin provider id (confirmed
// live, see lr-60781e task history) regardless of what is written to
// config.toml for them.
var reservedCodexProviderIDs = map[string]struct{}{
	"openai": {},
}

// defaultBedrockProjectName is the project name AWS Bedrock organizations
// use for the auto-created default project. Used only as the deterministic
// tie-break when GET /v1/organization/projects returns more than one
// project and the operator has not set an explicit override.
const defaultBedrockProjectName = "default"

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

// mantleRegionFromBaseURL extracts the AWS region from a Bedrock mantle
// base_url of the form "https://bedrock-mantle.{region}.api.aws/v1" (or any
// path suffix). Returns "" if baseURL does not match that host shape — the
// provider may be pointed somewhere else entirely, which is not this
// package's business to second-guess.
func mantleRegionFromBaseURL(baseURL string) string {
	const hostPrefix = "bedrock-mantle."
	const hostSuffix = ".api.aws"

	u := strings.TrimPrefix(baseURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	host := u
	if idx := strings.IndexByte(u, '/'); idx >= 0 {
		host = u[:idx]
	}
	if !strings.HasPrefix(host, hostPrefix) || !strings.HasSuffix(host, hostSuffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, hostPrefix), hostSuffix)
}

// bedrockProject is one entry from GET /v1/organization/projects.
type bedrockProject struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type bedrockProjectsResponse struct {
	Data []bedrockProject `json:"data"`
}

// discoverCodexProject calls GET /v1/organization/projects against the
// Bedrock mantle OpenAI-compatible surface for region and returns the
// project id to use.
//
// Selection rule, applied in order:
//  1. Zero projects returned: ("", nil) — feature off, not an error.
//  2. Exactly one project: use it.
//  3. Multiple projects: prefer the one named "default" (AWS's own
//     auto-created default project). If none is named "default", this is
//     genuinely ambiguous and returns an error — never a silent arbitrary
//     pick.
//
// Any HTTP/network/decode failure returns an error; callers must treat that
// as feature-off (see package doc), never as a reason to fail Invoke.
func discoverCodexProject(ctx context.Context, client *http.Client, region, apiKey string) (string, error) {
	if region == "" || apiKey == "" {
		return "", fmt.Errorf("codex_discovery: region and api key are both required for project discovery")
	}

	url := fmt.Sprintf("https://bedrock-mantle.%s.api.aws/v1/organization/projects", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("codex_discovery: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex_discovery: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("codex_discovery: GET %s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}

	var parsed bedrockProjectsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("codex_discovery: parse projects response: %w", err)
	}

	switch len(parsed.Data) {
	case 0:
		return "", nil
	case 1:
		return parsed.Data[0].ID, nil
	default:
		for _, p := range parsed.Data {
			if p.Name == defaultBedrockProjectName {
				return p.ID, nil
			}
		}
		ids := make([]string, len(parsed.Data))
		for i, p := range parsed.Data {
			ids[i] = p.ID
		}
		return "", fmt.Errorf(
			"codex_discovery: multiple projects found with no \"%s\"-named project to prefer (%s) — set openai_project_id explicitly to disambiguate",
			defaultBedrockProjectName, strings.Join(ids, ", "))
	}
}

// DiscoverCodexProjectHeader resolves both the provider id and project id
// for the codex_cli OpenAI-Project header injection, in one call, applying
// operator overrides where given and discovery otherwise. Called once at
// adapter construction time (never per-Invoke — see package doc on
// caching). apiKey is used only for the live project lookup; it is never
// logged.
//
// Any discovery failure (ambiguous provider, ambiguous project, HTTP
// failure, missing config) degrades to an empty providerID/projectID pair
// rather than propagating an error — codex_cli.go already treats an empty
// pair as "no header injection", so discovery failure can never break the
// request path. The failure reason is logged at Warn for operator
// visibility.
func DiscoverCodexProjectHeader(ctx context.Context, overrideProviderID, overrideProjectID, apiKey string) (providerID, projectID string) {
	providerID = overrideProviderID
	projectID = overrideProjectID

	var baseURL string
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
		baseURL = cand.BaseURL
	}

	if projectID == "" {
		if apiKey == "" {
			// No API key available for the live project lookup: feature off.
			// Not a warning — codex_cli backends commonly authenticate via
			// OAuth (ChatGPT Plus) with no api_key configured at all. The
			// provider id resolved above is still returned: codex_cli.go
			// only emits the header when BOTH values are non-empty, so an
			// empty projectID alone already suppresses injection correctly.
			return providerID, ""
		}
		region := mantleRegionFromBaseURL(baseURL)
		if region == "" {
			// Discovered provider isn't pointed at the mantle endpoint this
			// package knows how to query — nothing to discover, not an error.
			return providerID, ""
		}
		client := &http.Client{Timeout: 15 * time.Second}
		pid, err := discoverCodexProject(ctx, client, region, apiKey)
		if err != nil {
			logDiscoveryWarn("project", err)
			return providerID, ""
		}
		if pid == "" {
			return providerID, ""
		}
		projectID = pid
	}

	return providerID, projectID
}

// logDiscoveryWarn logs a discovery failure at Warn. Discovery failures
// never block Invoke (see DiscoverCodexProjectHeader doc) but should remain
// visible to an operator diagnosing why the header isn't being injected.
func logDiscoveryWarn(what string, err error) {
	slog.Warn("codex_cli discovery failed, feature disabled for this call", "what", what, "err", err)
}
