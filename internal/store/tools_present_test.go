// internal/store/tools_present_test.go — tests for the call_log.tools_present
// column (lr-4aaf2a): presence-only capture of whether a routed request
// carried tools, including migration onto an existing populated database.
package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestLogCall_ToolsPresent_RoundTrips verifies LogCall writes the
// tools_present bit and RecentCalls reads it back correctly for both true
// and false, on a freshly-created database.
func TestLogCall_ToolsPresent_RoundTrips(t *testing.T) {
	s := tempStore(t)

	s.LogCall(CallLogInput{BackendID: "b1", Outcome: "pass", ToolsPresent: true})
	s.LogCall(CallLogInput{BackendID: "b1", Outcome: "pass", ToolsPresent: false})

	rows, err := s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// RecentCalls returns reverse chronological order, so rows[0] is the
	// second LogCall (ToolsPresent: false), rows[1] the first (true).
	if rows[0].ToolsPresent {
		t.Errorf("rows[0].ToolsPresent: want false, got true")
	}
	if !rows[1].ToolsPresent {
		t.Errorf("rows[1].ToolsPresent: want true, got false")
	}
}

// TestLogCall_ToolsPresent_DefaultsFalse verifies a CallLogInput that does
// not set ToolsPresent (the zero value) persists as false, matching every
// pre-existing call site that predates this field.
func TestLogCall_ToolsPresent_DefaultsFalse(t *testing.T) {
	s := tempStore(t)
	s.LogCall(CallLogInput{BackendID: "b1", Outcome: "pass"})

	rows, err := s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].ToolsPresent {
		t.Errorf("ToolsPresent: want false (zero value), got true")
	}
}

// TestMigrateCallLog_ToolsPresent_AppliesToExistingPopulatedDatabase is the
// acceptance-critical regression: the migration must apply cleanly to a
// database that (a) predates the tools_present column and (b) already has
// rows in call_log, not just to a fresh database. It builds a call_log table
// using the pre-lr-4aaf2a column set (mirroring the schema before this
// column existed), inserts a row, reopens via Open (which runs
// migrateCallLog), and verifies the existing row is still readable with
// tools_present defaulting to 0/false and a new row can be written with the
// new column populated.
func TestMigrateCallLog_ToolsPresent_AppliesToExistingPopulatedDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pre_migration.db")

	// Build a call_log table using the schema shape that existed immediately
	// before this task (all columns through fallback_count, no tools_present).
	preMigrationSchema := `
CREATE TABLE call_log (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                    TEXT NOT NULL,
    backend_id            TEXT NOT NULL,
    tier_alias            TEXT NOT NULL DEFAULT '',
    chain_position        INTEGER NOT NULL DEFAULT 0,
    outcome               TEXT NOT NULL,
    error_type            TEXT NOT NULL DEFAULT '',
    prompt_tokens_est     INTEGER NOT NULL DEFAULT 0,
    completion_tokens_est INTEGER NOT NULL DEFAULT 0,
    latency_ms            INTEGER NOT NULL DEFAULT 0,
    cost_usd_est          REAL NOT NULL DEFAULT 0,
    model                 TEXT NOT NULL DEFAULT '',
    score                 REAL NOT NULL DEFAULT 0,
    request_id            TEXT NOT NULL DEFAULT '',
    rate_limit_type       TEXT NOT NULL DEFAULT '',
    utilization           REAL,
    fallback_count        INTEGER NOT NULL DEFAULT 0
);
`
	setupDB, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_timeout=5000")
	if err != nil {
		t.Fatalf("open setup db: %v", err)
	}
	if _, err := setupDB.Exec(preMigrationSchema); err != nil {
		t.Fatalf("create pre-migration schema: %v", err)
	}
	// Populate a real row before the column exists, exercising the "existing
	// populated database" acceptance criterion, not just an empty table.
	if _, err := setupDB.Exec(`
		INSERT INTO call_log (ts, backend_id, tier_alias, chain_position, outcome, error_type,
		                      prompt_tokens_est, completion_tokens_est, latency_ms, cost_usd_est,
		                      model, score, request_id, rate_limit_type, utilization, fallback_count)
		VALUES ('2026-01-01T00:00:00Z','b1','haiku',0,'pass','',100,50,120,0.001,
		        'test-model',0.9,'req-1','',NULL,0)`); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	if err := setupDB.Close(); err != nil {
		t.Fatalf("close setup db: %v", err)
	}

	// Open through the real Open() path — this is what runs migrateCallLog
	// against the existing populated database.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open on pre-migration database: %v", err)
	}
	defer s.db.Close()

	rows, err := s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls after migration: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 pre-existing row survived migration, got %d", len(rows))
	}
	if rows[0].BackendID != "b1" || rows[0].RequestID != "req-1" {
		t.Errorf("pre-existing row data corrupted by migration: %+v", rows[0])
	}
	if rows[0].ToolsPresent {
		t.Errorf("pre-existing row (predates tools_present): want ToolsPresent=false default, got true")
	}

	// A new row written after migration must carry tools_present correctly.
	s.LogCall(CallLogInput{BackendID: "b2", Outcome: "pass", ToolsPresent: true, RequestID: "req-2"})
	rows, err = s.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls after post-migration write: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows after post-migration write, got %d", len(rows))
	}
	var found bool
	for _, r := range rows {
		if r.RequestID == "req-2" {
			found = true
			if !r.ToolsPresent {
				t.Errorf("post-migration row: want ToolsPresent=true, got false")
			}
		}
	}
	if !found {
		t.Fatal("post-migration row (req-2) not found in RecentCalls result")
	}
}

// TestMigrateCallLog_ToolsPresent_IdempotentOnAlreadyMigratedDatabase verifies
// re-running Open (and therefore migrateCallLog) against a database that
// already has tools_present does not error — the duplicate-column-name guard
// must cover this new column exactly like the existing Phase 6 Slice A ones.
func TestMigrateCallLog_ToolsPresent_IdempotentOnAlreadyMigratedDatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "idempotent.db")

	s1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.LogCall(CallLogInput{BackendID: "b1", Outcome: "pass", ToolsPresent: true})
	if err := s1.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second Open (re-migration) should not error: %v", err)
	}
	defer s2.db.Close()

	rows, err := s2.RecentCalls(CallLogFilter{Limit: 10})
	if err != nil {
		t.Fatalf("RecentCalls: %v", err)
	}
	if len(rows) != 1 || !rows[0].ToolsPresent {
		t.Errorf("expected 1 row with ToolsPresent=true to survive re-open, got %+v", rows)
	}
}
