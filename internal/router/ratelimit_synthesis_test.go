// internal/router/ratelimit_synthesis_test.go — unit tests for
// synthesizeRateLimitEventFromHeaders and effectiveRateLimitEvent (lr-c98c
// Slice E: wiring openai_api/anthropic_api rate-limit headers into the same
// quota_snapshots table claude_cli's rate_limit_event populates).
package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

// TestSynthesizeRateLimitEventFromHeaders_TableDriven mirrors the
// table-driven pattern used by claude_cli's ratelimit_test.go, applied to
// the new header-synthesis path.
func TestSynthesizeRateLimitEventFromHeaders_TableDriven(t *testing.T) {
	resetAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name            string
		info            backend.RateLimitInfo
		wantNil         bool
		wantUtilization float64
		wantType        string
	}{
		{
			name:    "both windows absent (zero value, e.g. CLI adapter) -> nil",
			info:    backend.RateLimitInfo{},
			wantNil: true,
		},
		{
			name:    "remaining present but limit absent -> nil (never fabricate a denominator)",
			info:    backend.RateLimitInfo{TokensRemaining: 500},
			wantNil: true,
		},
		{
			name:    "limit present but remaining absent -> nil (never fabricate a numerator)",
			info:    backend.RateLimitInfo{TokensLimit: 1000},
			wantNil: true,
		},
		{
			name: "tokens window fully known -> synthesized event",
			info: backend.RateLimitInfo{
				TokensRemaining: 250,
				TokensLimit:     1000,
				TokensResetAt:   resetAt,
			},
			wantNil:         false,
			wantUtilization: 0.75,
			wantType:        "tokens",
		},
		{
			name: "requests window used when tokens window absent",
			info: backend.RateLimitInfo{
				RequestsRemaining: 10,
				RequestsLimit:     100,
				RequestsResetAt:   resetAt,
			},
			wantNil:         false,
			wantUtilization: 0.90,
			wantType:        "requests",
		},
		{
			name: "tokens window preferred when both windows known",
			info: backend.RateLimitInfo{
				TokensRemaining:   800,
				TokensLimit:       1000,
				RequestsRemaining: 1,
				RequestsLimit:     100,
			},
			wantNil:         false,
			wantUtilization: 0.20,
			wantType:        "tokens",
		},
		{
			name: "remaining greater than limit clamps utilization to 0, not negative",
			info: backend.RateLimitInfo{
				TokensRemaining: 1200,
				TokensLimit:     1000,
			},
			wantNil:         false,
			wantUtilization: 0,
			wantType:        "tokens",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := synthesizeRateLimitEventFromHeaders(tc.info)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil event, got nil")
			}
			if got.RateLimitType != tc.wantType {
				t.Errorf("RateLimitType = %q, want %q", got.RateLimitType, tc.wantType)
			}
			if got.Utilization == nil {
				t.Fatal("Utilization is nil, want a populated pointer")
			}
			if diff := *got.Utilization - tc.wantUtilization; diff < -0.0001 || diff > 0.0001 {
				t.Errorf("Utilization = %v, want %v", *got.Utilization, tc.wantUtilization)
			}
			if got.Status != "allowed" {
				t.Errorf("Status = %q, want %q", got.Status, "allowed")
			}
		})
	}
}

// TestEffectiveRateLimitEvent_RealEventWinsOverHeaders verifies claude_cli's
// real rate_limit_event is never displaced by a synthesized header event —
// see effectiveRateLimitEvent's doc for why the real event always wins.
func TestEffectiveRateLimitEvent_RealEventWinsOverHeaders(t *testing.T) {
	u := 0.42
	real := &backend.RateLimitEvent{Status: "allowed_warning", RateLimitType: "five_hour", Utilization: &u}
	resp := &backend.Response{
		RateLimitEvent: real,
		// Header data present too — must be ignored in favor of the real event.
		RateLimitInfo: backend.RateLimitInfo{TokensRemaining: 1, TokensLimit: 1000},
	}

	got := effectiveRateLimitEvent(resp)
	if got != real {
		t.Fatalf("expected the real RateLimitEvent to win, got %+v", got)
	}
}

// TestEffectiveRateLimitEvent_FallsBackToHeaders verifies the header-derived
// synthetic event is used when no real rate_limit_event is present.
func TestEffectiveRateLimitEvent_FallsBackToHeaders(t *testing.T) {
	resp := &backend.Response{
		RateLimitInfo: backend.RateLimitInfo{TokensRemaining: 100, TokensLimit: 200},
	}
	got := effectiveRateLimitEvent(resp)
	if got == nil {
		t.Fatal("expected a synthesized event, got nil")
	}
	if got.RateLimitType != "tokens" {
		t.Errorf("RateLimitType = %q, want tokens", got.RateLimitType)
	}
}

// TestEffectiveRateLimitEvent_NoSignalAtAll verifies the common no-signal
// case (gemini_cli, codex_cli, ollama_http, bedrock_api's non-header path)
// returns nil cleanly rather than fabricating a zero-utilization row.
func TestEffectiveRateLimitEvent_NoSignalAtAll(t *testing.T) {
	resp := &backend.Response{}
	if got := effectiveRateLimitEvent(resp); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestApplyRateLimitEvent_HeaderSynthesisPersistsToQuotaSnapshots is an
// integration-style test (mirrors tools_present_test.go's store-backed
// pattern) verifying a header-only response (as openai_api/anthropic_api
// would produce) ends up as a row in quota_snapshots with a non-null
// utilization — the actual acceptance criterion from lr-c98c's work item 3.
func TestApplyRateLimitEvent_HeaderSynthesisPersistsToQuotaSnapshots(t *testing.T) {
	const backendID = "openai-b"
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	adapter := &mockAdapter{id: backendID, invoke: func(ctx context.Context, req *backend.Request) (*backend.Response, error) {
		return &backend.Response{
			Content:       "ok",
			RateLimitInfo: backend.RateLimitInfo{TokensRemaining: 300, TokensLimit: 1000},
		}, nil
	}}
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			backendID: {Adapter: config.AdapterOpenAIAPI, Model: "gpt-test"},
		},
	}
	cfg.Routing.DegradedFailureThreshold = 3
	cfg.Routing.OfflineFailureThreshold = 6

	r := &Router{
		cfg:      cfg,
		states:   map[string]*state.BackendState{backendID: state.New(backendID)},
		adapters: map[string]backend.Adapter{backendID: adapter},
		store:    st,
		stopCh:   make(chan struct{}),
	}

	_, _, err = r.Route(context.Background(), &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "hi"}},
	}, []string{backendID})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}

	got, ok := st.LatestQuotaSnapshot(context.Background(), backendID)
	if !ok {
		t.Fatal("expected a quota_snapshots row, found none")
	}
	if got.RateLimitType != "tokens" {
		t.Errorf("rate_limit_type = %q, want tokens", got.RateLimitType)
	}
	if got.Utilization == nil {
		t.Fatal("utilization is nil, want a populated pointer (never a fabricated zero)")
	}
	wantUtil := 1.0 - (300.0 / 1000.0)
	if diff := *got.Utilization - wantUtil; diff < -0.0001 || diff > 0.0001 {
		t.Errorf("utilization = %v, want %v", *got.Utilization, wantUtil)
	}
}
