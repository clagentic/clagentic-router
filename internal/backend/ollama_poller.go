// internal/backend/ollama_poller.go — capacity poller for Ollama servers.
//
// OllamaPoller polls GET /api/ps to track VRAM usage and loaded model state.
// Ollama has no slot/concurrency API; the primary signals are:
//   - VRAM consumed by loaded models (vs operator-configured total)
//   - Whether the target model is currently loaded (ModelHot)
//   - HTTP 503 on inference requests signals queue overflow → caller marks degraded
package backend

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// OllamaCapacity is a single capacity reading from an Ollama server.
type OllamaCapacity struct {
	BackendID string
	// VRAMUsed is the sum of size_vram for all currently loaded models (bytes).
	VRAMUsed int64
	// VRAMTotal is the operator-configured total VRAM (bytes). 0 = unknown.
	VRAMTotal int64
	// VRAMHeadroom is VRAMTotal - VRAMUsed. -1 when VRAMTotal is unknown.
	VRAMHeadroom int64
	// ModelHot is true if TargetModel appears in the /api/ps response.
	// A cold model requires a load before the first inference, adding latency.
	ModelHot bool
	// TargetModel is the model this backend routes to.
	TargetModel string
	PolledAt    time.Time
}

// OllamaPoller polls GET /api/ps on an Ollama server and delivers capacity
// snapshots via OnUpdate. Start runs the poll loop until ctx is cancelled.
type OllamaPoller struct {
	BackendID string
	BaseURL   string
	// TotalVRAM is the operator-configured total VRAM bytes; 0 means unknown.
	TotalVRAM   int64
	Interval    time.Duration
	OnUpdate    func(snap OllamaCapacity)
	targetModel string

	client *http.Client
}

// NewOllamaPoller constructs an OllamaPoller. interval=0 defaults to 7s.
func NewOllamaPoller(backendID, baseURL, targetModel string, totalVRAM int64, interval time.Duration, onUpdate func(OllamaCapacity)) *OllamaPoller {
	if interval <= 0 {
		interval = 7 * time.Second
	}
	return &OllamaPoller{
		BackendID:   backendID,
		BaseURL:     baseURL,
		TotalVRAM:   totalVRAM,
		Interval:    interval,
		OnUpdate:    onUpdate,
		targetModel: targetModel,
		client:      &http.Client{Timeout: 5 * time.Second},
	}
}

// Start runs the poll loop until ctx is cancelled. Intended to be called in a goroutine.
func (p *OllamaPoller) Start(ctx context.Context) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := p.Poll(ctx)
			if err != nil {
				slog.Debug("ollama_poller: poll error", "backend", p.BackendID, "err", err)
				continue
			}
			if p.OnUpdate != nil {
				p.OnUpdate(snap)
			}
		}
	}
}

// Poll performs a single GET /api/ps call and returns the capacity snapshot.
// Callable directly for testing without a running goroutine.
func (p *OllamaPoller) Poll(ctx context.Context) (OllamaCapacity, error) {
	url := p.BaseURL + "/api/ps"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return OllamaCapacity{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return OllamaCapacity{}, err
	}
	defer resp.Body.Close()

	snap := OllamaCapacity{
		BackendID:    p.BackendID,
		VRAMTotal:    p.TotalVRAM,
		VRAMHeadroom: -1,
		TargetModel:  p.targetModel,
		PolledAt:     time.Now().UTC(),
	}

	var body struct {
		Models []struct {
			Name     string `json:"name"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		// Non-JSON or unexpected format — return what we have; no VRAM data.
		return snap, nil
	}

	var vramUsed int64
	for _, m := range body.Models {
		vramUsed += m.SizeVRAM
		if m.Name == p.targetModel {
			snap.ModelHot = true
		}
	}
	snap.VRAMUsed = vramUsed

	if p.TotalVRAM > 0 {
		snap.VRAMHeadroom = p.TotalVRAM - vramUsed
	}

	return snap, nil
}
