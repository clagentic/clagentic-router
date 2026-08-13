// internal/store/quota_snapshot.go — quota_snapshots table and insert.
//
// quota_snapshots records each rate_limit_event emitted by the claude CLI,
// providing a time-series of quota utilization data per backend. The table
// is append-only; no updates or deletes. Stale data on restart is intentional —
// the last_quota_snapshot field on BackendState is repopulated from the next
// live response.
//
// Import graph: store → state only. This file accepts a plain QuotaSnapshotInput
// struct (no backend import) — the router translates backend.RateLimitEvent before
// calling InsertQuotaSnapshot.
package store

import (
	"context"
	"log/slog"
	"time"
)

const quotaSnapshotsSchema = `
CREATE TABLE IF NOT EXISTS quota_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    backend_id      TEXT    NOT NULL,
    observed_at     INTEGER NOT NULL,  -- Unix nanoseconds
    status          TEXT    NOT NULL,
    rate_limit_type TEXT    NOT NULL,
    utilization     REAL,              -- NULL when absent (status=allowed, below threshold)
    resets_at       INTEGER,           -- Unix seconds
    surpassed_threshold REAL,          -- NULL when absent
    is_using_overage    INTEGER NOT NULL DEFAULT 0,
    overage_status      TEXT,
    overage_disabled_reason TEXT,
    overage_resets_at   INTEGER,       -- Unix seconds, NULL when absent
    raw_json        TEXT    NOT NULL   -- full rate_limit_info JSON for forward compat
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_backend_time
    ON quota_snapshots (backend_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_time
    ON quota_snapshots (observed_at);
`

// QuotaSnapshotInput is the store-layer input for InsertQuotaSnapshot.
// The router populates this from backend.RateLimitEvent — keeping store free
// of any backend package import (import graph: store → state only).
type QuotaSnapshotInput struct {
	Status                string
	RateLimitType         string
	Utilization           *float64 // nil when status=allowed (below threshold)
	ResetsAt              *int64   // Unix seconds; nil if zero time
	SurpassedThreshold    *float64 // nil when absent
	IsUsingOverage        bool
	OverageStatus         *string
	OverageDisabledReason *string
	OverageResetsAt       *int64 // Unix seconds; nil when absent
	RawJSON               string
}

// QuotaSnapshotRow is one row read back from quota_snapshots.
type QuotaSnapshotRow struct {
	BackendID             string
	ObservedAt            time.Time
	Status                string
	RateLimitType         string
	Utilization           *float64
	ResetsAt              *int64
	SurpassedThreshold    *float64
	IsUsingOverage        bool
	OverageStatus         *string
	OverageDisabledReason *string
	OverageResetsAt       *int64
	RawJSON               string
}

// InsertQuotaSnapshot inserts one quota snapshot row derived from a rate_limit_event.
// Absence of Utilization (nil) is not an error — it means status=allowed (below threshold)
// and is stored as NULL in the DB. The gap in utilization data is itself signal.
func (s *Store) InsertQuotaSnapshot(ctx context.Context, backendID string, e QuotaSnapshotInput) error {
	observedAt := time.Now().UTC().UnixNano()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_snapshots (
			backend_id, observed_at, status, rate_limit_type,
			utilization, resets_at, surpassed_threshold,
			is_using_overage, overage_status, overage_disabled_reason,
			overage_resets_at, raw_json
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		backendID, observedAt, e.Status, e.RateLimitType,
		e.Utilization, e.ResetsAt, e.SurpassedThreshold,
		boolToInt(e.IsUsingOverage), e.OverageStatus, e.OverageDisabledReason,
		e.OverageResetsAt, e.RawJSON,
	)
	if err != nil {
		slog.Warn("store: insert_quota_snapshot failed", "backend", backendID, "err", err)
		return err
	}
	return nil
}
