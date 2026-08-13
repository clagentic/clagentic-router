// internal/router/scorer_test.go — unit tests for the backend scoring algorithm.
//
// Score is a pure deterministic function — no randomness, no network calls.
// Tests assert exact ordering and boundary conditions without averaging loops.
package router

import (
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// defaultRoutingCfg returns a RoutingConfig with the same defaults as config.validate().
func defaultRoutingCfg() *config.RoutingConfig {
	return &config.RoutingConfig{
		Strategy:                   "scored",
		QuotaWarningThreshold:      0.2,
		HealthProbeIntervalSeconds: 120,
		QuotaPollIntervalSeconds:   300,
		DegradedFailureThreshold:   3,
		OfflineFailureThreshold:    6,
		LatencyPenaltyThresholdMs:  15000,
	}
}

// defaultBackendCfg returns a BackendConfig with neutral defaults.
func defaultBackendCfg() *config.BackendConfig {
	return &config.BackendConfig{
		CostWeight: 1.0,
	}
}

// healthySnap returns a Snapshot in StatusHealthy with no quota or rate pressure.
func healthySnap() state.Snapshot {
	return state.Snapshot{
		BackendID:            "test",
		Status:               state.StatusHealthy,
		QuotaTokensRemaining: -1, // unknown
		QuotaTokensTotal:     -1,
		LastSuccessAt:        time.Now().UTC(),
	}
}

func TestScore_OfflineIsZero(t *testing.T) {
	snap := healthySnap()
	snap.Status = state.StatusOffline
	if s := Score(snap, defaultBackendCfg(), defaultRoutingCfg(), 100); s != 0.0 {
		t.Errorf("offline: want 0.0, got %f", s)
	}
}

func TestScore_QuotaExhaustedIsZero(t *testing.T) {
	snap := healthySnap()
	snap.QuotaExhausted = true
	if s := Score(snap, defaultBackendCfg(), defaultRoutingCfg(), 100); s != 0.0 {
		t.Errorf("quota exhausted: want 0.0, got %f", s)
	}
}

func TestScore_RequestWontFitIsZero(t *testing.T) {
	snap := healthySnap()
	snap.QuotaTokensRemaining = 500
	snap.QuotaTokensTotal = 10000
	if s := Score(snap, defaultBackendCfg(), defaultRoutingCfg(), 1000); s != 0.0 {
		t.Errorf("request too large: want 0.0, got %f", s)
	}
}

func TestScore_RequestFitsIsPositive(t *testing.T) {
	snap := healthySnap()
	snap.QuotaTokensRemaining = 5000
	snap.QuotaTokensTotal = 10000
	if s := Score(snap, defaultBackendCfg(), defaultRoutingCfg(), 100); s <= 0.0 {
		t.Errorf("request fits: want >0.0, got %f", s)
	}
}

func TestScore_QuotaPressureReducesScore(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0

	snapHigh := healthySnap()
	snapHigh.QuotaTokensRemaining = 9000
	snapHigh.QuotaTokensTotal = 10000 // 90%

	snapLow := healthySnap()
	snapLow.QuotaTokensRemaining = 500
	snapLow.QuotaTokensTotal = 10000 // 5%

	cfg := defaultBackendCfg()
	high := Score(snapHigh, cfg, rcfg, 10)
	low := Score(snapLow, cfg, rcfg, 10)
	if low >= high {
		t.Errorf("low quota should score lower: high=%.4f low=%.4f", high, low)
	}
}

func TestScore_RateWindowPressureReducesScore(t *testing.T) {
	cfg := defaultBackendCfg()
	cfg.RateWindowMaxMessages = 100

	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0

	snapFull := healthySnap()
	snapFull.RateWindowMessages = 94 // 94% — heavy penalty

	snapEmpty := healthySnap()
	snapEmpty.RateWindowMessages = 10 // 10% — no penalty

	full := Score(snapFull, cfg, rcfg, 10)
	empty := Score(snapEmpty, cfg, rcfg, 10)
	if full >= empty {
		t.Errorf("near-full window should score lower: full=%.4f empty=%.4f", full, empty)
	}
}

func TestScore_RateWindowAt95PercentIsZero(t *testing.T) {
	snap := healthySnap()
	snap.RateWindowMessages = 95
	cfg := defaultBackendCfg()
	cfg.RateWindowMaxMessages = 100
	if s := Score(snap, cfg, defaultRoutingCfg(), 10); s != 0.0 {
		t.Errorf("95%% rate window: want 0.0, got %f", s)
	}
}

func TestScore_DegradedLowerThanHealthy(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	healthy := Score(healthySnap(), cfg, rcfg, 10)

	degraded := healthySnap()
	degraded.Status = state.StatusDegraded
	if Score(degraded, cfg, rcfg, 10) >= healthy {
		t.Errorf("degraded should score lower than healthy")
	}
}

func TestScore_RecoveringLowerThanHealthy(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	healthy := Score(healthySnap(), cfg, rcfg, 10)

	recovering := healthySnap()
	recovering.Status = state.StatusRecovering
	if Score(recovering, cfg, rcfg, 10) >= healthy {
		t.Errorf("recovering should score lower than healthy")
	}
}

func TestScore_StatusOrderDegradedBelowRecovering(t *testing.T) {
	// Degraded (×0.25) should score lower than Recovering (×0.4)
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	deg := healthySnap()
	deg.Status = state.StatusDegraded
	rec := healthySnap()
	rec.Status = state.StatusRecovering

	if Score(deg, cfg, rcfg, 10) >= Score(rec, cfg, rcfg, 10) {
		t.Errorf("degraded should score lower than recovering")
	}
}

func TestScore_CostWeightScalesScore(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	snap := healthySnap()

	cheap := Score(snap, &config.BackendConfig{CostWeight: 1.5}, rcfg, 10)
	expensive := Score(snap, &config.BackendConfig{CostWeight: 0.3}, rcfg, 10)
	if cheap <= expensive {
		t.Errorf("cheap backend should score higher: cheap=%.4f expensive=%.4f", cheap, expensive)
	}
}

func TestScore_LatencyPenaltyAtThreshold(t *testing.T) {
	// Exactly at threshold — no penalty.
	// Zero LastSuccessAt to eliminate recency bonus so latency is the only variable.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 15000

	atThreshold := healthySnap()
	atThreshold.LastSuccessAt = time.Time{} // no recency bonus
	atThreshold.LatencyEMAMs = 15000

	below := healthySnap()
	below.LastSuccessAt = time.Time{}
	below.LatencyEMAMs = 5000

	cfg := defaultBackendCfg()
	sAt := Score(atThreshold, cfg, rcfg, 10)
	sBelow := Score(below, cfg, rcfg, 10)
	// At threshold: score == below (no penalty kicks in until strictly above)
	if sAt != sBelow {
		t.Errorf("at threshold should equal below-threshold score: at=%.6f below=%.6f", sAt, sBelow)
	}
}

func TestScore_LatencyPenaltyAboveThreshold(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 15000

	fast := healthySnap()
	fast.LatencyEMAMs = 2000 // well below 15s

	slow := healthySnap()
	slow.LatencyEMAMs = 45000 // 3× threshold → ×0.33 penalty

	cfg := defaultBackendCfg()
	if Score(slow, cfg, rcfg, 10) >= Score(fast, cfg, rcfg, 10) {
		t.Errorf("slow backend should score lower than fast")
	}
}

func TestScore_LatencyPenaltyInverseProportion(t *testing.T) {
	// At 2× threshold the score should be exactly halved relative to no latency.
	// Zero LastSuccessAt so recency doesn't introduce time-dependent drift.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 10000

	noLatency := healthySnap()
	noLatency.LastSuccessAt = time.Time{}

	twoX := healthySnap()
	twoX.LastSuccessAt = time.Time{}
	twoX.LatencyEMAMs = 20000 // 2× threshold → multiplier = threshold/EMA = 0.5

	cfg := defaultBackendCfg()
	sNone := Score(noLatency, cfg, rcfg, 10)
	sTwoX := Score(twoX, cfg, rcfg, 10)
	want := sNone * 0.5
	if sTwoX != want {
		t.Errorf("2× latency penalty: want %.6f, got %.6f", want, sTwoX)
	}
}

func TestScore_LatencyPenaltyDisabledByZeroThreshold(t *testing.T) {
	// Zero LastSuccessAt on both so recency doesn't cause time-dependent drift.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0 // disabled

	verySlow := healthySnap()
	verySlow.LastSuccessAt = time.Time{}
	verySlow.LatencyEMAMs = 300000 // 5 minutes

	noLatency := healthySnap()
	noLatency.LastSuccessAt = time.Time{}

	cfg := defaultBackendCfg()
	if Score(verySlow, cfg, rcfg, 10) != Score(noLatency, cfg, rcfg, 10) {
		t.Errorf("latency penalty disabled: scores should be equal")
	}
}

func TestScore_NoLatencyDataSkipsPenalty(t *testing.T) {
	snap := healthySnap()
	snap.LatencyEMAMs = 0 // no data yet

	if s := Score(snap, defaultBackendCfg(), defaultRoutingCfg(), 10); s <= 0 {
		t.Errorf("no latency data: expected positive score, got %f", s)
	}
}

func TestScore_Deterministic(t *testing.T) {
	// Score with identical inputs must return identical outputs.
	// Zero LastSuccessAt to eliminate time.Since drift between calls.
	snap := healthySnap()
	snap.LastSuccessAt = time.Time{} // no recency bonus — removes time dependency
	snap.QuotaTokensRemaining = 7000
	snap.QuotaTokensTotal = 10000
	snap.LatencyEMAMs = 5000
	snap.RateWindowMessages = 20
	cfg := &config.BackendConfig{CostWeight: 1.2, RateWindowMaxMessages: 50}
	rcfg := defaultRoutingCfg()

	first := Score(snap, cfg, rcfg, 100)
	for i := 0; i < 20; i++ {
		if s := Score(snap, cfg, rcfg, 100); s != first {
			t.Errorf("Score is not deterministic: first=%.6f iter%d=%.6f", first, i, s)
		}
	}
}

func TestScore_RateLimitWindowPressureLowTokens(t *testing.T) {
	// When RateLimitTokensRemaining is below the configured threshold and reset is
	// in the future, the score should be penalized (×0.3 multiplier applied).
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	rcfg.RateLimitTokensWarningThreshold = 1000 // explicit threshold
	cfg := defaultBackendCfg()

	plenty := healthySnap()
	plenty.LastSuccessAt = time.Time{}
	plenty.RateLimitTokensRemaining = 5000
	plenty.RateLimitTokensResetAt = time.Now().Add(5 * time.Minute)

	scarce := healthySnap()
	scarce.LastSuccessAt = time.Time{}
	scarce.RateLimitTokensRemaining = 500 // below 1000 threshold
	scarce.RateLimitTokensResetAt = time.Now().Add(5 * time.Minute)

	sPlenty := Score(plenty, cfg, rcfg, 10)
	sScarce := Score(scarce, cfg, rcfg, 10)
	if sScarce >= sPlenty {
		t.Errorf("scarce rate-limit tokens should score lower: plenty=%.4f scarce=%.4f", sPlenty, sScarce)
	}
}

func TestScore_RateLimitWindowPressureNoDataSkipped(t *testing.T) {
	// When RateLimitTokensRemaining is 0 (no header data), the penalty is not applied.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	rcfg.RateLimitTokensWarningThreshold = 1000 // explicit threshold
	cfg := defaultBackendCfg()

	noData := healthySnap()
	noData.LastSuccessAt = time.Time{}
	noData.RateLimitTokensRemaining = 0 // no header data

	scarce := healthySnap()
	scarce.LastSuccessAt = time.Time{}
	scarce.RateLimitTokensRemaining = 500
	scarce.RateLimitTokensResetAt = time.Now().Add(5 * time.Minute)

	sNoData := Score(noData, cfg, rcfg, 10)
	sScarce := Score(scarce, cfg, rcfg, 10)
	// No-data case should score equal to a healthy backend, not penalized.
	if sNoData <= sScarce {
		t.Errorf("no-data backend should score higher than scarce: noData=%.4f scarce=%.4f", sNoData, sScarce)
	}
}

func TestScore_RateLimitWindowPressureExpiredResetSkipped(t *testing.T) {
	// When ResetAt is in the past, the data is stale — penalty must not be applied.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	rcfg.RateLimitTokensWarningThreshold = 1000 // explicit threshold
	cfg := defaultBackendCfg()

	staleData := healthySnap()
	staleData.LastSuccessAt = time.Time{}
	staleData.RateLimitTokensRemaining = 100                            // below threshold but stale
	staleData.RateLimitTokensResetAt = time.Now().Add(-1 * time.Minute) // past

	noData := healthySnap()
	noData.LastSuccessAt = time.Time{}
	noData.RateLimitTokensRemaining = 0

	// Stale data should NOT be penalized; scores should be equal
	sStale := Score(staleData, cfg, rcfg, 10)
	sNoData := Score(noData, cfg, rcfg, 10)
	if sStale != sNoData {
		t.Errorf("stale rate-limit data should not be penalized: stale=%.4f noData=%.4f", sStale, sNoData)
	}
}

func TestScore_RateLimitWindowPressureDisabledByZeroThreshold(t *testing.T) {
	// When RateLimitTokensWarningThreshold=0, the penalty component is disabled.
	// A scarce backend should score identically to one with no rate-limit data.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	rcfg.RateLimitTokensWarningThreshold = 0 // disabled
	cfg := defaultBackendCfg()

	scarce := healthySnap()
	scarce.LastSuccessAt = time.Time{}
	scarce.RateLimitTokensRemaining = 50 // well below any threshold
	scarce.RateLimitTokensResetAt = time.Now().Add(5 * time.Minute)

	noData := healthySnap()
	noData.LastSuccessAt = time.Time{}
	noData.RateLimitTokensRemaining = 0 // no header data

	sScarce := Score(scarce, cfg, rcfg, 10)
	sNoData := Score(noData, cfg, rcfg, 10)
	if sScarce != sNoData {
		t.Errorf("threshold=0 should disable rate-limit penalty: scarce=%.4f noData=%.4f", sScarce, sNoData)
	}
}

func TestScore_RecencyBonusDecays(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	recent := healthySnap()
	recent.LastSuccessAt = time.Now().UTC()

	stale := healthySnap()
	stale.LastSuccessAt = time.Now().UTC().Add(-90 * time.Minute) // past full decay

	if Score(stale, cfg, rcfg, 10) >= Score(recent, cfg, rcfg, 10) {
		t.Errorf("stale backend should score lower than recent")
	}
}

func TestScore_UnknownStatusPenalty(t *testing.T) {
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	healthy := healthySnap()
	unknown := healthySnap()
	unknown.Status = state.StatusUnknown

	if Score(unknown, cfg, rcfg, 10) >= Score(healthy, cfg, rcfg, 10) {
		t.Errorf("unknown should score lower than healthy")
	}
}

func TestScore_LocalSlotsZeroIdleIsHardBlock(t *testing.T) {
	// 0% idle slots → hard block (score must be 0).
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	snap := healthySnap()
	snap.LastSuccessAt = time.Time{}
	snap.LocalSlotsTotal = 4
	snap.LocalSlotsIdle = 0 // all slots busy

	if s := Score(snap, cfg, rcfg, 10); s != 0.0 {
		t.Errorf("0 idle slots: want 0.0 (hard block), got %f", s)
	}
}

func TestScore_LocalSlotsHeavyPressure(t *testing.T) {
	// <25% idle → ×0.3 penalty applied.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	full := healthySnap()
	full.LastSuccessAt = time.Time{}
	full.LocalSlotsTotal = 4
	full.LocalSlotsIdle = 4 // 100% idle — no penalty

	heavy := healthySnap()
	heavy.LastSuccessAt = time.Time{}
	heavy.LocalSlotsTotal = 4
	heavy.LocalSlotsIdle = 1 // 25% idle — triggers heavy penalty

	sFull := Score(full, cfg, rcfg, 10)
	sHeavy := Score(heavy, cfg, rcfg, 10)
	if sHeavy >= sFull {
		t.Errorf("heavy slot pressure should score lower: full=%.4f heavy=%.4f", sFull, sHeavy)
	}
}

func TestScore_LocalSlotsModerate(t *testing.T) {
	// 25–50% idle → ×0.6 penalty.
	// 50–100% idle → no slot penalty.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	noPress := healthySnap()
	noPress.LastSuccessAt = time.Time{}
	noPress.LocalSlotsTotal = 4
	noPress.LocalSlotsIdle = 2 // 50% idle — no penalty

	moderate := healthySnap()
	moderate.LastSuccessAt = time.Time{}
	moderate.LocalSlotsTotal = 8
	moderate.LocalSlotsIdle = 2 // 25% idle — moderate penalty (×0.6)

	sNoPress := Score(noPress, cfg, rcfg, 10)
	sModerate := Score(moderate, cfg, rcfg, 10)
	if sModerate >= sNoPress {
		t.Errorf("moderate slot pressure should score lower: noPress=%.4f moderate=%.4f", sNoPress, sModerate)
	}
}

func TestScore_LocalSlotsFullyIdle(t *testing.T) {
	// 100% idle → no slot penalty; score must be positive.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	snap := healthySnap()
	snap.LastSuccessAt = time.Time{}
	snap.LocalSlotsTotal = 4
	snap.LocalSlotsIdle = 4 // 100% idle

	if s := Score(snap, cfg, rcfg, 10); s <= 0 {
		t.Errorf("100%% idle slots: expected positive score, got %f", s)
	}
}

func TestScore_LocalSlotsNoData(t *testing.T) {
	// When LocalSlotsTotal=0 (no data from poller), the slot component is skipped.
	// Score should be identical to a snapshot with no local capacity data at all.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	noData := healthySnap()
	noData.LastSuccessAt = time.Time{}

	withZero := healthySnap()
	withZero.LastSuccessAt = time.Time{}
	withZero.LocalSlotsTotal = 0 // explicit zero — same as no data

	if Score(noData, cfg, rcfg, 10) != Score(withZero, cfg, rcfg, 10) {
		t.Errorf("LocalSlotsTotal=0 should produce same score as no local capacity data")
	}
}

func TestScore_LocalModelColdPenalty(t *testing.T) {
	// Model not loaded (cold) + Ollama-style (LocalSlotsTotal=0) → ×0.4 penalty.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	hot := healthySnap()
	hot.LastSuccessAt = time.Time{}
	hot.LocalSlotsTotal = 0 // Ollama (no slot API)
	hot.LocalModelHot = true
	hot.LocalCapacityUpdatedAt = time.Now().UTC()

	cold := healthySnap()
	cold.LastSuccessAt = time.Time{}
	cold.LocalSlotsTotal = 0
	cold.LocalModelHot = false
	cold.LocalCapacityUpdatedAt = time.Now().UTC()

	sHot := Score(hot, cfg, rcfg, 10)
	sCold := Score(cold, cfg, rcfg, 10)
	if sCold >= sHot {
		t.Errorf("cold model should score lower than hot: hot=%.4f cold=%.4f", sHot, sCold)
	}
}

func TestScore_LocalModelColdNoPenaltyWithoutUpdatedAt(t *testing.T) {
	// When LocalCapacityUpdatedAt is zero (no data from poller yet), cold-start
	// penalty must not be applied — no confirmed cold reading.
	rcfg := defaultRoutingCfg()
	rcfg.LatencyPenaltyThresholdMs = 0
	cfg := defaultBackendCfg()

	noPoll := healthySnap()
	noPoll.LastSuccessAt = time.Time{}
	noPoll.LocalSlotsTotal = 0
	noPoll.LocalModelHot = false
	// LocalCapacityUpdatedAt intentionally left as zero time

	coldWithData := healthySnap()
	coldWithData.LastSuccessAt = time.Time{}
	coldWithData.LocalSlotsTotal = 0
	coldWithData.LocalModelHot = false
	coldWithData.LocalCapacityUpdatedAt = time.Now().UTC()

	sNoPoll := Score(noPoll, cfg, rcfg, 10)
	sCold := Score(coldWithData, cfg, rcfg, 10)
	// No-poll case should score higher (no penalty) than confirmed cold
	if sNoPoll <= sCold {
		t.Errorf("no-poll backend should score higher than confirmed cold: noPoll=%.4f cold=%.4f", sNoPoll, sCold)
	}
}
