// internal/backend/codex_discovery_test.go — table-driven tests for codex_cli's
// automatic provider-id discovery, and for DiscoverCodexProjectHeader's
// override-only project id handling (lr-8dd85a; project-id discovery
// reverted at lr-698965).
//
// All ids below are fabricated placeholders, not real provider or project ids.
package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCodexConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return path
}

// TestDiscoverCodexProvider covers single/zero/multiple non-reserved
// model_providers entries, plus reserved-provider exclusion and a missing
// config file.
func TestDiscoverCodexProvider(t *testing.T) {
	cases := []struct {
		name       string
		config     string
		missing    bool
		wantID     string
		wantErr    bool
		wantURLSub string // substring expected in the resolved base_url, if wantID set
	}{
		{
			name: "single non-reserved provider",
			config: `
[model_providers.acme-bedrock]
name = "Acme Bedrock"
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
wire_api = "chat"
`,
			wantID:     "acme-bedrock",
			wantURLSub: "bedrock-mantle.us-fake-1.api.aws",
		},
		{
			name: "zero providers (empty file)",
			config: `
[some_other_section]
key = "value"
`,
			wantID: "",
		},
		{
			name: "zero non-reserved (only reserved builtin present)",
			config: `
[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
`,
			wantID: "",
		},
		{
			name: "multiple non-reserved providers is ambiguous",
			config: `
[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"

[model_providers.other-provider]
base_url = "https://bedrock-mantle.eu-fake-1.api.aws/v1"
`,
			wantErr: true,
		},
		{
			name: "reserved builtin ignored alongside one non-reserved",
			config: `
[model_providers.openai]
base_url = "https://api.openai.com/v1"

[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`,
			wantID:     "acme-bedrock",
			wantURLSub: "bedrock-mantle.us-fake-1.api.aws",
		},
		{
			name:    "missing config file",
			missing: true,
			wantID:  "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.missing {
				path = filepath.Join(t.TempDir(), "does-not-exist", "config.toml")
			} else {
				path = writeCodexConfig(t, t.TempDir(), tc.config)
			}

			cand, err := discoverCodexProvider(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected ambiguity error, got nil (candidate=%+v)", cand)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cand.ID != tc.wantID {
				t.Errorf("provider id = %q, want %q", cand.ID, tc.wantID)
			}
			if tc.wantURLSub != "" && !strings.Contains(cand.BaseURL, tc.wantURLSub) {
				t.Errorf("base_url = %q, want substring %q", cand.BaseURL, tc.wantURLSub)
			}
		})
	}
}

