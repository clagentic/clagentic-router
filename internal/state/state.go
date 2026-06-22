// internal/state/state.go — BackendState and state machine transitions.
//
// BackendState tracks the live health, quota, and rate limit state for one backend.
// All mutations go through the state machine methods, which enforce valid transitions
// and update derived fields (consecutive failures, rate window reset, etc.).
//
// BackendState is protected by a RWMutex. Callers use Snapshot() for reads.
package state

import (
	"sync"
	"time"
)

// Status is the current lifecycle state of a backend.
type Status string

const (
	StatusUnknown    Status = "unknown"
	StatusHealthy    Status = "healthy"
	StatusDegraded   Status = "degraded"
	StatusOffline    Status = "offline"
	StatusRecovering Status = "recovering"
)

// ErrorType mirrors backend.ErrorType to avoid an import cycle.
// String values are identical.
type ErrorType string

const (
	ErrTypeQuota     ErrorType = "quota"
	ErrTypeRateLimit ErrorType = "rate_limit"
	ErrTypeAuth      ErrorType = "auth"
	ErrTypeNetwork   ErrorType = "network"
	ErrTypeTimeout   ErrorType = "timeout"
	ErrTypeNotFound  ErrorType = "not_found"
	ErrTypeSchema    ErrorType = "schema"
	ErrTypeUnknown   ErrorType = "unknown"
)

// latencyEMAlpha is the EMA smoothing factor for call latency.
// Higher = faster to adapt to recent values. 0.3 gives a ~3-call half-life.
const latencyEMAlpha = 0.3

// BackendState is the authoritative live state for one backend.
// All fields are protected by mu.
type BackendState struct {
	mu sync.RWMutex

	BackendID string
	Status    Status

	ConsecutiveFailures int
	LastSuccessAt       time.Time // zero = never
	LastFailureAt       time.Time // zero = never
	LastErrorType       ErrorType
	LastErrorRaw        string

	// Quota (hard limit — daily/monthly credit)
	QuotaExhausted       bool
	QuotaResetAt         time.Time // zero = unknown
	QuotaTokensRemaining int64     // -1 = unknown
	QuotaTokensTotal     int64     // -1 = unknown
	// QuotaLowAlerted is set when a quota_low alert has been fired for the current
	// low-quota period. Cleared when quota rises back above the warning threshold,
	// preventing repeated alerts for the same event (edge-trigger contract).
	QuotaLowAlerted bool

	// Rate limit (soft limit — requests per time window)
	RateLimitResetAt    time.Time // zero = unknown
	RateWindowMessages  int
	RateWindowTokensEst int64
	RateWindowStart     time.Time // zero = window not started

	// RateLimit* fields are updated from provider response headers on every call.
	// These reflect per-minute windows, distinct from billing quota fields.
	// Ephemeral: not persisted to SQLite (windows reset frequently; stale data at
	// restart is worse than no data).
	RateLimitTokensRemaining   int64
	RateLimitTokensResetAt     time.Time
	RateLimitRequestsRemaining int64
	RateLimitRequestsResetAt   time.Time

	// Latency tracking — exponential moving average over successful calls, in milliseconds.
	// Zero means no data yet.
	LatencyEMAMs float64

	// LocalCapacity fields — set by llama.cpp or Ollama pollers via SetLocalCapacity.
	// Zero values mean no capacity data has been received yet.
	// Ephemeral: not persisted to SQLite (same rationale as RateLimit fields —
	// stale data at restart is worse than no data for capacity signals).
	LocalSlotsIdle         int
	LocalSlotsTotal        int
	LocalVRAMHeadroomPct   float64   // 0.0–1.0; -1.0 = unknown
	LocalModelHot          bool
	LocalCapacityUpdatedAt time.Time

	// Cost / usage tracking
	TotalCalls        int64
	TotalTokensEst    int64
	TotalCostUSDEst   float64
	SessionCostUSDEst float64

	// LastQuotaSnapshot is the most recent rate_limit_event summary received from
	// the claude CLI. nil until the first event arrives. Ephemeral — not persisted
	// to SQLite (repopulated on the next live request after restart).
	LastQuotaSnapshot *QuotaSnapshot

	// LastRecoveryProbeAt is the last time an offline-recovery probe was attempted
	// for this backend. Zero if no probe has been attempted. Ephemeral — not
	// persisted to SQLite; a missed probe window at restart is harmless.
	LastRecoveryProbeAt time.Time

	UpdatedAt time.Time
}

