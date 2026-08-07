// internal/server/messages_test.go — tests for POST /v1/messages.
//
// Covers: passthrough proxying (including streamed SSE), role routing
// round-trip, auth matrix (x-api-key and Bearer), and unknown-role 400.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newMessagesTestServer builds a Server with one stub backend registered
// under chain "reviewer-chain", plus an upstream passthrough target pointed
// at a local httptest server (upstream) so passthrough tests don't hit the
// network.
func newMessagesTestServer(t *testing.T, upstreamURL string) (*httptest.Server, func()) {
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

	srv := New(":0", "secret", "secret", false, r, nil, upstreamURL, "", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

func doMessagesRequest(t *testing.T, ts *httptest.Server, authHeader, authValue string, body map[string]interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set(authHeader, authValue)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// --- Auth matrix ---

func TestMessages_Auth_XAPIKeyAccepted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer upstream.Close()

	ts, cleanup := newMessagesTestServer(t, upstream.URL)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMessages_Auth_BearerAccepted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer upstream.Close()

	ts, cleanup := newMessagesTestServer(t, upstream.URL)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "Authorization", "Bearer secret", map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMessages_Auth_MissingRejected401(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
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

	var errBody anthropicMsgError
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Type != "authentication_error" {
		t.Errorf("error.type: want authentication_error, got %q", errBody.Error.Type)
	}
}

func TestMessages_Auth_WrongTokenRejected401(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "wrong-token", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: want 401, got %d", resp.StatusCode)
	}
}

// --- Routed mode ---

func TestMessages_Routed_RoleChainRoundTrip(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "review this"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if mode := resp.Header.Get("X-Router-Mode"); mode != "routed" {
		t.Errorf("X-Router-Mode: want routed, got %q", mode)
	}
	if bid := resp.Header.Get("X-Router-Backend"); bid != "test-backend" {
		t.Errorf("X-Router-Backend: want test-backend, got %q", bid)
	}

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Type != "message" {
		t.Errorf("type: want message, got %q", body.Type)
	}
	if body.Role != "assistant" {
		t.Errorf("role: want assistant, got %q", body.Role)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "text" || body.Content[0].Text != "stub" {
		t.Errorf("content: want single text block %q, got %+v", "stub", body.Content)
	}
	if body.StopReason != "end_turn" {
		t.Errorf("stop_reason: want end_turn, got %q", body.StopReason)
	}
}

func TestMessages_Routed_UnknownRole400(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:nonexistent",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}

	var errBody anthropicMsgError
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Type != "error" {
		t.Errorf("type: want error, got %q", errBody.Type)
	}
	if errBody.Error.Type != "invalid_request_error" {
		t.Errorf("error.type: want invalid_request_error, got %q", errBody.Error.Type)
	}
}

func TestMessages_Routed_ContentBlockArray(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages": []map[string]interface{}{
			{"role": "user", "content": []map[string]string{{"type": "text", "text": "hi from block"}}},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMessages_Routed_StreamProducesAnthropicSSE(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages",
		bytes.NewReader(mustMarshal(t, map[string]interface{}{
			"model":      "role:reviewer-chain",
			"max_tokens": 100,
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: want text/event-stream, got %q", ct)
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(events) != len(wantEvents) {
		t.Fatalf("event count: want %d, got %d: %v", len(wantEvents), len(events), events)
	}
	for i, want := range wantEvents {
		if events[i] != want {
			t.Errorf("event[%d]: want %q, got %q", i, want, events[i])
		}
	}
}

// --- Passthrough mode ---

func TestMessages_Passthrough_ForwardsRequestUnmodified(t *testing.T) {
	var receivedBody map[string]interface{}
	var receivedAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("x-api-key")
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_upstream","type":"message","role":"assistant","content":[{"type":"text","text":"from upstream"}]}`))
	}))
	defer upstream.Close()

	ts, cleanup := newMessagesTestServer(t, upstream.URL)
	defer cleanup()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages",
		bytes.NewReader(mustMarshal(t, map[string]interface{}{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 1024,
			"tools":      []map[string]string{{"name": "some_tool"}},
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Passthrough does not require the router's own inference token — only
	// the client's own Anthropic credential, which the router forwards
	// unchanged. This is what keeps "point ANTHROPIC_BASE_URL at the router"
	// transparent for a client that only knows its Anthropic key.
	req.Header.Set("x-api-key", "client-anthropic-key")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if mode := resp.Header.Get("X-Router-Mode"); mode != "passthrough" {
		t.Errorf("X-Router-Mode: want passthrough, got %q", mode)
	}
	// Client's own Anthropic credential must reach the upstream unchanged
	// (default passthrough auth behavior — no upstream_api_key configured).
	if receivedAPIKey != "client-anthropic-key" {
		t.Errorf("upstream x-api-key: want client-anthropic-key, got %q", receivedAPIKey)
	}
	// tools field must survive passthrough untouched — this is the whole point
	// of forwarding raw bytes rather than re-marshaling a typed struct.
	if _, ok := receivedBody["tools"]; !ok {
		t.Error("upstream request missing tools field — passthrough must forward body unmodified")
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["id"] != "msg_upstream" {
		t.Errorf("response id: want msg_upstream (from upstream, unmodified), got %v", body["id"])
	}
}

func TestMessages_Passthrough_UpstreamAPIKeyOverride(t *testing.T) {
	var receivedAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAPIKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()

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
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, upstream.URL, "router-owned-key", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	if receivedAPIKey != "router-owned-key" {
		t.Errorf("upstream x-api-key: want router-owned-key (override), got %q", receivedAPIKey)
	}
}

func TestMessages_Passthrough_StreamsSSEByteForByte(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt := []string{
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"a\"}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, chunk := range fmt {
			w.Write([]byte(chunk))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	ts, cleanup := newMessagesTestServer(t, upstream.URL)
	defer cleanup()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages",
		bytes.NewReader(mustMarshal(t, map[string]interface{}{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 100,
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	want := []string{"message_start", "content_block_delta", "message_stop"}
	if len(events) != len(want) {
		t.Fatalf("event count: want %d, got %d: %v", len(want), len(events), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d]: want %q, got %q", i, want[i], events[i])
		}
	}
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
