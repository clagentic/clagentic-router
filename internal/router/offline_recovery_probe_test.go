// internal/router/offline_recovery_probe_test.go — unit tests for offlineRecoveryProbe.
//
// These tests exercise the recovery probe in isolation using a minimal Router
// constructed with a mock adapter. No real LLM calls are made.
package router

import (
	"context"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// mockAdapter implements backend.Adapter and allows controlling Invoke outcomes.
type mockAdapter struct {
	id      string
	invoke  func(ctx context.Context, req *backend.Request) (*backend.Response, error)
}

func (m *mockAdapter) ID() string { return m.id }

func (m *mockAdapter) Invoke(ctx context.Context, req *backend.Request) (*backend.Response, error) {
	return m.invoke(ctx, req)
}

func (m *mockAdapter) Capabilities() backend.Capabilities {
	return backend.Capabilities{}
}

// newTestRouter builds a minimal Router with a single mock adapter and the
// supplied routing config. store and alertHook are nil (not needed for probe tests).
func newTestRouter(id string, adapterFn func(ctx context.Context, req *backend.Request) (*backend.Response, error), rcfg config.RoutingConfig) *Router {
	adapter := &mockAdapter{id: id, invoke: adapterFn}
	adapters := map[string]backend.Adapter{id: adapter}

	// Build a minimal valid Config. validate() fills defaults; we override Routing.
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			id: {Adapter: config.AdapterClaudeCLI, Model: "test"},
		},
		Routing: rcfg,
	}
	// Run validate to fill any missing defaults (ActiveProbeTimeoutSeconds etc.).
	// validate is unexported; call Load workaround is not available. We'll set
	// ActiveProbeTimeoutSeconds directly to a test-friendly value.
	if cfg.Routing.ActiveProbeTimeoutSeconds <= 0 {
		cfg.Routing.ActiveProbeTimeoutSeconds = 5
	}
	if cfg.Routing.DegradedFailureThreshold <= 0 {
		cfg.Routing.DegradedFailureThreshold = 3
	}
	if cfg.Routing.OfflineFailureThreshold <= 0 {
		cfg.Routing.OfflineFailureThreshold = 6
	}

	return &Router{
		cfg:      cfg,
		states:   map[string]*state.BackendState{id: state.New(id)},
		adapters: adapters,
		stopCh:   make(chan struct{}),
	}
}

// offlineRecoveryInterval builds a *int pointer with the given value for use in
// RoutingConfig.OfflineRecoveryProbeIntervalSeconds.
func offlineRecoveryInterval(v int) *int { return &v }

// --- Test: auth-tripped OFFLINE backend self-heals after probe interval ---

func TestOfflineRecoveryProbe_AuthOfflineBecomesRecovering(t *testing.T) {
	const backendID = "auth-b"

	// Probe always succeeds.
	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1), // short for test
	})

	bs := r.states[backendID]

	// Trip the backend OFFLINE via an auth failure (no reset time).
	bs.RecordFailure(state.ErrTypeAuth, "credential expired", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)
	if bs.Snapshot().Status != state.StatusOffline {
		t.Fatalf("precondition: want StatusOffline after auth failure, got %s", bs.Snapshot().Status)
	}

	// Back-date LastRecoveryProbeAt so the interval has elapsed.
	bs.MarkRecoveryProbed() // sets now
	// Force it far enough in the past: rewrite via lock
	bs.LastRecoveryProbeAt = time.Now().Add(-10 * time.Second)

	r.offlineRecoveryProbe()

	snap := bs.Snapshot()
	if snap.Status != state.StatusRecovering {
		t.Errorf("after successful probe: want StatusRecovering, got %s", snap.Status)
	}
}

// --- Test: probe is gated — not fired again before interval elapses ---

func TestOfflineRecoveryProbe_Gated_NotFiredBeforeInterval(t *testing.T) {
	const backendID = "gated-b"
	probeCount := 0

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		probeCount++
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(300), // 5 min
	})

	bs := r.states[backendID]
	bs.RecordFailure(state.ErrTypeAuth, "auth err", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)

	// First call — no prior probe, due immediately.
	r.offlineRecoveryProbe()
	if probeCount != 1 {
		t.Fatalf("first probe call: want 1 probe, got %d", probeCount)
	}

	// Backend recovered; trip offline again to test gating on a fresh offline.
	bs.RecordFailure(state.ErrTypeAuth, "auth err again", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)

	// Second call — LastRecoveryProbeAt was just set, interval not elapsed.
	r.offlineRecoveryProbe()
	if probeCount != 1 {
		t.Errorf("second probe call (before interval): want still 1 probe, got %d", probeCount)
	}
}

