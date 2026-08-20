// internal/server/cache_metrics_test.go — tests for GET <cache_metrics.path>
// (lr-718af0): the route must not exist at all when the feature is
// disabled (the config default), and must expose Prometheus-format
// per-(backend,model) aggregates when enabled.
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/store"
)

func newCacheMetricsTestServer(t *testing.T, enabled bool, path string) (*httptest.Server, *store.Store) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"test-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"test-backend": &stubAdapter{id: "test-backend"},
	}
	r := router.New(cfg, adapters, st, nil)

	srv := New(":0", "secret", "secret", false, r, st, "https://api.anthropic.com", "", "", "", enabled, path)
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts, st
}

// TestCacheMetrics_DisabledRouteNotRegistered verifies that with the feature
// disabled (the config default), GET /metrics/cache returns 404 — the route
// is never registered at all, not merely gated to an empty body. This
// matches the "unconfigured install has no new attack surface" contract in
// server.go's New doc.
func TestCacheMetrics_DisabledRouteNotRegistered(t *testing.T) {
	ts, _ := newCacheMetricsTestServer(t, false, "")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics/cache", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route must not be registered when disabled)", resp.StatusCode)
	}
}

// TestCacheMetrics_EnabledDefaultPath verifies the default path
// (/metrics/cache) is used when cache_metrics.path is left empty, and that
// the exposition format includes the expected metric names for a recorded
// aggregate.
func TestCacheMetrics_EnabledDefaultPath(t *testing.T) {
	ts, st := newCacheMetricsTestServer(t, true, "")

	st.RecordCacheUsage(context.Background(), "test-backend", store.CacheUsageInput{
		Model: "claude-opus-4-8", Reported: true,
		InputTokens: 100, CacheReadTokens: 70, CacheWriteTokens: 30,
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics/cache", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	for _, want := range []string{
		`router_cache_input_tokens_total{backend="test-backend",model="claude-opus-4-8"} 100`,
		`router_cache_read_tokens_total{backend="test-backend",model="claude-opus-4-8"} 70`,
		`router_cache_write_tokens_total{backend="test-backend",model="claude-opus-4-8"} 30`,
		`router_cache_calls_reported{backend="test-backend",model="claude-opus-4-8"} 1`,
		`router_cache_calls_unsupported{backend="test-backend",model="claude-opus-4-8"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected exposition to contain %q, got:\n%s", want, text)
		}
	}
}

// TestCacheMetrics_ConfiguredPath verifies a non-default path is honored.
func TestCacheMetrics_ConfiguredPath(t *testing.T) {
	ts, _ := newCacheMetricsTestServer(t, true, "/custom/cache-path")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/custom/cache-path", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 at configured path", resp.StatusCode)
	}

	// The default path must NOT also be registered once a custom path is set.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics/cache", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("default path status = %d, want 404 when a custom path is configured", resp2.StatusCode)
	}
}

// TestCacheMetrics_AdminAuthRequired verifies the endpoint is gated by the
// admin token, matching every other observability route (GET /metrics,
// /logs, /stats).
func TestCacheMetrics_AdminAuthRequired(t *testing.T) {
	ts, _ := newCacheMetricsTestServer(t, true, "")

	resp, err := http.Get(ts.URL + "/metrics/cache")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a bearer token", resp.StatusCode)
	}
}
