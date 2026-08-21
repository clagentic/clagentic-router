// internal/server/auth_fail_closed_test.go — regression tests for lr-7a26e0:
// empty-token auth middlewares must fail CLOSED (401) unless allowNoAuth is
// explicitly set, and honor pass-through only when it is.
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newEmptyTokenServer builds a Server with an empty inference token and an
// empty admin token, with allowNoAuth set as requested.
func newEmptyTokenServer(t *testing.T, allowNoAuth bool) (*httptest.Server, func()) {
	t.Helper()

	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"test-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Chains: map[string][]string{
			"reviewer-chain": {"test-backend"},
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

	srv := New(":0", "", "", allowNoAuth, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

// --- auth() (inference token) ---

func TestAuth_EmptyToken_AllowNoAuthFalse_Rejects401(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, false)
	defer cleanup()

	resp := doGet(t, ts, "/v1/models", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

func TestAuth_EmptyToken_AllowNoAuthTrue_PassesThrough(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, true)
	defer cleanup()

	resp := doGet(t, ts, "/v1/models", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

// --- anthropicTokenPresented() (routed /v1/messages) ---

func TestAnthropicTokenPresented_EmptyToken_AllowNoAuthFalse_Rejects401(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, false)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "", "", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

func TestAnthropicTokenPresented_EmptyToken_AllowNoAuthTrue_PassesThrough(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, true)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "", "", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

// --- adminAuth() ---

func TestAdminAuth_EmptyToken_AllowNoAuthFalse_Rejects401(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, false)
	defer cleanup()

	resp := doGet(t, ts, "/health", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

func TestAdminAuth_EmptyToken_AllowNoAuthTrue_PassesThrough(t *testing.T) {
	ts, cleanup := newEmptyTokenServer(t, true)
	defer cleanup()

	resp := doGet(t, ts, "/health", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}
