// internal/router/timeout_modelconfig_test.go — regression tests for
// lr-2f35bd: AC4 (chain exhausted by timeouts surfaces last_error_type=
// timeout) and B5's router-layer "never re-probed" requirement for
// ErrTypeModelConfig.
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

// TestRoute_ChainExhausted_TimeoutCarriesLastErrorType covers AC4 directly:
// a chain exhausted entirely by ErrTypeTimeout failures must carry
// state.ErrTypeTimeout as ChainExhaustedError.Type — the same field the
// server layer surfaces to the client as the 503's last_error_type. Mirrors
// TestRoute_ChainExhausted_CarriesLastErrorType's shape for ErrTypeAuth
// (chain_exhaustion_test.go), substituting the adapter failure this task
// adds classification for.
func TestRoute_ChainExhausted_TimeoutCarriesLastErrorType(t *testing.T) {
	const backendID = "timeout-only"

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return nil, &backend.InvokeError{Type: backend.ErrTypeTimeout, Raw: "context deadline exceeded"}
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
	if chainErr.Type != state.ErrTypeTimeout {
		t.Errorf("ChainExhaustedError.Type = %q, want %q (AC4)", chainErr.Type, state.ErrTypeTimeout)
	}
}

// TestRoute_ChainExhausted_TimeoutDoesNotHardOfflineOnFirstFailure is the
// end-to-end version of AC2 through Route (not just a direct RecordFailure
// call, as state_timeout_modelconfig_test.go already covers in isolation):
// a single ErrTypeTimeout failure classified by an adapter and returned
// through Route must not drive the backend straight to StatusOffline the
// way ErrTypeAuth does — it takes the soft/threshold path.
func TestRoute_ChainExhausted_TimeoutDoesNotHardOfflineOnFirstFailure(t *testing.T) {
	const backendID = "timeout-soft"

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return nil, &backend.InvokeError{Type: backend.ErrTypeTimeout, Raw: "context deadline exceeded"}
	}, config.RoutingConfig{})
	// newTestRouter defaults OfflineFailureThreshold to 6 — one failure must
	// not reach it.

	_, _, err := r.Route(context.Background(), &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "hi"}},
	}, []string{backendID})
	if err == nil {
		t.Fatal("expected chain exhaustion error")
	}

	snap := r.states[backendID].Snapshot()
	if snap.Status == state.StatusOffline {
		t.Errorf("after ONE ErrTypeTimeout failure (offlineThreshold=6): status = %q, want anything but Offline — timeout must not hard-offline like auth does", snap.Status)
	}
	if snap.LastErrorType != state.ErrTypeTimeout {
		t.Errorf("LastErrorType = %q, want %q", snap.LastErrorType, state.ErrTypeTimeout)
	}
}

// TestOfflineRecoveryProbe_ModelConfigOffline_NeverProbed covers B5's
// "sticky and NOT re-probed" acceptance point at the router layer: a
// backend offline due to ErrTypeModelConfig must be skipped by
// offlineRecoveryProbe even when the probe interval has long since
// elapsed — mirrors
// TestOfflineRecoveryProbe_QuotaOfflineFutureReset_NotProbed's shape
// (offline_recovery_probe_test.go) for the new sticky category.
func TestOfflineRecoveryProbe_ModelConfigOffline_NeverProbed(t *testing.T) {
	const backendID = "model-config-b"
	probeCount := 0

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		probeCount++
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1), // short; would fire if eligible
	})

	bs := r.states[backendID]
	bs.RecordFailure(state.ErrTypeModelConfig, "provided model identifier is invalid", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)
	if bs.Snapshot().Status != state.StatusOffline {
		t.Fatalf("precondition: want StatusOffline after model_config failure, got %s", bs.Snapshot().Status)
	}

	// Back-date LastRecoveryProbeAt so the interval gate alone would allow a
	// probe — proving the model_config skip, not the interval gate, is what
	// prevents it.
	bs.MarkRecoveryProbed()
	bs.LastRecoveryProbeAt = time.Now().Add(-10 * time.Second)

	r.offlineRecoveryProbe()

	if probeCount != 0 {
		t.Errorf("model_config-offline: want 0 probes (sticky, never re-probed per B5), got %d", probeCount)
	}
	if bs.Snapshot().Status != state.StatusOffline {
		t.Errorf("model_config-offline: want status to remain Offline (no probe ran), got %s", bs.Snapshot().Status)
	}
}
