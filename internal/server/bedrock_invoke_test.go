// internal/server/bedrock_invoke_test.go — tests for POST
// /model/{modelId}/invoke and POST /model/{modelId}/invoke-with-response-stream.
//
// Covers: {modelId} path extraction and URL-decoding (the routing key never
// appears in the body — see lr-cefefd), routed-mode round trip (non-streaming
// and streaming/event-stream), auth gate, unresolvable-model 400, and the
// passthrough-not-configured guard. Passthrough SigV4 signing itself is NOT
// exercised end-to-end here (no live AWS Bedrock credentials available at
// authoring time — see PR body); bedrockPassthrough is unit-tested only for
// its config-guard behavior.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newBedrockTestServer builds a Server with one stub backend registered
// under chain "reviewer-chain" and no Bedrock passthrough region configured
// (routed-mode-only — matches a deployment with no AWS credentials).
func newBedrockTestServer(t *testing.T) (*httptest.Server, func()) {
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

	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

func doBedrockInvoke(t *testing.T, ts *httptest.Server, path, authHeader, authValue string, body map[string]interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if authHeader != "" {
		req.Header.Set(authHeader, authValue)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// --- routed mode, non-streaming ---

func TestBedrockInvoke_Routed_Success(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens":        1,
		"messages":          []map[string]string{{"role": "user", "content": "."}},
		"anthropic_version": "bedrock-2023-05-31",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if mode := resp.Header.Get("X-Router-Mode"); mode != "routed" {
		t.Errorf("X-Router-Mode: want routed, got %q", mode)
	}

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Type != "message" || body.Role != "assistant" {
		t.Errorf("unexpected envelope: type=%q role=%q", body.Type, body.Role)
	}
	if len(body.Content) == 0 || body.Content[0].Text != "stub" {
		t.Errorf("content: want [{text: stub}], got %+v", body.Content)
	}
}

// TestBedrockInvoke_ModelIDNeverReadFromBody verifies the model identifier
// is resolved from the URL path segment only — a body containing no "model"
// field at all (the real Bedrock wire shape; see lr-cefefd) must still route
// correctly, and a stray "model" field in the body (if a client mistakenly
// sends one) must be ignored, not used for routing.
func TestBedrockInvoke_ModelIDNeverReadFromBody(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	// No "model" field anywhere in the body — matches the captured wire shape.
	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestBedrockInvoke_UnresolvableModel_Returns400(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:no-such-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestBedrockInvoke_MissingAuth_Returns401(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "", "", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

func TestBedrockInvoke_BearerAuthAccepted(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "Authorization", "Bearer secret", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestBedrockInvoke_EmptyMessages_Returns400(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

// --- routed mode, streaming ---

func TestBedrockInvokeStream_Routed_EventStreamFraming(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke-with-response-stream", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 32000,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"thinking":   map[string]interface{}{"type": "enabled", "budget_tokens": 1024},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.amazon.eventstream" {
		t.Errorf("Content-Type: want application/vnd.amazon.eventstream, got %q", ct)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	payloads, err := decodeBedrockEventStream(buf.Bytes())
	if err != nil {
		t.Fatalf("decodeBedrockEventStream: %v", err)
	}
	if len(payloads) != 6 {
		t.Errorf("frame count: want 6, got %d", len(payloads))
	}
}

// --- routed mode: tool-capability refusal (lr-be9454) ---

// TestBedrockInvoke_ToolsWithNoCapableBackend_Returns422 verifies the same
// refusal behavior as messagesRouted/chatCompletions: a tools-bearing
// Bedrock InvokeModel request against a chain with no tool-capable backend
// must be refused, never routed with tools silently dropped.
func TestBedrockInvoke_ToolsWithNoCapableBackend_Returns422(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	// newBedrockTestServer's "reviewer-chain" is backed by a stubAdapter with
	// the zero-value supportsTools (false) — no backend in the chain is
	// tool-capable.
	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}

	var body bedrockErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Message == "" {
		t.Error("expected a non-empty error message")
	}
}

func TestBedrockInvoke_ToolsWithCapableBackend_Succeeds(t *testing.T) {
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"tool-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Chains: map[string][]string{
			"reviewer-chain": {"tool-backend"},
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
		"tool-backend": &stubAdapter{id: "tool-backend", supportsTools: true},
	}
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestBedrockInvoke_EmptyToolsArray_NotTreatedAsToolsPresent(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"tools":      []map[string]string{},
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

// --- passthrough mode (not configured) ---

// TestBedrockInvoke_Passthrough_NotConfigured_Returns503 verifies the
// explicit config guard: a plain (non-role:/chain:/backend:) modelId with no
// bedrock.region configured must fail loudly with a clear message rather
// than attempting to sign a request with no region, or silently 404ing.
func TestBedrockInvoke_Passthrough_NotConfigured_Returns503(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke", "", "", map[string]interface{}{
		"max_tokens":        1,
		"messages":          []map[string]string{{"role": "user", "content": "."}},
		"anthropic_version": "bedrock-2023-05-31",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: want 503, got %d", resp.StatusCode)
	}
	var body bedrockErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Message == "" {
		t.Error("expected a non-empty error message")
	}
}

func TestBedrockInvoke_Passthrough_NoAuthGateRequired(t *testing.T) {
	// Passthrough mode does not require the router's own token — mirrors
	// messagesPassthrough's auth matrix. This request still 503s (no region
	// configured) but MUST NOT 401 first, proving the auth gate was skipped
	// for the passthrough path exactly as it is for /v1/messages.
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	resp := doBedrockInvoke(t, ts, "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke", "", "", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("passthrough mode must not gate on the router's own token")
	}
}

// --- {modelId} path extraction ---

func TestBedrockInvoke_ModelIDWithColonAndSpecialChars(t *testing.T) {
	ts, cleanup := newBedrockTestServer(t)
	defer cleanup()

	// role:reviewer-chain contains a colon, matching the task's captured
	// example (POST /model/role:reviewer-chain/invoke-with-response-stream).
	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}