// Snapshot is an immutable copy of BackendState for reading without holding the lock.
type Snapshot struct {
	BackendID string
	Status    Status

	ConsecutiveFailures int
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	LastErrorType       ErrorType
	LastErrorRaw        string

	QuotaExhausted       bool
	QuotaResetAt         time.Time
	QuotaTokensRemaining int64
	QuotaTokensTotal     int64
	QuotaLowAlerted      bool

	RateLimitResetAt    time.Time
	RateWindowMessages  int
	RateWindowTokensEst int64
	RateWindowStart     time.Time

	// RateLimit* fields are updated from provider response headers on every call.
	// These reflect per-minute windows, distinct from billing quota fields.
	// Ephemeral: not persisted to SQLite (windows reset frequently; stale data at
	// restart is worse than no data).
	RateLimitTokensRemaining   int64
	RateLimitTokensResetAt     time.Time
	RateLimitRequestsRemaining int64
	RateLimitRequestsResetAt   time.Time

	// LatencyEMAMs is the exponential moving average of successful call latencies (ms).
	// Zero means no data yet.
	LatencyEMAMs float64

	// LocalCapacity fields — set by llama.cpp or Ollama pollers via SetLocalCapacity.
	// Zero values mean no capacity data has been received yet.
	// Ephemeral: not persisted to SQLite.
	LocalSlotsIdle         int
	LocalSlotsTotal        int
	LocalVRAMHeadroomPct   float64   // 0.0–1.0; -1.0 = unknown
	LocalModelHot          bool
	LocalCapacityUpdatedAt time.Time

	TotalCalls        int64
	TotalTokensEst    int64
	TotalCostUSDEst   float64
	SessionCostUSDEst float64

	// LastQuotaSnapshot is the most recent rate_limit_event summary. Ephemeral —
	// not persisted to SQLite; repopulated on the next live request after restart.
	LastQuotaSnapshot *QuotaSnapshot

	// LastRecoveryProbeAt is the last time an offline-recovery probe was attempted.
	// Ephemeral — not persisted to SQLite.
	LastRecoveryProbeAt time.Time

	UpdatedAt time.Time
}

// New creates a new BackendState for the given backend ID.
func New(backendID string) *BackendState {
	return &BackendState{
		BackendID:            backendID,
		Status:               StatusUnknown,
		QuotaTokensRemaining: -1,
		QuotaTokensTotal:     -1,
		UpdatedAt:            time.Now().UTC(),
	}
}

