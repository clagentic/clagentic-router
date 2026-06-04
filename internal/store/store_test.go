// internal/store/store_test.go — unit tests for Store call log methods.
package store

import (
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

// logCall is a helper that inserts one row with the given ts (RFC3339) directly.
func insertLogRow(t *testing.T, s *Store, ts, backendID, outcome string, latencyMS int, costUSD float64) {
	t.Helper()
	_, err := s.db.Exec(`
		INSERT INTO call_log (ts, backend_id, tier_alias, chain_position, outcome, error_type,
		                      prompt_tokens_est, completion_tokens_est, latency_ms, cost_usd_est)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		ts, backendID, "haiku", 0, outcome, "", 100, 50, latencyMS, costUSD,
	)
	if err != nil {
		t.Fatalf("insert log row: %v", err)
	}
}

// --- RecentCalls ---

func TestRecentCalls_NoFilter_ReturnsAll(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		insertLogRow(t, s, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339), "b1", "pass", 100, 0.001)
	}
	rows, err := s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("want 5 rows, got %d", len(rows))
	}
}

func TestRecentCalls_BackendFilter(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	insertLogRow(t, s, now, "b1", "pass", 100, 0.001)
	insertLogRow(t, s, now, "b2", "pass", 200, 0.002)
	insertLogRow(t, s, now, "b1", "error", 150, 0)

	rows, err := s.RecentCalls(CallLogFilter{BackendID: "b1", Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("want 2 rows for b1, got %d", len(rows))
	}
	for _, r := range rows {
		if r.BackendID != "b1" {
			t.Errorf("unexpected backend_id %q in result", r.BackendID)
		}
	}
}

func TestRecentCalls_FromFilter(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertLogRow(t, s, base.Format(time.RFC3339), "b1", "pass", 100, 0)
	insertLogRow(t, s, base.Add(time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)
	insertLogRow(t, s, base.Add(2*time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)

	rows, err := s.RecentCalls(CallLogFilter{From: base.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("want 2 rows from base+1h, got %d", len(rows))
	}
}

func TestRecentCalls_ToFilter(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertLogRow(t, s, base.Format(time.RFC3339), "b1", "pass", 100, 0)
	insertLogRow(t, s, base.Add(time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)
	insertLogRow(t, s, base.Add(2*time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)

	// to is exclusive: want only rows < base+2h = 2 rows
	rows, err := s.RecentCalls(CallLogFilter{To: base.Add(2 * time.Hour), Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("want 2 rows before base+2h (exclusive), got %d", len(rows))
	}
}

func TestRecentCalls_FromToFilter(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		insertLogRow(t, s, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)
	}

	// want rows in [base+1h, base+3h) = 2 rows
	rows, err := s.RecentCalls(CallLogFilter{
		From:  base.Add(time.Hour),
		To:    base.Add(3 * time.Hour),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("want 2 rows in [base+1h, base+3h), got %d", len(rows))
	}
}

func TestRecentCalls_LimitDefault(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		insertLogRow(t, s, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339), "b1", "pass", 100, 0)
	}
	rows, err := s.RecentCalls(CallLogFilter{}) // limit=0 → default 50
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 50 {
		t.Errorf("want 50 rows (default limit), got %d", len(rows))
	}
}

func TestRecentCalls_LimitCap(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		insertLogRow(t, s, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339), "b1", "pass", 100, 0)
	}
	rows, err := s.RecentCalls(CallLogFilter{Limit: 9999}) // capped at 500
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	// only 10 rows exist so we get all 10, but the cap was applied
	if len(rows) != 10 {
		t.Errorf("want 10 rows, got %d", len(rows))
	}
}

func TestRecentCalls_ReverseChronological(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		insertLogRow(t, s, base.Add(time.Duration(i)*time.Hour).Format(time.RFC3339), "b1", "pass", 100, 0)
	}
	rows, err := s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	// Newest first: rows[0].TS > rows[1].TS > rows[2].TS
	if rows[0].TS < rows[1].TS {
		t.Errorf("expected reverse chronological order; rows[0].TS=%s rows[1].TS=%s", rows[0].TS, rows[1].TS)
	}
}

// --- CallStatsFor ---

func TestCallStatsFor_Empty(t *testing.T) {
	s := tempStore(t)
	stats, err := s.CallStatsFor(CallLogFilter{})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	if stats.TotalCalls != 0 {
		t.Errorf("expected TotalCalls=0 on empty store, got %d", stats.TotalCalls)
	}
}

// TestCallStatsFor_OutcomeClassification verifies that only "pass" counts as a
// success, and that the failure outcomes the router actually writes ("fallback",
// "degraded", "error") all count as errors.
//
// Router outcome vocabulary (router.go):
//
//	"pass"     — call succeeded on this backend (primary or fallback hop).
//	"fallback" — this hop failed; router advanced the chain to the next backend.
//	"degraded" — entire chain exhausted; no backend produced a response.
//	"error"    — generic error bucket.
func TestCallStatsFor_OutcomeClassification(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// Two successful calls.
	insertLogRow(t, s, now, "b1", "pass", 100, 0.001)
	insertLogRow(t, s, now, "b2", "pass", 200, 0.002)

	// Three failure outcomes that the router writes.
	insertLogRow(t, s, now, "b1", "fallback", 50, 0)  // hop failed, chain advanced
	insertLogRow(t, s, now, "b1", "degraded", 0, 0)   // whole chain exhausted
	insertLogRow(t, s, now, "b1", "error", 30, 0)     // generic error bucket

	stats, err := s.CallStatsFor(CallLogFilter{})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	if stats.TotalCalls != 5 {
		t.Errorf("TotalCalls: want 5, got %d", stats.TotalCalls)
	}
	if stats.SuccessCalls != 2 {
		t.Errorf("SuccessCalls: want 2 (only 'pass'), got %d", stats.SuccessCalls)
	}
	if stats.ErrorCalls != 3 {
		t.Errorf("ErrorCalls: want 3 (fallback+degraded+error), got %d", stats.ErrorCalls)
	}
}

// TestCallStatsFor_PassOnlyIsSuccess is a focused regression for the original
// bug where the store checked outcome == "pass" (a string the router never
// writes) causing every real successful call to be miscounted as an error.
func TestCallStatsFor_PassOnlyIsSuccess(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	insertLogRow(t, s, now, "b1", "pass", 100, 0.001)

	stats, err := s.CallStatsFor(CallLogFilter{})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	if stats.SuccessCalls != 1 {
		t.Errorf("SuccessCalls: want 1 for outcome='pass', got %d (regression: was checking for 'success' which router never writes)", stats.SuccessCalls)
	}
	if stats.ErrorCalls != 0 {
		t.Errorf("ErrorCalls: want 0 when only outcome is 'pass', got %d", stats.ErrorCalls)
	}
}

func TestCallStatsFor_AvgLatency(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	insertLogRow(t, s, now, "b1", "pass", 100, 0)
	insertLogRow(t, s, now, "b1", "pass", 200, 0)
	insertLogRow(t, s, now, "b1", "pass", 300, 0)

	stats, err := s.CallStatsFor(CallLogFilter{})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	want := 200.0
	if stats.AvgLatencyMS != want {
		t.Errorf("AvgLatencyMS: want %.1f, got %.1f", want, stats.AvgLatencyMS)
	}
}

func TestCallStatsFor_TotalCost(t *testing.T) {
	s := tempStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	insertLogRow(t, s, now, "b1", "pass", 100, 0.01)
	insertLogRow(t, s, now, "b1", "pass", 200, 0.02)

	stats, err := s.CallStatsFor(CallLogFilter{})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	if stats.TotalCostUSD < 0.029 || stats.TotalCostUSD > 0.031 {
		t.Errorf("TotalCostUSD: want ~0.03, got %v", stats.TotalCostUSD)
	}
}

func TestCallStatsFor_DateRangeFilter(t *testing.T) {
	s := tempStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertLogRow(t, s, base.Format(time.RFC3339), "b1", "pass", 100, 0)                      // outside
	insertLogRow(t, s, base.Add(time.Hour).Format(time.RFC3339), "b1", "pass", 200, 0)       // inside
	insertLogRow(t, s, base.Add(2*time.Hour).Format(time.RFC3339), "b1", "pass", 300, 0)     // inside
	insertLogRow(t, s, base.Add(3*time.Hour).Format(time.RFC3339), "b1", "pass", 400, 0)     // outside

	stats, err := s.CallStatsFor(CallLogFilter{
		From: base.Add(time.Hour),
		To:   base.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CallStatsFor: %v", err)
	}
	if stats.TotalCalls != 2 {
		t.Errorf("TotalCalls: want 2 in range, got %d", stats.TotalCalls)
	}
	if stats.AvgLatencyMS != 250.0 {
		t.Errorf("AvgLatencyMS: want 250.0, got %.1f", stats.AvgLatencyMS)
	}
}

func TestJoinAnd(t *testing.T) {
	cases := []struct {
		input []string
		want  string
	}{
		{[]string{"a = ?"}, "a = ?"},
		{[]string{"a = ?", "b = ?"}, "a = ? AND b = ?"},
		{[]string{"a = ?", "b = ?", "c = ?"}, "a = ? AND b = ? AND c = ?"},
	}
	for _, tc := range cases {
		if got := joinAnd(tc.input); got != tc.want {
			t.Errorf("joinAnd(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// Ensure tempStore actually writes to a file (not in-memory) so cross-function state is real.
func TestStore_PersistenceAcrossOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	insertLogRow(t, s, now, "b1", "pass", 100, 0.001)
	s.db.Close()

	// Reopen
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.db.Close()

	rows, err := s2.RecentCalls(CallLogFilter{})
	if err != nil {
		t.Fatalf("RecentCalls after reopen: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 persisted row, got %d", len(rows))
	}
}

