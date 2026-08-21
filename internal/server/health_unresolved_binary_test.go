// internal/server/health_unresolved_binary_test.go — regression test for
// lr-92ee18 B2: GET /health must report something other than plain "ok"
// when a configured backend's CLI binary never resolved at startup, and
// must name which backend(s) via unresolved_binaries. Before this fix, a
// backend in this state (WARN-only at startup, no runtime surface) was
// completely invisible to /health, which is the daemon's primary automated
// health-check surface.
package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// binaryCheckerStubAdapter is a no-op backend.Adapter that also implements
// backend.BinaryChecker, mirroring the real CLI adapters' shape without any
// subprocess-spawning behavior.
type binaryCheckerStubAdapter struct {
	id       string
	resolved bool
}

func (s *binaryCheckerStubAdapter) ID() string { return s.id }
func (s *binaryCheckerStubAdapter) Invoke(_ context.Context, _ *backend.Request) (*backend.Response, error) {
	return &backend.Response{Content: "stub"}, nil
}
func (s *binaryCheckerStubAdapter) Capabilities() backend.Capabilities {
	return backend.Capabilities{}
}
func (s *binaryCheckerStubAdapter) BinaryResolved() bool { return s.resolved }

func TestHealth_UnresolvedBinary_ReportsDegradedAndNamesBackend(t *testing.T) {
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"broken-cli": {Adapter: config.AdapterClaudeCLI, CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"broken-cli": &binaryCheckerStubAdapter{id: "broken-cli", resolved: false},
	}
	r := router.New(cfg, adapters, nil, nil)

	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test-rev")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doGet(t, ts, "/health", "secret")
	defer resp.Body.Close()

	var body struct {
		Status             string   `json:"status"`
		Version            string   `json:"version"`
		UnresolvedBinaries []string `json:"unresolved_binaries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}

	if body.Status == "ok" {
		t.Errorf("status = %q, want something other than \"ok\" when a configured backend's binary is unresolved", body.Status)
	}
	if len(body.UnresolvedBinaries) != 1 || body.UnresolvedBinaries[0] != "broken-cli" {
		t.Errorf("unresolved_binaries = %v, want [\"broken-cli\"]", body.UnresolvedBinaries)
	}
	if body.Version != "test-rev" {
		t.Errorf("version = %q, want %q", body.Version, "test-rev")
	}
}

func TestHealth_AllBinariesResolved_ReportsOK(t *testing.T) {
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"working-cli": {Adapter: config.AdapterClaudeCLI, CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"working-cli": &binaryCheckerStubAdapter{id: "working-cli", resolved: true},
	}
	r := router.New(cfg, adapters, nil, nil)

	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test-rev")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doGet(t, ts, "/health", "secret")
	defer resp.Body.Close()

	var body struct {
		Status             string   `json:"status"`
		UnresolvedBinaries []string `json:"unresolved_binaries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}

	if body.Status != "ok" {
		t.Errorf("status = %q, want \"ok\" when every backend's binary resolved", body.Status)
	}
	if len(body.UnresolvedBinaries) != 0 {
		t.Errorf("unresolved_binaries = %v, want empty", body.UnresolvedBinaries)
	}
}
