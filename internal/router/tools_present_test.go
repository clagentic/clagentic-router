// internal/router/tools_present_test.go — tests for tools-presence capture in
// call_log (lr-4aaf2a): Route must record req.HasTools on every LogCall site,
// and LogToolRefusal must record a row for the 422-refusal path where Route
// itself is never invoked.
package router

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

// toolCapableMockAdapter is mockAdapter with Capabilities().SupportsTools
// forced true, for tests that route a HasTools:true request and need the
// single candidate backend to survive Route's sticky-through-fallback
// tool-capability filter (lr-add405) rather than be filtered out before
// Invoke is ever called.
type toolCapableMockAdapter struct {
	mockAdapter
}

func (m *toolCapableMockAdapter) Capabilities() backend.Capabilities {
	return backend.Capabilities{SupportsTools: true}
}

// newStoreBackedTestRouter mirrors newTestRouter (offline_recovery_probe_test.go)
// but wires a real *store.Store so call_log writes are observable.
func newStoreBackedTestRouter(t *testing.T, id string, adapterFn func(ctx context.Context, req *backend.Request) (*backend.Response, error)) (*Router, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// A tool-capable adapter — most of this file's tests set req.HasTools on
	// a single-backend chain, and Route's sticky-through-fallback filter
	// (lr-add405) now excludes a candidate whose adapter is not
	// SupportsTools:true before Invoke is ever attempted.
	adapter := &toolCapableMockAdapter{mockAdapter{id: id, invoke: adapterFn}}
	adapters := map[string]backend.Adapter{id: adapter}
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			id: {Adapter: config.AdapterClaudeCLI, Model: "test-model"},
		},
		Routing: config.RoutingConfig{
			Strategy:                 "scored",
			DegradedFailureThreshold: 3,
			OfflineFailureThreshold:  6,
		},
	}
	r := &Router{
		cfg:      cfg,
		states:   map[string]*state.BackendState{id: state.New(id)},
		adapters: adapters,
		store:    st,
		stopCh:   make(chan struct{}),
	}
	return r, st
}

// TestRoute_SuccessRow_RecordsToolsPresent verifies the success LogCall site
// (Route's happy path) carries req.HasTools through to the stored row.
func TestRoute_SuccessRow_RecordsToolsPresent(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil
	})

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}, HasTools: true}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true on success row, got false")
	}
	if rows[0].Outcome != "pass" {
		t.Errorf("Outcome: want pass, got %q", rows[0].Outcome)
	}
}

// TestRoute_SuccessRow_NoTools_RecordsFalse is the control case: a request
// without tools must persist ToolsPresent=false, not true by default.
func TestRoute_SuccessRow_NoTools_RecordsFalse(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil
	})

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want false, got true")
	}
}

// TestRoute_DegradedRow_RecordsToolsPresent verifies the whole-chain-failed
// LogCall site also carries req.HasTools through.
func TestRoute_DegradedRow_RecordsToolsPresent(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return nil, &backend.InvokeError{Type: backend.ErrTypeUnknown, Raw: "boom"}
	})

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}, HasTools: true}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err == nil {
		t.Fatal("expected Route to fail (chain exhausted)")
	}

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	// One "fallback" row (the failed hop) and one "degraded" row (chain exhausted).
	var foundDegraded bool
	for _, row := range rows {
		if row.Outcome == "degraded" {
			foundDegraded = true
			if !row.ToolsPresent {
				t.Errorf("degraded row ToolsPresent: want true, got false")
			}
		}
		if row.Outcome == "fallback" && !row.ToolsPresent {
			t.Errorf("fallback row ToolsPresent: want true, got false")
		}
	}
	if !foundDegraded {
		t.Fatal("expected a degraded row after chain exhaustion")
	}
}

// TestLogToolRefusal_WritesRow verifies the 422-refusal path (where Route is
// never called) still produces a call_log row with ToolsPresent=true — the
// core acceptance criterion this task exists to satisfy.
func TestLogToolRefusal_WritesRow(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		t.Fatal("adapter Invoke must not be called on the refusal path")
		return nil, nil
	})

	r.LogToolRefusal(context.Background(), []string{"b1"}, "role:some-chain")

	rows, err := st.RecentCalls(store.CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row from LogToolRefusal, got %d", len(rows))
	}
	if !rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want true, got false")
	}
	if rows[0].Outcome != "refused_no_tool_capable_backend" {
		t.Errorf("Outcome: want refused_no_tool_capable_backend, got %q", rows[0].Outcome)
	}
	if rows[0].Model != "role:some-chain" {
		t.Errorf("Model: want role:some-chain, got %q", rows[0].Model)
	}
}

// TestLogToolRefusal_NilStore_NoOp verifies LogToolRefusal does not panic
// when store is nil (mirrors every other store-optional write path in Router).
func TestLogToolRefusal_NilStore_NoOp(t *testing.T) {
	r := newCapTestRouter(map[string]backend.Adapter{
		"a": &capAdapter{id: "a", caps: backend.Capabilities{SupportsTools: false}},
	}, nil, nil)

	r.LogToolRefusal(context.Background(), []string{"a"}, "backend:a") // must not panic
}
