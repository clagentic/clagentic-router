// internal/router/cache_usage_test.go — tests for recordCacheUsage wiring
// (lr-718af0): opt-in gating via CacheMetrics.Enabled, nil-store no-op, and
// correct translation of backend.CacheUsage (including the nil case) into
// store.CacheUsageInput.
package router

import (
	"context"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
)

// TestRoute_CacheUsage_DisabledByDefault verifies that with
// CacheMetrics.Enabled left at its zero value (false — the opt-in default,
// see config.CacheMetricsConfig doc), a successful Route call writes no
// cache_usage row at all, even though the adapter returned real CacheUsage
// data. This is the "unconfigured install has this feature fully off"
// acceptance criterion.
func TestRoute_CacheUsage_DisabledByDefault(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{
			Content:    "ok",
			CacheUsage: &backend.CacheUsage{InputTokens: 100, CacheReadTokens: 70, CacheWriteTokens: 30},
		}, nil
	})
	// CacheMetrics.Enabled is false by default on this test's cfg (zero value) —
	// no explicit assignment needed, this IS the case under test.

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rows, err := st.AllCacheUsage(context.Background())
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 cache_usage rows when CacheMetrics.Enabled is false, got %d: %+v", len(rows), rows)
	}
}

// TestRoute_CacheUsage_EnabledRecordsReported verifies that with
// CacheMetrics.Enabled true, a successful call with non-nil CacheUsage
// writes a reported row with the correct model/backend key and token
// totals.
func TestRoute_CacheUsage_EnabledRecordsReported(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{
			Content:    "ok",
			CacheUsage: &backend.CacheUsage{InputTokens: 100, CacheReadTokens: 70, CacheWriteTokens: 30},
		}, nil
	})
	r.cfg.CacheMetrics.Enabled = true

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rows, err := st.AllCacheUsage(context.Background())
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 cache_usage row, got %d", len(rows))
	}
	row := rows[0]
	if row.BackendID != "b1" || row.Model != "test-model" {
		t.Errorf("unexpected key: %s/%s", row.BackendID, row.Model)
	}
	if row.CallsReported != 1 || row.CallsUnsupported != 0 {
		t.Errorf("CallsReported=%d CallsUnsupported=%d, want 1/0", row.CallsReported, row.CallsUnsupported)
	}
	if row.InputTokensTotal != 100 || row.CacheReadTokensTotal != 70 || row.CacheWriteTokensTotal != 30 {
		t.Errorf("unexpected totals: %+v", row)
	}
}

// TestRoute_CacheUsage_EnabledRecordsUnsupported verifies that with
// CacheMetrics.Enabled true, a successful call whose adapter returned a nil
// CacheUsage (an unsupported family, e.g. gemini_cli/ollama_http) is
// recorded as calls_unsupported, never folded into calls_reported or the
// token totals — the core nil-vs-zero distinction this task is built
// around, verified end-to-end through the router's translation.
func TestRoute_CacheUsage_EnabledRecordsUnsupported(t *testing.T) {
	r, st := newStoreBackedTestRouter(t, "b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil // CacheUsage left nil
	})
	r.cfg.CacheMetrics.Enabled = true

	req := &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}
	if _, _, err := r.Route(context.Background(), req, []string{"b1"}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	rows, err := st.AllCacheUsage(context.Background())
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 cache_usage row, got %d", len(rows))
	}
	row := rows[0]
	if row.CallsReported != 0 {
		t.Errorf("CallsReported = %d, want 0 for a nil-CacheUsage response", row.CallsReported)
	}
	if row.CallsUnsupported != 1 {
		t.Errorf("CallsUnsupported = %d, want 1", row.CallsUnsupported)
	}
	if row.InputTokensTotal != 0 || row.CacheReadTokensTotal != 0 || row.CacheWriteTokensTotal != 0 {
		t.Errorf("expected zero token totals for an unsupported call, got %+v", row)
	}
}

// TestRecordCacheUsage_NilStoreNoop verifies recordCacheUsage never panics
// or blocks when r.store is nil — mirrors every other store write in this
// package (fire-and-forget, safe with no persistence configured).
func TestRecordCacheUsage_NilStoreNoop(t *testing.T) {
	r := newTestRouter("b1", func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{Content: "ok"}, nil
	}, config.RoutingConfig{})
	r.cfg.CacheMetrics.Enabled = true
	// r.store is nil here (newTestRouter does not wire one) — this must not panic.
	r.recordCacheUsage(context.Background(), "b1", &backend.Response{
		CacheUsage: &backend.CacheUsage{InputTokens: 10},
	})
}
