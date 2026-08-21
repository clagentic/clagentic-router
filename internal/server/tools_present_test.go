// internal/server/tools_present_test.go — end-to-end tests for tools-presence
// capture in call_log (lr-4aaf2a), through the real HTTP handlers.
//
// Covers the acceptance-critical case: a routed tools-bearing request that is
// refused with 422 no_tool_capable_backend never reaches router.Route, so
// without explicit refusal logging it would produce NO call_log row at all —
// verified here by asserting a row DOES appear, with tools_present=true, and
// that GET /logs surfaces the field.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/store"
)

// newStoreBackedTestServer mirrors newChatCompletionsTestServer /
// newMessagesTestServer but wires a real *store.Store (backed by a temp
// file) so call_log writes are observable through GET /logs.
func newStoreBackedTestServer(t *testing.T, supportsTools bool) (*httptest.Server, *store.Store, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

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
		"test-backend": &stubAdapter{id: "test-backend", supportsTools: supportsTools},
	}
	r := router.New(cfg, adapters, st, nil)
	srv := New(":0", "secret", "secret", false, r, st, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, st, func() { ts.Close(); st.Close() }
}

func doLogsRequest(t *testing.T, ts *httptest.Server, token string) map[string]interface{} {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/logs", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /logs status: want 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /logs response: %v", err)
	}
	return body
}

// TestChatCompletions_ToolsRefusal_WritesCallLogRow verifies that a
// tools-bearing request refused with 422 (no tool-capable backend) still
// produces a call_log row — the acceptance criterion this task exists for.
// Without this, router.Route is never called and no row would exist at all.
func TestChatCompletions_ToolsRefusal_WritesCallLogRow(t *testing.T) {
	ts, st, cleanup := newStoreBackedTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 call_log row for the refused request, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true on the refused row, got false")
	}
	if rows[0].Outcome != "refused_no_tool_capable_backend" {
		t.Errorf("Outcome: want refused_no_tool_capable_backend, got %q", rows[0].Outcome)
	}
}

// TestChatCompletions_ToolsSuccess_WritesToolsPresentTrue verifies a
// tools-bearing request that DOES route successfully also records
// tools_present=true on the resulting "pass" row.
func TestChatCompletions_ToolsSuccess_WritesToolsPresentTrue(t *testing.T) {
	ts, st, cleanup := newStoreBackedTestServer(t, true)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 call_log row, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true, got false")
	}
	if rows[0].Outcome != "pass" {
		t.Errorf("Outcome: want pass, got %q", rows[0].Outcome)
	}
}

// TestChatCompletions_NoTools_WritesToolsPresentFalse is the control case.
func TestChatCompletions_NoTools_WritesToolsPresentFalse(t *testing.T) {
	ts, st, cleanup := newStoreBackedTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 call_log row, got %d", len(rows))
	}
	if rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want false, got true")
	}
}

// TestLogs_ExposesToolsPresentField verifies GET /logs (the actual read path
// an operator queries) surfaces tools_present in the JSON response — the
// "surface it on /logs" acceptance criterion.
func TestLogs_ExposesToolsPresentField(t *testing.T) {
	ts, _, cleanup := newStoreBackedTestServer(t, true)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	body := doLogsRequest(t, ts, "secret")
	rows, ok := body["rows"].([]interface{})
	if !ok || len(rows) != 1 {
		t.Fatalf("expected 1 row in /logs response, got %#v", body["rows"])
	}
	row, ok := rows[0].(map[string]interface{})
	if !ok {
		t.Fatalf("row is not an object: %#v", rows[0])
	}
	val, present := row["ToolsPresent"]
	if !present {
		t.Fatalf("/logs row missing ToolsPresent field entirely: %#v", row)
	}
	if val != true {
		t.Errorf("ToolsPresent in /logs response: want true, got %#v", val)
	}
}

// TestMessagesRouted_ToolsRefusal_WritesCallLogRow verifies the same refusal
// logging behavior on the Anthropic Messages routed-mode entry point.
func TestMessagesRouted_ToolsRefusal_WritesCallLogRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

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
		"test-backend": &stubAdapter{id: "test-backend", supportsTools: false},
	}
	r := router.New(cfg, adapters, st, nil)
	srv := New(":0", "secret", "secret", false, r, st, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	raw, _ := json.Marshal(map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"tools":      []map[string]string{{"name": "some_tool"}},
		"messages":   []map[string]string{{"role": "user", "content": "use a tool"}},
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 call_log row for the refused messages request, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true, got false")
	}
	if rows[0].Outcome != "refused_no_tool_capable_backend" {
		t.Errorf("Outcome: want refused_no_tool_capable_backend, got %q", rows[0].Outcome)
	}
}

// TestBedrockRouted_ToolsRefusal_WritesCallLogRow verifies the fourth routed
// entry point (AWS Bedrock InvokeModel wire shape, bedrock_invoke.go) has the
// same refusal-logging fix as chatCompletions/messagesRouted — found and
// folded into this PR while auditing all router.Route callers for the
// tools-presence gap (lr-4aaf2a fold-in).
func TestBedrockRouted_ToolsRefusal_WritesCallLogRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

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
		"test-backend": &stubAdapter{id: "test-backend", supportsTools: false},
	}
	r := router.New(cfg, adapters, st, nil)
	srv := New(":0", "secret", "secret", false, r, st, "https://api.anthropic.com", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens":        100,
		"tools":             []map[string]string{{"name": "some_tool"}},
		"messages":          []map[string]string{{"role": "user", "content": "use a tool"}},
		"anthropic_version": "bedrock-2023-05-31",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", resp.StatusCode)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 call_log row for the refused bedrock invoke request, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true, got false")
	}
	if rows[0].Outcome != "refused_no_tool_capable_backend" {
		t.Errorf("Outcome: want refused_no_tool_capable_backend, got %q", rows[0].Outcome)
	}
}
