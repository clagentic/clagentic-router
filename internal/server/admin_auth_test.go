// internal/server/admin_auth_test.go — tests for admin token role separation.
//
// Verifies that when adminToken differs from the inference token:
//   - Inference endpoints accept the inference token and reject the admin token.
//   - Admin endpoints accept the admin token and reject the inference token.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newSplitTokenServer builds a Server where inference and admin tokens differ.
func newSplitTokenServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"test-backend": {Adapter: "stub", CostWeight: 1.0},
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
		"test-backend": &stubAdapter{id: "test-backend"},
	}
	r := router.New(cfg, adapters, nil, nil)

	inferenceToken := "inference-token"
	adminToken := "admin-token-separate"

	srv := New(":0", inferenceToken, adminToken, r, nil)
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

func doGet(t *testing.T, ts *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestAdminTokenSplit_InferenceAcceptsInferenceToken verifies inference endpoints
// accept the inference token when tokens are separate.
func TestAdminTokenSplit_InferenceAcceptsInferenceToken(t *testing.T) {
	ts, cleanup := newSplitTokenServer(t)
	defer cleanup()

	// GET /v1/models uses h.auth (inference token)
	resp := doGet(t, ts, "/v1/models", "inference-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models with inference token: want 200, got %d", resp.StatusCode)
	}
}

// TestAdminTokenSplit_InferenceRejectsAdminToken verifies inference endpoints
// reject the admin token when tokens are separate.
func TestAdminTokenSplit_InferenceRejectsAdminToken(t *testing.T) {
	ts, cleanup := newSplitTokenServer(t)
	defer cleanup()

	resp := doGet(t, ts, "/v1/models", "admin-token-separate")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/models with admin token: want 401, got %d", resp.StatusCode)
	}
}

// TestAdminTokenSplit_AdminAcceptsAdminToken verifies admin endpoints accept
// the admin token when tokens are separate.
func TestAdminTokenSplit_AdminAcceptsAdminToken(t *testing.T) {
	ts, cleanup := newSplitTokenServer(t)
	defer cleanup()

	resp := doGet(t, ts, "/health", "admin-token-separate")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health with admin token: want 200, got %d", resp.StatusCode)
	}
}

// TestAdminTokenSplit_AdminRejectsInferenceToken verifies admin endpoints
// reject the inference token when tokens are separate.
func TestAdminTokenSplit_AdminRejectsInferenceToken(t *testing.T) {
	ts, cleanup := newSplitTokenServer(t)
	defer cleanup()

	resp := doGet(t, ts, "/health", "inference-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /health with inference token: want 401, got %d", resp.StatusCode)
	}
}

// TestAdminTokenSplit_AdminRejectsMissingAuth verifies admin endpoints reject
// unauthenticated requests when tokens are set.
func TestAdminTokenSplit_AdminRejectsMissingAuth(t *testing.T) {
	ts, cleanup := newSplitTokenServer(t)
	defer cleanup()

	resp := doGet(t, ts, "/health", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /health with no token: want 401, got %d", resp.StatusCode)
	}
}

// TestAdminTokenSame_BothEndpointsAcceptSameToken verifies that when inference
// and admin tokens are identical (the default migration path), all endpoints
// accept the same credential.
func TestAdminTokenSame_BothEndpointsAcceptSameToken(t *testing.T) {
	ts, cleanup := newTestServer(t) // newTestServer uses "secret" for both
	defer cleanup()

	// Inference endpoint
	resp := doGet(t, ts, "/v1/models", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models with shared token: want 200, got %d", resp.StatusCode)
	}

	// Admin endpoint
	resp2 := doGet(t, ts, "/health", "secret")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("GET /health with shared token: want 200, got %d", resp2.StatusCode)
	}
}
