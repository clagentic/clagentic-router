// internal/state/ratelimit_event.go — UpdateFromRateLimitEvent method on BackendState.
//
// UpdateFromRateLimitEvent applies quota data from a parsed rate_limit_event
// (emitted by the claude CLI in stream-json mode) to the BackendState. This
// feeds the existing scorer without changing the state machine transitions:
//   - utilization present → update quota fields via SetQuotaFromUsage
//   - utilization absent (status=allowed) → clear QuotaLowAlerted if set
//
// Parameters are passed as a plain struct to avoid importing backend. The
// caller (router) owns the type conversion from backend.RateLimitEvent.
package state

import "time"

// RateLimitEventData carries the fields from a rate_limit_event that affect
// BackendState. Using a local struct keeps the state package free of backend imports.
type RateLimitEventData struct {
	Status        string
	RateLimitType string
	ResetsAt      time.Time
	// Utilization is nil when status is "allowed" (below 0.75 threshold).
	Utilization *float64
	// LastQuotaSnapshot is the human-readable snapshot stored on BackendState
	// for the /v1/capacity endpoint. Populated by the router from the full event.
	LastQuotaSnapshot *QuotaSnapshot
}

// QuotaSnapshot is a lightweight summary of the last rate_limit_event, stored
// in BackendState for surfacing via the /v1/capacity endpoint. Ephemeral —
// not persisted to SQLite (repopulated on the next live request).
type QuotaSnapshot struct {
	Status        string    `json:"status"`
	RateLimitType string    `json:"rate_limit_type"`
	Utilization   *float64  `json:"utilization"`   // null when below threshold
	ResetsAt      time.Time `json:"resets_at"`
	ObservedAt    time.Time `json:"observed_at"`
}

// UpdateFromRateLimitEvent applies rate_limit_event data to the backend state.
//
// When utilization is present (status=allowed_warning or rejected):
//   - Calls SetQuotaFromUsage with a 1e6-unit scale derived from (1-utilization).
//   - Updates QuotaResetAt from e.ResetsAt.
//
// When utilization is nil (status=allowed, below threshold):
//   - Clears QuotaLowAlerted if set (quota has recovered).
//
// In both cases, stores LastQuotaSnapshot for the /v1/capacity endpoint.
func (s *BackendState) UpdateFromRateLimitEvent(e RateLimitEventData) {
	if e.Utilization != nil {
		// Scale utilization to token-equivalent units: remaining = (1-u)*1e6, total = 1e6.
		// This maps directly into the scorer's QuotaTokensRemaining/Total fields.
		const scale = 1_000_000
		remaining := int64((1.0 - *e.Utilization) * scale)
		if remaining < 0 {
			remaining = 0
		}
		s.SetQuotaFromUsage(remaining, scale, e.ResetsAt)
	} else {
		// Utilization absent means we are below the threshold — quota is OK.
		// Clear QuotaLowAlerted so a future crossing will fire a fresh alert.
		s.mu.Lock()
		if s.QuotaLowAlerted {
			s.QuotaLowAlerted = false
		}
		if !e.ResetsAt.IsZero() {
			s.QuotaResetAt = e.ResetsAt
		}
		s.mu.Unlock()
	}

	// Store snapshot for /v1/capacity (ephemeral; always overwrite).
	if e.LastQuotaSnapshot != nil {
		s.mu.Lock()
		s.LastQuotaSnapshot = e.LastQuotaSnapshot
		s.mu.Unlock()
	}
}