// --- Test: interval=0 disables the recovery probe entirely ---

func TestOfflineRecoveryProbe_ZeroIntervalDisabled(t *testing.T) {
	const backendID = "disabled-b"
	probeCount := 0

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		probeCount++
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(0), // disabled
	})

	bs := r.states[backendID]
	bs.RecordFailure(state.ErrTypeAuth, "auth err", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)

	r.offlineRecoveryProbe()
	if probeCount != 0 {
		t.Errorf("interval=0: want 0 probes (disabled), got %d", probeCount)
	}
}

// --- Test: quota-offline with future reset time is NOT recovery-probed ---

func TestOfflineRecoveryProbe_QuotaOfflineFutureReset_NotProbed(t *testing.T) {
	const backendID = "quota-b"
	probeCount := 0

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		probeCount++
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1), // short; would fire if eligible
	})

	bs := r.states[backendID]

	// Trip OFFLINE via quota with a reset time 1 hour in the future.
	futureReset := time.Now().Add(time.Hour)
	bs.RecordFailure(state.ErrTypeQuota, "quota exhausted", futureReset,
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)
	if bs.Snapshot().Status != state.StatusOffline {
		t.Fatalf("precondition: want StatusOffline, got %s", bs.Snapshot().Status)
	}
	if !bs.HasPendingReset() {
		t.Fatal("precondition: want HasPendingReset()=true for future quota reset")
	}

	r.offlineRecoveryProbe()
	if probeCount != 0 {
		t.Errorf("quota-offline with future reset: want 0 probes (TryRecover owns it), got %d", probeCount)
	}
}

// --- Test: failed probe logs warning but does not transition state ---

func TestOfflineRecoveryProbe_FailedProbeKeepsOffline(t *testing.T) {
	const backendID = "fail-b"

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return nil, &backend.InvokeError{Type: backend.ErrTypeAuth, Raw: "still broken"}
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1),
	})

	bs := r.states[backendID]
	bs.RecordFailure(state.ErrTypeAuth, "auth err", time.Time{},
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)

	r.offlineRecoveryProbe()

	snap := bs.Snapshot()
	if snap.Status != state.StatusOffline {
		t.Errorf("failed probe: want status still StatusOffline, got %s", snap.Status)
	}
	// LastRecoveryProbeAt should have been set (so we don't hammer the backend).
	if snap.LastRecoveryProbeAt.IsZero() {
		t.Error("failed probe: LastRecoveryProbeAt should be set after probe attempt")
	}
}

// --- Test: soft-failure cascade to OFFLINE self-heals ---

func TestOfflineRecoveryProbe_SoftFailureCascadeHeals(t *testing.T) {
	const backendID = "soft-b"

	r := newTestRouter(backendID, func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{
		OfflineRecoveryProbeIntervalSeconds: offlineRecoveryInterval(1),
	})

	bs := r.states[backendID]
	// Drive through offline threshold with unknown-type failures (soft failure cascade).
	for i := 0; i < r.cfg.Routing.OfflineFailureThreshold+1; i++ {
		bs.RecordFailure(state.ErrTypeUnknown, "network glitch", time.Time{},
			r.cfg.Routing.DegradedFailureThreshold,
			r.cfg.Routing.OfflineFailureThreshold,
		)
	}
	if bs.Snapshot().Status != state.StatusOffline {
		t.Fatalf("precondition: want StatusOffline after %d failures, got %s",
			r.cfg.Routing.OfflineFailureThreshold+1, bs.Snapshot().Status)
	}
	// No pending reset — soft failure has no reset time.
	if bs.HasPendingReset() {
		t.Fatal("precondition: want HasPendingReset()=false for soft-failure offline")
	}

	r.offlineRecoveryProbe()

	if bs.Snapshot().Status != state.StatusRecovering {
		t.Errorf("soft-failure cascade: want StatusRecovering after successful probe, got %s",
			bs.Snapshot().Status)
	}
}

