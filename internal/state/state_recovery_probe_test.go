// internal/state/state_recovery_probe_test.go — unit tests for offline-recovery
// probe gating methods added in lr-0bca.
package state

import (
	"testing"
	"time"
)

// --- HasPendingReset ---

func TestHasPendingReset_NonOfflineReturnsFalse(t *testing.T) {
	bs := New("b")
	bs.RecordSuccess(0, 0, 0, 100, 3, 6) // → Healthy
	if bs.HasPendingReset() {
		t.Error("HasPendingReset: want false for Healthy backend, got true")
	}
}

func TestHasPendingReset_AuthOfflineNoResetTime(t *testing.T) {
	bs := New("b")
	// Auth failure drives OFFLINE with no reset time (permanent until operator).
	bs.RecordFailure(ErrTypeAuth, "auth error", time.Time{}, 3, 6)
	if bs.HasPendingReset() {
		t.Error("HasPendingReset: want false for auth-offline (no reset time), got true")
	}
}

func TestHasPendingReset_QuotaOfflineFutureResetTime(t *testing.T) {
	bs := New("b")
	futureReset := time.Now().Add(time.Hour)
	bs.RecordFailure(ErrTypeQuota, "quota", futureReset, 3, 6)
	if !bs.HasPendingReset() {
		t.Error("HasPendingReset: want true for quota-offline with future reset, got false")
	}
}

func TestHasPendingReset_QuotaOfflinePastResetTime(t *testing.T) {
	bs := New("b")
	pastReset := time.Now().Add(-time.Hour)
	bs.RecordFailure(ErrTypeQuota, "quota", pastReset, 3, 6)
	// Reset time is in the past — TryRecover would have already fired; pending = false.
	if bs.HasPendingReset() {
		t.Error("HasPendingReset: want false for quota-offline with past reset time, got true")
	}
}

func TestHasPendingReset_RateLimitOfflineFutureResetTime(t *testing.T) {
	// Force OFFLINE with a future rate-limit reset by manipulating state directly
	// (RecordFailure for rate-limit only sets DEGRADED, not OFFLINE, so we need
	// to set the RateLimitResetAt field and force Status offline).
	bs := New("b")
	bs.mu.Lock()
	bs.Status = StatusOffline
	bs.RateLimitResetAt = time.Now().Add(30 * time.Minute)
	bs.mu.Unlock()
	if !bs.HasPendingReset() {
		t.Error("HasPendingReset: want true for rate-limit offline with future reset, got false")
	}
}

// --- RecoveryProbeDue ---

func TestRecoveryProbeDue_ZeroIntervalAlwaysFalse(t *testing.T) {
	bs := New("b")
	if bs.RecoveryProbeDue(0) {
		t.Error("RecoveryProbeDue(0): want false (disabled), got true")
	}
}

func TestRecoveryProbeDue_NoProbeYetReturnsTrue(t *testing.T) {
	bs := New("b")
	if !bs.RecoveryProbeDue(300) {
		t.Error("RecoveryProbeDue: want true when no probe has ever run, got false")
	}
}

func TestRecoveryProbeDue_RecentProbeReturnsFalse(t *testing.T) {
	bs := New("b")
	bs.MarkRecoveryProbed() // marks now
	// Interval is 300 s; time since probe is < 1 s — not due yet.
	if bs.RecoveryProbeDue(300) {
		t.Error("RecoveryProbeDue: want false immediately after probe, got true")
	}
}

func TestRecoveryProbeDue_ElapsedIntervalReturnsTrue(t *testing.T) {
	bs := New("b")
	// Back-date the last probe time so the interval has clearly elapsed.
	bs.mu.Lock()
	bs.LastRecoveryProbeAt = time.Now().Add(-10 * time.Minute)
	bs.mu.Unlock()
	if !bs.RecoveryProbeDue(300) { // 300 s < 10 min
		t.Error("RecoveryProbeDue: want true after interval elapsed, got false")
	}
}

// --- MarkRecoveryProbed ---

func TestMarkRecoveryProbed_SetsTimestamp(t *testing.T) {
	bs := New("b")
	before := time.Now().UTC()
	bs.MarkRecoveryProbed()
	snap := bs.Snapshot()
	if snap.LastRecoveryProbeAt.IsZero() {
		t.Error("MarkRecoveryProbed: LastRecoveryProbeAt still zero after call")
	}
	if snap.LastRecoveryProbeAt.Before(before) {
		t.Errorf("MarkRecoveryProbed: timestamp %v is before call start %v", snap.LastRecoveryProbeAt, before)
	}
}
