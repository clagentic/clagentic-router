// internal/server/chat_completions_tools_test.go — tests for the
// tool-capability refusal path on POST /v1/chat/completions (lr-be9454).
//
// A tools-bearing request against a chain with no tool-capable backend must
// return an explicit, actionable error — never a 200 with tools silently
// stripped.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

func doChatCompletionsRequest(t *testing.T, ts *httptest.Server, token string, body map[string]interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(raw))
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

// newChatCompletionsTestServer builds a Server with one backend registered
// directly under its own ID (no chain/tier indirection needed for
// chatCompletions — "backend:<id>" resolves straight to it).
func newChatCompletionsTestServer(t *testing.T, supportsTools bool) (*httptest.Server, func()) {
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
		"test-backend": &stubAdapter{id: "test-backend", supportsTools: supportsTools},
	}
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

func TestChatCompletions_ToolsWithNoCapableBackend_Returns422(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}

	var errBody errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Code != "no_tool_capable_backend" {
		t.Errorf("error.code: want no_tool_capable_backend, got %q", errBody.Error.Code)
	}
}

func TestChatCompletions_ToolsWithCapableBackend_Succeeds(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, true)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestChatCompletions_NoTools_RoutesNormallyToIncapableBackend(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	// No tools field at all — must not trigger the capability filter, must
	// route exactly as before this change.
	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestChatCompletions_EmptyToolsArray_NotTreatedAsToolsPresent(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{},
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

// --- GET /v1/models: capability pre-flight discovery (lr-be9454) ---

func TestModels_ExposesCapabilities(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, true)
	defer cleanup()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID           string `json:"id"`
			Capabilities struct {
				SupportsTools     bool `json:"supports_tools"`
				SupportsStreaming bool `json:"supports_streaming"`
				SupportsImages    bool `json:"supports_images"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 model entry, got %d", len(body.Data))
	}
	if body.Data[0].ID != "test-backend" {
		t.Errorf("id: want test-backend, got %q", body.Data[0].ID)
	}
	if !body.Data[0].Capabilities.SupportsTools {
		t.Error("capabilities.supports_tools: want true (pre-flight discovery must reflect the adapter's declared capability)")
	}
}
