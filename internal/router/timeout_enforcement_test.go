// Only the HTTP adapters ever honored timeout_seconds; the CLI and bedrock
// adapters ignored it, so a hung subprocess ran unbounded. Route is the one
// place the bound is applied now, so these tests pin it there (independent of
// adapter family), pin that a client leaving is not charged to the backend, and
// pin the chain-derived request deadline the HTTP layer builds on.
package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
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

// A request deadline below the backend's own timeout (explicit
// max_request_seconds under the chain sum) must stop Invoke at the request
// deadline, charge nothing to the backend, and log a non-pass outcome.
func TestRoute_RequestDeadline_NoBackendPenalty_LoggedNotPass(t *testing.T) {
	var observedAt time.Duration
	var begin time.Time
	r, st := newStoreBackedTestRouter(t, "slow", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		<-ctx.Done()
		observedAt = time.Since(begin)
		return nil, ctx.Err()
	})
	r.cfg.Backends["slow"].TimeoutSeconds = 30

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin = time.Now()
	_, _, err := r.Route(ctx, testReq(), []string{"slow"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want error wrapping context.DeadlineExceeded, got %v", err)
	}
	if observedAt < 80*time.Millisecond || observedAt > 5*time.Second {
		t.Errorf("adapter saw cancellation at %s, want about the 100ms request deadline", observedAt)
	}
	snap := r.states["slow"].Snapshot()
	if snap.ConsecutiveFailures != 0 || snap.LastErrorType != "" {
		t.Errorf("request deadline penalized backend: failures=%d last_error_type=%q", snap.ConsecutiveFailures, snap.LastErrorType)
	}
	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome != "request_deadline" {
		t.Fatalf("want one request_deadline row, got %+v", rows)
	}
}

func TestRequestDeadline_SumsMaxPerEntryPlusMarginAndHonorsCap(t *testing.T) {
	r := newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"a": okFn, "b": okFn, "c": okFn,
	}, map[string]int{"a": 10, "b": 20, "c": 30})
	r.cfg.Tiers["t"] = []string{"b", "c"}

	// entry "a" -> 10s; tier "t" -> max(20,30)=30s; plus margin.
	want := 10*time.Second + 30*time.Second + deliveryMargin
	if got, capped := r.RequestDeadline([]string{"a", "t"}); got != want || capped {
		t.Errorf("RequestDeadline = %s capped=%v, want %s uncapped", got, capped, want)
	}

	r.cfg.Proxy.MaxRequestSeconds = 50
	if got, capped := r.RequestDeadline([]string{"a", "t"}); got != 50*time.Second || !capped {
		t.Errorf("explicit max_request_seconds must cap: got %s capped=%v, want 50s capped", got, capped)
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
