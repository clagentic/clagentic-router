// internal/router/timeout_enforcement_test.go — Route enforces the per-backend
// timeout for every adapter, distinguishes it from client cancellation, and
// derives the per-request HTTP deadline from the chain.
package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// newMultiRouter builds a Router over several mock adapters. timeouts maps
// backend id -> timeout_seconds (0 = unset).
func newMultiRouter(fns map[string]func(ctx context.Context, req *backend.Request) (*backend.Response, error), timeouts map[string]int) *Router {
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{},
		Tiers:    map[string][]string{},
		Routing: config.RoutingConfig{
			Strategy:                 "ordered",
			DegradedFailureThreshold: 3,
			OfflineFailureThreshold:  6,
		},
	}
	adapters := map[string]backend.Adapter{}
	states := map[string]*state.BackendState{}
	for id, fn := range fns {
		adapters[id] = &mockAdapter{id: id, invoke: fn}
		cfg.Backends[id] = &config.BackendConfig{Adapter: config.AdapterClaudeCLI, Model: "m", TimeoutSeconds: timeouts[id]}
		states[id] = state.New(id)
	}
	return &Router{cfg: cfg, states: states, adapters: adapters, stopCh: make(chan struct{})}
}

func okFn(ctx context.Context, req *backend.Request) (*backend.Response, error) {
	return &backend.Response{Content: "ok"}, nil
}

// blockFn blocks until ctx is done and returns a bare ctx error — the shape an
// SDK/HTTP adapter yields, i.e. NOT pre-classified as a timeout by the adapter.
func blockFn(ctx context.Context, req *backend.Request) (*backend.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func testReq() *backend.Request {
	return &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}
}

func TestRoute_BackendTimeoutEnforced_ClassifiedAndChainAdvances(t *testing.T) {
	r := newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"slow": blockFn,
		"fast": okFn,
	}, map[string]int{"slow": 1})

	start := time.Now()
	resp, meta, err := r.Route(context.Background(), testReq(), []string{"slow", "fast"})
	if err != nil {
		t.Fatalf("expected fall-through to fast tier, got err: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("per-backend timeout (1s) not enforced, took %s", elapsed)
	}
	if resp.Content != "ok" || meta.BackendID != "fast" || meta.ChainPosition != 1 {
		t.Errorf("want fast@1, got backend=%q pos=%d content=%q", meta.BackendID, meta.ChainPosition, resp.Content)
	}
	snap := r.states["slow"].Snapshot()
	if snap.LastErrorType != state.ErrTypeTimeout {
		t.Errorf("slow LastErrorType = %q, want %q", snap.LastErrorType, state.ErrTypeTimeout)
	}
}

func TestRoute_ClientCancel_NoBackendPenalty(t *testing.T) {
	r := newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"slow": blockFn,
		"fast": okFn,
	}, map[string]int{"slow": 30})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, err := r.Route(ctx, testReq(), []string{"slow", "fast"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want error wrapping context.Canceled, got %v", err)
	}
	snap := r.states["slow"].Snapshot()
	if snap.ConsecutiveFailures != 0 || snap.LastErrorType != "" {
		t.Errorf("client cancel penalized backend: failures=%d last_error_type=%q", snap.ConsecutiveFailures, snap.LastErrorType)
	}
	if fast := r.states["fast"].Snapshot(); fast.TotalCalls != 0 {
		t.Errorf("chain advanced after client cancel: fast.TotalCalls=%d", fast.TotalCalls)
	}
}

func TestRequestDeadline_SumsMaxPerEntryPlusMarginAndHonorsCap(t *testing.T) {
	r := newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"a": okFn, "b": okFn, "c": okFn,
	}, map[string]int{"a": 10, "b": 20, "c": 30})
	r.cfg.Tiers["t"] = []string{"b", "c"}

	// entry "a" -> 10s; tier "t" -> max(20,30)=30s; plus margin.
	want := 10*time.Second + 30*time.Second + deliveryMargin
	if got := r.RequestDeadline([]string{"a", "t"}); got != want {
		t.Errorf("RequestDeadline = %s, want %s", got, want)
	}

	r.cfg.Proxy.MaxRequestSeconds = 50
	if got := r.RequestDeadline([]string{"a", "t"}); got != 50*time.Second {
		t.Errorf("explicit max_request_seconds must cap: got %s, want 50s", got)
	}
}

// FallbackReason must name why the chain advanced (the failed tier's error
// type), not the winning backend's own state.
func TestRoute_FallbackReasonIsFailedTierErrorType(t *testing.T) {
	r := newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"first": func(context.Context, *backend.Request) (*backend.Response, error) {
			return nil, &backend.InvokeError{Type: backend.ErrTypeNetwork, Raw: "connection reset"}
		},
		"second": okFn,
	}, nil)

	_, meta, err := r.Route(context.Background(), testReq(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if meta.BackendID != "second" {
		t.Fatalf("winner = %q, want second", meta.BackendID)
	}
	if meta.FallbackReason != string(state.ErrTypeNetwork) {
		t.Errorf("FallbackReason = %q, want %q", meta.FallbackReason, state.ErrTypeNetwork)
	}
}
