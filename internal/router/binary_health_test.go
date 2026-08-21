// internal/router/binary_health_test.go — regression tests for lr-92ee18 B2:
// a configured backend whose CLI binary never resolved at startup must be
// visible from Router.UnresolvedBinaryBackends, not silently invisible until
// the first real request fails.
package router

import (
	"context"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// binaryCheckerMockAdapter extends mockAdapter (offline_recovery_probe_test.go)
// with a BinaryResolved() method so it satisfies backend.BinaryChecker —
// mirroring the real CLI adapters' shape (claude_cli, codex_cli,
// codex_subagent, gemini_cli) without depending on their subprocess-spawning
// internals.
type binaryCheckerMockAdapter struct {
	mockAdapter
	resolved bool
}

func (m *binaryCheckerMockAdapter) BinaryResolved() bool { return m.resolved }

// httpOnlyMockAdapter implements backend.Adapter but deliberately does NOT
// implement backend.BinaryChecker — mirroring the HTTP adapters
// (anthropic_api, openai_api, bedrock_api, ollama_http), which have no
// subprocess and no binary-resolution concept at all.
type httpOnlyMockAdapter struct {
	id string
}

func (m *httpOnlyMockAdapter) ID() string { return m.id }
func (m *httpOnlyMockAdapter) Invoke(ctx context.Context, req *backend.Request) (*backend.Response, error) {
	return &backend.Response{Content: "ok"}, nil
}
func (m *httpOnlyMockAdapter) Capabilities() backend.Capabilities { return backend.Capabilities{} }

// newMultiAdapterTestRouter builds a Router with the given adapters directly
// (bypassing newTestRouter's single-adapter convenience), for tests that
// need to mix adapter types.
func newMultiAdapterTestRouter(adapters map[string]backend.Adapter) *Router {
	backends := make(map[string]*config.BackendConfig, len(adapters))
	states := make(map[string]*state.BackendState, len(adapters))
	for id := range adapters {
		backends[id] = &config.BackendConfig{Adapter: config.AdapterClaudeCLI, Model: "test"}
		states[id] = state.New(id)
	}
	cfg := &config.Config{
		Backends: backends,
		Routing: config.RoutingConfig{
			DegradedFailureThreshold: 3,
			OfflineFailureThreshold:  6,
		},
	}
	return &Router{
		cfg:      cfg,
		states:   states,
		adapters: adapters,
		stopCh:   make(chan struct{}),
	}
}

func TestUnresolvedBinaryBackends_ReportsUnresolvedCLIAdapter(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"broken-cli": &binaryCheckerMockAdapter{
			mockAdapter: mockAdapter{id: "broken-cli"},
			resolved:    false,
		},
	}
	r := newMultiAdapterTestRouter(adapters)

	got := r.UnresolvedBinaryBackends()
	if len(got) != 1 || got[0] != "broken-cli" {
		t.Errorf("UnresolvedBinaryBackends() = %v, want [\"broken-cli\"]", got)
	}
}

func TestUnresolvedBinaryBackends_ResolvedCLIAdapterNotReported(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"working-cli": &binaryCheckerMockAdapter{
			mockAdapter: mockAdapter{id: "working-cli"},
			resolved:    true,
		},
	}
	r := newMultiAdapterTestRouter(adapters)

	got := r.UnresolvedBinaryBackends()
	if len(got) != 0 {
		t.Errorf("UnresolvedBinaryBackends() = %v, want empty for a resolved binary", got)
	}
}

// TestUnresolvedBinaryBackends_HTTPAdapterNeverReported verifies that an
// adapter with no binary-resolution concept at all (HTTP adapters) is never
// reported as "unresolved" — that would conflate "not applicable" with
// "failed," which is a distinct, wrong signal (see BinaryChecker's doc).
func TestUnresolvedBinaryBackends_HTTPAdapterNeverReported(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"http-backend": &httpOnlyMockAdapter{id: "http-backend"},
	}
	r := newMultiAdapterTestRouter(adapters)

	got := r.UnresolvedBinaryBackends()
	if len(got) != 0 {
		t.Errorf("UnresolvedBinaryBackends() = %v, want empty for an HTTP-only adapter with no BinaryChecker", got)
	}
}

// TestUnresolvedBinaryBackends_MixedFleet exercises a realistic mixed
// deployment: one CLI backend whose binary never resolved, one CLI backend
// that resolved fine, and one HTTP backend with no binary concept — only the
// first should be reported.
func TestUnresolvedBinaryBackends_MixedFleet(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"broken-cli": &binaryCheckerMockAdapter{
			mockAdapter: mockAdapter{id: "broken-cli"},
			resolved:    false,
		},
		"working-cli": &binaryCheckerMockAdapter{
			mockAdapter: mockAdapter{id: "working-cli"},
			resolved:    true,
		},
		"http-backend": &httpOnlyMockAdapter{id: "http-backend"},
	}
	r := newMultiAdapterTestRouter(adapters)

	got := r.UnresolvedBinaryBackends()
	if len(got) != 1 || got[0] != "broken-cli" {
		t.Errorf("UnresolvedBinaryBackends() = %v, want [\"broken-cli\"] only", got)
	}
}
