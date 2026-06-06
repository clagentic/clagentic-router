// internal/store/store.go — SQLite persistence for backend state and call log.
//
// Store provides best-effort persistence. The router never blocks on store
// operations — all writes are fire-and-forget (errors are logged, not returned).
// State is read at startup; writes happen on every state transition + periodic flush.
package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/clagentic/clagentic-router/internal/state"
)

const schema = `
CREATE TABLE IF NOT EXISTS backend_states (
    backend_id             TEXT PRIMARY KEY,
    status                 TEXT NOT NULL DEFAULT 'unknown',
    consecutive_failures   INTEGER NOT NULL DEFAULT 0,
    last_success_at        TEXT,
    last_failure_at        TEXT,
    last_error_type        TEXT NOT NULL DEFAULT '',
    last_error_raw         TEXT NOT NULL DEFAULT '',
    quota_exhausted        INTEGER NOT NULL DEFAULT 0,
    quota_reset_at         TEXT,
    quota_tokens_remaining INTEGER NOT NULL DEFAULT -1,
    quota_tokens_total     INTEGER NOT NULL DEFAULT -1,
    rate_limit_reset_at    TEXT,
    rate_window_messages   INTEGER NOT NULL DEFAULT 0,
    rate_window_tokens_est INTEGER NOT NULL DEFAULT 0,
    rate_window_start      TEXT,
    total_calls            INTEGER NOT NULL DEFAULT 0,
    total_tokens_est       INTEGER NOT NULL DEFAULT 0,
    total_cost_usd_est     REAL NOT NULL DEFAULT 0,
    updated_at             TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS call_log (
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
    -- Routing intelligence fields (added Phase 6 Slice A)
    model                 TEXT NOT NULL DEFAULT '',     -- model string used for this request
    score                 REAL NOT NULL DEFAULT 0,      -- router score assigned to this backend
    request_id            TEXT NOT NULL DEFAULT '',     -- HTTP request_id for correlation
    rate_limit_type       TEXT NOT NULL DEFAULT '',     -- active bucket at routing time (seven_day, five_hour, etc.)
    utilization           REAL,                         -- utilization at routing time; NULL if unknown
    fallback_count        INTEGER NOT NULL DEFAULT 0    -- number of backends tried before this one
);

CREATE INDEX IF NOT EXISTS call_log_ts      ON call_log(ts);
CREATE INDEX IF NOT EXISTS call_log_backend ON call_log(backend_id, ts);
-- call_log_request_id and call_log_model are created in migrateCallLog
-- after the new columns are guaranteed to exist.

CREATE TABLE IF NOT EXISTS webhooks (
    id         TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    events     TEXT NOT NULL,
    secret     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
`

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath.
func Open(dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", filepath.Dir(dbPath), err)
	}
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	if _, err := db.Exec(quotaSnapshotsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init quota_snapshots schema: %w", err)
	}
	if err := migrateCallLog(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate call_log: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateCallLog adds columns introduced in Phase 6 Slice A to an existing
// call_log table. SQLite does not support IF NOT EXISTS on ALTER TABLE ADD COLUMN,
// so we attempt each column and ignore "duplicate column name" errors — those
// mean the column already exists (fresh DB or already migrated).
func migrateCallLog(db *sql.DB) error {
	migrations := []string{
		`ALTER TABLE call_log ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE call_log ADD COLUMN score REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE call_log ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE call_log ADD COLUMN rate_limit_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE call_log ADD COLUMN utilization REAL`,
		`ALTER TABLE call_log ADD COLUMN fallback_count INTEGER NOT NULL DEFAULT 0`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			// "duplicate column name" means the column already exists — safe to ignore.
			if !isDuplicateColumnErr(err) {
				return fmt.Errorf("%s: %w", m, err)
			}
		}
	}
	// Add new indexes (CREATE INDEX IF NOT EXISTS is idempotent).
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS call_log_request_id ON call_log(request_id)`,
		`CREATE INDEX IF NOT EXISTS call_log_model ON call_log(model, ts)`,
	}
	for _, idx := range indexes {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("%s: %w", idx, err)
		}
	}
	return nil
}

// isDuplicateColumnErr reports whether err is a SQLite "duplicate column name" error.
func isDuplicateColumnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveState upserts one backend's state.
func (s *Store) SaveState(snap state.Snapshot) {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO backend_states (
			backend_id, status, consecutive_failures,
			last_success_at, last_failure_at,
			last_error_type, last_error_raw,
			quota_exhausted, quota_reset_at,
			quota_tokens_remaining, quota_tokens_total,
			rate_limit_reset_at, rate_window_messages, rate_window_tokens_est,
			rate_window_start, total_calls, total_tokens_est, total_cost_usd_est,
			updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		snap.BackendID, string(snap.Status), snap.ConsecutiveFailures,
		nullableTime(snap.LastSuccessAt), nullableTime(snap.LastFailureAt),
		string(snap.LastErrorType), snap.LastErrorRaw,
		boolToInt(snap.QuotaExhausted), nullableTime(snap.QuotaResetAt),
		snap.QuotaTokensRemaining, snap.QuotaTokensTotal,
		nullableTime(snap.RateLimitResetAt), snap.RateWindowMessages, snap.RateWindowTokensEst,
		nullableTime(snap.RateWindowStart), snap.TotalCalls, snap.TotalTokensEst, snap.TotalCostUSDEst,
		snap.UpdatedAt.Format(time.RFC3339),
	)
	if err != nil {
		slog.Warn("store: save_state failed", "backend", snap.BackendID, "err", err)
	}
}

// LoadState loads a backend's persisted state. Returns error if not found.
func (s *Store) LoadState(backendID string) (state.Snapshot, error) {
	row := s.db.QueryRow(`
		SELECT status, consecutive_failures,
		       last_success_at, last_failure_at,
		       last_error_type, last_error_raw,
		       quota_exhausted, quota_reset_at,
		       quota_tokens_remaining, quota_tokens_total,
		       rate_limit_reset_at, rate_window_messages, rate_window_tokens_est,
		       rate_window_start, total_calls, total_tokens_est, total_cost_usd_est,
		       updated_at
		FROM backend_states WHERE backend_id = ?`, backendID)

	var snap state.Snapshot
	snap.BackendID = backendID

	var status, lastSuccessAt, lastFailureAt, lastErrorType, lastErrorRaw string
	var quotaResetAt, rateLimitResetAt, rateWindowStart, updatedAt sql.NullString
	var quotaExhausted int

	err := row.Scan(
		&status, &snap.ConsecutiveFailures,
		&lastSuccessAt, &lastFailureAt,
		&lastErrorType, &lastErrorRaw,
		&quotaExhausted, &quotaResetAt,
		&snap.QuotaTokensRemaining, &snap.QuotaTokensTotal,
		&rateLimitResetAt, &snap.RateWindowMessages, &snap.RateWindowTokensEst,
		&rateWindowStart, &snap.TotalCalls, &snap.TotalTokensEst, &snap.TotalCostUSDEst,
		&updatedAt,
	)
	if err == sql.ErrNoRows {
		return snap, fmt.Errorf("not found")
	}
	if err != nil {
		return snap, fmt.Errorf("store: load_state %s: %w", backendID, err)
	}

	snap.Status = state.Status(status)
	snap.LastErrorType = state.ErrorType(lastErrorType)
	snap.LastErrorRaw = lastErrorRaw
	snap.QuotaExhausted = quotaExhausted != 0
	snap.LastSuccessAt = parseTime(lastSuccessAt)
	snap.LastFailureAt = parseTime(lastFailureAt)
	snap.QuotaResetAt = parseNullTime(quotaResetAt)
	snap.RateLimitResetAt = parseNullTime(rateLimitResetAt)
	snap.RateWindowStart = parseNullTime(rateWindowStart)
	snap.UpdatedAt = parseTime(updatedAt.String)

	return snap, nil
}

// CallLogInput carries all fields for one call_log row.
// Using a struct avoids a long positional argument list and makes adding fields
// non-breaking (callers set only the fields they have).
type CallLogInput struct {
	BackendID           string
	TierAlias           string
	ChainPosition       int
	Outcome             string
	ErrorType           string
	PromptTokensEst     int
	CompletionTokensEst int
	LatencyMS           int
	CostUSD             float64
	// Routing intelligence fields (Phase 6 Slice A)
	Model         string   // model string used for this request
	Score         float64  // router score assigned at selection time
	RequestID     string   // HTTP request_id for cross-row correlation
	RateLimitType string   // active quota bucket at routing time
	Utilization   *float64 // utilization at routing time; nil if unknown
	FallbackCount int      // backends tried before this one succeeded/failed
}

// LogCall appends one row to the call log.
func (s *Store) LogCall(in CallLogInput) {
	_, err := s.db.Exec(`
		INSERT INTO call_log (ts, backend_id, tier_alias, chain_position, outcome, error_type,
		                      prompt_tokens_est, completion_tokens_est, latency_ms, cost_usd_est,
		                      model, score, request_id, rate_limit_type, utilization, fallback_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339),
		in.BackendID, in.TierAlias, in.ChainPosition, in.Outcome, in.ErrorType,
		in.PromptTokensEst, in.CompletionTokensEst, in.LatencyMS, in.CostUSD,
		in.Model, in.Score, in.RequestID, in.RateLimitType, in.Utilization, in.FallbackCount,
	)
	if err != nil {
		slog.Warn("store: log_call failed", "err", err)
	}
}