// Snapshot returns an immutable copy for reading.
func (s *BackendState) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		BackendID:            s.BackendID,
		Status:               s.Status,
		ConsecutiveFailures:  s.ConsecutiveFailures,
		LastSuccessAt:        s.LastSuccessAt,
		LastFailureAt:        s.LastFailureAt,
		LastErrorType:        s.LastErrorType,
		LastErrorRaw:         s.LastErrorRaw,
		QuotaExhausted:       s.QuotaExhausted,
		QuotaResetAt:         s.QuotaResetAt,
		QuotaTokensRemaining: s.QuotaTokensRemaining,
		QuotaTokensTotal:     s.QuotaTokensTotal,
		QuotaLowAlerted:      s.QuotaLowAlerted,
		RateLimitResetAt:           s.RateLimitResetAt,
		RateWindowMessages:         s.RateWindowMessages,
		RateWindowTokensEst:        s.RateWindowTokensEst,
		RateWindowStart:            s.RateWindowStart,
		RateLimitTokensRemaining:   s.RateLimitTokensRemaining,
		RateLimitTokensResetAt:     s.RateLimitTokensResetAt,
		RateLimitRequestsRemaining: s.RateLimitRequestsRemaining,
		RateLimitRequestsResetAt:   s.RateLimitRequestsResetAt,
		LatencyEMAMs:               s.LatencyEMAMs,
		LocalSlotsIdle:         s.LocalSlotsIdle,
		LocalSlotsTotal:        s.LocalSlotsTotal,
		LocalVRAMHeadroomPct:   s.LocalVRAMHeadroomPct,
		LocalModelHot:          s.LocalModelHot,
		LocalCapacityUpdatedAt: s.LocalCapacityUpdatedAt,
		TotalCalls:           s.TotalCalls,
		TotalTokensEst:       s.TotalTokensEst,
		TotalCostUSDEst:      s.TotalCostUSDEst,
		SessionCostUSDEst:    s.SessionCostUSDEst,
		LastQuotaSnapshot:    s.LastQuotaSnapshot,
		LastRecoveryProbeAt:  s.LastRecoveryProbeAt,
		UpdatedAt:            s.UpdatedAt,
	}
}

// RecordSuccess records a successful invocation and transitions the state machine.
// promptTokens and completionTokens are best-effort estimates.
// costUSD is the provider-reported cost (0 if unknown).
// latencyMS is the wall-clock call duration used to update the latency EMA.
func (s *BackendState) RecordSuccess(promptTokens, completionTokens int, costUSD float64, latencyMS int64, degradedThreshold, offlineThreshold int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	s.LastSuccessAt = now
	s.ConsecutiveFailures = 0
	s.LastErrorType = ""
	s.LastErrorRaw = ""

	// Advance state machine
	switch s.Status {
	case StatusUnknown, StatusDegraded, StatusRecovering:
		s.Status = StatusHealthy
	case StatusOffline:
		s.Status = StatusRecovering // one success from offline → recovering, not healthy
	}

	// Latency EMA — seed with first sample, then smooth
	if latencyMS > 0 {
		if s.LatencyEMAMs == 0 {
			s.LatencyEMAMs = float64(latencyMS)
		} else {
			s.LatencyEMAMs = latencyEMAlpha*float64(latencyMS) + (1-latencyEMAlpha)*s.LatencyEMAMs
		}
	}

	// Usage tracking
	tokens := int64(promptTokens + completionTokens)
	s.TotalCalls++
	s.TotalTokensEst += tokens
	s.TotalCostUSDEst += costUSD
	s.SessionCostUSDEst += costUSD

	// Rate window
	s.recordWindowCall(now, tokens)

	// Reduce quota estimate if known
	if s.QuotaTokensRemaining > 0 {
		s.QuotaTokensRemaining -= tokens
		if s.QuotaTokensRemaining < 0 {
			s.QuotaTokensRemaining = 0
		}
	}

	s.UpdatedAt = now
}

// RecordFailure records a failed invocation and transitions the state machine.
func (s *BackendState) RecordFailure(errType ErrorType, errRaw string, resetAt time.Time, degradedThreshold, offlineThreshold int) StatusChange {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	prev := s.Status

	s.LastFailureAt = now
	s.ConsecutiveFailures++
	s.LastErrorType = errType
	s.LastErrorRaw = errRaw

	// Hard transitions on specific error types
	switch errType {
	case ErrTypeQuota:
		s.QuotaExhausted = true
		if !resetAt.IsZero() {
			s.QuotaResetAt = resetAt
		}
		s.Status = StatusOffline
	case ErrTypeAuth, ErrTypeNotFound:
		// Permanent until operator intervenes
		s.Status = StatusOffline
	case ErrTypeRateLimit:
		if !resetAt.IsZero() {
			s.RateLimitResetAt = resetAt
		}
		// Rate limit is degraded, not fully offline
		if s.Status == StatusHealthy || s.Status == StatusUnknown {
			s.Status = StatusDegraded
		}
	default:
		// Soft failure — advance through thresholds
		switch {
		case s.ConsecutiveFailures >= offlineThreshold:
			s.Status = StatusOffline
		case s.ConsecutiveFailures >= degradedThreshold:
			s.Status = StatusDegraded
		}
	}

	s.UpdatedAt = now
	return StatusChange{BackendID: s.BackendID, From: prev, To: s.Status, ErrorType: errType}
}

