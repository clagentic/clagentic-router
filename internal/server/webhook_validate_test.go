// internal/server/webhook_validate_test.go — tests for webhook URL validation,
// event allowlist, and list-response secret redaction.
package server

import (
	"bytes"
	"encoding/json"
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

// --- validateWebhookURL unit tests ---

// TestValidateWebhookURL_blocked verifies that IP literals in blocked ranges
// are rejected without DNS resolution.
func TestValidateWebhookURL_blocked(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"loopback-v4", "https://127.0.0.1/hook"},
		{"loopback-v4-range", "https://127.1.2.3/hook"},
		{"link-local-v4", "https://169.254.10.20/hook"},
		{"rfc1918-10", "https://10.0.0.1/hook"},
		{"rfc1918-192168", "https://192.168.1.1/hook"},
		{"rfc1918-172-16", "https://172.16.0.1/hook"},
		{"rfc1918-172-31", "https://172.31.255.255/hook"},
		{"cgnat", "https://100.64.0.1/hook"},
		{"test-net-1", "https://192.0.2.1/hook"},
		{"test-net-2", "https://198.51.100.1/hook"},
		{"test-net-3", "https://203.0.113.1/hook"},
		{"multicast", "https://224.0.0.1/hook"},
		{"this-network", "https://0.0.0.1/hook"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := validateWebhookURL(tc.url, false)
			if err == nil {
				t.Errorf("expected error for %q, got nil", tc.url)
			}
		})
	}
}

// TestValidateWebhookURL_scheme verifies scheme enforcement.
func TestValidateWebhookURL_scheme(t *testing.T) {
	// http not allowed when allowHTTP=false
	if err := validateWebhookURL("http://203.0.114.1/hook", false); err == nil {
		t.Error("expected error for http:// with allowHTTP=false, got nil")
	}
	// http allowed when allowHTTP=true and IP is not blocked
	if err := validateWebhookURL("http://203.0.114.1/hook", true); err != nil {
		t.Errorf("expected nil for http:// with allowHTTP=true, got: %v", err)
	}
	// ftp is never allowed
	if err := validateWebhookURL("ftp://203.0.114.1/hook", false); err == nil {
		t.Error("expected error for ftp:// scheme, got nil")
	}
}

// TestValidateWebhookURL_valid verifies that a valid https URL with a public IP
// literal is accepted. Uses a documentation-range IP (203.0.114.x is outside
// the blocked TEST-NET-3 range 203.0.113.0/24) to avoid DNS.
func TestValidateWebhookURL_valid(t *testing.T) {
	// 203.0.114.1 is not in any blocked CIDR.
	if err := validateWebhookURL("https://203.0.114.1/hook", false); err != nil {
		t.Errorf("expected nil for valid https URL, got: %v", err)
	}
}

// --- webhookCreate / webhookList integration tests via httptest ---

