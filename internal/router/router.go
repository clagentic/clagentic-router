// internal/router/router.go — core routing engine.
//
// Router is the central coordinator. It owns all backend state, all adapters,
// and the fallback chain walking logic. It is the only component that mutates
// BackendState.
//
// All public methods are safe for concurrent use.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

// ErrAllFailed is returned when every backend in the chain fails.
var ErrAllFailed = errors.New("all backends in chain failed or unavailable")

// ErrNoChain is returned when the chain resolves to no backends.
var ErrNoChain = errors.New("chain resolved to no configured backends")

// localPoller is the interface implemented by LlamaCppPoller and OllamaPoller.
// Only Start is required; the router manages the goroutine lifecycle.
type localPoller interface {
	Start(ctx context.Context)
}

// RouteMeta carries routing metadata for the caller (surfaced as HTTP headers).
type RouteMeta struct {
	BackendID      string
	ChainPosition  int    // 0 = primary used, 1+ = fallback step N
	FallbackReason string // non-empty when chain was advanced
	LatencyMS      int64
}

// Notification is a status-change event passed to the alert hook.
type Notification struct {
	Event     string
	BackendID string
	Snapshot  state.Snapshot
	Fallback  string // backend that took over, if any
	ChainName string // empty for ad-hoc calls
}

// AlertHook is called on significant state transitions.
type AlertHook func(n Notification)

