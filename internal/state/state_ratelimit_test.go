// internal/state/state_ratelimit_test.go — tests for rate-limit header state methods.
package state

import (
	"testing"
	"time"
)

func TestUpdateRateLimitFromResponse(t *testing.T) {
	bs := New("test-backend")

	now := time.Now().UTC()
	tokensResetAt := now.Add(6 * time.Minute)
	reqResetAt := now.Add(1 * time.Minute)

	bs.UpdateRateLimitFromResponse(8000, 50, tokensResetAt, reqResetAt)

	snap := bs.Snapshot()
	if snap.RateLimitTokensRemaining != 8000 {
		t.Errorf("RateLimitTokensRemaining = %d, want 8000", snap.RateLimitTokensRemaining)
	}
	if snap.RateLimitRequestsRemaining != 50 {
		t.Errorf("RateLimitRequestsRemaining = %d, want 50", snap.RateLimitRequestsRemaining)
	}
	if !snap.RateLimitTokensResetAt.Equal(tokensResetAt) {
		t.Errorf("RateLimitTokensResetAt = %v, want %v", snap.RateLimitTokensResetAt, tokensResetAt)
	}
	if !snap.RateLimitRequestsResetAt.Equal(reqResetAt) {
		t.Errorf("RateLimitRequestsResetAt = %v, want %v", snap.RateLimitRequestsResetAt, reqResetAt)
	}
}

func TestUpdateRateLimitFromResponse_ZeroValues(t *testing.T) {
	// Zero-value call should still update fields to zero (headers were absent).
	bs := New("test-backend")
	// Pre-populate to verify they get overwritten to zero
	bs.UpdateRateLimitFromResponse(5000, 20, time.Now().Add(time.Minute), time.Now().Add(time.Minute))

	bs.UpdateRateLimitFromResponse(0, 0, time.Time{}, time.Time{})

	snap := bs.Snapshot()
	if snap.RateLimitTokensRemaining != 0 {
		t.Errorf("RateLimitTokensRemaining = %d, want 0", snap.RateLimitTokensRemaining)
	}
	if snap.RateLimitRequestsRemaining != 0 {
		t.Errorf("RateLimitRequestsRemaining = %d, want 0", snap.RateLimitRequestsRemaining)
	}
	if !snap.RateLimitTokensResetAt.IsZero() {
		t.Errorf("RateLimitTokensResetAt = %v, want zero time", snap.RateLimitTokensResetAt)
	}
}

func TestSnapshot_RateLimitFieldsPropagated(t *testing.T) {
	// Verify Snapshot includes the four rate-limit fields and they round-trip correctly.
	bs := New("test-backend")

	resetAt := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	bs.UpdateRateLimitFromResponse(12000, 75, resetAt, resetAt)

	snap := bs.Snapshot()
	if snap.RateLimitTokensRemaining != 12000 {
		t.Errorf("Snapshot RateLimitTokensRemaining = %d, want 12000", snap.RateLimitTokensRemaining)
	}
	if snap.RateLimitRequestsRemaining != 75 {
		t.Errorf("Snapshot RateLimitRequestsRemaining = %d, want 75", snap.RateLimitRequestsRemaining)
	}
	if !snap.RateLimitTokensResetAt.Equal(resetAt) {
		t.Errorf("Snapshot RateLimitTokensResetAt = %v, want %v", snap.RateLimitTokensResetAt, resetAt)
	}
	if !snap.RateLimitRequestsResetAt.Equal(resetAt) {
		t.Errorf("Snapshot RateLimitRequestsResetAt = %v, want %v", snap.RateLimitRequestsResetAt, resetAt)
	}
}

// --- SetRateLimitWarning ---

func TestSetRateLimitWarning_SetsTokensAndResetAt(t *testing.T) {
	bs := New("test-backend")
	resetsAt := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)

	bs.SetRateLimitWarning("five_hour", resetsAt)

	snap := bs.Snapshot()
	if snap.RateLimitTokensRemaining != 1 {
		t.Errorf("RateLimitTokensRemaining = %d, want 1", snap.RateLimitTokensRemaining)
	}
	if !snap.RateLimitTokensResetAt.Equal(resetsAt) {
		t.Errorf("RateLimitTokensResetAt = %v, want %v", snap.RateLimitTokensResetAt, resetsAt)
	}
}

func TestSetRateLimitWarning_ZeroResetsAt_DoesNotClearExisting(t *testing.T) {
	bs := New("test-backend")
	existingReset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	// Populate a real reset time via UpdateRateLimitFromResponse
	bs.UpdateRateLimitFromResponse(5000, 10, existingReset, existingReset)

	// Warning with zero resetsAt should not overwrite the existing reset time
	bs.SetRateLimitWarning("seven_day", time.Time{})

	snap := bs.Snapshot()
	if !snap.RateLimitTokensResetAt.Equal(existingReset) {
		t.Errorf("RateLimitTokensResetAt = %v, want existing %v (zero resetsAt must not clear)", snap.RateLimitTokensResetAt, existingReset)
	}
}

func TestSetRateLimitWarning_DoesNotChangeStatus(t *testing.T) {
	bs := New("test-backend")
	bs.RecordSuccess(10, 10, 0, 100, 3, 6) // drive to Healthy

	snap := bs.Snapshot()
	if snap.Status != StatusHealthy {
		t.Fatalf("precondition: want Healthy, got %s", snap.Status)
	}

	bs.SetRateLimitWarning("five_hour", time.Now().Add(time.Hour))

	snap = bs.Snapshot()
	if snap.Status != StatusHealthy {
		t.Errorf("Status changed after warning: want Healthy, got %s", snap.Status)
	}
}

func TestRateLimitFieldsEphemeral_NotRestoredFromSnapshot(t *testing.T) {
	// Rate-limit header fields are ephemeral — RestoreFromSnapshot intentionally does
	// not restore them. Stale window data at restart is worse than no data.
	bs := New("test-backend")
	resetAt := time.Now().Add(5 * time.Minute)
	bs.UpdateRateLimitFromResponse(9000, 40, resetAt, resetAt)

	snap := bs.Snapshot()

	// Create a fresh state and restore from snapshot
	bs2 := New("test-backend")
	bs2.RestoreFromSnapshot(snap)

	snap2 := bs2.Snapshot()
	// Rate-limit fields should be zero after restore — they are ephemeral
	if snap2.RateLimitTokensRemaining != 0 {
		t.Errorf("after RestoreFromSnapshot: RateLimitTokensRemaining = %d, want 0 (ephemeral)", snap2.RateLimitTokensRemaining)
	}
	if snap2.RateLimitRequestsRemaining != 0 {
		t.Errorf("after RestoreFromSnapshot: RateLimitRequestsRemaining = %d, want 0 (ephemeral)", snap2.RateLimitRequestsRemaining)
	}
	if !snap2.RateLimitTokensResetAt.IsZero() {
		t.Errorf("after RestoreFromSnapshot: RateLimitTokensResetAt = %v, want zero time (ephemeral)", snap2.RateLimitTokensResetAt)
	}
}