// CallLogFilter controls which rows RecentCalls returns.
// Zero values mean "no filter": From/To zero = no time bound, BackendID "" = all backends.
type CallLogFilter struct {
	BackendID string
	From      time.Time // inclusive lower bound on ts (UTC RFC3339); zero = no lower bound
	To        time.Time // exclusive upper bound on ts (UTC RFC3339); zero = no upper bound
	Limit     int       // max rows returned; 0 → default 50, capped at 500
}

// RecentCalls returns call log rows matching f, in reverse chronological order.
// The BackendID field on f replaces the old backendID parameter; callers that
// only need a simple recent list can pass a CallLogFilter with only Limit set.
func (s *Store) RecentCalls(f CallLogFilter) ([]CallLogRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	q := `SELECT ts, backend_id, tier_alias, chain_position, outcome, error_type,
	             prompt_tokens_est, completion_tokens_est, latency_ms, cost_usd_est,
	             model, score, request_id, rate_limit_type, utilization, fallback_count
	      FROM call_log`
	args := []interface{}{}
	var where []string

	if f.BackendID != "" {
		where = append(where, "backend_id = ?")
		args = append(args, f.BackendID)
	}
	if !f.From.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339))
	}
	if !f.To.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, f.To.UTC().Format(time.RFC3339))
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CallLogRow
	for rows.Next() {
		var r CallLogRow
		if err := rows.Scan(
			&r.TS, &r.BackendID, &r.TierAlias, &r.ChainPosition,
			&r.Outcome, &r.ErrorType, &r.PromptTokensEst, &r.CompletionTokensEst,
			&r.LatencyMS, &r.CostUSD,
			&r.Model, &r.Score, &r.RequestID, &r.RateLimitType, &r.Utilization, &r.FallbackCount,
		); err != nil {
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// CallStats is aggregated statistics over a set of call log rows matching a filter.
type CallStats struct {
	TotalCalls          int     `json:"total_calls"`
	SuccessCalls        int     `json:"success_calls"`
	ErrorCalls          int     `json:"error_calls"`
	TotalPromptTokens   int     `json:"total_prompt_tokens_est"`
	TotalComplTokens    int     `json:"total_completion_tokens_est"`
	TotalCostUSD        float64 `json:"total_cost_usd_est"`
	AvgLatencyMS        float64 `json:"avg_latency_ms"`
	P95LatencyMS        int     `json:"p95_latency_ms"`
}

// CallStatsFor returns aggregated statistics for rows matching f.
// Uses the same filter as RecentCalls but ignores the Limit field (aggregates all matching rows).
func (s *Store) CallStatsFor(f CallLogFilter) (CallStats, error) {
	q := `SELECT outcome, prompt_tokens_est, completion_tokens_est, latency_ms, cost_usd_est
	      FROM call_log`
	args := []interface{}{}
	var where []string

	if f.BackendID != "" {
		where = append(where, "backend_id = ?")
		args = append(args, f.BackendID)
	}
	if !f.From.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.From.UTC().Format(time.RFC3339))
	}
	if !f.To.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, f.To.UTC().Format(time.RFC3339))
	}
	if len(where) > 0 {
		q += " WHERE " + joinAnd(where)
	}
	q += " ORDER BY latency_ms ASC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return CallStats{}, err
	}
	defer rows.Close()

	var stats CallStats
	var latencies []int
	var totalLatency int64

	for rows.Next() {
		var outcome string
		var prompt, compl, latency int
		var cost float64
		if err := rows.Scan(&outcome, &prompt, &compl, &latency, &cost); err != nil {
			continue
		}
		stats.TotalCalls++
		stats.TotalPromptTokens += prompt
		stats.TotalComplTokens += compl
		stats.TotalCostUSD += cost
		totalLatency += int64(latency)
		latencies = append(latencies, latency)
		// "pass" is the only outcome written by the router for a call that
		// produced a response. "fallback" is written per-hop when a backend
		// failed and the chain advanced; "degraded" is written when the whole
		// chain exhausted. Both are failures from the caller's perspective.
		if outcome == "pass" {
			stats.SuccessCalls++
		} else {
			stats.ErrorCalls++
		}
	}
	if stats.TotalCalls > 0 {
		stats.AvgLatencyMS = float64(totalLatency) / float64(stats.TotalCalls)
		p95idx := int(float64(len(latencies)) * 0.95)
		if p95idx >= len(latencies) {
			p95idx = len(latencies) - 1
		}
		stats.P95LatencyMS = latencies[p95idx]
	}
	return stats, nil
}