// Router routes LLM calls across configured backends with fallback and scoring.
type Router struct {
	cfg       *config.Config
	states    map[string]*state.BackendState
	adapters  map[string]backend.Adapter
	store     *store.Store
	alertHook AlertHook

	// usagePollers are started by RegisterUsagePollers and run until Stop.
	usagePollers []*backend.UsagePoller

	// localPollers are started in backgroundLoop and run until Stop.
	// Registered via RegisterLlamaCppPoller / RegisterOllamaPoller.
	localPollers []localPoller

	mu sync.RWMutex

	// Background goroutine management
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// New creates a Router from the given config and adapter map.
// store may be nil (no persistence). alertHook may be nil.
func New(cfg *config.Config, adapters map[string]backend.Adapter, st *store.Store, hook AlertHook) *Router {
	states := make(map[string]*state.BackendState, len(adapters))
	for id := range adapters {
		// Try to restore from store; fall back to fresh state.
		if st != nil {
			if snap, err := st.LoadState(id); err == nil {
				bs := state.New(id)
				bs.RestoreFromSnapshot(snap)
				states[id] = bs
				continue
			}
		}
		states[id] = state.New(id)
	}
	return &Router{
		cfg:       cfg,
		states:    states,
		adapters:  adapters,
		store:     st,
		alertHook: hook,
		stopCh:    make(chan struct{}),
	}
}

// RegisterUsagePollers attaches usage pollers to the router.
// Pollers are started when Start is called and stopped when Stop is called.
// The poller's OnUpdate callback is set to update the corresponding backend state.
// Call before Start; safe to call with an empty slice.
func (r *Router) RegisterUsagePollers(pollers []*backend.UsagePoller) {
	for _, p := range pollers {
		// Capture loop variable
		pp := p
		bid := pp.BackendID
		pp.OnUpdate = func(sample backend.UsageSample) {
			bs := r.getState(bid)
			if bs == nil {
				return
			}
			// Scale USD to token-equivalent units (1 USD = 1,000,000 units).
			// Only the ratio matters for scoring, so the unit choice is arbitrary.
			const usdScale = 1_000_000
			remaining := int64(sample.RemainingUSD * usdScale)
			total := int64(sample.TotalUSD * usdScale)
			bs.SetQuotaFromUsage(remaining, total, sample.ResetAt)
			if r.store != nil {
				r.store.SaveState(bs.Snapshot())
			}
			slog.Info("usage_poller: quota updated",
				"backend", bid,
				"remaining_usd", fmt.Sprintf("%.2f", sample.RemainingUSD),
				"total_usd", fmt.Sprintf("%.2f", sample.TotalUSD),
			)
			// Fire quota_low alert if quota just crossed below the warning threshold
			// (edge-triggered — same contract as the other alert events).
			if r.alertHook != nil {
				if fired, _ := bs.TestAndSetQuotaLow(r.cfg.Routing.QuotaWarningThreshold); fired {
					r.alertHook(Notification{
						Event:     "quota_low",
						BackendID: bid,
						Snapshot:  bs.Snapshot(),
					})
				}
			}
		}
		r.usagePollers = append(r.usagePollers, pp)
	}
}

// RegisterLlamaCppPoller attaches a llama.cpp capacity poller and wires its
// OnUpdate callback to call SetLocalCapacity on the corresponding backend state.
// Call before Start.
func (r *Router) RegisterLlamaCppPoller(p *backend.LlamaCppPoller) {
	bid := p.BackendID
	p.OnUpdate = func(snap backend.LlamaCppCapacity) {
		bs := r.getState(bid)
		if bs == nil {
			return
		}
		slotsIdle := snap.SlotsIdle
		slotsTotal := snap.TotalSlots
		if !snap.Healthy {
			// Server is loading; treat as 0 idle, 0 total so scorer hard-blocks.
			slotsIdle = 0
			slotsTotal = 0
		}
		// llama.cpp has no VRAM API in this slice; pass -1 (unknown).
		// ModelHot is always true for llama.cpp (it serves one model).
		bs.SetLocalCapacity(slotsIdle, slotsTotal, -1.0, snap.Healthy)
		slog.Debug("llamacpp_poller: capacity updated",
			"backend", bid,
			"slots_idle", slotsIdle,
			"slots_total", slotsTotal,
			"healthy", snap.Healthy,
		)
	}
	r.localPollers = append(r.localPollers, p)
}

// RegisterOllamaPoller attaches an Ollama capacity poller and wires its
// OnUpdate callback to call SetLocalCapacity on the corresponding backend state.
// Call before Start.
func (r *Router) RegisterOllamaPoller(p *backend.OllamaPoller) {
	bid := p.BackendID
	p.OnUpdate = func(snap backend.OllamaCapacity) {
		bs := r.getState(bid)
		if bs == nil {
			return
		}
		// Ollama has no slot API; pass 0 for both slot fields.
		// VRAMHeadroomPct is computed from the snap; -1 when total is unknown.
		var vramPct float64 = -1.0
		if snap.VRAMTotal > 0 {
			vramPct = float64(snap.VRAMHeadroom) / float64(snap.VRAMTotal)
		}
		bs.SetLocalCapacity(0, 0, vramPct, snap.ModelHot)
		slog.Debug("ollama_poller: capacity updated",
			"backend", bid,
			"vram_used", snap.VRAMUsed,
			"vram_total", snap.VRAMTotal,
			"model_hot", snap.ModelHot,
		)
	}
	r.localPollers = append(r.localPollers, p)
}

// Start launches background maintenance goroutines (health probing, state flush).
func (r *Router) Start() {
	r.wg.Add(1)
	go r.backgroundLoop()
}

// Stop shuts down background goroutines gracefully.
func (r *Router) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// Route sends req to the best available backend from chain and returns the response.
// chain is a list of tier aliases or backend IDs in preference order.
// On success, meta describes which backend was used and at what chain position.
func (r *Router) Route(ctx context.Context, req *backend.Request, chain []string) (*backend.Response, *RouteMeta, error) {
	if len(chain) == 0 {
		return nil, nil, ErrNoChain
	}

	reqID := backend.RequestIDFromCtx(ctx)
	var lastErr error
	tried := make([]string, 0, len(chain))

	for i, entry := range chain {
		candidates := r.resolveChainEntry(entry)
		if len(candidates) == 0 {
			slog.Debug("router: chain entry resolved to no backends", "entry", entry, "request_id", reqID)
			continue
		}

		bid := r.selectBest(candidates, req)
		if bid == "" {
			slog.Debug("router: all backends in tier unavailable", "entry", entry, "request_id", reqID)
			continue
		}

		tried = append(tried, bid)
		start := time.Now()
		resp, err := r.adapters[bid].Invoke(ctx, req)
		latencyMS := time.Since(start).Milliseconds()

		if err == nil {
			// Success
			r.recordSuccess(bid, resp, latencyMS)

			meta := &RouteMeta{
				BackendID:     bid,
				ChainPosition: i,
				LatencyMS:     latencyMS,
			}
			if i > 0 {
				meta.FallbackReason = string(r.getState(bid).Snapshot().LastErrorType)
				if meta.FallbackReason == "" {
					meta.FallbackReason = "chain_advance"
				}
			}

			if r.store != nil {
				r.store.LogCall(bid, entry, i, "pass", "", resp.PromptTokensEst, resp.CompletionTokensEst, int(latencyMS), resp.CostUSD)
			}

			return resp, meta, nil
		}

		// Failure — classify and record
		var ie *backend.InvokeError
		errType := state.ErrTypeUnknown
		errRaw := err.Error()
		if errors.As(err, &ie) {
			errType = state.ErrorType(ie.Type)
			errRaw = ie.Raw
		}

		change := r.recordFailure(bid, errType, errRaw, backend.ParseResetTime(errRaw), latencyMS)

		if r.store != nil {
			r.store.LogCall(bid, entry, i, "fallback", string(errType), 0, 0, int(latencyMS), 0)
		}

		// Fire alert on state change
		if change.Changed() && r.alertHook != nil {
			snap := r.getState(bid).Snapshot()
			r.alertHook(Notification{
				Event:     stateChangeEvent(change.To, change.ErrorType),
				BackendID: bid,
				Snapshot:  snap,
			})
		}

		lastErr = err
		slog.Info("router: backend failed, advancing chain",
			"backend", bid, "chain_pos", i, "error_type", errType, "latency_ms", latencyMS, "request_id", reqID)

		// Don't try the same backend again in a later chain position
	}

	if r.store != nil {
		for _, bid := range tried {
			r.store.LogCall(bid, strings.Join(chain, ","), len(chain)-1, "degraded", "all_failed", 0, 0, 0, 0)
			break // one degraded row for the whole call
		}
	}

	if lastErr != nil {
		return nil, nil, fmt.Errorf("%w: last error: %v", ErrAllFailed, lastErr)
	}
	return nil, nil, ErrAllFailed
}

// ResolveModel parses the model field from a chat completion request into a chain.
// Syntax:
//   - "chain:alias1,alias2,alias3"  → explicit chain
//   - "role:chain-name"             → named chain from config
//   - "backend:backend-id"          → single backend (skip scoring)
//   - "alias"                       → single tier alias
func (r *Router) ResolveModel(model string) []string {
	switch {
	case strings.HasPrefix(model, "chain:"):
		parts := strings.Split(strings.TrimPrefix(model, "chain:"), ",")
		cleaned := make([]string, 0, len(parts))
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				cleaned = append(cleaned, t)
			}
		}
		return cleaned

	case strings.HasPrefix(model, "role:"):
		name := strings.TrimPrefix(model, "role:")
		if chain, ok := r.cfg.Chains[name]; ok {
			return chain
		}
		slog.Warn("router: unknown role in model field", "role", name)
		return nil

	case strings.HasPrefix(model, "backend:"):
		bid := strings.TrimPrefix(model, "backend:")
		if _, ok := r.adapters[bid]; ok {
			return []string{bid}
		}
		slog.Warn("router: unknown backend in model field", "backend", bid)
		return nil

	default:
		// Try as tier alias, then as direct backend ID
		if candidates := r.resolveChainEntry(model); len(candidates) > 0 {
			return []string{model} // return the alias, resolveChainEntry handles it
		}
		return nil
	}
}

