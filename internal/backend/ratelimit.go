// internal/backend/ratelimit.go — parsing for claude CLI rate_limit_event stream lines.
//
// The claude CLI emits a rate_limit_event JSON line in stream-json mode on every
// response that crosses a quota utilization threshold. This file provides the
// struct and parser for those events. Callers (the router) are responsible for
// persisting and acting on the parsed data.
package backend

import (
	"encoding/json"
	"time"
)

// RateLimitEvent holds the parsed rate_limit_info from a claude_cli stream-json response.
// All pointer fields are optional — absent in the JSON when status is "allowed"
// (utilization below 0.75 threshold).
type RateLimitEvent struct {
	Status                string
	RateLimitType         string
	ResetsAt              time.Time
	Utilization           *float64   // nil when absent (status=allowed, below threshold)
	SurpassedThreshold    *float64   // nil when absent
	IsUsingOverage        bool
	OverageStatus         *string
	OverageDisabledReason *string
	OverageResetsAt       *time.Time
	RawJSON               string // full raw rate_limit_info JSON for forward compat
}

// streamLine is the outer wrapper of a claude CLI stream-json line.
type streamLine struct {
	Type          string          `json:"type"`
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`
}

// rateLimitInfoJSON is the wire format for rate_limit_info inside a rate_limit_event.
type rateLimitInfoJSON struct {
	Status                string   `json:"status"`
	RateLimitType         string   `json:"rateLimitType"`
	ResetsAt              int64    `json:"resetsAt"` // Unix seconds
	Utilization           *float64 `json:"utilization"`
	SurpassedThreshold    *float64 `json:"surpassedThreshold"`
	IsUsingOverage        bool     `json:"isUsingOverage"`
	OverageStatus         *string  `json:"overageStatus"`
	OverageDisabledReason *string  `json:"overageDisabledReason"`
	OverageResetsAt       *int64   `json:"overageResetsAt"` // Unix seconds; optional
}

// parseRateLimitEvent attempts to parse a rate_limit_event from a stream-json line.
// Returns nil if the line is not a rate_limit_event or cannot be parsed.
func parseRateLimitEvent(line []byte) *RateLimitEvent {
	var outer streamLine
	if err := json.Unmarshal(line, &outer); err != nil {
		return nil
	}
	if outer.Type != "rate_limit_event" {
		return nil
	}
	if len(outer.RateLimitInfo) == 0 {
		return nil
	}

	var info rateLimitInfoJSON
	if err := json.Unmarshal(outer.RateLimitInfo, &info); err != nil {
		return nil
	}
	if info.Status == "" || info.RateLimitType == "" {
		return nil
	}

	e := &RateLimitEvent{
		Status:                info.Status,
		RateLimitType:         info.RateLimitType,
		ResetsAt:              time.Unix(info.ResetsAt, 0).UTC(),
		Utilization:           info.Utilization,
		SurpassedThreshold:    info.SurpassedThreshold,
		IsUsingOverage:        info.IsUsingOverage,
		OverageStatus:         info.OverageStatus,
		OverageDisabledReason: info.OverageDisabledReason,
		RawJSON:               string(outer.RateLimitInfo),
	}
	if info.OverageResetsAt != nil {
		t := time.Unix(*info.OverageResetsAt, 0).UTC()
		e.OverageResetsAt = &t
	}
	return e
}