// joinAnd joins clauses with " AND ".
func joinAnd(clauses []string) string {
	result := clauses[0]
	for _, c := range clauses[1:] {
		result += " AND " + c
	}
	return result
}

// CallLogRow is one row from call_log.
type CallLogRow struct {
	TS                  string
	BackendID           string
	TierAlias           string
	ChainPosition       int
	Outcome             string
	ErrorType           string
	PromptTokensEst     int
	CompletionTokensEst int
	LatencyMS           int
	CostUSD             float64
	// Routing intelligence fields (Phase 6 Slice A)
	Model         string
	Score         float64
	RequestID     string
	RateLimitType string
	Utilization   *float64
	FallbackCount int
}

// SaveWebhook upserts a webhook registration.
func (s *Store) SaveWebhook(id, url, events, secret string) {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO webhooks (id, url, events, secret, created_at)
		VALUES (?,?,?,?,?)`,
		id, url, events, secret, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		slog.Warn("store: save_webhook failed", "err", err)
	}
}

// DeleteWebhook removes a webhook by ID.
func (s *Store) DeleteWebhook(id string) {
	s.db.Exec(`DELETE FROM webhooks WHERE id = ?`, id)
}

// ListWebhooks returns all registered webhooks.
func (s *Store) ListWebhooks() ([]WebhookRow, error) {
	rows, err := s.db.Query(`SELECT id, url, events, secret, created_at FROM webhooks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WebhookRow
	for rows.Next() {
		var r WebhookRow
		rows.Scan(&r.ID, &r.URL, &r.Events, &r.Secret, &r.CreatedAt)
		result = append(result, r)
	}
	return result, nil
}

// WebhookRow is one row from webhooks.
type WebhookRow struct {
	ID        string
	URL       string
	Events    string
	Secret    string
	CreatedAt string
}

// --- helpers ---

func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func parseNullTime(ns sql.NullString) time.Time {
	if !ns.Valid || ns.String == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, ns.String)
	return t
}