// BackendIDs returns the IDs of all configured backends.
func (r *Router) BackendIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.adapters))
	for id := range r.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// StateSnapshot returns a snapshot of a backend's current state.
func (r *Router) StateSnapshot(backendID string) (state.Snapshot, bool) {
	r.mu.RLock()
	bs, ok := r.states[backendID]
	r.mu.RUnlock()
	if !ok {
		return state.Snapshot{}, false
	}
	return bs.Snapshot(), true
}

// AllSnapshots returns snapshots of all backends.
func (r *Router) AllSnapshots() map[string]state.Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]state.Snapshot, len(r.states))
	for id, bs := range r.states {
		result[id] = bs.Snapshot()
	}
	return result
}

// ForceReset resets a backend's state to Unknown for re-probing.
func (r *Router) ForceReset(backendID string) error {
	r.mu.RLock()
	bs, ok := r.states[backendID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown backend: %s", backendID)
	}
	bs.ForceReset()
	if r.store != nil {
		r.store.SaveState(bs.Snapshot())
	}
	return nil
}

// ForceDisable marks a backend offline manually.
func (r *Router) ForceDisable(backendID string) error {
	r.mu.RLock()
	bs, ok := r.states[backendID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown backend: %s", backendID)
	}
	bs.ForceOffline()
	if r.store != nil {
		r.store.SaveState(bs.Snapshot())
	}
	return nil
}

