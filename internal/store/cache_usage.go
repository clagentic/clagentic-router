// internal/store/cache_usage.go — per-model prompt-cache token aggregates.
//
// cache_usage tracks running counts of input/cache_read/cache_write tokens
// per (backend_id, model), plus a call counter distinguishing adapters that
// reported real cache data from ones that could not (lr-718af0). Unlike
// quota_snapshots (an append-only time series), this table holds one
// upserted row per (backend_id, model) — the acceptance criterion here is an
// aggregate hit-rate metric, not a historical timeline, and an ever-growing
// per-call log would duplicate call_log's existing row-per-call shape for
// no benefit this feature needs.
//
// Import graph: store → state only. This file accepts a plain
// CacheUsageInput struct (no backend import) — the router translates
// backend.CacheUsage before calling RecordCacheUsage, identically to how
// quota_snapshot.go's InsertQuotaSnapshot is fed from a translated
// backend.RateLimitEvent.
//
// Counts/aggregates only (lr-718af0 constraint, same boundary lr-4aaf2a
// draws for call_log's tools_present bit): no prompt content, request body,
// or response text is ever written to this table — only integer token
// counts and a call counter.
package store

import (
	"context"
	"log/slog"
)

const cacheUsageSchema = `
CREATE TABLE IF NOT EXISTS cache_usage (
    backend_id           TEXT    NOT NULL,
    model                TEXT    NOT NULL,
    input_tokens_total    INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens_total  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens_total INTEGER NOT NULL DEFAULT 0,
    -- calls_reported counts invocations where the adapter returned a non-nil
    -- CacheUsage (real data, possibly all-zero fields = a genuine miss).
    -- calls_unsupported counts invocations where CacheUsage was nil (the
    -- adapter family cannot report cache data at all for this call) — kept
    -- as a SEPARATE counter, never folded into calls_reported or into the
    -- token totals, so "no data" and "reported zero" remain distinguishable
    -- downstream (the acceptance criterion this task is built around).
    calls_reported        INTEGER NOT NULL DEFAULT 0,
    calls_unsupported      INTEGER NOT NULL DEFAULT 0,
    updated_at            TEXT    NOT NULL,
    PRIMARY KEY (backend_id, model)
);
`

// CacheUsageInput is the store-layer input for RecordCacheUsage — one call's
// worth of cache accounting to fold into the running (backend_id, model)
// aggregate. Reported is false when the adapter returned a nil CacheUsage
// (cannot report); when Reported is false, InputTokens/CacheReadTokens/
// CacheWriteTokens are ignored (must be zero from the caller, but this
// struct does not itself enforce that — see RecordCacheUsage).
type CacheUsageInput struct {
	Model            string
	Reported         bool
	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// CacheUsageRow is one row read back from cache_usage.
type CacheUsageRow struct {
	BackendID             string
	Model                 string
	InputTokensTotal      int64
	CacheReadTokensTotal  int64
	CacheWriteTokensTotal int64
	CallsReported         int64
	CallsUnsupported      int64
}

// HitRate returns CacheReadTokensTotal / (InputTokensTotal), or (0, false)
// when InputTokensTotal is zero (no reported data to compute a rate from —
// distinct from a rate of exactly 0.0, hence the bool). Callers MUST check
// CallsReported > 0 before treating a zero HitRate as a genuine miss rather
// than "no data at all" for this backend/model — this method alone cannot
// make that distinction because a zero InputTokensTotal is ambiguous
// between the two.
func (r CacheUsageRow) HitRate() (float64, bool) {
	if r.InputTokensTotal == 0 {
		return 0, false
	}
	return float64(r.CacheReadTokensTotal) / float64(r.InputTokensTotal), true
}

// RecordCacheUsage folds one call's cache accounting into the running
// (backend_id, model) aggregate row, creating it if absent. Best-effort,
// like every other store write in this package — errors are logged, never
// returned to the router (the router must never block routing on storage
// failures).
func (s *Store) RecordCacheUsage(ctx context.Context, backendID string, in CacheUsageInput) {
	reportedDelta, unsupportedDelta := 0, 1
	inputDelta, readDelta, writeDelta := int64(0), int64(0), int64(0)
	if in.Reported {
		reportedDelta, unsupportedDelta = 1, 0
		inputDelta = in.InputTokens
		readDelta = in.CacheReadTokens
		writeDelta = in.CacheWriteTokens
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cache_usage (
			backend_id, model, input_tokens_total, cache_read_tokens_total,
			cache_write_tokens_total, calls_reported, calls_unsupported, updated_at
		) VALUES (?,?,?,?,?,?,?, datetime('now'))
		ON CONFLICT(backend_id, model) DO UPDATE SET
			input_tokens_total       = input_tokens_total + excluded.input_tokens_total,
			cache_read_tokens_total  = cache_read_tokens_total + excluded.cache_read_tokens_total,
			cache_write_tokens_total = cache_write_tokens_total + excluded.cache_write_tokens_total,
			calls_reported           = calls_reported + excluded.calls_reported,
			calls_unsupported        = calls_unsupported + excluded.calls_unsupported,
			updated_at               = datetime('now')`,
		backendID, in.Model, inputDelta, readDelta, writeDelta, reportedDelta, unsupportedDelta,
	)
	if err != nil {
		slog.Warn("store: record_cache_usage failed", "backend", backendID, "model", in.Model, "err", err)
	}
}

// AllCacheUsage returns every cache_usage row, ordered by backend_id then
// model — the read side RecordCacheUsage's aggregates feed to the /metrics
// exposition surface (see server/handlers.go's cacheMetrics handler).
func (s *Store) AllCacheUsage(ctx context.Context) ([]CacheUsageRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT backend_id, model, input_tokens_total, cache_read_tokens_total,
		       cache_write_tokens_total, calls_reported, calls_unsupported
		FROM cache_usage
		ORDER BY backend_id, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CacheUsageRow
	for rows.Next() {
		var r CacheUsageRow
		if err := rows.Scan(
			&r.BackendID, &r.Model, &r.InputTokensTotal, &r.CacheReadTokensTotal,
			&r.CacheWriteTokensTotal, &r.CallsReported, &r.CallsUnsupported,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
