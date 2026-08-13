// internal/backend/codex_discovery_test.go — table-driven tests for codex_cli's
// automatic provider/project discovery (lr-8dd85a).
//
// All ids below are fabricated placeholders, not real provider or project ids.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeRegion = "us-fake-1"

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

// TestMantleRegionFromBaseURL covers region extraction from the mantle host
// shape, and rejection of an unrelated base_url or an extracted region that
// fails character-class validation (e.g. an injected userinfo/path
// component landing between the anchored prefix/suffix).
func TestMantleRegionFromBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{"standard mantle url", "https://bedrock-mantle.us-fake-1.api.aws/v1", "us-fake-1"},
		{"mantle url no path", "https://bedrock-mantle.eu-fake-2.api.aws", "eu-fake-2"},
		{"unrelated provider url", "https://api.openai.com/v1", ""},
		{"empty", "", ""},
		{"region with uppercase rejected", "https://bedrock-mantle.US-FAKE-1.api.aws/v1", ""},
		{"region with at-sign rejected", "https://bedrock-mantle.evil@us-fake-1.api.aws/v1", ""},
		{"region with dot rejected", "https://bedrock-mantle.us.fake.1.api.aws/v1", ""},
		{"region with slash-like control char rejected", "https://bedrock-mantle.us-fake-1\t.api.aws/v1", ""},
		{"region too long rejected", "https://bedrock-mantle." + strings.Repeat("a", maxMantleRegionLen+1) + ".api.aws/v1", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := mantleRegionFromBaseURL(tc.baseURL)
			if got != tc.want {
				t.Errorf("mantleRegionFromBaseURL(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestValidateMantleRegion covers the character-class/length validator in
// isolation, including boundary lengths and each rejected character class.
func TestValidateMantleRegion(t *testing.T) {
	cases := []struct {
		name   string
		region string
		want   bool
	}{
		{"valid lowercase alnum hyphen", "us-fake-1", true},
		{"valid at max length", strings.Repeat("a", maxMantleRegionLen), true},
		{"empty rejected", "", false},
		{"too long rejected", strings.Repeat("a", maxMantleRegionLen+1), false},
		{"uppercase rejected", "US-FAKE-1", false},
		{"dot rejected", "us.fake.1", false},
		{"underscore rejected", "us_fake_1", false},
		{"at-sign rejected", "us-fake-1@evil", false},
		{"slash rejected", "us-fake-1/../other", false},
		{"space rejected", "us fake 1", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := validateMantleRegion(tc.region)
			if got != tc.want {
				t.Errorf("validateMantleRegion(%q) = %v, want %v", tc.region, got, tc.want)
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

// fakeProjectsServer returns an httptest.Server that responds to
// GET /v1/organization/projects with the given JSON body and status.
func fakeProjectsServer(t *testing.T, status int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing Authorization header on request to %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
}

// TestDiscoverCodexProject_ViaFakeServer covers one/multiple/empty project
// list responses and an HTTP failure, using discoverCodexProjectAt (the
// production URL-taking helper discoverCodexProject itself delegates to),
// pointed at an httptest server since discoverCodexProject's public entry
// point hardcodes the bedrock-mantle host shape from a region string, not
// an arbitrary URL.
func TestDiscoverCodexProject_ViaFakeServer(t *testing.T) {
	const fakeAPIKey = "fake-bearer-key-0000"

	t.Run("single project", func(t *testing.T) {
		srv := fakeProjectsServer(t, http.StatusOK, bedrockProjectsResponse{
			Data: []bedrockProject{{ID: "proj_fake_solo", Name: "solo", Status: "active"}},
		})
		defer srv.Close()

		id, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "proj_fake_solo" {
			t.Errorf("project id = %q, want proj_fake_solo", id)
		}
	})

	t.Run("multiple projects prefers default-named", func(t *testing.T) {
		srv := fakeProjectsServer(t, http.StatusOK, bedrockProjectsResponse{
			Data: []bedrockProject{
				{ID: "proj_fake_other", Name: "other", Status: "active"},
				{ID: "proj_fake_default", Name: defaultBedrockProjectName, Status: "active"},
			},
		})
		defer srv.Close()

		id, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "proj_fake_default" {
			t.Errorf("project id = %q, want proj_fake_default (deterministic default-name preference)", id)
		}
	})

	t.Run("multiple projects no default is ambiguous", func(t *testing.T) {
		srv := fakeProjectsServer(t, http.StatusOK, bedrockProjectsResponse{
			Data: []bedrockProject{
				{ID: "proj_fake_a", Name: "a", Status: "active"},
				{ID: "proj_fake_b", Name: "b", Status: "active"},
			},
		})
		defer srv.Close()

		_, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err == nil {
			t.Fatal("expected ambiguity error, got nil")
		}
	})

	t.Run("empty project list", func(t *testing.T) {
		srv := fakeProjectsServer(t, http.StatusOK, bedrockProjectsResponse{Data: nil})
		defer srv.Close()

		id, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "" {
			t.Errorf("project id = %q, want empty for zero projects", id)
		}
	})

	t.Run("HTTP failure", func(t *testing.T) {
		srv := fakeProjectsServer(t, http.StatusInternalServerError, nil)
		defer srv.Close()

		_, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err == nil {
			t.Fatal("expected error on HTTP 500, got nil")
		}
	})

	t.Run("oversized response body rejected without parsing", func(t *testing.T) {
		// A hostile or malfunctioning endpoint returning an arbitrarily
		// large body must be rejected via the io.LimitReader cap, never
		// buffered in full — see maxBedrockProjectsResponseBytes.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Valid JSON prefix, then padding well past the cap so a naive
			// decoder streaming the body would still succeed if the cap
			// were absent.
			_, _ = w.Write([]byte(`{"data":[`))
			pad := strings.Repeat("0", maxBedrockProjectsResponseBytes+1024)
			_, _ = w.Write([]byte(pad))
		}))
		defer srv.Close()

		_, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err == nil {
			t.Fatal("expected error for oversized response body, got nil")
		}
		if !strings.Contains(err.Error(), "byte cap") {
			t.Errorf("expected byte-cap error, got: %v", err)
		}
	})

	t.Run("response exactly at cap is accepted", func(t *testing.T) {
		// Boundary check: a response landing exactly at the cap (not over
		// it) must still be read and parsed successfully — the +1 sentinel
		// read must not itself cause a false rejection at the boundary.
		project := bedrockProject{ID: "proj_fake_boundary", Name: "boundary", Status: "active"}
		encoded, err := json.Marshal(bedrockProjectsResponse{Data: []bedrockProject{project}})
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if len(encoded) >= maxBedrockProjectsResponseBytes {
			t.Fatalf("fixture body (%d bytes) is not smaller than the cap (%d) — adjust test", len(encoded), maxBedrockProjectsResponseBytes)
		}
		// Pad with JSON whitespace (valid, ignored by encoding/json) up to
		// exactly the cap.
		padded := append(encoded, bytes.Repeat([]byte(" "), maxBedrockProjectsResponseBytes-len(encoded))...)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(padded)
		}))
		defer srv.Close()

		id, err := discoverCodexProjectAt(context.Background(), srv.Client(), srv.URL+"/v1/organization/projects", fakeAPIKey)
		if err != nil {
			t.Fatalf("unexpected error at exactly-cap body size: %v", err)
		}
		if id != "proj_fake_boundary" {
			t.Errorf("project id = %q, want proj_fake_boundary", id)
		}
	})
}

// TestDiscoverCodexProject_RealFunctionHTTPFailure exercises the real
// discoverCodexProject (region-based URL construction) against an
// unreachable region string to confirm the failure path returns an error
// rather than panicking, and that region/apiKey validation short-circuits
// before any network call.
func TestDiscoverCodexProject_RealFunctionHTTPFailure(t *testing.T) {
	t.Run("empty region", func(t *testing.T) {
		_, err := discoverCodexProject(context.Background(), http.DefaultClient, "", "fake-key")
		if err == nil {
			t.Fatal("expected error for empty region, got nil")
		}
	})

	t.Run("empty api key", func(t *testing.T) {
		_, err := discoverCodexProject(context.Background(), http.DefaultClient, fakeRegion, "")
		if err == nil {
			t.Fatal("expected error for empty api key, got nil")
		}
	})
}

// TestDiscoverCodexProjectHeader_FailureNeverBreaksInvoke covers the
// end-to-end DiscoverCodexProjectHeader entry point: discovery failure at
// any stage (no config, ambiguous config, no api key) must degrade to an
// empty pair, never an error the caller has to handle.
func TestDiscoverCodexProjectHeader_FailureNeverBreaksInvoke(t *testing.T) {
	t.Run("no codex config at all", func(t *testing.T) {
		dir := t.TempDir() // empty dir, no config.toml
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "fake-key")
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

		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "fake-key")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair on ambiguous provider discovery, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("provider resolved but no api key for project lookup", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers.acme-bedrock]
base_url = "https://bedrock-mantle.us-fake-1.api.aws/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "")
		// Provider discovery succeeds independently of project discovery;
		// projectID stays empty (no api key for the live lookup), which
		// alone suppresses codex_cli.go's header injection (it requires
		// both to be non-empty) — so this is still functionally feature-off.
		if providerID != "acme-bedrock" {
			t.Errorf("expected provider to resolve to acme-bedrock even without api key, got %q", providerID)
		}
		if projectID != "" {
			t.Errorf("expected empty project id with no api key, got %q", projectID)
		}
	})

	t.Run("explicit overrides bypass discovery entirely", func(t *testing.T) {
		dir := t.TempDir() // no config.toml — discovery would fail if it ran
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "override-provider", "override-project", "")
		if providerID != "override-provider" || projectID != "override-project" {
			t.Errorf("expected overrides to pass through unchanged, got providerID=%q projectID=%q", providerID, projectID)
		}
	})

	t.Run("provider not pointed at mantle endpoint yields empty project without network call", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, `
[model_providers.acme-other]
base_url = "https://not-bedrock-mantle.example.com/v1"
`)
		t.Setenv("CODEX_HOME", dir)

		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "fake-key")
		if providerID != "acme-other" {
			t.Errorf("expected provider to resolve to acme-other, got %q", providerID)
		}
		if projectID != "" {
			t.Errorf("expected empty project id when base_url isn't a mantle endpoint, got %q", projectID)
		}
	})

	t.Run("invalid override providerID degrades to empty pair, not error", func(t *testing.T) {
		dir := t.TempDir() // no config.toml — discovery would fail if it ran
		t.Setenv("CODEX_HOME", dir)

		// An operator-set override is validated exactly like a
		// config.toml-discovered id: a value that would corrupt codex's -c
		// TOML-override syntax must degrade to feature-off, never be passed
		// through because it came from an override rather than discovery.
		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), `evil"}.http_headers={"x"="y`, "override-project", "")
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
		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "fake-key")
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
		providerID, projectID := DiscoverCodexProjectHeader(context.Background(), "", "", "fake-key")
		if providerID != "" || projectID != "" {
			t.Errorf("expected empty pair (validator must reject \"acme+bedrock\"), got providerID=%q projectID=%q", providerID, projectID)
		}
	})
}