// ForceEnable is an alias for ForceReset — clears offline state and re-probes.
func (r *Router) ForceEnable(backendID string) error {
	return r.ForceReset(backendID)
}

// ApplyRateLimitEvent updates backend state from an external rate-limit event
// (e.g. a rate_limit_event from the clagentic-console Anthropic SDK).
//
// status must be "warning" or "exhausted":
//   - "warning"   — calls SetRateLimitWarning; soft penalty only, backend stays online.
//   - "exhausted" — calls RecordFailure(ErrTypeQuota, ...); marks backend offline with
//     the provided resetAt as the quota reset time.
//
// limitType is informational and stored in the log message; it does not affect routing.
// resetsAt may be zero when the value is unknown.
func (r *Router) ApplyRateLimitEvent(backendID, limitType, status string, resetsAt time.Time) error {
	r.mu.RLock()
	bs, ok := r.states[backendID]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown backend: %s", backendID)
	}
	switch status {
	case "warning":
		bs.SetRateLimitWarning(limitType, resetsAt)
		slog.Info("rate_limit_event: warning recorded",
			"backend", backendID, "limit_type", limitType, "resets_at", resetsAt)
	case "exhausted":
		change := bs.RecordFailure(
			state.ErrTypeQuota,
			"rate_limit_event: exhausted (limit_type="+limitType+")",
			resetsAt,
			r.cfg.Routing.DegradedFailureThreshold,
			r.cfg.Routing.OfflineFailureThreshold,
		)
		slog.Info("rate_limit_event: exhausted, backend marked offline",
			"backend", backendID, "limit_type", limitType, "resets_at", resetsAt)
		if change.Changed() && r.alertHook != nil {
			r.alertHook(Notification{
				Event:     "quota_exhausted",
				BackendID: backendID,
				Snapshot:  bs.Snapshot(),
			})
		}
	default:
		return fmt.Errorf("unknown status %q: must be warning or exhausted", status)
	}
	if r.store != nil {
		r.store.SaveState(bs.Snapshot())
	}
	return nil
}

// ProbeBackend runs a live test invocation on one backend.
// Used by /doctor to check actual connectivity.
func (r *Router) ProbeBackend(ctx context.Context, backendID string) (latencyMS int64, err error) {
	adapter, ok := r.adapters[backendID]
	if !ok {
		return 0, fmt.Errorf("unknown backend: %s", backendID)
	}
	probeReq := &backend.Request{
		Messages: []backend.Message{
			{Role: "user", Content: "Reply with the single word: ok"},
		},
		MaxTokens: 10,
	}
	start := time.Now()
	resp, err := adapter.Invoke(ctx, probeReq)
	latencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return latencyMS, err
	}
	if resp.Content == "" {
		return latencyMS, fmt.Errorf("empty response from probe")
	}
	return latencyMS, nil
}

// --- internal helpers ---

func (r *Router) resolveChainEntry(entry string) []string {
	// Tier alias from config tiers map
	if tier, ok := r.cfg.Tiers[entry]; ok {
		return tier
	}
	// Direct backend ID
	if _, ok := r.adapters[entry]; ok {
		return []string{entry}
	}
	return nil
}

// nearTieEpsilon is the fractional score difference within which backends are
// considered tied and subject to random selection. 0.05 = within 5% of the best.
const nearTieEpsilon = 0.05

