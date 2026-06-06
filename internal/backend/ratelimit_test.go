// internal/backend/ratelimit_test.go — tests for parseRateLimitEvent.
package backend

import (
	"testing"
	"time"
)

// fullEventJSON is a synthetic rate_limit_event with all optional fields present.
const fullEventJSON = `{
	"type": "rate_limit_event",
	"rate_limit_info": {
		"status": "allowed_warning",
		"rateLimitType": "seven_day",
		"resetsAt": 1780963200,
		"utilization": 0.78,
		"surpassedThreshold": 0.75,
		"isUsingOverage": false,
		"overageStatus": "enabled",
		"overageDisabledReason": "",
		"overageResetsAt": 1780963200
	}
}`

// allowedEventJSON is a rate_limit_event with status=allowed (below threshold).
// utilization and surpassedThreshold are absent, as the spec mandates.
const allowedEventJSON = `{
	"type": "rate_limit_event",
	"rate_limit_info": {
		"status": "allowed",
		"rateLimitType": "five_hour",
		"resetsAt": 1780945600,
		"isUsingOverage": false
	}
}`

// overageEventJSON has overage fields set.
const overageEventJSON = `{
	"type": "rate_limit_event",
	"rate_limit_info": {
		"status": "allowed_warning",
		"rateLimitType": "seven_day_sonnet",
		"resetsAt": 1780963200,
		"utilization": 0.82,
		"surpassedThreshold": 0.75,
		"isUsingOverage": true,
		"overageDisabledReason": "out_of_credits",
		"overageResetsAt": 1780966800
	}
}`

func TestParseRateLimitEvent_AllFields(t *testing.T) {
	e := parseRateLimitEvent([]byte(fullEventJSON))
	if e == nil {
		t.Fatal("parseRateLimitEvent returned nil for valid full event")
	}
	if e.Status != "allowed_warning" {
		t.Errorf("Status = %q, want %q", e.Status, "allowed_warning")
	}
	if e.RateLimitType != "seven_day" {
		t.Errorf("RateLimitType = %q, want %q", e.RateLimitType, "seven_day")
	}
	wantResetsAt := time.Unix(1780963200, 0).UTC()
	if !e.ResetsAt.Equal(wantResetsAt) {
		t.Errorf("ResetsAt = %v, want %v", e.ResetsAt, wantResetsAt)
	}
	if e.Utilization == nil {
		t.Fatal("Utilization is nil, want 0.78")
	}
	if *e.Utilization != 0.78 {
		t.Errorf("Utilization = %v, want 0.78", *e.Utilization)
	}
	if e.SurpassedThreshold == nil {
		t.Fatal("SurpassedThreshold is nil, want 0.75")
	}
	if *e.SurpassedThreshold != 0.75 {
		t.Errorf("SurpassedThreshold = %v, want 0.75", *e.SurpassedThreshold)
	}
	if e.IsUsingOverage {
		t.Error("IsUsingOverage = true, want false")
	}
	if e.OverageStatus == nil || *e.OverageStatus != "enabled" {
		t.Errorf("OverageStatus = %v, want %q", e.OverageStatus, "enabled")
	}
	if e.OverageResetsAt == nil {
		t.Fatal("OverageResetsAt is nil, want a time value")
	}
	if !e.OverageResetsAt.Equal(wantResetsAt) {
		t.Errorf("OverageResetsAt = %v, want %v", *e.OverageResetsAt, wantResetsAt)
	}
	if e.RawJSON == "" {
		t.Error("RawJSON is empty, want non-empty")
	}
}

func TestParseRateLimitEvent_AllowedStatus_NilOptionals(t *testing.T) {
	e := parseRateLimitEvent([]byte(allowedEventJSON))
	if e == nil {
		t.Fatal("parseRateLimitEvent returned nil for allowed event")
	}
	if e.Status != "allowed" {
		t.Errorf("Status = %q, want %q", e.Status, "allowed")
	}
	if e.RateLimitType != "five_hour" {
		t.Errorf("RateLimitType = %q, want %q", e.RateLimitType, "five_hour")
	}
	if e.Utilization != nil {
		t.Errorf("Utilization = %v, want nil (absent when status=allowed)", e.Utilization)
	}
	if e.SurpassedThreshold != nil {
		t.Errorf("SurpassedThreshold = %v, want nil (absent when status=allowed)", e.SurpassedThreshold)
	}
	if e.IsUsingOverage {
		t.Error("IsUsingOverage = true, want false")
	}
	if e.OverageResetsAt != nil {
		t.Errorf("OverageResetsAt = %v, want nil", e.OverageResetsAt)
	}
}

func TestParseRateLimitEvent_OverageFields(t *testing.T) {
	e := parseRateLimitEvent([]byte(overageEventJSON))
	if e == nil {
		t.Fatal("parseRateLimitEvent returned nil for overage event")
	}
	if !e.IsUsingOverage {
		t.Error("IsUsingOverage = false, want true")
	}
	if e.OverageDisabledReason == nil || *e.OverageDisabledReason != "out_of_credits" {
		t.Errorf("OverageDisabledReason = %v, want %q", e.OverageDisabledReason, "out_of_credits")
	}
	if e.OverageResetsAt == nil {
		t.Fatal("OverageResetsAt is nil, want a time value")
	}
	wantOverageReset := time.Unix(1780966800, 0).UTC()
	if !e.OverageResetsAt.Equal(wantOverageReset) {
		t.Errorf("OverageResetsAt = %v, want %v", *e.OverageResetsAt, wantOverageReset)
	}
	if e.RateLimitType != "seven_day_sonnet" {
		t.Errorf("RateLimitType = %q, want %q", e.RateLimitType, "seven_day_sonnet")
	}
}

func TestParseRateLimitEvent_NonRateLimitEvent_ReturnsNil(t *testing.T) {
	nonEvent := `{"type":"result","result":"hello","cost_usd":0.001}`
	e := parseRateLimitEvent([]byte(nonEvent))
	if e != nil {
		t.Errorf("expected nil for non-rate_limit_event line, got %+v", e)
	}
}

func TestParseRateLimitEvent_MalformedJSON_ReturnsNil(t *testing.T) {
	malformed := `{"type": "rate_limit_event", "rate_limit_info": {not valid json`
	e := parseRateLimitEvent([]byte(malformed))
	if e != nil {
		t.Errorf("expected nil for malformed JSON, got %+v", e)
	}
}

func TestParseRateLimitEvent_EmptyLine_ReturnsNil(t *testing.T) {
	e := parseRateLimitEvent([]byte(""))
	if e != nil {
		t.Errorf("expected nil for empty line, got %+v", e)
	}
}

func TestParseRateLimitEvent_MissingRateLimitInfo_ReturnsNil(t *testing.T) {
	// rate_limit_event type but no rate_limit_info field
	noInfo := `{"type":"rate_limit_event"}`
	e := parseRateLimitEvent([]byte(noInfo))
	if e != nil {
		t.Errorf("expected nil when rate_limit_info is absent, got %+v", e)
	}
}

func TestParseRateLimitEvent_MissingStatus_ReturnsNil(t *testing.T) {
	// rate_limit_info present but missing required status field
	noStatus := `{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"seven_day","resetsAt":1780963200}}`
	e := parseRateLimitEvent([]byte(noStatus))
	if e != nil {
		t.Errorf("expected nil when status is absent, got %+v", e)
	}
}
