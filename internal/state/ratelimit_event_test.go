// internal/state/ratelimit_event_test.go — tests for UpdateFromRateLimitEvent.
package state

import (
	"testing"
	"time"
)

func float64Ptr(v float64) *float64 { return &v }

// TestUpdateFromRateLimitEvent_WithUtilization verifies that when utilization is
// present the quota fields are updated via SetQuotaFromUsage and the snapshot
// is stored.
func TestUpdateFromRateLimitEvent_WithUtilization(t *testing.T) {
	bs := New("test-backend")
	resetsAt := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	u := 0.78

	snap := &QuotaSnapshot{
		Status:        "allowed_warning",
		RateLimitType: "seven_day",
		Utilization:   &u,
		ResetsAt:      resetsAt,
		ObservedAt:    time.Now().UTC(),
	}

	bs.UpdateFromRateLimitEvent(RateLimitEventData{
		Status:            "allowed_warning",
		RateLimitType:     "seven_day",
		ResetsAt:          resetsAt,
		Utilization:       &u,
		LastQuotaSnapshot: snap,
	})

	s := bs.Snapshot()

	// SetQuotaFromUsage scales: remaining = int64((1-utilization)*1e6), total = 1e6.
	// Use a float64 variable (not a constant expression) to match the implementation's
	// IEEE 754 arithmetic and avoid compile-time constant folding discrepancies.
	const scale int64 = 1_000_000
	uVar := float64(u) // same as the pointer-dereferenced value in the implementation
	wantRemaining := int64((1.0 - uVar) * float64(scale))
	if s.QuotaTokensRemaining != wantRemaining {
		t.Errorf("QuotaTokensRemaining = %d, want %d", s.QuotaTokensRemaining, wantRemaining)
	}
	if s.QuotaTokensTotal != scale {
		t.Errorf("QuotaTokensTotal = %d, want %d", s.QuotaTokensTotal, scale)
	}
	if s.QuotaResetAt.IsZero() {
		t.Error("QuotaResetAt is zero, want a time value")
	}
	if s.LastQuotaSnapshot == nil {
		t.Fatal("LastQuotaSnapshot is nil, want non-nil")
	}
	if s.LastQuotaSnapshot.Status != "allowed_warning" {
		t.Errorf("LastQuotaSnapshot.Status = %q, want %q", s.LastQuotaSnapshot.Status, "allowed_warning")
	}
	if s.LastQuotaSnapshot.Utilization == nil || *s.LastQuotaSnapshot.Utilization != 0.78 {
		t.Errorf("LastQuotaSnapshot.Utilization = %v, want 0.78", s.LastQuotaSnapshot.Utilization)
	}
}

// TestUpdateFromRateLimitEvent_NilUtilization_ClearsQuotaLowAlerted verifies
// that when utilization is nil (status=allowed), QuotaLowAlerted is cleared.
func TestUpdateFromRateLimitEvent_NilUtilization_ClearsQuotaLowAlerted(t *testing.T) {
	bs := New("test-backend")

	// Simulate a previous quota_low alert having been fired.
	bs.mu.Lock()
	bs.QuotaLowAlerted = true
	bs.mu.Unlock()

	resetsAt := time.Now().UTC().Add(5 * time.Hour)

	bs.UpdateFromRateLimitEvent(RateLimitEventData{
		Status:        "allowed",
		RateLimitType: "five_hour",
		ResetsAt:      resetsAt,
		Utilization:   nil, // below threshold
	})

	s := bs.Snapshot()
	if s.QuotaLowAlerted {
		t.Error("QuotaLowAlerted should be cleared when utilization is nil (below threshold)")
	}
}

// TestUpdateFromRateLimitEvent_NilUtilization_DoesNotTouchTokens verifies
// that when utilization is nil, quota token fields are NOT updated (we don't
// know the actual utilization, so we must not fabricate values).
func TestUpdateFromRateLimitEvent_NilUtilization_DoesNotTouchTokens(t *testing.T) {
	bs := New("test-backend")
	// Pre-populate quota from a usage poll.
	bs.SetQuotaFromUsage(500_000, 1_000_000, time.Now().Add(7*24*time.Hour))
	snapBefore := bs.Snapshot()

	bs.UpdateFromRateLimitEvent(RateLimitEventData{
		Status:        "allowed",
		RateLimitType: "five_hour",
		ResetsAt:      time.Now().Add(5 * time.Hour),
		Utilization:   nil,
	})

	snapAfter := bs.Snapshot()
	// Token counts should be unchanged when utilization is absent.
	if snapAfter.QuotaTokensRemaining != snapBefore.QuotaTokensRemaining {
		t.Errorf("QuotaTokensRemaining changed from %d to %d on nil-utilization event",
			snapBefore.QuotaTokensRemaining, snapAfter.QuotaTokensRemaining)
	}
	if snapAfter.QuotaTokensTotal != snapBefore.QuotaTokensTotal {
		t.Errorf("QuotaTokensTotal changed from %d to %d on nil-utilization event",
			snapBefore.QuotaTokensTotal, snapAfter.QuotaTokensTotal)
	}
}

// TestUpdateFromRateLimitEvent_SnapshotStored verifies that LastQuotaSnapshot
// is stored on BackendState and surfaces in Snapshot().
func TestUpdateFromRateLimitEvent_SnapshotStored(t *testing.T) {
	bs := New("test-backend")

	// Initially no snapshot.
	if bs.Snapshot().LastQuotaSnapshot != nil {
		t.Fatal("LastQuotaSnapshot should be nil before any event")
	}

	u := 0.5
	observed := time.Now().UTC()
	qs := &QuotaSnapshot{
		Status:        "allowed_warning",
		RateLimitType: "seven_day_opus",
		Utilization:   &u,
		ResetsAt:      time.Now().Add(7 * 24 * time.Hour),
		ObservedAt:    observed,
	}

	bs.UpdateFromRateLimitEvent(RateLimitEventData{
		Status:            "allowed_warning",
		RateLimitType:     "seven_day_opus",
		ResetsAt:          time.Now().Add(7 * 24 * time.Hour),
		Utilization:       &u,
		LastQuotaSnapshot: qs,
	})

	s := bs.Snapshot()
	if s.LastQuotaSnapshot == nil {
		t.Fatal("LastQuotaSnapshot is nil after UpdateFromRateLimitEvent")
	}
	if s.LastQuotaSnapshot.RateLimitType != "seven_day_opus" {
		t.Errorf("RateLimitType = %q, want %q", s.LastQuotaSnapshot.RateLimitType, "seven_day_opus")
	}
}
