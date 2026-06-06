// internal/store/quota_snapshot_test.go — tests for InsertQuotaSnapshot.
package store

import (
	"context"
	"testing"
	"time"
)

func float64Ptr(v float64) *float64 { return &v }
func stringPtr(v string) *string    { return &v }
func int64Ptr(v int64) *int64       { return &v }

// TestInsertQuotaSnapshot_FullEvent inserts a fully-populated event and
// verifies the row round-trips through the database correctly.
func TestInsertQuotaSnapshot_FullEvent(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	resetsAtUnix := int64(1780963200)
	overageResetsAtUnix := int64(1780966800)
	u := 0.78
	surp := 0.75

	e := QuotaSnapshotInput{
		Status:                "allowed_warning",
		RateLimitType:         "seven_day",
		ResetsAt:              &resetsAtUnix,
		Utilization:           &u,
		SurpassedThreshold:    &surp,
		IsUsingOverage:        false,
		OverageStatus:         stringPtr("enabled"),
		OverageDisabledReason: nil,
		OverageResetsAt:       &overageResetsAtUnix,
		RawJSON:               `{"status":"allowed_warning","rateLimitType":"seven_day"}`,
	}

	if err := s.InsertQuotaSnapshot(ctx, "backend-a", e); err != nil {
		t.Fatalf("InsertQuotaSnapshot failed: %v", err)
	}

	// Read back the row directly.
	row := s.db.QueryRow(`
		SELECT backend_id, status, rate_limit_type,
		       utilization, resets_at, surpassed_threshold,
		       is_using_overage, overage_status, overage_disabled_reason,
		       overage_resets_at, raw_json
		FROM quota_snapshots
		WHERE backend_id = 'backend-a'
		LIMIT 1`)

	var backendID, status, rateLimitType, rawJSON string
	var utilization, surpassedThreshold *float64
	var resetsAt *int64
	var overageResetsAt *int64
	var isUsingOverage int
	var overageStatus, overageDisabledReason *string

	err := row.Scan(
		&backendID, &status, &rateLimitType,
		&utilization, &resetsAt, &surpassedThreshold,
		&isUsingOverage, &overageStatus, &overageDisabledReason,
		&overageResetsAt, &rawJSON,
	)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if backendID != "backend-a" {
		t.Errorf("backend_id = %q, want %q", backendID, "backend-a")
	}
	if status != "allowed_warning" {
		t.Errorf("status = %q, want %q", status, "allowed_warning")
	}
	if rateLimitType != "seven_day" {
		t.Errorf("rate_limit_type = %q, want %q", rateLimitType, "seven_day")
	}
	if utilization == nil || *utilization != 0.78 {
		t.Errorf("utilization = %v, want 0.78", utilization)
	}
	if surpassedThreshold == nil || *surpassedThreshold != 0.75 {
		t.Errorf("surpassed_threshold = %v, want 0.75", surpassedThreshold)
	}
	if isUsingOverage != 0 {
		t.Errorf("is_using_overage = %d, want 0", isUsingOverage)
	}
	if overageStatus == nil || *overageStatus != "enabled" {
		t.Errorf("overage_status = %v, want %q", overageStatus, "enabled")
	}
	if overageDisabledReason != nil {
		t.Errorf("overage_disabled_reason = %v, want nil", overageDisabledReason)
	}
	if overageResetsAt == nil || *overageResetsAt != overageResetsAtUnix {
		t.Errorf("overage_resets_at = %v, want %d", overageResetsAt, overageResetsAtUnix)
	}
	if resetsAt == nil || *resetsAt != resetsAtUnix {
		t.Errorf("resets_at = %v, want %d", resetsAt, resetsAtUnix)
	}
	if rawJSON == "" {
		t.Error("raw_json is empty")
	}
}

// TestInsertQuotaSnapshot_NullFields verifies that a status=allowed event
// stores NULL for utilization, surpassed_threshold, and optional overage fields.
// Absence of utilization is not an error — it means below-threshold (status=allowed).
func TestInsertQuotaSnapshot_NullFields(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	resetsAt := int64(1780945600)
	e := QuotaSnapshotInput{
		Status:        "allowed",
		RateLimitType: "five_hour",
		ResetsAt:      &resetsAt,
		Utilization:   nil, // legitimately absent when status=allowed
		IsUsingOverage: false,
		RawJSON:       `{"status":"allowed"}`,
	}

	if err := s.InsertQuotaSnapshot(ctx, "backend-b", e); err != nil {
		t.Fatalf("InsertQuotaSnapshot failed: %v", err)
	}

	row := s.db.QueryRow(`
		SELECT utilization, surpassed_threshold, overage_status, overage_resets_at
		FROM quota_snapshots
		WHERE backend_id = 'backend-b'
		LIMIT 1`)

	var utilization, surpassedThreshold *float64
	var overageStatus *string
	var overageResetsAt *int64

	if err := row.Scan(&utilization, &surpassedThreshold, &overageStatus, &overageResetsAt); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if utilization != nil {
		t.Errorf("utilization = %v, want NULL (below-threshold)", *utilization)
	}
	if surpassedThreshold != nil {
		t.Errorf("surpassed_threshold = %v, want NULL", *surpassedThreshold)
	}
	if overageStatus != nil {
		t.Errorf("overage_status = %v, want NULL", *overageStatus)
	}
	if overageResetsAt != nil {
		t.Errorf("overage_resets_at = %v, want NULL", *overageResetsAt)
	}
}

// TestInsertQuotaSnapshot_MultipleBackends verifies that two events for
// different backends are stored independently.
func TestInsertQuotaSnapshot_MultipleBackends(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	resetsAt := int64(1780963200)
	u := 0.5
	for _, bid := range []string{"backend-x", "backend-y"} {
		e := QuotaSnapshotInput{
			Status:        "allowed_warning",
			RateLimitType: "seven_day",
			ResetsAt:      &resetsAt,
			Utilization:   &u,
			RawJSON:       `{"status":"allowed_warning"}`,
		}
		if err := s.InsertQuotaSnapshot(ctx, bid, e); err != nil {
			t.Fatalf("InsertQuotaSnapshot(%s): %v", bid, err)
		}
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_snapshots`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("row count = %d, want 2", count)
	}
}

// TestInsertQuotaSnapshot_ObservedAtMonotonic verifies that observed_at is
// populated and is a reasonable Unix nanosecond timestamp.
func TestInsertQuotaSnapshot_ObservedAtMonotonic(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	before := time.Now().UTC().UnixNano()
	resetsAt := int64(1780963200)
	e := QuotaSnapshotInput{
		Status: "allowed", RateLimitType: "seven_day",
		ResetsAt: &resetsAt, RawJSON: `{}`,
	}
	if err := s.InsertQuotaSnapshot(ctx, "b", e); err != nil {
		t.Fatalf("insert: %v", err)
	}
	after := time.Now().UTC().UnixNano()

	var observedAt int64
	if err := s.db.QueryRow(`SELECT observed_at FROM quota_snapshots LIMIT 1`).Scan(&observedAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if observedAt < before || observedAt > after {
		t.Errorf("observed_at %d outside [%d, %d]", observedAt, before, after)
	}
}
