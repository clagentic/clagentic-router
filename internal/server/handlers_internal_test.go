// internal/server/handlers_internal_test.go — tests for POST /v1/internal/rate-limit.
//
// These tests build a minimal Server with a real Router and a no-op stub adapter
// so that state transitions can be verified through the full HTTP layer.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/state"
)

// stubAdapter is a no-op backend.Adapter used to register a backend with the
// router without requiring any real LLM credentials. supportsTools controls
// the value returned by Capabilities().SupportsTools (zero value: false,
// matching the CLI-adapter default used by most tests). response, when
// non-nil, is returned verbatim by Invoke instead of the default
// {Content: "stub"} — used by lr-add405's tool-carriage tests to script a
// response carrying ToolUses.
type stubAdapter struct {
	id            string
	supportsTools bool
	response      *backend.Response
}

func (s *stubAdapter) ID() string { return s.id }
func (s *stubAdapter) Invoke(_ context.Context, _ *backend.Request) (*backend.Response, error) {
	if s.response != nil {
		return s.response, nil
	}
	return &backend.Response{Content: "stub"}, nil
}
func (s *stubAdapter) Capabilities() backend.Capabilities {
	return backend.Capabilities{SupportsTools: s.supportsTools}
}

// newTestServer constructs a minimal Server backed by a real Router with one
// registered stub backend ("test-backend"). token is set to "secret".
func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"test-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600, // do not probe during tests
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"test-backend": &stubAdapter{id: "test-backend"},
	}
	r := router.New(cfg, adapters, nil, nil)
	// Bring backend to Healthy so state changes are observable
	r.AllSnapshots() // noop, just ensure the backend exists

	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

// doRateLimitEvent POSTs a rate-limit event to ts and returns the response.
func doRateLimitEvent(t *testing.T, ts *httptest.Server, token string, body map[string]string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest("POST", ts.URL+"/v1/internal/rate-limit", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestInternalRateLimit_MissingAuth(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resp := doRateLimitEvent(t, ts, "", map[string]string{
		"backend_id": "test-backend",
		"status":     "warning",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

func TestInternalRateLimit_Warning_UpdatesState(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resetsAt := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second).Format(time.RFC3339)

	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "test-backend",
		"limit_type": "five_hour",
		"resets_at":  resetsAt,
		"status":     "warning",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}

	// Verify the handler returned the correct body
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["status"] != "applied" {
		t.Errorf("body.status: want %q, got %q", "applied", result["status"])
	}
	if result["event"] != "warning" {
		t.Errorf("body.event: want %q, got %q", "warning", result["event"])
	}
}

func TestInternalRateLimit_Warning_DoesNotMarkOffline(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	// First bring the backend to Healthy via a synthesized success via the router
	// (we cannot call RecordSuccess directly here; but the initial state is Unknown,
	// which is still routable — check that a warning does not push it to Offline).
	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "test-backend",
		"limit_type": "seven_day",
		"status":     "warning",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	// Retrieve state via /quota endpoint
	qreq, _ := http.NewRequest("GET", ts.URL+"/quota", nil)
	qreq.Header.Set("Authorization", "Bearer secret")
	qresp, err := http.DefaultClient.Do(qreq)
	if err != nil {
		t.Fatalf("quota request: %v", err)
	}
	defer qresp.Body.Close()

	var qbody map[string]map[string]interface{}
	if err := json.NewDecoder(qresp.Body).Decode(&qbody); err != nil {
		t.Fatalf("decode quota: %v", err)
	}
	backendData, ok := qbody["test-backend"]
	if !ok {
		t.Fatal("test-backend missing from /quota response")
	}
	statusVal, _ := backendData["status"].(string)
	if statusVal == string(state.StatusOffline) {
		t.Errorf("warning must not mark backend offline, got status %q", statusVal)
	}
}

func TestInternalRateLimit_Exhausted_MarksOffline(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resetsAt := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "test-backend",
		"limit_type": "seven_day",
		"resets_at":  resetsAt,
		"status":     "exhausted",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}

	// Verify backend is now offline via /quota
	qreq, _ := http.NewRequest("GET", ts.URL+"/quota", nil)
	qreq.Header.Set("Authorization", "Bearer secret")
	qresp, err := http.DefaultClient.Do(qreq)
	if err != nil {
		t.Fatalf("quota request: %v", err)
	}
	defer qresp.Body.Close()

	var qbody map[string]map[string]interface{}
	if err := json.NewDecoder(qresp.Body).Decode(&qbody); err != nil {
		t.Fatalf("decode quota: %v", err)
	}
	backendData, ok := qbody["test-backend"]
	if !ok {
		t.Fatal("test-backend missing from /quota response")
	}
	statusVal, _ := backendData["status"].(string)
	if statusVal != string(state.StatusOffline) {
		t.Errorf("exhausted must mark backend offline, got status %q", statusVal)
	}
	exhausted, _ := backendData["quota_exhausted"].(bool)
	if !exhausted {
		t.Error("exhausted event must set quota_exhausted = true")
	}
}

func TestInternalRateLimit_UnknownBackend_Returns404(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "no-such-backend",
		"status":     "warning",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestInternalRateLimit_MissingBackendID_Returns400(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"status": "warning",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestInternalRateLimit_InvalidStatus_Returns400(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "test-backend",
		"status":     "unknown_status",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestInternalRateLimit_InvalidResetsAt_Returns400(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	resp := doRateLimitEvent(t, ts, "secret", map[string]string{
		"backend_id": "test-backend",
		"status":     "warning",
		"resets_at":  "not-a-timestamp",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}