// selectBest scores all candidates and returns the best backend ID.
// When multiple backends fall within nearTieEpsilon of the top score, one is
// chosen uniformly at random — this spreads load and prevents thundering-herd
// routing without masking real score differences. Returns "" if all score 0.
func (r *Router) selectBest(candidates []string, req *backend.Request) string {
	if r.cfg.Routing.Strategy == "ordered" {
		// Ordered strategy: return first non-offline candidate, no scoring
		for _, bid := range candidates {
			if bs, ok := r.states[bid]; ok {
				snap := bs.Snapshot()
				if snap.Status != state.StatusOffline && !snap.QuotaExhausted {
					return bid
				}
			}
		}
		return ""
	}

	// Scored strategy: collect scores, then randomly pick among near-ties
	tokensEst := backend.EstimateTokens(req.Messages)
	type scored struct {
		id    string
		score float64
	}
	scores := make([]scored, 0, len(candidates))
	bestScore := 0.0

	for _, bid := range candidates {
		bs, ok := r.states[bid]
		if !ok {
			continue
		}
		cfg, ok := r.cfg.Backends[bid]
		if !ok {
			continue
		}
		s := Score(bs.Snapshot(), cfg, &r.cfg.Routing, tokensEst)
		if s > 0 {
			scores = append(scores, scored{bid, s})
			if s > bestScore {
				bestScore = s
			}
		}
	}
	if bestScore == 0 {
		return ""
	}

	// Collect backends within nearTieEpsilon of the best score
	threshold := bestScore * (1.0 - nearTieEpsilon)
	var tied []string
	for _, sc := range scores {
		if sc.score >= threshold {
			tied = append(tied, sc.id)
		}
	}
	if len(tied) == 1 {
		return tied[0]
	}
	// Randomly pick among near-ties to spread load
	return tied[rand.IntN(len(tied))]
}

func (r *Router) getState(backendID string) *state.BackendState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.states[backendID]
}

func (r *Router) recordSuccess(bid string, resp *backend.Response, latencyMS int64) {
	bs := r.getState(bid)
	if bs == nil {
		return
	}
	bs.RecordSuccess(
		resp.PromptTokensEst, resp.CompletionTokensEst, resp.CostUSD, latencyMS,
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)
	// Harvest rate-limit header data from the response (fields are zero-value when
	// the adapter did not populate them, e.g. CLI adapters).
	if resp.RateLimitInfo.TokensRemaining > 0 || resp.RateLimitInfo.RequestsRemaining > 0 {
		bs.UpdateRateLimitFromResponse(
			resp.RateLimitInfo.TokensRemaining,
			resp.RateLimitInfo.RequestsRemaining,
			resp.RateLimitInfo.TokensResetAt,
			resp.RateLimitInfo.RequestsResetAt,
		)
	}
	if r.store != nil {
		r.store.SaveState(bs.Snapshot())
	}
	// Fire quota_low alert if quota just crossed below the warning threshold
	// (edge-triggered — fires once per crossing, not on every call while low).
	if r.alertHook != nil {
		if fired, _ := bs.TestAndSetQuotaLow(r.cfg.Routing.QuotaWarningThreshold); fired {
			r.alertHook(Notification{
				Event:     "quota_low",
				BackendID: bid,
				Snapshot:  bs.Snapshot(),
			})
		}
	}
}

func (r *Router) recordFailure(bid string, errType state.ErrorType, errRaw string, resetAt time.Time, latencyMS int64) state.StatusChange {
	bs := r.getState(bid)
	if bs == nil {
		return state.StatusChange{}
	}
	change := bs.RecordFailure(errType, errRaw,
		resetAt,
		r.cfg.Routing.DegradedFailureThreshold,
		r.cfg.Routing.OfflineFailureThreshold,
	)
	if r.store != nil {
		r.store.SaveState(bs.Snapshot())
	}
	return change
}

