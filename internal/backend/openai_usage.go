// internal/backend/openai_usage.go — OpenAI billing / usage API poller.
//
// UsagePoller polls the OpenAI dashboard billing endpoints on a configurable
// interval to fetch remaining API credit and total allocation. The poller is
// intentionally decoupled from state management: it calls an OnUpdate callback
// with raw USD values and leaves the state update to the caller.
//
// # Endpoints
//
// OpenAI has two generations of billing/usage APIs:
//
//  1. Legacy dashboard API (used here):
//     GET /v1/dashboard/billing/subscription — hard_limit_usd (requires account admin key)
//     GET /v1/dashboard/billing/usage?start_date=YYYY-MM-01&end_date=YYYY-MM-DD
//     Returns total_usage in cents for the billing cycle. Works with sk-... keys that
//     have account admin or "View usage" permission.
//
//  2. Organization Usage API (v2, not yet used):
//     GET /v1/organization/usage/completions — per-model token counts
//     GET /v1/organization/costs — per-day USD costs
//     These require an admin API key with "Usage" scope.
//
// # Requirements
//
// The configured openai_api_key must have one of:
//   - Account "Owner" role (grants full dashboard access)
//   - Admin API key with the "View usage" or "Billing" permission
//
// Standard project-scoped sk-proj-... keys do NOT have billing scope and will
// receive 401. In that case usage polling silently backs off and quota state is
// not updated (routing continues with the last known or unknown quota state).
//
// To use a separate admin key from the inference key, set openai_api_key to the
// admin key (env:OPENAI_ADMIN_KEY) and api_key to the inference key.
//
// The poller is safe for concurrent use. Start may be called at most once per
// instance; use a new UsagePoller for each backend.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const openaiUsageDefaultURL = "https://api.openai.com"

// UsageSample is the result of one successful poll.
type UsageSample struct {
	BackendID    string
	RemainingUSD float64
	TotalUSD     float64
	// ResetAt is the start of the next billing cycle; zero if unknown.
	ResetAt time.Time
}

// UsagePoller polls the OpenAI billing API for one backend.
type UsagePoller struct {
	// BackendID is the router backend ID this poller is tracking.
	BackendID string

	// APIKey is the OpenAI API key. Supports env: references via the caller
	// resolving them before constructing the poller.
	APIKey string

	// APIURL is the base URL for OpenAI API calls.
	// Empty defaults to https://api.openai.com.
	APIURL string

	// Interval is the polling interval. Default 5 minutes if zero.
	Interval time.Duration

	// OnUpdate is called after each successful poll with the latest usage sample.
	// It is called from the polling goroutine; implementations must be safe for
	// concurrent use and must not block for extended periods.
	OnUpdate func(sample UsageSample)

	client *http.Client
}

// NewUsagePoller creates a UsagePoller with a resolved API key and optional URL override.
func NewUsagePoller(backendID, apiKey, apiURL string, interval time.Duration, onUpdate func(UsageSample)) *UsagePoller {
	if apiURL == "" {
		apiURL = openaiUsageDefaultURL
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &UsagePoller{
		BackendID: backendID,
		APIKey:    apiKey,
		APIURL:    strings.TrimRight(apiURL, "/"),
		Interval:  interval,
		OnUpdate:  onUpdate,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Start launches the polling loop in a background goroutine.
// The loop runs until ctx is cancelled. Start must be called at most once.
func (p *UsagePoller) Start(ctx context.Context) {
	go p.loop(ctx)
}

// Poll performs a single usage poll and returns the result.
// Safe to call directly for testing or one-off queries.
func (p *UsagePoller) Poll(ctx context.Context) (UsageSample, error) {
	if p.APIKey == "" {
		return UsageSample{}, fmt.Errorf("usage_poller %s: no api_key", p.BackendID)
	}

	sub, err := p.fetchSubscription(ctx)
	if err != nil {
		return UsageSample{}, fmt.Errorf("usage_poller %s: subscription: %w", p.BackendID, err)
	}

	now := time.Now().UTC()
	usageCents, err := p.fetchUsage(ctx, now)
	if err != nil {
		return UsageSample{}, fmt.Errorf("usage_poller %s: usage: %w", p.BackendID, err)
	}

	usedUSD := float64(usageCents) / 100.0
	remainingUSD := sub.HardLimitUSD - usedUSD
	if remainingUSD < 0 {
		remainingUSD = 0
	}

	return UsageSample{
		BackendID:    p.BackendID,
		RemainingUSD: remainingUSD,
		TotalUSD:     sub.HardLimitUSD,
		ResetAt:      sub.AccessUntil,
	}, nil
}

func (p *UsagePoller) loop(ctx context.Context) {
	// Poll once immediately on start, then on interval.
	p.runOnce(ctx)

	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.runOnce(ctx)
		}
	}
}

func (p *UsagePoller) runOnce(ctx context.Context) {
	sample, err := p.Poll(ctx)
	if err != nil {
		slog.Warn("usage_poller: poll failed", "backend", p.BackendID, "err", err)
		return
	}
	slog.Debug("usage_poller: poll ok",
		"backend", p.BackendID,
		"remaining_usd", fmt.Sprintf("%.2f", sample.RemainingUSD),
		"total_usd", fmt.Sprintf("%.2f", sample.TotalUSD),
	)
	if p.OnUpdate != nil {
		p.OnUpdate(sample)
	}
}

// --- billing API response types ---

type billingSubscription struct {
	HardLimitUSD float64   `json:"hard_limit_usd"`
	SoftLimitUSD float64   `json:"soft_limit_usd"`
	AccessUntil  time.Time `json:"access_until"`
}

type billingUsage struct {
	// TotalUsage is in cents.
	TotalUsage float64 `json:"total_usage"`
}

// fetchSubscription calls /v1/dashboard/billing/subscription.
func (p *UsagePoller) fetchSubscription(ctx context.Context) (*billingSubscription, error) {
	url := p.APIURL + "/v1/dashboard/billing/subscription"
	body, err := p.get(ctx, url)
	if err != nil {
		return nil, err
	}
	var sub billingSubscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse subscription: %w", err)
	}
	return &sub, nil
}

// fetchUsage calls /v1/dashboard/billing/usage for the current calendar month.
// The OpenAI billing usage endpoint uses an exclusive end_date, so we pass
// tomorrow's date to include today's usage. On the first of the month, start and
// end would otherwise be equal, which the endpoint rejects with 400.
func (p *UsagePoller) fetchUsage(ctx context.Context, now time.Time) (float64, error) {
	startDate := fmt.Sprintf("%d-%02d-01", now.Year(), now.Month())
	// end_date is exclusive — use tomorrow to include today's usage
	tomorrow := now.AddDate(0, 0, 1)
	endDate := fmt.Sprintf("%d-%02d-%02d", tomorrow.Year(), tomorrow.Month(), tomorrow.Day())
	url := p.APIURL + "/v1/dashboard/billing/usage?start_date=" + startDate + "&end_date=" + endDate

	body, err := p.get(ctx, url)
	if err != nil {
		return 0, err
	}
	var usage billingUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return 0, fmt.Errorf("parse usage: %w", err)
	}
	return usage.TotalUsage, nil
}

// get is a shared GET helper: sets Authorization header, reads and returns the body.
func (p *UsagePoller) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}
