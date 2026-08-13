// internal/webhook/deliverer.go — HTTP webhook delivery with HMAC-SHA256 and retry.
//
// Deliverer dispatches alert notifications to registered webhook endpoints.
// It is intentionally decoupled from the router: it receives a DeliveryEvent
// (event name + backend ID + state snapshot) and handles all HTTP concerns.
//
// # Delivery contract
//
//   - POST to the registered URL with a JSON body.
//   - X-Clagentic-Signature header: "sha256=<hex>" HMAC-SHA256 of the body
//     using the webhook secret. Empty secret → header omitted.
//   - X-Clagentic-Event header: event name.
//   - X-Clagentic-Delivery header: unique delivery UUID.
//   - Exponential retry on non-2xx response or network error, up to MaxRetry.
//
// # Filtering
//
// Webhooks registered with a non-empty events list only receive events they
// subscribe to. An empty events list receives all events.
// Both store-registered and config-file webhooks are supported.
//
// # Thread safety
//
// Deliverer is safe for concurrent use. Enqueue never blocks; jobs that
// overflow the internal queue are dropped with a slog.Warn (backpressure signal).
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

// DeliveryEvent is the normalized alert event passed to Enqueue.
// It mirrors router.Notification but avoids an import cycle.
type DeliveryEvent struct {
	Event     string
	BackendID string
	Snapshot  state.Snapshot
	At        time.Time
}

// deliveryJob is one queued HTTP delivery attempt.
type deliveryJob struct {
	deliveryID string
	event      string
	url        string
	secret     string
	body       []byte
	attempt    int
	retryAfter time.Time
}

// Config holds delivery tuning parameters. All fields have defaults.
type Config struct {
	// MaxRetry is the maximum number of delivery attempts (including the first).
	// Default 5.
	MaxRetry int

	// InitialBackoffMs is the backoff before the second attempt in milliseconds.
	// Backoff doubles on each subsequent retry. Default 500.
	InitialBackoffMs int

	// TimeoutSeconds is the per-attempt HTTP timeout. Default 10.
	TimeoutSeconds int
}

func (c *Config) maxRetry() int {
	if c.MaxRetry <= 0 {
		return 5
	}
	return c.MaxRetry
}

func (c *Config) initialBackoff() time.Duration {
	if c.InitialBackoffMs <= 0 {
		return 500 * time.Millisecond
	}
	return time.Duration(c.InitialBackoffMs) * time.Millisecond
}

func (c *Config) timeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// Deliverer dispatches webhook notifications.
type Deliverer struct {
	cfg    Config
	store  *store.Store     // may be nil if only config-file webhooks are used
	static []StaticEndpoint // from config file
	queue  chan *deliveryJob
	client *http.Client

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// StaticEndpoint is a config-file-defined webhook target.
type StaticEndpoint struct {
	URL    string
	Events []string // empty = all events
	Secret string
}

// New creates a Deliverer. store may be nil (only static endpoints used).
// staticEndpoints are built from config.AlertsConfig.Webhooks by the caller.
func New(cfg Config, st *store.Store, static []StaticEndpoint) *Deliverer {
	return &Deliverer{
		cfg:    cfg,
		store:  st,
		static: static,
		queue:  make(chan *deliveryJob, 256),
		client: &http.Client{Timeout: cfg.timeout()},
		stopCh: make(chan struct{}),
	}
}

// NewStaticEndpoint constructs one static endpoint from URL, events, secret.
func NewStaticEndpoint(url string, events []string, secret string) StaticEndpoint {
	return StaticEndpoint{URL: url, Events: events, Secret: secret}
}

// Start launches the background delivery worker. Call once before Enqueue.
func (d *Deliverer) Start() {
	d.wg.Add(1)
	go d.worker()
}

// Stop drains in-flight deliveries and shuts down the worker.
func (d *Deliverer) Stop() {
	close(d.stopCh)
	d.wg.Wait()
}

// Enqueue queues delivery of event to all matching registered endpoints.
// Non-blocking: drops with a warning if the internal queue is full.
func (d *Deliverer) Enqueue(evt DeliveryEvent) {
	if evt.At.IsZero() {
		evt.At = time.Now().UTC()
	}
	body, err := marshalEvent(evt)
	if err != nil {
		slog.Warn("webhook: marshal event failed", "event", evt.Event, "err", err)
		return
	}
	endpoints := d.resolveEndpoints(evt.Event)
	for _, ep := range endpoints {
		job := &deliveryJob{
			deliveryID: uuid.NewString(),
			event:      evt.Event,
			url:        ep.URL,
			secret:     ep.Secret,
			body:       body,
			attempt:    1,
		}
		select {
		case d.queue <- job:
		default:
			slog.Warn("webhook: queue full, dropping delivery",
				"event", evt.Event, "url", ep.URL)
		}
	}
}

// resolveEndpoints returns all endpoints (static + store-registered) that
// subscribe to the given event.
func (d *Deliverer) resolveEndpoints(event string) []StaticEndpoint {
	var result []StaticEndpoint

	// Config-file endpoints
	for _, ep := range d.static {
		if matchesEvent(ep.Events, event) {
			result = append(result, ep)
		}
	}

	// Store-registered endpoints
	if d.store != nil {
		rows, err := d.store.ListWebhooks()
		if err != nil {
			slog.Warn("webhook: list webhooks failed", "err", err)
		} else {
			for _, row := range rows {
				var events []string
				if row.Events != "" && row.Events != "[]" {
					_ = json.Unmarshal([]byte(row.Events), &events)
				}
				if matchesEvent(events, event) {
					result = append(result, StaticEndpoint{
						URL:    row.URL,
						Events: events,
						Secret: row.Secret,
					})
				}
			}
		}
	}
	return result
}

func (d *Deliverer) worker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.stopCh:
			// Drain remaining queue without retries
			for {
				select {
				case job := <-d.queue:
					d.attempt(job)
				default:
					return
				}
			}
		case job := <-d.queue:
			// Wait until retry-after time if set
			if !job.retryAfter.IsZero() {
				wait := time.Until(job.retryAfter)
				if wait > 0 {
					select {
					case <-time.After(wait):
					case <-d.stopCh:
						return
					}
				}
			}
			d.attempt(job)
		}
	}
}