// stateChangeEvent maps a status transition to a named alert event.
// When transitioning to OFFLINE, the error type determines whether it is a
// quota exhaustion, auth failure, or generic backend_offline event.
func stateChangeEvent(to state.Status, errType state.ErrorType) string {
	switch to {
	case state.StatusOffline:
		switch errType {
		case state.ErrTypeQuota:
			return "quota_exhausted"
		case state.ErrTypeAuth:
			return "auth_failure"
		}
		return "backend_offline"
	case state.StatusDegraded:
		return "backend_degraded"
	case state.StatusHealthy, state.StatusRecovering:
		return "backend_recovered"
	default:
		return "backend_status_change"
	}
}

// backgroundLoop runs health probing, rate window updates, and usage polling.
func (r *Router) backgroundLoop() {
	defer r.wg.Done()

	// Start usage pollers with a context tied to stopCh.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()

	for _, p := range r.usagePollers {
		p.Start(pollerCtx)
	}
	for _, p := range r.localPollers {
		go p.Start(pollerCtx)
	}

	probeTicker := time.NewTicker(time.Duration(r.cfg.Routing.HealthProbeIntervalSeconds) * time.Second)
	flushTicker := time.NewTicker(30 * time.Second)
	defer probeTicker.Stop()
	defer flushTicker.Stop()

	for {
		select {
		case <-r.stopCh:
			return

		case <-probeTicker.C:
			r.passiveProbe()
			if r.cfg.Routing.ActiveProbeEnabled {
				r.activeProbe()
			}

		case <-flushTicker.C:
			r.flushRateWindows()
			if r.store != nil {
				r.flushStatesToStore()
			}
		}
	}
}

// passiveProbe checks if any offline backends should transition to recovering.
func (r *Router) passiveProbe() {
	r.mu.RLock()
	ids := make([]string, 0, len(r.states))
	for id := range r.states {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	for _, id := range ids {
		bs := r.getState(id)
		if bs == nil {
			continue
		}
		if bs.TryRecover() {
			slog.Info("router: backend transitioned to recovering", "backend", id)
			if r.store != nil {
				r.store.SaveState(bs.Snapshot())
			}
		}
	}
}

// activeProbe runs live probe calls against RECOVERING and (optionally) DEGRADED
// backends to confirm they are usable before routing real traffic to them.
// Only called when active_probe_enabled = true.
func (r *Router) activeProbe() {
	r.mu.RLock()
	ids := make([]string, 0, len(r.states))
	for id := range r.states {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	for _, id := range ids {
		bs := r.getState(id)
		if bs == nil {
			continue
		}
		snap := bs.Snapshot()
		// Only probe RECOVERING (mandatory) and DEGRADED (opt-in via active_probe_enabled).
		if snap.Status != state.StatusRecovering && snap.Status != state.StatusDegraded {
			continue
		}

		slog.Debug("router: active probe starting", "backend", id, "status", snap.Status)
		probeTimeout := time.Duration(r.cfg.Routing.ActiveProbeTimeoutSeconds) * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		latencyMS, err := r.ProbeBackend(ctx, id)
		cancel()

		if err == nil {
			slog.Info("router: active probe succeeded", "backend", id, "latency_ms", latencyMS)
			// Synthesize a zero-token success to drive the state machine forward.
			bs.RecordSuccess(0, 0, 0, latencyMS,
				r.cfg.Routing.DegradedFailureThreshold,
				r.cfg.Routing.OfflineFailureThreshold,
			)
			if r.store != nil {
				r.store.SaveState(bs.Snapshot())
			}
		} else {
			slog.Warn("router: active probe failed", "backend", id, "err", err)
		}
	}
}

// flushRateWindows resets expired rate windows for all backends.
func (r *Router) flushRateWindows() {
	r.mu.RLock()
	ids := make([]string, 0, len(r.states))
	for id := range r.states {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	for _, id := range ids {
		cfg, ok := r.cfg.Backends[id]
		if !ok {
			continue
		}
		bs := r.getState(id)
		if bs != nil {
			bs.UpdateRateWindow(cfg.RateWindowSeconds)
		}
	}
}

// flushStatesToStore persists all backend states to SQLite.
func (r *Router) flushStatesToStore() {
	snaps := r.AllSnapshots()
	for _, snap := range snaps {
		r.store.SaveState(snap)
	}
}