// newTestServerWithStore builds a minimal Server that has a real SQLite store,
// enabling the webhook endpoints to function. Returns the test server and cleanup.
func newTestServerWithStore(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

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

	srv := New(":0", "secret", "secret", false, r, st, "https://api.anthropic.com", "", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() {
		ts.Close()
	}
}

// doWebhookCreate POSTs a webhook registration to ts.
func doWebhookCreate(t *testing.T, ts *httptest.Server, payload map[string]interface{}) *http.Response {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req, err := http.NewRequest("POST", ts.URL+"/webhooks", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// doWebhookList calls GET /webhooks and returns the response.
func doWebhookList(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+"/webhooks", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestWebhookList_SecretRedacted registers a webhook with a secret and verifies
// that GET /webhooks returns has_secret:true but no "secret" key.
func TestWebhookList_SecretRedacted(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	// Register a webhook with a known secret and a valid public IP.
	createResp := doWebhookCreate(t, ts, map[string]interface{}{
		"url":    "https://203.0.114.1/hook",
		"events": []string{"quota_low"},
		"secret": "s3cr3t-value",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", createResp.StatusCode)
	}

	// List and inspect the response.
	listResp := doWebhookList(t, ts)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list: want 200, got %d", listResp.StatusCode)
	}

	// Decode as raw JSON to check field presence without a typed struct.
	var body map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode list response: %v", err)
	}

	webhooksRaw, ok := body["webhooks"]
	if !ok {
		t.Fatal("response missing webhooks key")
	}
	webhooks, ok := webhooksRaw.([]interface{})
	if !ok || len(webhooks) == 0 {
		t.Fatalf("expected at least one webhook, got: %v", webhooksRaw)
	}

	item, ok := webhooks[0].(map[string]interface{})
	if !ok {
		t.Fatalf("webhook item is not an object: %T", webhooks[0])
	}

	// has_secret must be true.
	hasSecret, ok := item["has_secret"].(bool)
	if !ok || !hasSecret {
		t.Errorf("has_secret: want true, got %v", item["has_secret"])
	}

	// "secret" key must not be present in the response.
	if _, present := item["secret"]; present {
		t.Error("secret field must not appear in list response")
	}
}

// TestWebhookCreate_BlockedIP verifies that SSRF-target URLs are rejected.
func TestWebhookCreate_BlockedIP(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	resp := doWebhookCreate(t, ts, map[string]interface{}{
		"url": "https://127.0.0.1/hook",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for loopback URL, got %d", resp.StatusCode)
	}
}

// TestWebhookCreate_UnknownEvent verifies that unknown event names are rejected.
func TestWebhookCreate_UnknownEvent(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	resp := doWebhookCreate(t, ts, map[string]interface{}{
		"url":    "https://203.0.114.1/hook",
		"events": []string{"quota_low", "not_a_real_event"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for unknown event, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
		errObj, _ := body["error"].(map[string]interface{})
		if code, _ := errObj["code"].(string); code != "invalid_event" {
			t.Errorf("error code: want %q, got %q", "invalid_event", code)
		}
	}
}

// TestWebhookCreate_HTTPSchemeRejected verifies that http:// URLs are rejected.
func TestWebhookCreate_HTTPSchemeRejected(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	resp := doWebhookCreate(t, ts, map[string]interface{}{
		"url": "http://203.0.114.1/hook",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for http:// URL, got %d", resp.StatusCode)
	}
}

// TestWebhookCreate_ValidKnownEvents verifies that a well-formed registration succeeds.
func TestWebhookCreate_ValidKnownEvents(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	resp := doWebhookCreate(t, ts, map[string]interface{}{
		"url":    "https://203.0.114.1/hook",
		"events": []string{"quota_low", "backend_offline", "auth_failure"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		// Read body for diagnostics.
		var b map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&b)
		t.Errorf("want 201 for valid registration, got %d: %v", resp.StatusCode, b)
	}
}

// TestWebhookList_NoSecret verifies that has_secret is false when no secret is provided.
func TestWebhookList_NoSecret(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	createResp := doWebhookCreate(t, ts, map[string]interface{}{
		"url": "https://203.0.114.1/hook",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", createResp.StatusCode)
	}

	listResp := doWebhookList(t, ts)
	defer listResp.Body.Close()

	var body map[string]interface{}
	if err := json.NewDecoder(listResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	webhooks, _ := body["webhooks"].([]interface{})
	if len(webhooks) == 0 {
		t.Fatal("expected at least one webhook")
	}
	item, _ := webhooks[0].(map[string]interface{})
	hasSecret, _ := item["has_secret"].(bool)
	if hasSecret {
		t.Error("has_secret: want false for webhook with no secret, got true")
	}
	// "secret" must still be absent.
	if _, present := item["secret"]; present {
		t.Error("secret field must not appear in list response")
	}
}

// TestWebhookList_BodyAsString verifies that the raw JSON response body never
// contains the literal secret value.
func TestWebhookList_BodyAsString(t *testing.T) {
	ts, cleanup := newTestServerWithStore(t)
	defer cleanup()

	createResp := doWebhookCreate(t, ts, map[string]interface{}{
		"url":    "https://203.0.114.1/hook",
		"secret": "canary-secret-xyz",
	})
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", createResp.StatusCode)
	}

	listResp := doWebhookList(t, ts)
	defer listResp.Body.Close()

	raw, err := io.ReadAll(listResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(raw), "canary-secret-xyz") {
		t.Errorf("list response body must not contain the raw secret value")
	}
}
