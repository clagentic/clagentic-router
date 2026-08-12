// internal/router/tool_capability_test.go — unit tests for FilterChainForTools
// and AdapterCapabilities.
//
// Covers the refusal half of lr-be9454: a tool-bearing routed request must
// never silently route to a backend that cannot carry tools.
package router

import (
	"context"
	"errors"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// capAdapter is a minimal Adapter stub whose Capabilities() is configurable,
// for exercising tool-capability filtering in isolation.
type capAdapter struct {
	id   string
	caps backend.Capabilities
}

func (c *capAdapter) ID() string { return c.id }
func (c *capAdapter) Invoke(_ context.Context, _ *backend.Request) (*backend.Response, error) {
	return &backend.Response{Content: "ok"}, nil
}
func (c *capAdapter) Capabilities() backend.Capabilities { return c.caps }

// newCapTestRouter builds a Router with the given adapters, tiers, and
// chains, bypassing config validation (mirrors newTestRouter in
// offline_recovery_probe_test.go).
func newCapTestRouter(adapters map[string]backend.Adapter, tiers, chains map[string][]string) *Router {
	backends := make(map[string]*config.BackendConfig, len(adapters))
	states := make(map[string]*state.BackendState, len(adapters))
	for id := range adapters {
		backends[id] = &config.BackendConfig{Adapter: config.AdapterClaudeCLI, Model: "test"}
		states[id] = state.New(id)
	}
	cfg := &config.Config{
		Backends: backends,
		Tiers:    tiers,
		Chains:   chains,
		Routing: config.RoutingConfig{
			Strategy:                 "scored",
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

func TestFilterChainForTools_AllCapable_ChainUnchanged(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"a": &capAdapter{id: "a", caps: backend.Capabilities{SupportsTools: true}},
		"b": &capAdapter{id: "b", caps: backend.Capabilities{SupportsTools: true}},
	}
	r := newCapTestRouter(adapters, nil, nil)

	filtered, err := r.FilterChainForTools([]string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("expected both entries kept, got %v", filtered)
	}
}

func TestFilterChainForTools_MixedChain_KeepsOnlyCapableEntries(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"cli-a": &capAdapter{id: "cli-a", caps: backend.Capabilities{SupportsTools: false}},
		"api-b": &capAdapter{id: "api-b", caps: backend.Capabilities{SupportsTools: true}},
	}
	r := newCapTestRouter(adapters, nil, nil)

	filtered, err := r.FilterChainForTools([]string{"cli-a", "api-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filtered) != 1 || filtered[0] != "api-b" {
		t.Errorf("expected only api-b kept, got %v", filtered)
	}
}

func TestFilterChainForTools_NoCapableBackend_ReturnsErrNoToolCapableBackend(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"cli-a": &capAdapter{id: "cli-a", caps: backend.Capabilities{SupportsTools: false}},
		"cli-b": &capAdapter{id: "cli-b", caps: backend.Capabilities{SupportsTools: false}},
	}
	r := newCapTestRouter(adapters, nil, nil)

	_, err := r.FilterChainForTools([]string{"cli-a", "cli-b"})
	if !errors.Is(err, ErrNoToolCapableBackend) {
		t.Errorf("expected ErrNoToolCapableBackend, got %v", err)
	}
}

func TestFilterChainForTools_EmptyChain_ReturnsErrNoChain(t *testing.T) {
	r := newCapTestRouter(map[string]backend.Adapter{}, nil, nil)

	_, err := r.FilterChainForTools(nil)
	if !errors.Is(err, ErrNoChain) {
		t.Errorf("expected ErrNoChain, got %v", err)
	}
}

func TestFilterChainForTools_TierAlias_ResolvesCandidates(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"local-cli": &capAdapter{id: "local-cli", caps: backend.Capabilities{SupportsTools: false}},
		"cloud-api": &capAdapter{id: "cloud-api", caps: backend.Capabilities{SupportsTools: true}},
	}
	tiers := map[string][]string{
		"fast-tier": {"local-cli", "cloud-api"},
	}
	r := newCapTestRouter(adapters, tiers, nil)

	filtered, err := r.FilterChainForTools([]string{"fast-tier"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The tier alias entry is kept as-is (not expanded) since at least one
	// candidate within it is tool-capable — Route's own selectBest/fallback
	// walk still needs the full candidate list at that position.
	if len(filtered) != 1 || filtered[0] != "fast-tier" {
		t.Errorf("expected tier alias entry kept, got %v", filtered)
	}
}

func TestAdapterCapabilities_KnownBackend(t *testing.T) {
	adapters := map[string]backend.Adapter{
		"a": &capAdapter{id: "a", caps: backend.Capabilities{SupportsTools: true, SupportsImages: true}},
	}
	r := newCapTestRouter(adapters, nil, nil)

	caps, ok := r.AdapterCapabilities("a")
	if !ok {
		t.Fatal("expected ok=true for known backend")
	}
	if !caps.SupportsTools || !caps.SupportsImages {
		t.Errorf("capabilities not propagated correctly: %+v", caps)
	}
}

func TestAdapterCapabilities_UnknownBackend(t *testing.T) {
	r := newCapTestRouter(map[string]backend.Adapter{}, nil, nil)

	_, ok := r.AdapterCapabilities("nonexistent")
	if ok {
		t.Error("expected ok=false for unknown backend")
	}
}
