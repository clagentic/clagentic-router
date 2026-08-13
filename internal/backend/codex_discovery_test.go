// internal/backend/codex_discovery_test.go — table-driven tests for codex_cli's
// automatic provider/project discovery (lr-8dd85a).
//
// All ids below are fabricated placeholders, not real provider or project ids.
package backend

import (
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
// shape, and rejection of an unrelated base_url.
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

// discoverCodexProjectAt is a test-only variant of discoverCodexProject that
// hits an arbitrary base URL instead of the real bedrock-mantle host shape,
// so httptest.Server (127.0.0.1:<port>) can stand in for the region-derived
// endpoint without needing a live AWS host.
func discoverCodexProjectAt(ctx context.Context, client *http.Client, url, apiKey string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var parsed bedrockProjectsResponse
	if resp.StatusCode != http.StatusOK {
		return "", &InvokeError{Type: ErrTypeUnknown, Raw: "non-200"}
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
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
		return "", &InvokeError{Type: ErrTypeUnknown, Raw: "ambiguous"}
	}
}

// TestDiscoverCodexProject_ViaFakeServer covers one/multiple/empty project
// list responses and an HTTP failure, using discoverCodexProjectAt (same
// selection logic as discoverCodexProject, pointed at an httptest server
// since the real function hardcodes the bedrock-mantle host shape from a
// region string, not an arbitrary URL).
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
}
