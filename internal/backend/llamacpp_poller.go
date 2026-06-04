// internal/backend/llamacpp_poller.go — capacity poller for llama.cpp servers.
//
// LlamaCppPoller polls GET /health on a llama.cpp server to track slot capacity.
// Slot occupancy is the primary capacity signal; /health returns slot counts in its
// JSON body and also reflects model-loading state via HTTP 503.
//
// TODO(lr-1f7e): extend to parse GET /metrics Prometheus text for KV cache usage
// ratio (llamacpp_kv_cache_usage_ratio) and deferred request count
// (llamacpp_requests_deferred) as a stronger overload signal.
package backend

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// LlamaCppCapacity is a single capacity reading from a llama.cpp server.
type LlamaCppCapacity struct {
	BackendID       string
	SlotsIdle       int
	SlotsProcessing int
	// TotalSlots is SlotsIdle + SlotsProcessing.
	TotalSlots int
	// Healthy is false when /health returns HTTP 503 (model is still loading).
	// A 200 response with slots_idle=0 means loaded but saturated: Healthy=true, SlotsIdle=0.
	Healthy  bool
	PolledAt time.Time
}

// LlamaCppPoller polls GET /health on a llama.cpp server and delivers capacity
// snapshots via OnUpdate. Start runs the poll loop until ctx is cancelled.
type LlamaCppPoller struct {
	BackendID string
	BaseURL   string
	Interval  time.Duration
	OnUpdate  func(snap LlamaCppCapacity)

	client *http.Client
}

// NewLlamaCppPoller constructs a LlamaCppPoller. interval=0 defaults to 4s.
func NewLlamaCppPoller(backendID, baseURL string, interval time.Duration, onUpdate func(LlamaCppCapacity)) *LlamaCppPoller {
	if interval <= 0 {
		interval = 4 * time.Second
	}
	return &LlamaCppPoller{
		BackendID: backendID,
		BaseURL:   baseURL,
		Interval:  interval,
		OnUpdate:  onUpdate,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Start runs the poll loop until ctx is cancelled. Intended to be called in a goroutine.
func (p *LlamaCppPoller) Start(ctx context.Context) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := p.Poll(ctx)
			if err != nil {
				slog.Debug("llamacpp_poller: poll error", "backend", p.BackendID, "err", err)
				continue
			}
			if p.OnUpdate != nil {
				p.OnUpdate(snap)
			}
		}
	}
}

// Poll performs a single GET /health call and returns the capacity snapshot.
// Callable directly for testing without a running goroutine.
func (p *LlamaCppPoller) Poll(ctx context.Context) (LlamaCppCapacity, error) {
	url := p.BaseURL + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return LlamaCppCapacity{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return LlamaCppCapacity{}, err
	}
	defer resp.Body.Close()

	snap := LlamaCppCapacity{
		BackendID: p.BackendID,
		PolledAt:  time.Now().UTC(),
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		// 503 means the model is still loading; not ready to serve requests.
		snap.Healthy = false
		return snap, nil
	}

	// Parse JSON body regardless of other non-200 codes; llama.cpp may return
	// additional status codes in future versions. Best-effort decode.
	var body struct {
		Status          string `json:"status"`
		SlotsIdle       int    `json:"slots_idle"`
		SlotsProcessing int    `json:"slots_processing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		// Non-JSON body or unexpected format — mark healthy (server responded)
		// but we have no slot data.
		snap.Healthy = true
		return snap, nil
	}

	snap.Healthy = true
	snap.SlotsIdle = body.SlotsIdle
	snap.SlotsProcessing = body.SlotsProcessing
	snap.TotalSlots = body.SlotsIdle + body.SlotsProcessing
	return snap, nil
}