// TryRecover checks if a quota/rate-limit reset time has passed and transitions
// to StatusRecovering if so. Called by the background probe loop.
// Returns true if the backend should be probed.
func (s *BackendState) TryRecover() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status != StatusOffline {
		return false
	}

	now := time.Now().UTC()

	// Quota reset
	if s.QuotaExhausted && !s.QuotaResetAt.IsZero() && now.After(s.QuotaResetAt) {
		s.QuotaExhausted = false
		s.QuotaResetAt = time.Time{}
		s.QuotaTokensRemaining = -1
		s.Status = StatusRecovering
		s.ConsecutiveFailures = 0
		s.UpdatedAt = now
		return true
	}

	// Rate limit window reset
	if !s.RateLimitResetAt.IsZero() && now.After(s.RateLimitResetAt) {
		s.RateLimitResetAt = time.Time{}
		s.RateWindowMessages = 0
		s.RateWindowTokensEst = 0
		s.RateWindowStart = time.Time{}
		s.Status = StatusRecovering
		s.ConsecutiveFailures = 0
		s.UpdatedAt = now
		return true
	}

	return false
}

// HasPendingReset returns true when the backend is OFFLINE due to quota or
// rate-limit exhaustion AND the known reset time has not yet passed.
// When this returns true the offline-recovery probe should be skipped:
// TryRecover() already owns these backends and will transition them once the
// reset time elapses.
func (s *BackendState) HasPendingReset() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Status != StatusOffline {
		return false
	}
	now := time.Now().UTC()
	// Quota offline with a future reset time — TryRecover will handle it.
	if s.QuotaExhausted && !s.QuotaResetAt.IsZero() && s.QuotaResetAt.After(now) {
		return true
	}
	// Rate-limit offline with a future reset time — TryRecover will handle it.
	if !s.RateLimitResetAt.IsZero() && s.RateLimitResetAt.After(now) {
		return true
	}
	return false
}

// MarkRecoveryProbed records the current time as the last offline-recovery probe
// attempt timestamp. Called by offlineRecoveryProbe after each probe (success or
// failure) so the interval gate prevents hammering.
func (s *BackendState) MarkRecoveryProbed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastRecoveryProbeAt = time.Now().UTC()
}

// RecoveryProbeDue returns true if enough time has passed since the last recovery
// probe (or no probe has ever been attempted). intervalSeconds == 0 always returns false.
func (s *BackendState) RecoveryProbeDue(intervalSeconds int) bool {
	if intervalSeconds <= 0 {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.LastRecoveryProbeAt.IsZero() {
		return true
	}
	return time.Since(s.LastRecoveryProbeAt) >= time.Duration(intervalSeconds)*time.Second
}

// ForceOffline manually sets the backend offline. Used by /backends/{id}/disable.
func (s *BackendState) ForceOffline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusOffline
	s.UpdatedAt = time.Now().UTC()
}

// ForceReset clears error state and sets status to Unknown for re-probing.
// Used by /backends/{id}/reset.
func (s *BackendState) ForceReset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = StatusUnknown
	s.ConsecutiveFailures = 0
	s.QuotaExhausted = false
	s.QuotaResetAt = time.Time{}
	s.QuotaTokensRemaining = -1
	s.RateLimitResetAt = time.Time{}
	s.LastErrorType = ""
	s.LastErrorRaw = ""
	s.UpdatedAt = time.Now().UTC()
}