func (d *Deliverer) attempt(job *deliveryJob) {
	err := d.deliver(job)
	if err == nil {
		slog.Debug("webhook: delivered",
			"delivery_id", job.deliveryID, "event", job.event,
			"url", job.url, "attempt", job.attempt)
		return
	}

	slog.Warn("webhook: delivery failed",
		"delivery_id", job.deliveryID, "event", job.event,
		"url", job.url, "attempt", job.attempt, "err", err)

	if job.attempt >= d.cfg.maxRetry() {
		slog.Warn("webhook: max retries reached, dropping",
			"delivery_id", job.deliveryID, "url", job.url)
		return
	}

	// Exponential backoff: 500ms, 1s, 2s, 4s, ...
	backoff := d.cfg.initialBackoff() * (1 << uint(job.attempt-1))
	job.attempt++
	job.retryAfter = time.Now().Add(backoff)

	select {
	case d.queue <- job:
	default:
		slog.Warn("webhook: queue full on retry, dropping",
			"delivery_id", job.deliveryID)
	}
}

func (d *Deliverer) deliver(job *deliveryJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.timeout())
	defer cancel()

	// TODO(lr-5619): re-validate webhook URL at delivery time to defend against
	// DNS rebinding attacks. Registration-time validation (validateWebhookURL) is
	// insufficient if the DNS record changes after registration.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.url, bytes.NewReader(job.body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Clagentic-Event", job.event)
	req.Header.Set("X-Clagentic-Delivery", job.deliveryID)

	if job.secret != "" {
		req.Header.Set("X-Clagentic-Signature", computeSignature(job.body, job.secret))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("non-2xx status: %d", resp.StatusCode)
	}
	return nil
}

// computeSignature returns "sha256=<hex-encoded HMAC-SHA256(body, secret)>".
func computeSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// matchesEvent returns true if the event matches the subscription list.
// An empty subscription list matches all events.
func matchesEvent(subscribed []string, event string) bool {
	if len(subscribed) == 0 {
		return true
	}
	for _, s := range subscribed {
		if s == event || s == "*" {
			return true
		}
	}
	return false
}

// marshalEvent serializes a DeliveryEvent to JSON for the HTTP body.
func marshalEvent(evt DeliveryEvent) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"event":      evt.Event,
		"backend_id": evt.BackendID,
		"at":         evt.At.UTC().Format(time.RFC3339),
		"snapshot": map[string]interface{}{
			"status":                 string(evt.Snapshot.Status),
			"consecutive_failures":   evt.Snapshot.ConsecutiveFailures,
			"quota_exhausted":        evt.Snapshot.QuotaExhausted,
			"quota_tokens_remaining": evt.Snapshot.QuotaTokensRemaining,
			"quota_tokens_total":     evt.Snapshot.QuotaTokensTotal,
			"last_error_type":        string(evt.Snapshot.LastErrorType),
			"total_calls":            evt.Snapshot.TotalCalls,
			"session_cost_usd":       evt.Snapshot.SessionCostUSDEst,
		},
	})
}
