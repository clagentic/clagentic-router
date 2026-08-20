// internal/router/chain_exhaustion_test.go — unit tests for chain-exhaustion
// error-type propagation (lr-807319).
//
// MILLER's diagnosis found that fixing the misclassification defect in the
// CLI adapters (internal/backend) was necessary but not sufficient: the
// router's own Route() discarded the classified ErrorType of the last
// failure once the chain was exhausted, and the existing state-machine
// recovery machinery (RecordFailure's ErrTypeAuth -> StatusOffline
// transition, offlineRecoveryProbe) was claimed to already work correctly
// once classification is fixed. These tests verify that claim end-to-end
// through Route (not just via a direct RecordFailure call, as
// offline_recovery_probe_test.go already covers) and verify the new
// ChainExhaustedError carries the classified type for the server layer to
// surface via the existing last_error_type channel.
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

// TestRoute_ChainExhausted_CarriesLastErrorType verifies that when every
// backend in the chain fails, Route's returned error wraps
// *ChainExhaustedError with the classified ErrorType of the last failure —
// not silently dropped, and not the raw error text (no InvokeError.Raw
// content should be reachable from the returned error's Type field).
func TestRoute_ChainExhausted_CarriesLastErrorType(t *testing.T) {
	const backendID = "auth-only"

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return nil, &backend.InvokeError{Type: backend.ErrTypeAuth, Raw: "failed to load AWS credentials: credential expired"}
	}, config.RoutingConfig{})

	_, meta, err := r.Route(context.Background(), &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "hi"}},
	}, []string{backendID})

	if err == nil {
		t.Fatal("expected an error when the only backend in chain fails, got nil")
	}
	if meta != nil {
		t.Errorf("expected nil meta on failure, got %+v", meta)
	}
	if !errors.Is(err, ErrAllFailed) {
		t.Errorf("expected errors.Is(err, ErrAllFailed) to hold, err=%v", err)
	}

	var chainErr *ChainExhaustedError
	if !errors.As(err, &chainErr) {
		t.Fatalf("expected errors.As to find *ChainExhaustedError, err=%v", err)
	}
	if chainErr.Type != state.ErrTypeAuth {
		t.Errorf("ChainExhaustedError.Type = %q, want %q", chainErr.Type, state.ErrTypeAuth)
	}
}

// TestRoute_ChainExhausted_AuthFailureDrivesOfflineAndProbeEligible is the
// end-to-end verification of the claim in lr-807319's dispatch: an
// ErrTypeAuth failure classified by an adapter and returned through Route
// (not injected directly via RecordFailure) drives state.RecordFailure's
// ErrTypeAuth -> StatusOffline transition, and the resulting OFFLINE backend
// becomes eligible for offlineRecoveryProbe with no operator action. This
// exercises the full path Route -> recordFailure -> BackendState, then
// invokes offlineRecoveryProbe exactly as offline_recovery_probe_test.go
// does, confirming no changes to that machinery were needed.
func TestRoute_ChainExhausted_AuthFailureDrivesOfflineAndProbeEligible(t *testing.T) {
	const backendID = "auth-then-recovers"

	// First Invoke call (via Route) fails with an auth error classified from
	// full (non-truncated) output — mirrors the CLI adapters post-fix
	// behavior. Second Invoke call (via the recovery probe) succeeds,
	// simulating "aws sso login" having happened out of band.
	calls := 0
	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		calls++
		if calls == 1 {
			return nil, &backend.InvokeError{Type: backend.ErrTypeAuth, Raw: "credential expired"}
		}
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1),
	})

	_, _, err := r.Route(context.Background(), &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "hi"}},
	}, []string{backendID})
	if err == nil {
		t.Fatal("expected chain exhaustion error")
	}

	bs := r.states[backendID]
	snap := bs.Snapshot()
	if snap.Status != state.StatusOffline {
		t.Fatalf("after Route's auth failure: want StatusOffline, got %s", snap.Status)
	}
	if snap.LastErrorType != state.ErrTypeAuth {
		t.Fatalf("after Route's auth failure: want LastErrorType=%q, got %q", state.ErrTypeAuth, snap.LastErrorType)
	}
	if bs.HasPendingReset() {
		t.Fatal("auth failure should carry no quota/rate-limit reset time")
	}

	// Make the probe due (mirrors offline_recovery_probe_test.go's pattern).
	bs.MarkRecoveryProbed()
	bs.LastRecoveryProbeAt = time.Now().Add(-10 * time.Second)

	r.offlineRecoveryProbe()

	if got := bs.Snapshot().Status; got != state.StatusRecovering {
		t.Errorf("after recovery probe: want StatusRecovering, got %s", got)
	}
}
