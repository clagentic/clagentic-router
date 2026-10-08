// internal/server/bedrock_exhaustion_test.go — a routed Bedrock request whose
// chain is exhausted must answer 503, like the chat/messages handlers.
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

type failingAdapter struct{ id string }

func (f *failingAdapter) ID() string { return f.id }
func (f *failingAdapter) Invoke(context.Context, *backend.Request) (*backend.Response, error) {
	return nil, &backend.InvokeError{Type: backend.ErrTypeNetwork, Raw: "boom"}
}
func (f *failingAdapter) Capabilities() backend.Capabilities { return backend.Capabilities{} }

func TestBedrockRouted_ChainExhaustionIs503(t *testing.T) {
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{"bad": {Adapter: "stub", CostWeight: 1.0}},
		Chains:   map[string][]string{"bad-chain": {"bad"}},
		Routing: config.RoutingConfig{
			Strategy:                   "ordered",
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	r := router.New(cfg, map[string]backend.Adapter{"bad": &failingAdapter{id: "bad"}}, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "http://unused.invalid", "", "", "", false, "", "test")
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	resp, body, err := post(t, ts.URL+"/model/role:bad-chain/invoke",
		`{"anthropic_version":"bedrock-2023-05-31","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, bearer)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
	}
}
