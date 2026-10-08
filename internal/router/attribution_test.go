// internal/router/attribution_test.go — guards the invariant that Route charges
// a failure to whoever's deadline or cancel fired FIRST (context.Cause of the
// innermost context), not to whichever context looks done once Invoke returns.
package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

func twoTierRouter(slow func(context.Context, *backend.Request) (*backend.Response, error)) *Router {
	return newMultiRouter(map[string]func(context.Context, *backend.Request) (*backend.Response, error){
		"slow": slow,
		"fast": okFn,
	}, map[string]int{"slow": 30})
}

// An expired chain budget is charged to the backend as a timeout, and no later
// tier is tried: the budget it would run in is gone.
func TestRoute_ChainBudgetCause_TimeoutPenaltyNoAdvance(t *testing.T) {
	r := twoTierRouter(blockFn)
	ctx, cancel := context.WithTimeoutCause(context.Background(), 100*time.Millisecond, ErrChainBudget)
	defer cancel()

	_, _, err := r.Route(ctx, testReq(), []string{"slow", "fast"})

	var ce *ChainExhaustedError
	if !errors.As(err, &ce) || ce.Type != state.ErrTypeTimeout {
		t.Fatalf("want ChainExhaustedError{timeout}, got %v", err)
	}
	if snap := r.states["slow"].Snapshot(); snap.LastErrorType != state.ErrTypeTimeout || snap.ConsecutiveFailures != 1 {
		t.Errorf("slow not charged a timeout: %+v", snap)
	}
	if fast := r.states["fast"].Snapshot(); fast.TotalCalls != 0 {
		t.Errorf("chain advanced past an exhausted budget: fast.TotalCalls=%d", fast.TotalCalls)
	}
}

// The operator's cap is not the backend's fault.
func TestRoute_RequestCapCause_NoPenalty(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "slow", blockFn)
	r.cfg.Backends["slow"].TimeoutSeconds = 30
	ctx, cancel := context.WithTimeoutCause(context.Background(), 100*time.Millisecond, ErrRequestCap)
	defer cancel()

	_, _, err := r.Route(ctx, testReq(), []string{"slow"})

	if !errors.Is(err, ErrRequestCap) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want error carrying ErrRequestCap (a deadline), got %v", err)
	}
	if snap := r.states["slow"].Snapshot(); snap.ConsecutiveFailures != 0 || snap.LastErrorType != "" {
		t.Errorf("request cap penalized backend: %+v", snap)
	}
	rows, qerr := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if qerr != nil || len(rows) != 1 || rows[0].Outcome != "request_deadline" {
		t.Fatalf("want one request_deadline row, got %+v (%v)", rows, qerr)
	}
}

// First event wins: the backend's own deadline fired, then the client left
// before Invoke returned. The backend is still charged, and the chain does not
// advance for a request nobody is waiting on.
func TestRoute_ClientCancelAfterBackendTimeout_BackendStillCharged(t *testing.T) {
	release := make(chan struct{})
	r := twoTierRouter(func(ctx context.Context, _ *backend.Request) (*backend.Response, error) {
		<-ctx.Done()
		<-release // linger past the client cancel, as a subprocess draining after kill does
		return nil, ctx.Err()
	})
	r.cfg.Backends["slow"].TimeoutSeconds = 1

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1200 * time.Millisecond) // after the 1s backend deadline
		cancel()
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()

	_, _, err := r.Route(ctx, testReq(), []string{"slow", "fast"})

	var ce *ChainExhaustedError
	if !errors.As(err, &ce) || ce.Type != state.ErrTypeTimeout {
		t.Fatalf("want ChainExhaustedError{timeout}, got %v", err)
	}
	if snap := r.states["slow"].Snapshot(); snap.LastErrorType != state.ErrTypeTimeout {
		t.Errorf("backend deadline fired first but was not charged: %+v", snap)
	}
	if fast := r.states["fast"].Snapshot(); fast.TotalCalls != 0 {
		t.Errorf("chain advanced for a departed client: fast.TotalCalls=%d", fast.TotalCalls)
	}
}

func TestCauseLabel_DistinguishesOwners(t *testing.T) {
	cases := map[string]error{
		"none":              nil,
		"backend_timeout":   ErrBackendTimeout,
		"chain_budget":      ErrChainBudget,
		"request_cap":       ErrRequestCap,
		"work_deadline":     ErrWorkDeadline,
		"write_deadline":    ErrWriteDeadline,
		"client_cancelled":  context.Canceled,
		"deadline_exceeded": context.DeadlineExceeded,
	}
	for want, cause := range cases {
		if got := CauseLabel(cause); got != want {
			t.Errorf("CauseLabel(%v) = %q, want %q", cause, got, want)
		}
	}
}
