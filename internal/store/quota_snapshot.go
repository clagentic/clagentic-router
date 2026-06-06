// internal/store/quota_snapshot.go — quota_snapshots table and insert.
//
// quota_snapshots records each rate_limit_event emitted by the claude CLI,
// providing a time-series of quota utilization data per backend. The table
// is append-only; no updates or deletes. Stale data on restart is intentional —
// the last_quota_snapshot field on BackendState is repopulated from the next
// live response.
package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
)

const quotaSnapshotsSchema = `
CREATE TABLE IF NOT EXISTS quota_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    backend_id      TEXT    NOT NULL,
    observed_at     INTEGER NOT NULL,  -- Unix nanoseconds
    status          TEXT    NOT NULL,
    rate_limit_type TEXT    NOT NULL,
    utilization     REAL,              -- NULL when absent (status=allowed)
    resets_at       INTEGER,           -- Unix seconds
    surpassed_threshold REAL,          -- NULL when absent
    is_using_overage    INTEGER NOT NULL DEFAULT 0,
    overage_status      TEXT,
    overage_disabled_reason TEXT,
    overage_resets_at   INTEGER,
    raw_json        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_backend_time
    ON quota_snapshots (backend_id, observed_at);
CREATE INDEX IF NOT EXISTS idx_quota_snapshots_time
    ON quota_snapshots (observed_at);
`

// QuotaSnapshotRow is one row from quota_snapshots.
type QuotaSnapshotRow struct {
	BackendID              string
	ObservedAt             time.Time
	Status                 string
	RateLimitType          string
	Utilization            *float64
	ResetsAt               *int64
	SurpassedThreshold     *float64
	IsUsingOverage         bool
	OverageStatus          *string
	OverageDisabledReason  *string
	OverageResetsAt        *int64
	RawJSON                string
}

// InsertQuotaSnapshot inserts one quota snapshot row derived from a rate_limit_event.
// ctx is accepted for future use (prepared statements, query timeout); the current
// implementation uses a fire-and-forget pattern consistent with LogCall.
func (s *Store) InsertQuotaSnapshot(ctx context.Context, backendID string, e *backend.RateLimitEvent) error {
	observedAt := time.Now().UTC().UnixNano()
	resetsAtUnix := e.ResetsAt.Unix()

	var surpassedThreshold interface{} = nil
	if e.SurpassedThreshold != nil {
		surpassedThreshold = *e.SurpassedThreshold
	}
	var utilization interface{} = nil
	if e.Utilization != nil {
		utilization = *e.Utilization
	}
	var overageResetsAt interface{} = nil
	if e.OverageResetsAt != nil {
		overageResetsAt = e.OverageResetsAt.Unix()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO quota_snapshots (
			backend_id, observed_at, status, rate_limit_type,
			utilization, resets_at, surpassed_threshold,
			is_using_overage, overage_status, overage_disabled_reason,
			overage_resets_at, raw_json
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		backendID, observedAt, e.Status, e.RateLimitType,
		utilization, resetsAtUnix, surpassedThreshold,
		boolToInt(e.IsUsingOverage), e.OverageStatus, e.OverageDisabledReason,
		overageResetsAt, e.RawJSON,
	)
	if err != nil {
		slog.Warn("store: insert_quota_snapshot failed", "backend", backendID, "err", err)
		return err
	}
	return nil
}
