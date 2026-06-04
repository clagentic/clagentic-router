// internal/router/scorer.go — backend scoring algorithm.
//
// Score assigns a float64 score to a backend given its current state and config.
// Higher score = preferred. Score 0.0 = do not use (hard block).
//
// The algorithm is intentionally visible and parameterized — operators tune
// cost_weight in backend config and latency_penalty_threshold_ms in routing
// config to shift routing preference without modifying code.
//
// Components (applied in order):
//  1. Hard blocks: offline, quota exhausted, request won't fit
//  2. Quota pressure: quadratic decay below 20% remaining
//  3. Rate window pressure: linear penalty above 60% fill; hard block at 95%
//  4. Status penalties: degraded, recovering, unknown
//  5. Cost weight: config-driven preference multiplier
//  6. Recency bonus: decays over 1 hour
//  7. Rate-limit window pressure: soft penalty when provider header data shows low remaining tokens
//  8. Local slot capacity: hard block at 0 idle; tiered penalty below 25%/50% idle (llama.cpp)
//  9. Local model hot check: cold-start penalty when model not loaded (Ollama)
// 10. Latency penalty: inverse-proportional above threshold
//
// Score is a pure deterministic function — it does not introduce random noise.
// Tie-breaking jitter is handled by selectBest in router.go so that Score
// remains unit-testable without variance and individual scoring decisions
// are observable in logs.
package router

import (
	"math"
	"time"

	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
)

// Score returns a routing score for a backend.
// 0.0 = hard block (offline, quota exhausted, or request won't fit).
// >0.0 = usable; higher is better.
//
// routingCfg is the global routing config; it carries latency_penalty_threshold_ms
// and other tunable parameters. Must not be nil.
func Score(snap state.Snapshot, cfg *config.BackendConfig, routingCfg *config.RoutingConfig, requestTokensEst int) float64 {
	// Hard blocks — never route here
	if snap.Status == state.StatusOffline {
		return 0.0
	}
	if snap.QuotaExhausted {
		return 0.0
	}

	// Proactive capacity skip — if we have a remaining estimate and the request
	// definitely won't fit, don't even try (saves a round-trip that will fail).
	if snap.QuotaTokensRemaining >= 0 && snap.QuotaTokensTotal > 0 {
		if int64(requestTokensEst) > snap.QuotaTokensRemaining {
			return 0.0
		}
	}

	score := 1.0

	// --- Quota pressure ---
	// When remaining capacity is known, penalize as it drops below 20%.
	if snap.QuotaTokensRemaining >= 0 && snap.QuotaTokensTotal > 0 {
		ratio := float64(snap.QuotaTokensRemaining) / float64(snap.QuotaTokensTotal)
		if ratio < 0.2 {
			// Quadratic decay: at 20% = score*1.0, at 0% = score*0.0
			score *= (ratio / 0.2) * (ratio / 0.2)
		}
	}

	// --- Rate window pressure ---
	// As the window fills, softly penalize to avoid hitting the hard limit.
	if cfg.RateWindowMaxMessages > 0 && snap.RateWindowMessages > 0 {
		pressure := float64(snap.RateWindowMessages) / float64(cfg.RateWindowMaxMessages)
		if pressure > 0.6 {
			// Linear penalty from 60% fill: at 60% = score*1.0, at 100% = score*0.05
			penalty := 1.0 - (pressure-0.6)/0.4*0.95
			score *= math.Max(0.05, penalty)
		}
		// Hard skip at 95% — preserve the last 5% for urgent calls
		if pressure >= 0.95 {
			return 0.0
		}
	}

	// --- Status penalties ---
	switch snap.Status {
	case state.StatusDegraded:
		score *= 0.25
	case state.StatusRecovering:
		// Try recovering backends but not first
		score *= 0.4
	case state.StatusUnknown:
		// Unknown is treated cautiously — might be unprobed
		score *= 0.7
	}

	// --- Cost weight ---
	// Config-driven preference. Free/local backends get higher weight;
	// expensive flagship models get lower weight so they're used only when needed.
	score *= cfg.ResolvedCostWeight()

	// --- Recency bonus ---
	// Prefer backends that recently succeeded. Decays over 1 hour.
	if !snap.LastSuccessAt.IsZero() {
		ageSec := time.Since(snap.LastSuccessAt).Seconds()
		recency := math.Max(0.5, 1.0-ageSec/3600.0)
		score *= recency
	}

	// --- Rate-limit window pressure (per-minute, from response headers) ---
	// Penalize when remaining tokens in the current window are below threshold.
	// Only applies when we have live data (TokensRemaining > 0 and ResetAt is future).
	// Threshold=0 (unset) disables this component entirely.
	if rlThreshold := routingCfg.RateLimitTokensWarningThreshold; rlThreshold > 0 {
		if snap.RateLimitTokensRemaining > 0 && snap.RateLimitTokensResetAt.After(time.Now()) {
			if snap.RateLimitTokensRemaining < rlThreshold {
				score *= 0.3
			}
		}
	}

	// --- Local backend slot capacity ---
	// For llama.cpp backends: penalize when slots are saturated.
	// Only applies when we have data (LocalSlotsTotal > 0).
	if snap.LocalSlotsTotal > 0 {
		ratio := float64(snap.LocalSlotsIdle) / float64(snap.LocalSlotsTotal)
		if ratio == 0 {
			// Hard block: no idle slots — all slots busy, caller would queue or fail.
			return 0.0
		}
		if ratio < 0.25 {
			score *= 0.3 // heavy pressure: <25% idle
		} else if ratio < 0.5 {
			score *= 0.6 // moderate pressure: <50% idle
		}
	}

	// --- Local model hot check ---
	// If we have fresh capacity data and the model is not loaded, apply a cold-start
	// penalty — first inference will incur model load latency.
	if snap.LocalCapacityUpdatedAt.After(time.Time{}) && !snap.LocalModelHot {
		// Only penalize when we have a confirmed hot/cold reading, not when
		// LocalCapacityUpdatedAt is zero (no data yet from poller).
		// LocalSlotsTotal > 0 implies llama.cpp, which serves one model and is
		// always "hot" when healthy — this branch applies mainly to Ollama.
		if snap.LocalSlotsTotal == 0 {
			score *= 0.4 // model not loaded; cold start latency expected
		}
	}

	// --- Latency penalty ---
	// If the backend's latency EMA is above the configured threshold, penalize
	// using an inverse-proportional factor: at 2× threshold → ×0.5, at 4× → ×0.25.
	// Below the threshold, no penalty. Zero EMA (no data) skips this component.
	if snap.LatencyEMAMs > 0 && routingCfg.LatencyPenaltyThresholdMs > 0 {
		threshold := float64(routingCfg.LatencyPenaltyThresholdMs)
		if snap.LatencyEMAMs > threshold {
			score *= threshold / snap.LatencyEMAMs
		}
	}

	return score
}