// ResetRateWindow resets the rate window counters. Called when the window expires.
func (s *BackendState) resetRateWindow(now time.Time) {
	s.RateWindowMessages = 0
	s.RateWindowTokensEst = 0
	s.RateWindowStart = now
}

// recordWindowCall updates rate window counters. Must be called with lock held.
func (s *BackendState) recordWindowCall(now time.Time, tokens int64) {
	// Reset window if expired (window length is not tracked in state; caller manages)
	// For now just increment — window expiry is handled by TryRecover.
	if s.RateWindowStart.IsZero() {
		s.RateWindowStart = now
	}
	s.RateWindowMessages++
	s.RateWindowTokensEst += tokens
}

// RestoreFromSnapshot restores state from a persisted snapshot (called at startup).
func (s *BackendState) RestoreFromSnapshot(snap Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Status = snap.Status
	s.ConsecutiveFailures = snap.ConsecutiveFailures
	s.LastSuccessAt = snap.LastSuccessAt
	s.LastFailureAt = snap.LastFailureAt
	s.LastErrorType = snap.LastErrorType
	s.LastErrorRaw = snap.LastErrorRaw
	s.QuotaExhausted = snap.QuotaExhausted
	s.QuotaResetAt = snap.QuotaResetAt
	s.QuotaTokensRemaining = snap.QuotaTokensRemaining
	s.QuotaTokensTotal = snap.QuotaTokensTotal
	s.RateLimitResetAt = snap.RateLimitResetAt
	s.RateWindowMessages = snap.RateWindowMessages
	s.RateWindowTokensEst = snap.RateWindowTokensEst
	s.RateWindowStart = snap.RateWindowStart
	s.QuotaLowAlerted = snap.QuotaLowAlerted
	s.LatencyEMAMs = snap.LatencyEMAMs
	s.TotalCalls = snap.TotalCalls
	s.TotalTokensEst = snap.TotalTokensEst
	s.TotalCostUSDEst = snap.TotalCostUSDEst
	// SessionCostUSDEst intentionally not restored — resets each daemon start
	s.UpdatedAt = snap.UpdatedAt
}

// UpdateRateLimitFromResponse stores rate-limit header data harvested from a
// provider response. Called by the router after every successful Invoke.
// Parameters are passed individually to avoid an import cycle (state must not
// import backend).
func (s *BackendState) UpdateRateLimitFromResponse(
	tokensRemaining, requestsRemaining int64,
	tokensResetAt, requestsResetAt time.Time,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.RateLimitTokensRemaining = tokensRemaining
	s.RateLimitTokensResetAt = tokensResetAt
	s.RateLimitRequestsRemaining = requestsRemaining
	s.RateLimitRequestsResetAt = requestsResetAt
	s.UpdatedAt = time.Now().UTC()
}

// TestAndSetQuotaLow atomically checks whether the quota-low condition is newly
// true (below threshold) or newly false (recovered above threshold).
// Returns (firedAlert, clearedAlert):
//   - firedAlert: true if quota just crossed below the threshold (caller should fire quota_low alert)
//   - clearedAlert: true if quota recovered above threshold (informational; no alert defined yet)
//
// threshold is the fractional quota remaining at which to alert (0.0–1.0).
func (s *BackendState) TestAndSetQuotaLow(threshold float64) (firedAlert bool, clearedAlert bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.QuotaTokensTotal <= 0 || s.QuotaTokensRemaining < 0 {
		return false, false
	}
	ratio := float64(s.QuotaTokensRemaining) / float64(s.QuotaTokensTotal)
	isLow := ratio < threshold
	if isLow && !s.QuotaLowAlerted {
		s.QuotaLowAlerted = true
		return true, false
	}
	if !isLow && s.QuotaLowAlerted {
		s.QuotaLowAlerted = false
		return false, true
	}
	return false, false
}

