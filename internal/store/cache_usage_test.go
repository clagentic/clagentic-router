// internal/store/cache_usage_test.go — tests for RecordCacheUsage/AllCacheUsage.
package store

import (
	"context"
	"testing"
)

// TestRecordCacheUsage_ReportedAggregates verifies that multiple reported
// calls to the same (backend, model) accumulate token totals and the
// calls_reported counter, while calls_unsupported stays at zero.
func TestRecordCacheUsage_ReportedAggregates(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	s.RecordCacheUsage(ctx, "backend-a", CacheUsageInput{
		Model: "claude-opus-4-8", Reported: true,
		InputTokens: 100, CacheReadTokens: 70, CacheWriteTokens: 30,
	})
	s.RecordCacheUsage(ctx, "backend-a", CacheUsageInput{
		Model: "claude-opus-4-8", Reported: true,
		InputTokens: 50, CacheReadTokens: 50, CacheWriteTokens: 0,
	})

	rows, err := s.AllCacheUsage(ctx)
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.BackendID != "backend-a" || r.Model != "claude-opus-4-8" {
		t.Errorf("unexpected key: %s/%s", r.BackendID, r.Model)
	}
	if r.InputTokensTotal != 150 {
		t.Errorf("InputTokensTotal = %d, want 150", r.InputTokensTotal)
	}
	if r.CacheReadTokensTotal != 120 {
		t.Errorf("CacheReadTokensTotal = %d, want 120", r.CacheReadTokensTotal)
	}
	if r.CacheWriteTokensTotal != 30 {
		t.Errorf("CacheWriteTokensTotal = %d, want 30", r.CacheWriteTokensTotal)
	}
	if r.CallsReported != 2 {
		t.Errorf("CallsReported = %d, want 2", r.CallsReported)
	}
	if r.CallsUnsupported != 0 {
		t.Errorf("CallsUnsupported = %d, want 0", r.CallsUnsupported)
	}

	rate, ok := r.HitRate()
	if !ok {
		t.Fatal("expected HitRate to report ok=true when InputTokensTotal > 0")
	}
	if want := 120.0 / 150.0; rate != want {
		t.Errorf("HitRate = %v, want %v", rate, want)
	}
}

// TestRecordCacheUsage_UnsupportedNeverPollutesTotals verifies the core
// acceptance criterion: an unsupported call (Reported: false) must
// increment calls_unsupported ONLY, never touch token totals or
// calls_reported — collapsing the two would make a zero indistinguishable
// from a real cache miss.
func TestRecordCacheUsage_UnsupportedNeverPollutesTotals(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	s.RecordCacheUsage(ctx, "backend-b", CacheUsageInput{
		Model: "gemini-2.5-flash", Reported: false,
	})
	s.RecordCacheUsage(ctx, "backend-b", CacheUsageInput{
		Model: "gemini-2.5-flash", Reported: false,
	})

	rows, err := s.AllCacheUsage(ctx)
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.CallsReported != 0 {
		t.Errorf("CallsReported = %d, want 0 for an unsupported-only backend/model", r.CallsReported)
	}
	if r.CallsUnsupported != 2 {
		t.Errorf("CallsUnsupported = %d, want 2", r.CallsUnsupported)
	}
	if r.InputTokensTotal != 0 || r.CacheReadTokensTotal != 0 || r.CacheWriteTokensTotal != 0 {
		t.Errorf("expected zero token totals for an unsupported-only backend/model, got %+v", r)
	}

	// HitRate must report ok=false here — there is no reported data to
	// compute a rate from, and a caller must not treat this as "0% hit rate".
	if _, ok := r.HitRate(); ok {
		t.Error("expected HitRate ok=false when no calls have ever reported data")
	}
}

// TestRecordCacheUsage_MixedReportedAndUnsupported verifies a backend/model
// pair that has BOTH reported and unsupported calls over its lifetime (e.g.
// an operator switched adapters) keeps the two counters independently
// correct, and HitRate is computed only from the reported portion.
func TestRecordCacheUsage_MixedReportedAndUnsupported(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	s.RecordCacheUsage(ctx, "backend-c", CacheUsageInput{
		Model: "shared-model", Reported: true,
		InputTokens: 100, CacheReadTokens: 0, CacheWriteTokens: 0,
	})
	s.RecordCacheUsage(ctx, "backend-c", CacheUsageInput{
		Model: "shared-model", Reported: false,
	})

	rows, err := s.AllCacheUsage(ctx)
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.CallsReported != 1 || r.CallsUnsupported != 1 {
		t.Errorf("CallsReported=%d CallsUnsupported=%d, want 1/1", r.CallsReported, r.CallsUnsupported)
	}
	rate, ok := r.HitRate()
	if !ok {
		t.Fatal("expected HitRate ok=true — there is reported data")
	}
	if rate != 0 {
		t.Errorf("HitRate = %v, want 0 (the one reported call was a genuine miss)", rate)
	}
}

// TestAllCacheUsage_MultipleBackendsAndModels verifies rows are kept
// separate per (backend_id, model) and returned in stable order.
func TestAllCacheUsage_MultipleBackendsAndModels(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	s.RecordCacheUsage(ctx, "backend-a", CacheUsageInput{Model: "model-1", Reported: true, InputTokens: 10, CacheReadTokens: 5})
	s.RecordCacheUsage(ctx, "backend-a", CacheUsageInput{Model: "model-2", Reported: true, InputTokens: 20, CacheReadTokens: 10})
	s.RecordCacheUsage(ctx, "backend-b", CacheUsageInput{Model: "model-1", Reported: true, InputTokens: 30, CacheReadTokens: 15})

	rows, err := s.AllCacheUsage(ctx)
	if err != nil {
		t.Fatalf("AllCacheUsage: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 distinct (backend,model) rows, got %d: %+v", len(rows), rows)
	}
}
