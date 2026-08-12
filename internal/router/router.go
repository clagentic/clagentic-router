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

// ErrNoToolCapableBackend is returned by FilterChainForTools when a chain
// resolves to at least one backend, but none of them declare
// Capabilities().SupportsTools. Callers use this to refuse a tool-bearing
// request explicitly rather than silently dropping the tools and returning
// a 200 (the defect this capability model exists to close).
var ErrNoToolCapableBackend = errors.New("chain has no tool-capable backend")

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

	// quotaProbers runs idle-quota probe loops for claude_cli backends that have
	// quota_probe.enabled=true. Keyed by backend ID.
	quotaProbers map[string]*backend.QuotaProber

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
	// Build quota probers for claude_cli backends that have quota_probe.enabled.
	probers := make(map[string]*backend.QuotaProber)
	for id, bcfg := range cfg.Backends {
		if !bcfg.QuotaProbe.Enabled {
			continue
		}
		adp, ok := adapters[id]
		if !ok {
			continue
		}
		// Only create probers for ClaudeCLIAdapter; other adapters do not emit
		// rate_limit_event lines so the probe would produce no useful data.
		if _, isClaude := adp.(*backend.ClaudeCLIAdapter); !isClaude {
			continue
		}
		probers[id] = backend.NewQuotaProber(id, bcfg.QuotaProbe, adp)
	}
	return &Router{
		cfg:          cfg,
		states:       states,
		adapters:     adapters,
		store:        st,
		alertHook:    hook,
		stopCh:       make(chan struct{}),
		quotaProbers: probers,
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

// startQuotaProbers launches all registered quota probe loops.
// Called from backgroundLoop so they share the same context lifetime.
func (r *Router) startQuotaProbers(ctx context.Context) {
	for bid, p := range r.quotaProbers {
		// Capture loop variables.
		probeBid := bid
		prober := p
		prober.Start(ctx, func(resp *backend.Response) {
			r.applyRateLimitEvent(ctx, probeBid, resp)
		})
		slog.Info("quota_probe: started probe loop", "backend", probeBid)
	}
}

// stopQuotaProbers shuts down all running quota probe loops.
func (r *Router) stopQuotaProbers() {
	for _, p := range r.quotaProbers {
		p.Stop()
	}
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

		bid, bidScore := r.selectBest(candidates, req)
		if bid == "" {
			slog.Debug("router: all backends in tier unavailable", "entry", entry, "request_id", reqID)
			continue
		}

		// Snapshot quota state at routing time for the log row.
		var routingUtilization *float64
		var routingRateLimitType string
		if bs := r.getState(bid); bs != nil {
			snap := bs.Snapshot()
			if snap.LastQuotaSnapshot != nil {
				routingRateLimitType = snap.LastQuotaSnapshot.RateLimitType
				routingUtilization = snap.LastQuotaSnapshot.Utilization
			}
		}

		tried = append(tried, bid)
		start := time.Now()
		resp, err := r.adapters[bid].Invoke(ctx, req)
		latencyMS := time.Since(start).Milliseconds()

		if err == nil {
			// Success
			r.recordSuccess(bid, resp, latencyMS)
			r.applyRateLimitEvent(ctx, bid, resp)

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
				r.store.LogCall(store.CallLogInput{
					BackendID:           bid,
					TierAlias:           entry,
					ChainPosition:       i,
					Outcome:             "pass",
					PromptTokensEst:     resp.PromptTokensEst,
					CompletionTokensEst: resp.CompletionTokensEst,
					LatencyMS:           int(latencyMS),
					CostUSD:             resp.CostUSD,
					Model:               r.cfg.Backends[bid].Model,
					Score:               bidScore,
					RequestID:           reqID,
					RateLimitType:       routingRateLimitType,
					Utilization:         routingUtilization,
					FallbackCount:       i,
				})
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
			r.store.LogCall(store.CallLogInput{
				BackendID:     bid,
				TierAlias:     entry,
				ChainPosition: i,
				Outcome:       "fallback",
				ErrorType:     string(errType),
				LatencyMS:     int(latencyMS),
				Model:         r.cfg.Backends[bid].Model,
				Score:         bidScore,
				RequestID:     reqID,
				RateLimitType: routingRateLimitType,
				Utilization:   routingUtilization,
				FallbackCount: i,
			})
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
			r.store.LogCall(store.CallLogInput{
				BackendID:     bid,
				TierAlias:     strings.Join(chain, ","),
				ChainPosition: len(chain) - 1,
				Outcome:       "degraded",
				ErrorType:     "all_failed",
				Model:         r.cfg.Backends[bid].Model,
				RequestID:     reqID,
				FallbackCount: len(tried),
			})
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

// AdapterCapabilities returns the declared capabilities of one backend's
// adapter, or (Capabilities{}, false) if backendID is not configured.
func (r *Router) AdapterCapabilities(backendID string) (backend.Capabilities, bool) {
	r.mu.RLock()
	adp, ok := r.adapters[backendID]
	r.mu.RUnlock()
	if !ok {
		return backend.Capabilities{}, false
	}
	return adp.Capabilities(), true
}

// FilterChainForTools narrows chain to entries that resolve to at least one
// tool-capable backend. A tier-alias or role-chain entry whose candidate set
// contains a mix of capable and incapable backends is kept as-is — Route's
// own selectBest/fallback walk still needs the full candidate list at that
// position, and per-candidate exclusion happens inside Route via the normal
// scoring/fallback path once a caller has confirmed (via this filter) that
// tool-bearing traffic is permitted on the chain at all.
//
// Returns ErrNoToolCapableBackend when chain is non-empty but resolves to no
// tool-capable backend anywhere in it — the caller (server layer) must
// refuse the request rather than route it through an incapable backend that
// would silently drop the tools.
func (r *Router) FilterChainForTools(chain []string) ([]string, error) {
	if len(chain) == 0 {
		return nil, ErrNoChain
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	filtered := make([]string, 0, len(chain))
	anyCapable := false

	for _, entry := range chain {
		candidates := r.resolveChainEntry(entry)
		entryHasCapable := false
		for _, bid := range candidates {
			adp, ok := r.adapters[bid]
			if !ok {
				continue
			}
			if adp.Capabilities().SupportsTools {
				entryHasCapable = true
				anyCapable = true
				break
			}
		}
		if entryHasCapable {
			filtered = append(filtered, entry)
		}
	}

	if !anyCapable {
		return nil, ErrNoToolCapableBackend
	}
	return filtered, nil
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

// selectBest scores all candidates and returns the best backend ID and its score.
// When multiple backends fall within nearTieEpsilon of the top score, one is
// chosen uniformly at random — this spreads load and prevents thundering-herd
// routing without masking real score differences. Returns ("", 0) if all score 0.
func (r *Router) selectBest(candidates []string, req *backend.Request) (string, float64) {
	if r.cfg.Routing.Strategy == "ordered" {
		// Ordered strategy: return first non-offline candidate, no scoring
		for _, bid := range candidates {
			if bs, ok := r.states[bid]; ok {
				snap := bs.Snapshot()
				if snap.Status != state.StatusOffline && !snap.QuotaExhausted {
					return bid, 1.0 // score=1.0 sentinel for ordered strategy
				}
			}
		}
		return "", 0
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
		return "", 0
	}

	// Collect backends within nearTieEpsilon of the best score
	threshold := bestScore * (1.0 - nearTieEpsilon)
	type tiedEntry struct {
		id    string
		score float64
	}
	var tied []tiedEntry
	for _, sc := range scores {
		if sc.score >= threshold {
			tied = append(tied, tiedEntry{sc.id, sc.score})
		}
	}
	if len(tied) == 1 {
		return tied[0].id, tied[0].score
	}
	// Randomly pick among near-ties to spread load
	pick := tied[rand.IntN(len(tied))]
	return pick.id, pick.score
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

// applyRateLimitEvent processes a rate_limit_event embedded in a successful response.
// It persists a quota snapshot to the store, updates BackendState quota fields,
// and logs the event. Called after every successful invoke that returns a Response.
func (r *Router) applyRateLimitEvent(ctx context.Context, bid string, resp *backend.Response) {
	e := resp.RateLimitEvent
	if e == nil {
		return
	}

	// Persist to quota_snapshots table.
	// Translate backend.RateLimitEvent → store.QuotaSnapshotInput to keep
	// store free of backend imports (import graph: store → state only).
	if r.store != nil {
		inp := store.QuotaSnapshotInput{
			Status:                e.Status,
			RateLimitType:         e.RateLimitType,
			Utilization:           e.Utilization,
			SurpassedThreshold:    e.SurpassedThreshold,
			IsUsingOverage:        e.IsUsingOverage,
			OverageStatus:         e.OverageStatus,
			OverageDisabledReason: e.OverageDisabledReason,
			RawJSON:               e.RawJSON,
		}
		if !e.ResetsAt.IsZero() {
			v := e.ResetsAt.Unix()
			inp.ResetsAt = &v
		}
		if e.OverageResetsAt != nil {
			v := e.OverageResetsAt.Unix()
			inp.OverageResetsAt = &v
		}
		if err := r.store.InsertQuotaSnapshot(ctx, bid, inp); err != nil {
			// Already logged by the store; don't block routing on storage errors.
			_ = err
		}
	}

	// Update in-memory state.
	bs := r.getState(bid)
	if bs == nil {
		return
	}

	// Build the quota snapshot for the /v1/capacity endpoint.
	snap := &state.QuotaSnapshot{
		Status:        e.Status,
		RateLimitType: e.RateLimitType,
		Utilization:   e.Utilization,
		ResetsAt:      e.ResetsAt,
		ObservedAt:    time.Now().UTC(),
	}

	bs.UpdateFromRateLimitEvent(state.RateLimitEventData{
		Status:            e.Status,
		RateLimitType:     e.RateLimitType,
		ResetsAt:          e.ResetsAt,
		Utilization:       e.Utilization,
		LastQuotaSnapshot: snap,
	})

	utilizationLog := "below-threshold"
	if e.Utilization != nil {
		utilizationLog = fmt.Sprintf("%.4f", *e.Utilization)
	}
	slog.Info("rate_limit_event",
		"backend_id", bid,
		"rate_limit_type", e.RateLimitType,
		"status", e.Status,
		"utilization", utilizationLog,
		"resets_at", e.ResetsAt,
	)

	// Notify the quota prober (if any) so it resets its idle timer.
	// This prevents a probe from firing when organic data just arrived.
	if p, ok := r.quotaProbers[bid]; ok {
		p.NotifyEvent()
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

	// Start usage pollers, local pollers, and quota probers with a context tied to stopCh.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	defer r.stopQuotaProbers()

	for _, p := range r.usagePollers {
		p.Start(pollerCtx)
	}
	for _, p := range r.localPollers {
		go p.Start(pollerCtx)
	}
	r.startQuotaProbers(pollerCtx)

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
			if r.cfg.Routing.OfflineRecoveryProbeInterval() > 0 {
				r.offlineRecoveryProbe()
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

// offlineRecoveryProbe runs bounded recovery probes against OFFLINE backends
// that have been stuck offline without a known quota/rate-limit reset time.
//
// Motivation: TryRecover() (called by passiveProbe) only transitions OFFLINE
// backends that carry a future QuotaResetAt or RateLimitResetAt. Backends that
// tripped OFFLINE due to auth failures, not-found errors, or soft-failure
// cascades carry no such reset time and would stay offline indefinitely without
// organic traffic to test them. This probe fills that gap.
//
// Gating rule: skip any backend whose quota or rate-limit reset time is still
// in the future — those are already owned by TryRecover and must not be forced
// back into rotation prematurely (QuotaResetAt/RateLimitResetAt are contract
// times, not suggestions).
//
// Rate: at most one probe per backend per OfflineRecoveryProbeIntervalSeconds.
// On success: transition to RECOVERING via a synthesized zero-token success
// (mirrors activeProbe) and persist via store. On failure: log a warning and
// record the attempt timestamp so the interval gate prevents hammering.
func (r *Router) offlineRecoveryProbe() {
	intervalSeconds := r.cfg.Routing.OfflineRecoveryProbeInterval()
	if intervalSeconds <= 0 {
		return
	}

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
		if snap.Status != state.StatusOffline {
			continue
		}

		// Skip backends where TryRecover already owns recovery via a pending
		// quota or rate-limit reset time. Probing them early would bypass the
		// provider's back-off window.
		if bs.HasPendingReset() {
			continue
		}

		// Gate: only probe once per interval.
		if !bs.RecoveryProbeDue(intervalSeconds) {
			continue
		}

		slog.Info("router: offline recovery probe starting", "backend", id,
			"last_error_type", snap.LastErrorType,
			"offline_since", snap.LastFailureAt,
		)

		probeTimeout := time.Duration(r.cfg.Routing.ActiveProbeTimeoutSeconds) * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		latencyMS, err := r.ProbeBackend(ctx, id)
		cancel()

		// Always record the probe timestamp to gate the next attempt, regardless
		// of outcome. This prevents a broken backend from being hammered every tick.
		bs.MarkRecoveryProbed()

		if err == nil {
			slog.Info("router: offline recovery probe succeeded", "backend", id,
				"latency_ms", latencyMS,
			)
			// Synthesize a zero-token success to advance OFFLINE → RECOVERING.
			// (A second success from RECOVERING → HEALTHY via RecordSuccess.)
			bs.RecordSuccess(0, 0, 0, latencyMS,
				r.cfg.Routing.DegradedFailureThreshold,
				r.cfg.Routing.OfflineFailureThreshold,
			)
			if r.store != nil {
				r.store.SaveState(bs.Snapshot())
			}
		} else {
			slog.Warn("router: offline recovery probe failed", "backend", id, "err", err)
			// LastRecoveryProbeAt is in-memory/ephemeral — it is not part of the
			// SQLite schema and SaveState does not write it. The gate therefore
			// resets to zero on daemon restart (a missed probe window at restart
			// is harmless). No store write is needed here: the state/status has
			// not changed and nothing durable has been updated.
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
