// internal/backend/openai_usage_test.go — unit tests for UsagePoller.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// buildSubscriptionBody returns a JSON billing/subscription response body.
func buildSubscriptionBody(hardLimitUSD, softLimitUSD float64, accessUntil time.Time) []byte {
	type body struct {
		HardLimitUSD float64   `json:"hard_limit_usd"`
		SoftLimitUSD float64   `json:"soft_limit_usd"`
		AccessUntil  time.Time `json:"access_until"`
	}
	data, _ := json.Marshal(body{hardLimitUSD, softLimitUSD, accessUntil})
	return data
}

// buildUsageBody returns a JSON billing/usage response with total_usage in cents.
func buildUsageBody(totalUsageCents float64) []byte {
	data, _ := json.Marshal(map[string]float64{"total_usage": totalUsageCents})
	return data
}

// mockBillingServer returns a test server that handles subscription and usage endpoints.
func mockBillingServer(t *testing.T, hardLimitUSD, usedCents float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected Authorization: %s", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			w.Header().Set("Content-Type", "application/json")
			w.Write(buildSubscriptionBody(hardLimitUSD, hardLimitUSD*0.8, time.Time{}))
		case "/v1/dashboard/billing/usage":
			w.Header().Set("Content-Type", "application/json")
			w.Write(buildUsageBody(usedCents))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestUsagePoller_PollHappyPath(t *testing.T) {
	// $100 total, $30 used → $70 remaining
	srv := mockBillingServer(t, 100.0, 3000.0) // 3000 cents = $30
	defer srv.Close()

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, time.Minute, nil)
	sample, err := poller.Poll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sample.BackendID != "test-backend" {
		t.Errorf("unexpected BackendID: %q", sample.BackendID)
	}
	if sample.TotalUSD != 100.0 {
		t.Errorf("unexpected TotalUSD: %.2f", sample.TotalUSD)
	}
	// remainingUSD = 100 - 30 = 70
	wantRemaining := 70.0
	if sample.RemainingUSD != wantRemaining {
		t.Errorf("unexpected RemainingUSD: %.2f (want %.2f)", sample.RemainingUSD, wantRemaining)
	}
}

func TestUsagePoller_RemainingClampedToZero(t *testing.T) {
	// Over-budget: $100 total, $120 used → should clamp to $0 remaining, not negative
	srv := mockBillingServer(t, 100.0, 12000.0) // 12000 cents = $120
	defer srv.Close()

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, time.Minute, nil)
	sample, err := poller.Poll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sample.RemainingUSD < 0 {
		t.Errorf("remaining should be clamped to 0, got %.2f", sample.RemainingUSD)
	}
	if sample.RemainingUSD != 0.0 {
		t.Errorf("expected 0 remaining when over-budget, got %.2f", sample.RemainingUSD)
	}
}

func TestUsagePoller_NoAPIKeyReturnsError(t *testing.T) {
	poller := NewUsagePoller("test-backend", "", "https://api.openai.com", time.Minute, nil)
	_, err := poller.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error for empty api_key, got nil")
	}
}

func TestUsagePoller_SubscriptionHTTPErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer srv.Close()

	poller := NewUsagePoller("test-backend", "bad-key", srv.URL, time.Minute, nil)
	_, err := poller.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

func TestUsagePoller_UsageHTTPErrorReturnsError(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/v1/dashboard/billing/subscription" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(buildSubscriptionBody(100.0, 80.0, time.Time{}))
			return
		}
		// usage endpoint fails
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal_error"}`))
	}))
	defer srv.Close()

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, time.Minute, nil)
	_, err := poller.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error on usage 500, got nil")
	}
}

func TestUsagePoller_OnUpdateCallback(t *testing.T) {
	srv := mockBillingServer(t, 50.0, 1000.0) // $50 total, $10 used → $40 remaining
	defer srv.Close()

	var called atomic.Int32
	var lastSample UsageSample

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, time.Minute, func(s UsageSample) {
		called.Add(1)
		lastSample = s
	})

	sample, err := poller.Poll(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// OnUpdate is called by runOnce, not Poll directly; call runOnce to trigger it
	poller.runOnce(context.Background())

	if called.Load() < 1 {
		t.Error("OnUpdate was not called")
	}
	_ = sample
	if lastSample.BackendID != "test-backend" {
		t.Errorf("unexpected BackendID in callback: %q", lastSample.BackendID)
	}
}

func TestUsagePoller_DefaultInterval(t *testing.T) {
	// Zero interval should default to 5 minutes.
	poller := NewUsagePoller("test-backend", "test-key", "", 0, nil)
	if poller.Interval != 5*time.Minute {
		t.Errorf("expected default interval 5m, got %v", poller.Interval)
	}
}

func TestUsagePoller_DefaultAPIURL(t *testing.T) {
	poller := NewUsagePoller("test-backend", "test-key", "", time.Minute, nil)
	if poller.APIURL != "https://api.openai.com" {
		t.Errorf("expected default APIURL, got %q", poller.APIURL)
	}
}

func TestUsagePoller_StartAndCancel(t *testing.T) {
	// Verify Start doesn't hang and respects context cancellation.
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount.Add(1)
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			w.Header().Set("Content-Type", "application/json")
			w.Write(buildSubscriptionBody(100.0, 80.0, time.Time{}))
		case "/v1/dashboard/billing/usage":
			w.Header().Set("Content-Type", "application/json")
			w.Write(buildUsageBody(500.0))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, 50*time.Millisecond, nil)
	poller.Start(ctx)

	// Wait for context to expire
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond) // let the goroutine settle

	if pollCount.Load() == 0 {
		t.Error("expected at least one poll during the test window")
	}
}

func TestUsagePoller_MalformedSubscriptionJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{not valid json`)
	}))
	defer srv.Close()

	poller := NewUsagePoller("test-backend", "test-key", srv.URL, time.Minute, nil)
	_, err := poller.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}