// TestValidateCodexProviderID covers the character-class/length validator
// applied to providerID before it is interpolated into codex's -c
// model_providers.<id>.http_headers override syntax.
func TestValidateCodexProviderID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"valid lowercase slug", "acme-bedrock", true},
		{"valid mixed case with underscore", "Acme_Bedrock-2", true},
		{"valid at max length", strings.Repeat("a", maxCodexProviderIDLen), true},
		{"empty rejected", "", false},
		{"too long rejected", strings.Repeat("a", maxCodexProviderIDLen+1), false},
		{"dot rejected (TOML table separator)", "acme.bedrock", false},
		{"quote rejected", `acme"bedrock`, false},
		{"brace rejected (TOML inline table)", "acme}bedrock", false},
		{"equals rejected", "acme=bedrock", false},
		{"space rejected", "acme bedrock", false},
		{"newline rejected", "acme\nbedrock", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := validateCodexProviderID(tc.id)
			if got != tc.want {
				t.Errorf("validateCodexProviderID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestDiscoverCodexProjectHeader_FailureNeverBreaksInvoke covers the
// end-to-end DiscoverCodexProjectHeader entry point: discovery failure at
// any stage (no config, ambiguous config) must degrade to an empty pair,
// never an error the caller has to handle. It also proves projectID is
// override-only: unset means no header injection, set means the operator
// value passes through verbatim regardless of provider discovery outcome.
func TestDiscoverCodexProjectHeader_FailureNeverBreaksInvoke(t *testing.T) {
	t.Run("no codex config at all", func(t *testing.T) {
		dir := t.TempDir() // empty dir, no config.toml
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader("", "")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair with no config, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("ambiguous providers degrades to empty, not error", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"

[model_providers.other-provider]
base_url = "https://bedrock-mantle.eu-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader("", "")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair on ambiguous provider discovery, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("provider resolved, openai_project_id unset means header not injected", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader("", "")
		// Provider discovery has no dependency on a project id or api key;
		// projectID stays empty because it is override-only and nothing
		// was set. codex_cli.go requires both values non-empty to inject
		// the header, so an empty projectID alone suppresses injection.
		if providerID != "acme-bedrock" {
			t.Errorf("expected provider to resolve to acme-bedrock, got %q", providerID)
		}
		if projectID != "" {
			t.Errorf("expected empty project id when openai_project_id is unset, got %q", projectID)
		}
	})

	t.Run("provider discovered, openai_project_id set injects header with operator value verbatim", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader("", "operator-project-id")
		if providerID != "acme-bedrock" {
			t.Errorf("expected provider to resolve to acme-bedrock, got %q", providerID)
		}
		if projectID != "operator-project-id" {
			t.Errorf("expected operator project id to pass through verbatim, got %q", projectID)
		}
	})

	t.Run("explicit overrides bypass discovery entirely", func(t *testing.T) {
		dir := t.TempDir() // no config.toml — discovery would fail if it ran
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader("override-provider", "override-project")
		if providerID != "override-provider" || projectID != "override-project" {
			t.Errorf("expected overrides to pass through unchanged, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("invalid override providerID degrades to empty pair, not error", func(t *testing.T) {
		dir := t.TempDir() // no config.toml — discovery would fail if it ran
		t.Setenv("CODEX_HOME", dir)

		// An operator-set override is validated exactly like a
		// config.toml-discovered id: a value that would corrupt codex's -c
		// TOML-override syntax must degrade to feature-off, never be passed
		// through because it came from an override rather than discovery.
		providerID, projectID := DiscoverCodexProjectHeader(`evil"}.http_headers={"x"="y`, "override-project")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair for invalid override providerID, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("dotted discovered id rejected upstream by parseModelProviders, never reaches the validator", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers."acme.bedrock"]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		// parseModelProviders already rejects a "." inside a bare
		// [model_providers.<id>] header (see header-parsing loop: no "."
		// tolerated) — this proves that upstream rejection path lands on
		// the same safe (empty pair) outcome. It does NOT exercise
		// validateCodexProviderID: with no non-reserved candidate
		// surviving discoverCodexProvider, DiscoverCodexProjectHeader
		// returns feature-off before the validator is ever reached. See
		// the "+"-id case below for a fixture that genuinely reaches it.
		providerID, projectID := DiscoverCodexProjectHeader("", "")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("discovered id survives parseModelProviders but fails validateCodexProviderID", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers."acme+bedrock"]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		// "+" contains none of the characters parseModelProviders rejects
		// (it only rejects "."), and is stripped of nothing by the
		// quote-trim, so discoverCodexProvider returns candidate ID
		// "acme+bedrock" — a genuine non-reserved, single candidate. That
		// means DiscoverCodexProjectHeader's providerID != "" branch is
		// taken and validateCodexProviderID is the ONLY thing standing
		// between this id and being interpolated into codex's -c
		// override syntax: "+" is not in isCodexProviderIDChar's
		// alnum/hyphen/underscore class, so the validator must reject it.
		// (Deleting the validateCodexProviderID call in
		// DiscoverCodexProjectHeader would make this test fail, since
		// "acme+bedrock" would otherwise be returned unchanged.)
		providerID, projectID := DiscoverCodexProjectHeader("", "")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair (validator must reject \"acme+bedrock\"), got providerID=%q projectID=%q", providerID, projectID)
		}
	})
}
