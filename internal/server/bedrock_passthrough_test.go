// internal/server/bedrock_passthrough_test.go — tests for the SigV4-signed
// POST /model/{modelId}/invoke[-with-response-stream] passthrough path.
//
// Exercises real request-building and real SigV4 signing (via the actual
// aws-sdk-go-v2 v4.Signer) against an httptest upstream standing in for AWS
// Bedrock, with credentials injected via bedrockCredentialsFn so no live AWS
// account or network access is required. This verifies the signing call
// shape and header production deterministically; it does NOT verify that
// AWS itself would accept the signature (that requires live credentials —
// see PR body and task description, which note the reporter had no Bedrock
// account access).
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newBedrockPassthroughTestServer builds a Handler wired directly (bypassing
// New, which does not expose the test-only injection points) with
// bedrockUpstreamBaseURL pointed at upstreamURL and bedrockCredentialsFn
// returning a fixed stub credential.
func newBedrockPassthroughTestServer(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()

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

	h := &Handler{
		router:                 r,
		token:                  "secret",
		adminToken:             "secret",
		anthropicUpstreamURL:   "https://api.anthropic.com",
		bedrockRegion:          "us-east-1",
		bedrockUpstreamBaseURL: upstreamURL,
		bedrockCredentialsFn: func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     "AKIAEXAMPLESTUBKEY",
				SecretAccessKey: "stub-secret-key-never-real",
				SessionToken:    "stub-session-token",
			}, nil
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /model/{modelId}/invoke", h.bedrockInvoke)
	mux.HandleFunc("POST /model/{modelId}/invoke-with-response-stream", h.bedrockInvokeStream)
	return httptest.NewServer(mux)
}

func TestBedrockPassthrough_ForwardsBodyAndSigns(t *testing.T) {
	var gotAuth, gotDate, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("X-Amz-Date")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer upstream.Close()

	ts := newBedrockPassthroughTestServer(t, upstream.URL)
	defer ts.Close()

	reqBody := `{"max_tokens":1,"messages":[{"role":"user","content":"."}],"anthropic_version":"bedrock-2023-05-31"}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status: want 200, got %d (body: %s)", resp.StatusCode, body)
	}
	if mode := resp.Header.Get("X-Router-Mode"); mode != "passthrough" {
		t.Errorf("X-Router-Mode: want passthrough, got %q", mode)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization header not SigV4-shaped: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "Credential=AKIAEXAMPLESTUBKEY/") {
		t.Errorf("Authorization header missing expected access key: %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "/bedrock/aws4_request") {
		t.Errorf("Authorization header missing bedrock service scope: %q", gotAuth)
	}
	if gotDate == "" {
		t.Error("X-Amz-Date header not set")
	}
	if gotBody != reqBody {
		t.Errorf("upstream body: want %q, got %q (passthrough must forward unmodified)", reqBody, gotBody)
	}
}

func TestBedrockPassthrough_StreamingActionInURL(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	ts := newBedrockPassthroughTestServer(t, upstream.URL)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke-with-response-stream",
		strings.NewReader(`{"max_tokens":1,"messages":[{"role":"user","content":"."}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	wantPath := "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke-with-response-stream"
	if gotPath != wantPath {
		t.Errorf("upstream path: want %q, got %q", wantPath, gotPath)
	}
}

func TestBedrockPassthrough_CredentialResolutionFailure_Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called when credential resolution fails")
	}))
	defer upstream.Close()

	ts := newBedrockPassthroughTestServer(t, upstream.URL)
	defer ts.Close()

	// Swap in a failing credentials func by hitting a fresh handler instance
	// wired directly, since ts above already has a working stub.
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"test-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{Strategy: "scored", HealthProbeIntervalSeconds: 3600, DegradedFailureThreshold: 3, OfflineFailureThreshold: 6},
	}
	adapters := map[string]backend.Adapter{"test-backend": &stubAdapter{id: "test-backend"}}
	r := router.New(cfg, adapters, nil, nil)
	h := &Handler{
		router:                 r,
		bedrockRegion:          "us-east-1",
		bedrockUpstreamBaseURL: upstream.URL,
		bedrockCredentialsFn: func(ctx context.Context) (aws.Credentials, error) {
			return aws.Credentials{}, io.ErrUnexpectedEOF
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /model/{modelId}/invoke", h.bedrockInvoke)
	failTS := httptest.NewServer(mux)
	defer failTS.Close()

	req, err := http.NewRequest(http.MethodPost, failTS.URL+"/model/anthropic.claude-3-5-sonnet-20241022-v2:0/invoke",
		strings.NewReader(`{"max_tokens":1,"messages":[{"role":"user","content":"."}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: want 502, got %d", resp.StatusCode)
	}
}
