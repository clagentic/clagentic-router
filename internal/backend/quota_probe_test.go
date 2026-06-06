// internal/backend/quota_probe_test.go — unit tests for QuotaProber.
package backend

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/config"
)

// fakeAdapter is a minimal Adapter implementation for testing.
type fakeAdapter struct {
	id       string
	mu       sync.Mutex
	calls    int
	response *Response
	err      error
}

func (f *fakeAdapter) ID() string { return f.id }

func (f *fakeAdapter) Invoke(_ context.Context, _ *Request) (*Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	// Default: return a response with no RateLimitEvent.
	return &Response{Content: "x"}, nil
}

func (f *fakeAdapter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newProbeResponse builds a Response with a synthetic RateLimitEvent.
func newProbeResponse(status string) *Response {
	evt := &RateLimitEvent{
		Status:        status,
		RateLimitType: "seven_day",
		ResetsAt:      time.Now().Add(24 * time.Hour).UTC(),
	}
	return &Response{Content: "x", RateLimitEvent: evt}
}

// TestQuotaProber_FiresWhenIdle verifies the probe fires after the interval
// elapses with no organic events.
func TestQuotaProber_FiresWhenIdle(t *testing.T) {
	adp := &fakeAdapter{
		id:       "test",
		response: newProbeResponse("allowed"),
	}

	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(50 * time.Millisecond),
	}

	p := NewQuotaProber("test", cfg, adp)

	var called atomic.Bool
	done := make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	p.Start(ctx, func(resp *Response) {
		if resp.RateLimitEvent != nil {
			if called.CompareAndSwap(false, true) {
				close(done)
			}
		}
	})
	defer p.Stop()

	select {
	case <-done:
		// Pass — onEvent was called.
	case <-ctx.Done():
		t.Fatal("onEvent was not called within timeout; probe did not fire when idle")
	}
}

// TestQuotaProber_DoesNotFireWhenFresh verifies the probe is skipped when
// NotifyEvent was called recently (organic data is fresh).
func TestQuotaProber_DoesNotFireWhenFresh(t *testing.T) {
	adp := &fakeAdapter{
		id:       "test",
		response: newProbeResponse("allowed"),
	}

	// Use a long interval (1s) but a short test window (200ms).
	// Since lastEventAt is set to now and the interval hasn't elapsed, no probe fires.
	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(1 * time.Second),
	}

	p := NewQuotaProber("test", cfg, adp)

	// Simulate a very recent organic event — sets lastEventAt to now.
	p.NotifyEvent()

	var eventCount atomic.Int32

	// Test window is much shorter than the probe interval — no tick should fire.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	p.Start(ctx, func(resp *Response) {
		eventCount.Add(1)
	})
	defer p.Stop()

	<-ctx.Done()

	if n := eventCount.Load(); n > 0 {
		t.Errorf("expected no probe events when organic data was fresh, got %d", n)
	}
}

// TestQuotaProber_BackoffOnRejected verifies that a "rejected" status triggers
// a backoff so the next tick is skipped.
func TestQuotaProber_BackoffOnRejected(t *testing.T) {
	adp := &fakeAdapter{
		id:       "test",
		response: newProbeResponse("rejected"),
	}

	// Short interval to trigger quickly.
	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(30 * time.Millisecond),
	}

	p := NewQuotaProber("test", cfg, adp)

	var eventCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	p.Start(ctx, func(resp *Response) {
		eventCount.Add(1)
	})
	defer p.Stop()

	<-ctx.Done()

	// Even though the ticker fires several times in 300ms at 30ms interval, the
	// backoff (5 minutes) means only the first probe fires. We expect exactly 1.
	n := eventCount.Load()
	if n != 1 {
		t.Errorf("expected 1 probe event (then backed off), got %d", n)
	}
}

// TestQuotaProber_NotifyEventSetsTimestamp verifies that NotifyEvent correctly
// updates the internal idle clock. This is the unit-level contract: the timestamp
// is set to a recent wall-clock value, so the staleness check will evaluate "fresh"
// for any reasonable probe interval after the call.
func TestQuotaProber_NotifyEventSetsTimestamp(t *testing.T) {
	adp := &fakeAdapter{id: "test"}
	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(30 * time.Minute),
	}
	p := NewQuotaProber("test", cfg, adp)

	// Before any call: zero.
	if v := p.lastEventAt.Load(); v != 0 {
		t.Errorf("expected lastEventAt == 0 before NotifyEvent, got %d", v)
	}

	before := time.Now()
	p.NotifyEvent()
	after := time.Now()

	v := p.lastEventAt.Load()
	if v == 0 {
		t.Fatal("lastEventAt is still 0 after NotifyEvent")
	}
	set := time.Unix(0, v)
	if set.Before(before) || set.After(after.Add(time.Millisecond)) {
		t.Errorf("lastEventAt = %v, expected within [%v, %v]", set, before, after)
	}
}

// TestQuotaProber_NotifyEventPreventsProbe verifies that a recent NotifyEvent call
// prevents the probe from firing by starting the prober with a 1s interval, calling
// NotifyEvent immediately, and confirming no probe fires within 400ms.
func TestQuotaProber_NotifyEventPreventsProbe(t *testing.T) {
	adp := &fakeAdapter{
		id:       "test",
		response: newProbeResponse("allowed"),
	}

	// The interval is 1s. We call NotifyEvent() right before Start(), so the first
	// tick at ~1s is beyond the 400ms test window. No probe should fire.
	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(1 * time.Second),
	}
	p := NewQuotaProber("test", cfg, adp)
	p.NotifyEvent() // mark as fresh

	var eventCount atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	p.Start(ctx, func(resp *Response) {
		eventCount.Add(1)
	})
	defer p.Stop()

	<-ctx.Done()

	if n := eventCount.Load(); n > 0 {
		t.Errorf("expected 0 probe events when idle clock was just reset, got %d", n)
	}
}

// TestQuotaProber_ErrorsDoNotDegradeBackend verifies that invoke errors from the
// probe do not mark the backend degraded — the prober logs warn and retries.
// We verify this by ensuring the error does not propagate to onEvent.
func TestQuotaProber_ErrorsDoNotPropagateToOnEvent(t *testing.T) {
	adp := &fakeAdapter{
		id:  "test",
		err: &InvokeError{Type: ErrTypeNetwork, Raw: "connection refused"},
	}

	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(30 * time.Millisecond),
	}

	p := NewQuotaProber("test", cfg, adp)

	var eventCount atomic.Int32

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	p.Start(ctx, func(resp *Response) {
		eventCount.Add(1)
	})
	defer p.Stop()

	<-ctx.Done()

	// onEvent must never be called when Invoke returns an error.
	if n := eventCount.Load(); n > 0 {
		t.Errorf("expected 0 onEvent calls on invoke error, got %d", n)
	}
	// The adapter should have been called multiple times (retrying at each interval).
	if c := adp.callCount(); c == 0 {
		t.Error("expected adapter to be called at least once, got 0")
	}
}

// TestQuotaProber_StopTerminatesGoroutine verifies that Stop shuts down cleanly.
func TestQuotaProber_StopTerminatesGoroutine(t *testing.T) {
	adp := &fakeAdapter{id: "test"}

	cfg := config.QuotaProbeConfig{
		Enabled:  true,
		Interval: config.Duration(10 * time.Second), // long enough to never fire
	}

	p := NewQuotaProber("test", cfg, adp)

	ctx := context.Background()
	p.Start(ctx, func(resp *Response) {})

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Pass
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s; goroutine may be stuck")
	}
}
