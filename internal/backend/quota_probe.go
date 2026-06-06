// internal/backend/quota_probe.go — idle quota probe loop for claude_cli backends.
//
// QuotaProber fires a minimal claude CLI ping on a configurable interval when no
// organic rate_limit_event has been received recently. This keeps quota utilization
// and reset time current while the router is idle, without waiting for a real request.
//
// The probe call goes through the full Adapter.Invoke path so RateLimitEvent parsing
// is identical to organic traffic. The completion response content is discarded; only
// resp.RateLimitEvent matters.
//
// Probe calls are NOT recorded in call_log — the onEvent callback injected by the
// router must NOT call store.LogCall for probe responses.
package backend

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/clagentic/clagentic-router/internal/config"
)

const (
	probeDefaultInterval   = 30 * time.Minute
	probeDefaultModel      = "claude-haiku-4-5"
	probeInvokeTimeout     = 30 * time.Second
	probeRejectedBackoff   = 5 * time.Minute
)

// QuotaProber fires cheap Adapter pings on a configurable interval when no organic
// rate_limit_event has been seen recently. It is only meaningful for claude_cli
// backends but the interface is adapter-agnostic.
//
// Concurrency: NotifyEvent is safe to call from any goroutine. Start and Stop are
// not reentrant — call Start at most once.
type QuotaProber struct {
	backendID string
	cfg       config.QuotaProbeConfig
	adapter   Adapter

	// lastEventAt tracks the most recent organic (or probe) rate_limit_event, used
	// to decide whether the idle window has elapsed.
	lastEventAt atomic.Int64 // unix nanoseconds; 0 = never seen

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewQuotaProber creates a QuotaProber for backendID.
// cfg.Interval and cfg.Model are defaulted inside Start if zero/empty.
func NewQuotaProber(backendID string, cfg config.QuotaProbeConfig, adapter Adapter) *QuotaProber {
	return &QuotaProber{
		backendID: backendID,
		cfg:       cfg,
		adapter:   adapter,
		stopCh:    make(chan struct{}),
	}
}

// interval returns the effective probe interval, applying the 30-minute default.
func (p *QuotaProber) interval() time.Duration {
	if d := time.Duration(p.cfg.Interval); d > 0 {
		return d
	}
	return probeDefaultInterval
}

// model returns the effective probe model, applying the haiku default.
func (p *QuotaProber) model() string {
	if p.cfg.Model != "" {
		return p.cfg.Model
	}
	return probeDefaultModel
}

// NotifyEvent records that an organic rate_limit_event just arrived.
// This resets the idle timer, preventing the next probe from firing until the
// interval elapses again.
func (p *QuotaProber) NotifyEvent() {
	p.lastEventAt.Store(time.Now().UnixNano())
}

// Start launches the background probe goroutine.
// ctx cancellation and Stop() both terminate the loop.
// onEvent is called with the probe response so the router can route the
// RateLimitEvent through its existing applyRateLimitEvent path.
func (p *QuotaProber) Start(ctx context.Context, onEvent func(resp *Response)) {
	interval := p.interval()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Track a per-probe backoff state (backoff kicks in on "rejected" status).
		var backoffUntil time.Time

		for {
			select {
			case <-p.stopCh:
				return
			case <-ctx.Done():
				return

			case now := <-ticker.C:
				// Skip if still in backoff window.
				if now.Before(backoffUntil) {
					slog.Debug("quota_probe: skipping tick, in backoff",
						"backend", p.backendID,
						"backoff_until", backoffUntil)
					continue
				}

				// Skip if organic data is fresh (last event within the interval).
				lastNano := p.lastEventAt.Load()
				if lastNano > 0 {
					lastAt := time.Unix(0, lastNano)
					if now.Sub(lastAt) <= interval {
						slog.Debug("quota_probe: organic data is fresh, skipping",
							"backend", p.backendID,
							"last_event_age", now.Sub(lastAt).Round(time.Second))
						continue
					}
				}

				slog.Debug("quota_probe: firing probe", "backend", p.backendID, "model", p.model())

				// Use a fresh, short-lived context for the probe call — the parent ctx
				// may be long-lived (daemon lifetime).
				probeCtx, cancel := context.WithTimeout(context.Background(), probeInvokeTimeout)
				resp, err := p.adapter.Invoke(probeCtx, &Request{
					Messages: []Message{
						{Role: "user", Content: "x"},
					},
					MaxTokens: 1,
				})
				cancel()

				if err != nil {
					// Probe errors do not degrade the backend — they are maintenance traffic.
					slog.Warn("quota_probe: invoke error (will retry at next interval)",
						"backend", p.backendID, "error", err)
					continue
				}

				// Record that we received a probe response, resetting the idle clock.
				p.lastEventAt.Store(time.Now().UnixNano())

				// Check for rejected status → enter short backoff.
				if resp.RateLimitEvent != nil && resp.RateLimitEvent.Status == "rejected" {
					backoffUntil = time.Now().Add(probeRejectedBackoff)
					slog.Info("quota_probe: rejected status received, backing off",
						"backend", p.backendID,
						"backoff_until", backoffUntil)
					// Still deliver the event so the router can update state.
					onEvent(resp)
					continue
				}

				// Reset backoff on a non-rejected response.
				backoffUntil = time.Time{}

				if resp.RateLimitEvent != nil {
					onEvent(resp)
				}
			}
		}
	}()
}

// Stop shuts down the probe goroutine and waits for it to exit.
func (p *QuotaProber) Stop() {
	close(p.stopCh)
	p.wg.Wait()
}