// SetQuotaFromUsage updates the quota fields from an external usage poll (e.g. OpenAI billing API).
// remainingUnits and totalUnits are in the same arbitrary units (caller chooses scale).
// resetAt is the next quota reset time; zero if unknown.
// If remainingUnits <= 0, the backend is marked quota-exhausted and taken offline.
func (s *BackendState) SetQuotaFromUsage(remainingUnits, totalUnits int64, resetAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.QuotaTokensRemaining = remainingUnits
	s.QuotaTokensTotal = totalUnits
	if !resetAt.IsZero() {
		s.QuotaResetAt = resetAt
	}
	if remainingUnits <= 0 {
		s.QuotaExhausted = true
		s.Status = StatusOffline
	} else if s.QuotaExhausted {
		// Quota came back — allow re-probe and reset failure counter so a single
		// subsequent failure doesn't immediately re-trip the offline threshold.
		s.QuotaExhausted = false
		s.ConsecutiveFailures = 0
		if s.Status == StatusOffline {
			s.Status = StatusRecovering
		}
	}
	s.UpdatedAt = time.Now().UTC()
}

// SetRateLimitWarning records a rate-limit warning received from an external source
// (e.g. clagentic-console SDK rate_limit_event). Does not change Status — only updates
// the rate-limit window fields so the scorer can apply a soft penalty on subsequent
// requests. Use RecordFailure(ErrTypeQuota, ...) for exhausted (hard-offline) events.
//
// limitType is informational (five_hour, seven_day, seven_day_sonnet, seven_day_opus).
// resetsAt is the reset timestamp; zero is accepted when the value is unknown.
func (s *BackendState) SetRateLimitWarning(limitType string, resetsAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Signal the scorer via RateLimitTokensRemaining: set to 1 (non-zero so the
	// scorer's "remaining > 0" guard passes, but low enough to depress the score).
	// Requests remaining unknown; leave RateLimitRequestsRemaining at its current value.
	s.RateLimitTokensRemaining = 1
	if !resetsAt.IsZero() {
		s.RateLimitTokensResetAt = resetsAt
	}
	s.UpdatedAt = time.Now().UTC()
}

// SetLocalCapacity stores slot and VRAM headroom data from a local backend poller.
// Called by the router's OnUpdate callback when a LlamaCppPoller or OllamaPoller
// delivers a fresh reading. Parameters use primitives to avoid importing backend.
//
// slotsIdle and slotsTotal are from llama.cpp /health; both 0 when not applicable.
// vramHeadroomPct is VRAMHeadroom/VRAMTotal in [0.0, 1.0]; -1.0 when VRAMTotal is unknown.
// modelHot is true when the target model is confirmed loaded (Ollama /api/ps).
func (s *BackendState) SetLocalCapacity(slotsIdle, slotsTotal int, vramHeadroomPct float64, modelHot bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LocalSlotsIdle = slotsIdle
	s.LocalSlotsTotal = slotsTotal
	s.LocalVRAMHeadroomPct = vramHeadroomPct
	s.LocalModelHot = modelHot
	s.LocalCapacityUpdatedAt = time.Now().UTC()
}

// UpdateRateWindow checks if the rate window has expired and resets if so.
// windowSeconds is loaded from backend config.
func (s *BackendState) UpdateRateWindow(windowSeconds int) {
	if windowSeconds <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RateWindowStart.IsZero() {
		return
	}
	elapsed := time.Since(s.RateWindowStart).Seconds()
	if elapsed >= float64(windowSeconds) {
		s.resetRateWindow(time.Now().UTC())
	}
}

// StatusChange describes a backend status transition.
type StatusChange struct {
	BackendID string
	From      Status
	To        Status
	ErrorType ErrorType
}

// Changed returns true if the status actually changed.
func (sc StatusChange) Changed() bool {
	return sc.From != sc.To
}
