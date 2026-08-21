// internal/state/state_timeout_modelconfig_test.go — regression tests for
// lr-2f35bd: RecordFailure's handling of ErrTypeTimeout (AC2) and the new
// ErrTypeModelConfig sticky-offline category (B5).
package state

import (
	"testing"
	"time"
)

// TestRecordFailure_Timeout_NeverAuthHardOffline covers AC2 directly: a
// single ErrTypeTimeout failure must NOT take the ErrTypeAuth/ErrTypeNotFound
// hard-offline branch. offlineThreshold is set high (6) so a soft-path
// single failure stays HEALTHY/UNKNOWN rather than OFFLINE — proving the
// transition taken is the threshold-based default branch, not the
// unconditional-offline Auth/NotFound branch.
func TestRecordFailure_Timeout_NeverAuthHardOffline(t *testing.T) {
	bs := New("b")
	change := bs.RecordFailure(ErrTypeTimeout, "context deadline exceeded", time.Time{}, 3, 6)
	if change.To == StatusOffline {
		t.Errorf("RecordFailure(ErrTypeTimeout, ...) on first failure with offlineThreshold=6 transitioned to %q — timeout must take the soft/threshold path, never the unconditional auth hard-offline transition", change.To)
	}
}

// TestRecordFailure_Timeout_AdvancesThroughThresholdsLikeSoftFailure
// confirms timeout failures still count toward the SAME degraded/offline
// thresholds every other soft failure type uses (this task's non-goal: "no
// change to state.go thresholds") — repeated timeouts eventually do reach
// OFFLINE via the threshold path, not the Auth/NotFound unconditional path.
func TestRecordFailure_Timeout_AdvancesThroughThresholdsLikeSoftFailure(t *testing.T) {
	bs := New("b")
	const degraded, offline = 2, 4
	var last StatusChange
	for i := 0; i < offline; i++ {
		last = bs.RecordFailure(ErrTypeTimeout, "context deadline exceeded", time.Time{}, degraded, offline)
	}
	if last.To != StatusOffline {
		t.Errorf("after %d consecutive ErrTypeTimeout failures (offlineThreshold=%d), status = %q, want %q", offline, offline, last.To, StatusOffline)
	}
}

// TestRecordFailure_ModelConfig_HardOffline covers B5's sticky-offline
// requirement: a single ErrTypeModelConfig failure transitions straight to
// OFFLINE regardless of threshold, mirroring Auth/NotFound — no probe
// interval or retry makes a misconfigured model identifier valid.
func TestRecordFailure_ModelConfig_HardOffline(t *testing.T) {
	bs := New("b")
	change := bs.RecordFailure(ErrTypeModelConfig, "provided model identifier is invalid", time.Time{}, 3, 6)
	if change.To != StatusOffline {
		t.Errorf("RecordFailure(ErrTypeModelConfig, ...) on first failure = %q, want %q (sticky hard-offline, same bucket as Auth/NotFound)", change.To, StatusOffline)
	}
}

// TestRecordFailure_ModelConfig_NoResetTimeSet covers the "not re-probed"
// half of B5's acceptance at the state layer: ErrTypeModelConfig must not
// set QuotaExhausted or a reset time, so HasPendingReset (which only
// recognizes quota/rate-limit reset ownership) correctly reports false — the
// router-layer skip in offlineRecoveryProbe is what actually prevents
// re-probing (see router package), but that skip is keyed on LastErrorType,
// which this test confirms is set correctly.
func TestRecordFailure_ModelConfig_NoResetTimeSet(t *testing.T) {
	bs := New("b")
	bs.RecordFailure(ErrTypeModelConfig, "provided model identifier is invalid", time.Time{}, 3, 6)
	snap := bs.Snapshot()
	if snap.QuotaExhausted {
		t.Error("ErrTypeModelConfig must not set QuotaExhausted — it is a config fault, not a quota fault")
	}
	if !snap.QuotaResetAt.IsZero() {
		t.Error("ErrTypeModelConfig must not set a QuotaResetAt reset time")
	}
	if !snap.RateLimitResetAt.IsZero() {
		t.Error("ErrTypeModelConfig must not set a RateLimitResetAt reset time")
	}
	if snap.LastErrorType != ErrTypeModelConfig {
		t.Errorf("LastErrorType = %q, want %q", snap.LastErrorType, ErrTypeModelConfig)
	}
	if bs.HasPendingReset() {
		t.Error("HasPendingReset() = true for a model_config failure — must be false so the router's dedicated model_config skip (not HasPendingReset) is what governs re-probe suppression")
	}
}
