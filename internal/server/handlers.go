// internal/server/handlers.go — HTTP request handlers.
//
// All handlers use writeJSON/writeError for consistent response shape.
// The /v1/chat/completions handler is OpenAI-compatible so any OpenAI SDK
// can point at clagentic-router without client changes.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

// Handler holds the shared dependencies for all HTTP handlers.
type Handler struct {
	router     *router.Router
	store      *store.Store
	token      string // inference token
	adminToken string // admin token (may equal token if not separately configured)

	// buildVersion is the running binary's revision string (main.version, set
	// at build time via -ldflags -X — see cmd/clagentic-router/main.go).
	// Surfaced on /health, /doctor, and /version so a stale deployment is
	// visible from the same diagnostic surfaces an operator already checks,
	// without a separate build-vs-running-binary comparison step (lr-92ee18
	// B1). Named distinctly from the version(...) handler method below —
	// Go permits a field and method sharing a name on different receivers,
	// but not within the same struct's own field/method set.
	buildVersion string

	// allowNoAuth is the explicit, operator-set intent to run without
	// authentication. Set ONLY by cmdServe when --unsafe-no-auth was
	// actually passed (see main.go). When false, an empty token/adminToken
	// causes the corresponding auth middleware to reject with 401 instead
	// of the old empty-string-means-open behavior. (lr-7a26e0)
	allowNoAuth bool

	// anthropicUpstreamURL is the passthrough target for POST /v1/messages
	// requests whose model is not role:/chain:/backend:-prefixed.
	anthropicUpstreamURL string
	// anthropicUpstreamAPIKey, when non-empty, overrides the client's own
	// credential on the upstream passthrough request. Empty means forward
	// the client's x-api-key/Authorization header unchanged.
	anthropicUpstreamAPIKey string

	// bedrockRegion is the AWS region POST /model/{modelId}/invoke[-with-response-stream]
	// passthrough requests are SigV4-signed and sent to. Empty disables
	// Bedrock passthrough (routed role:/chain:/backend: model IDs still work).
	bedrockRegion string
	// bedrockProfile is an optional named AWS shared-config/credentials
	// profile used to resolve passthrough signing credentials. Empty uses
	// the standard SDK credential chain with no profile override.
	bedrockProfile string
	// bedrockUpstreamBaseURL overrides the real AWS Bedrock Runtime endpoint
	// (https://bedrock-runtime.<region>.amazonaws.com) for the passthrough
	// path. Empty (production default) uses the real endpoint; tests set it
	// to an httptest server URL to verify request-building and SigV4 signing
	// deterministically without live AWS credentials.
	bedrockUpstreamBaseURL string

	// bedrockCredentialsFn resolves AWS credentials for passthrough SigV4
	// signing. nil (production default) uses resolveBedrockCredentials (the
	// real AWS SDK credential chain); tests inject a stub so passthrough
	// request-building/signing is verifiable without live AWS credentials,
	// IMDS access, or network calls.
	bedrockCredentialsFn func(ctx context.Context) (aws.Credentials, error)
}

// --- OpenAI-compatible types ---

type chatCompletionRequest struct {
	Model     string            `json:"model"`
	Messages  []backend.Message `json:"messages"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
	// Tools is decoded only far enough to detect presence at THIS struct
	// level (any non-null, non-empty-array JSON value) — kept as
	// json.RawMessage rather than a typed slice so a malformed tools array
	// only fails when routed mode's stricter decodeOpenAITools actually
	// needs it, not on every request regardless of whether tools are
	// present. A non-empty Tools value must route only to a tool-capable
	// backend or be refused outright; see chatCompletions' tool-capability
	// check. When present and a tool-capable backend is available,
	// decodeOpenAITools translates it into backend.Request.Tools
	// (lr-add405).
	Tools json.RawMessage `json:"tools,omitempty"`
	// WorkingDir is an opt-in absolute directory subprocess (CLI) adapters
	// use as their cmd.Dir. Empty (the default) falls through to
	// backend.DefaultWorkingDir with no inference — never guessed from
	// server-side state. Validated at this boundary via
	// backend.ResolveWorkingDir; ignored entirely by HTTP adapters.
	WorkingDir string `json:"working_dir,omitempty"`
}

// openAIToolDef is the wire shape of one entry in the OpenAI Chat
// Completions "tools" array:
// {"type":"function","function":{name,description,parameters}}. Decoded
// only far enough to translate into the neutral backend.ToolDef IR
// (decodeOpenAITools/toBackendToolDefsFromOpenAI) — see messages.go's
// anthropicMsgToolDef for the Anthropic-side equivalent.
type openAIToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// decodeOpenAITools decodes req.Tools (json.RawMessage, presence-checked
// separately by hasTools) into []openAIToolDef, called only after
// hasTools(req.Tools) is true.
func decodeOpenAITools(raw json.RawMessage) ([]openAIToolDef, error) {
	if !hasTools(raw) {
		return nil, nil
	}
	var tools []openAIToolDef
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return tools, nil
}

// toBackendToolDefsFromOpenAI translates decoded OpenAI tool definitions
// into the neutral backend.ToolDef IR. Returns nil for an empty input,
// matching the "absent means no tools" contract of backend.Request.Tools.
func toBackendToolDefsFromOpenAI(tools []openAIToolDef) []backend.ToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]backend.ToolDef, len(tools))
	for i, t := range tools {
		out[i] = backend.ToolDef{Name: t.Function.Name, Description: t.Function.Description, InputSchema: t.Function.Parameters}
	}
	return out
}

// toOpenAIToolCalls translates backend.ToolUse results into OpenAI
// message.tool_calls entries for the chat completion response.
func toOpenAIToolCalls(uses []backend.ToolUse) []openAIToolCall {
	if len(uses) == 0 {
		return nil
	}
	out := make([]openAIToolCall, len(uses))
	for i, u := range uses {
		out[i] = openAIToolCall{ID: u.ID, Type: "function"}
		out[i].Function.Name = u.Name
		out[i].Function.Arguments = string(u.Input)
	}
	return out
}

// openAIToolCall is one entry of message.tool_calls in a chat completion
// response — the OpenAI wire shape for a model-requested tool invocation.
// Function.Arguments is a JSON-encoded string (not a raw object), matching
// the real OpenAI API's wire contract.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatCompletionFinishReason returns "tool_calls" when the backend returned
// any tool_use blocks (lr-add405), "stop" otherwise — the OpenAI
// finish_reason taxonomy's tool-calling value.
func chatCompletionFinishReason(resp *backend.Response) string {
	if len(resp.ToolUses) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// hasTools reports whether raw is a present, non-null, non-empty-array JSON
// value. Mirrors the same "detect presence without deep validation" approach
// used for the Anthropic Messages "system" field in messages.go.
func hasTools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != "[]"
}

type chatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Index        int                   `json:"index"`
	Message      chatCompletionRespMsg `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

// chatCompletionRespMsg is the response-side message shape — distinct from
// backend.Message (the internal request/history type) because only the
// response needs to carry tool_calls; adding it to backend.Message would
// leak a wire-response concern into the internal neutral IR (lr-add405).
type chatCompletionRespMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SSE streaming types — used when stream:true is set on the request.
// Backends return complete responses; we emit one content chunk then [DONE].

type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// chunkDelta carries the incremental content for one SSE chunk.
// Role is set on the first chunk; Content or ToolCalls on the content
// chunk; all empty on the final chunk.
type chunkDelta struct {
	Role      string               `json:"role,omitempty"`
	Content   string               `json:"content,omitempty"`
	ToolCalls []chunkToolCallDelta `json:"tool_calls,omitempty"`
}

// chunkToolCallDelta is one entry of delta.tool_calls in a streaming chunk.
// Index is required by the OpenAI streaming grammar (accumulates
// tool_calls by position across chunks); since this router emits each
// tool_call's full id/name/arguments in a single chunk rather than true
// incremental deltas (same "NOT true token streaming" limitation as the
// text path), Index is simply the tool_call's position in resp.ToolUses.
type chunkToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// chatCompletions handles POST /v1/chat/completions.
// Model field syntax:
//   - "claude-haiku"                   tier alias
//   - "chain:alias1,alias2,alias3"     explicit chain
//   - "role:summarizer"                named chain from config
//   - "backend:backend-id"             direct backend (no scoring)
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Hard limits defend against oversized requests. Values are conservative defaults;
	// make them configurable in a follow-up. (TODO(lr-c7ac): expose via config)
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024) // 4 MB

	var req chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("decode body: %v", err))
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if len(req.Model) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "model field too long")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}
	if len(req.Messages) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "too many messages (max 200)")
		return
	}
	for _, msg := range req.Messages {
		if len(msg.Content) > 512*1024 {
			writeError(w, http.StatusBadRequest, "invalid_request", "message content too large (max 512KB)")
			return
		}
	}
	if req.MaxTokens > 32000 {
		writeError(w, http.StatusBadRequest, "invalid_request", "max_tokens exceeds limit (max 32000)")
		return
	}
	workingDir, err := backend.ResolveWorkingDir(req.WorkingDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chain := h.router.ResolveModel(req.Model)
	if len(chain) == 0 {
		writeError(w, http.StatusBadRequest, "unknown_model",
			fmt.Sprintf("model %q did not resolve to any configured backends", req.Model))
		return
	}

	reqHasTools := hasTools(req.Tools)
	var toolDefs []backend.ToolDef
	if reqHasTools {
		filtered, err := h.router.FilterChainForTools(chain)
		if err != nil {
			if errors.Is(err, router.ErrNoToolCapableBackend) {
				// Refused before Route is ever called — Route's own call_log
				// writes never fire for this request, so record the refusal
				// explicitly (presence only; see LogToolRefusal doc).
				h.router.LogToolRefusal(r.Context(), chain, req.Model)
				writeError(w, http.StatusUnprocessableEntity, "no_tool_capable_backend",
					fmt.Sprintf("request carries tools but model %q resolves to no tool-capable backend; "+
						"remove tools or route to a backend whose adapter declares Capabilities().SupportsTools", req.Model))
				return
			}
			writeError(w, http.StatusBadRequest, "unknown_model",
				fmt.Sprintf("model %q did not resolve to any configured backends", req.Model))
			return
		}
		chain = filtered

		openAITools, err := decodeOpenAITools(req.Tools)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		toolDefs = toBackendToolDefsFromOpenAI(openAITools)
	}

	routerReq := &backend.Request{
		Messages:   req.Messages,
		MaxTokens:  req.MaxTokens,
		WorkingDir: workingDir,
		HasTools:   reqHasTools,
		Tools:      toolDefs,
	}

	resp, meta, err := h.router.Route(r.Context(), routerReq, chain)
	if err != nil {
		if errors.Is(err, router.ErrAllFailed) || errors.Is(err, router.ErrNoChain) {
			// Log the raw error server-side; do not include it in the client
			// response to avoid leaking internal backend error details to
			// inference callers. lastErrorType (if present) is type-only —
			// see errorResponse.LastErrorType's doc.
			slog.Error("chat: chain exhausted", "err", err, "request_id", RequestID(r.Context()))
			var lastErrorType string
			var chainErr *router.ChainExhaustedError
			if errors.As(err, &chainErr) {
				lastErrorType = string(chainErr.Type)
			}
			writeChainExhaustedError(w, lastErrorType)
			return
		}
		// Log the raw error server-side; do not include it in the client response
		// to avoid leaking internal backend error details to inference callers.
		slog.Error("chat: backend error", "err", err, "request_id", RequestID(r.Context()))
		writeError(w, http.StatusBadGateway, "backend_error", "upstream backend failed")
		return
	}

	// Set routing metadata headers (present on both streaming and non-streaming responses).
	w.Header().Set("X-Router-Backend", meta.BackendID)
	w.Header().Set("X-Router-Chain-Position", strconv.Itoa(meta.ChainPosition))
	if meta.FallbackReason != "" {
		w.Header().Set("X-Router-Fallback-Reason", meta.FallbackReason)
	}
	w.Header().Set("X-Router-Latency-Ms", strconv.FormatInt(meta.LatencyMS, 10))

	if req.Stream {
		writeSSEStream(w, req.Model, resp)
		return
	}

	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      "chatcmpl-" + uuid.NewString()[:8],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []choice{{
			Index: 0,
			Message: chatCompletionRespMsg{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: toOpenAIToolCalls(resp.ToolUses),
			},
			FinishReason: chatCompletionFinishReason(resp),
		}},
		Usage: usage{
			PromptTokens:     resp.PromptTokensEst,
			CompletionTokens: resp.CompletionTokensEst,
			TotalTokens:      resp.PromptTokensEst + resp.CompletionTokensEst,
		},
	})
}

// models handles GET /v1/models — lists all configured backends with status
// and capabilities.
//
// The capabilities object lets a caller do pre-flight discovery: know before
// sending whether a given backend can carry tools, so a tool-bearing request
// can be routed correctly instead of hitting the routed-mode refusal at
// request time. Extending this existing endpoint (rather than adding a new
// one) keeps status and capability discovery in the same read, since a
// caller choosing a backend needs both together.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	snaps := h.router.AllSnapshots()
	type modelData struct {
		ID           string      `json:"id"`
		Object       string      `json:"object"`
		Router       interface{} `json:"router"`
		Capabilities interface{} `json:"capabilities"`
	}
	models := make([]modelData, 0, len(snaps))
	for id, snap := range snaps {
		caps, _ := h.router.AdapterCapabilities(id)
		models = append(models, modelData{
			ID:     id,
			Object: "model",
			Router: map[string]interface{}{
				"status":               string(snap.Status),
				"consecutive_failures": snap.ConsecutiveFailures,
				"quota_exhausted":      snap.QuotaExhausted,
				"rate_window_messages": snap.RateWindowMessages,
				"last_success_at":      timeOrNull(snap.LastSuccessAt),
				"last_error_type":      string(snap.LastErrorType),
				"session_cost_usd":     snap.SessionCostUSDEst,
				"total_calls":          snap.TotalCalls,
			},
			Capabilities: map[string]interface{}{
				"supports_tools":     caps.SupportsTools,
				"supports_streaming": caps.SupportsStreaming,
				"supports_images":    caps.SupportsImages,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   models,
	})
}

// health handles GET /health — fast cached status.
//
// overall is "ok" only when every configured backend both has a resolved CLI
// binary (or has no binary concept at all — HTTP adapters) AND is not
// StatusOffline. A backend whose binary never resolved at startup used to be
// invisible here — /health reported "ok" indefinitely even with 4 of 5
// configured backends permanently unable to serve a request, because
// ResolveBinPath's failure was WARN-only startup-log noise with no runtime
// surface (lr-92ee18 B2). unresolved_binaries names exactly which backends
// are in that state so an operator does not have to grep the startup log to
// find out.
func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	snaps := h.router.AllSnapshots()
	unresolved := h.router.UnresolvedBinaryBackends()
	unresolvedSet := make(map[string]struct{}, len(unresolved))
	for _, id := range unresolved {
		unresolvedSet[id] = struct{}{}
	}

	backendStatus := make(map[string]string, len(snaps))
	overall := "ok"
	for id, snap := range snaps {
		backendStatus[id] = string(snap.Status)
		if snap.Status == state.StatusOffline {
			overall = "degraded"
		}
		if _, bad := unresolvedSet[id]; bad {
			overall = "degraded"
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":              overall,
		"version":             h.buildVersion,
		"backends":            backendStatus,
		"unresolved_binaries": unresolved,
	})
}

// doctor handles GET /doctor — live probes every backend.
func (h *Handler) doctor(w http.ResponseWriter, r *http.Request) {
	ids := h.router.BackendIDs()
	type probeResult struct {
		BackendID   string `json:"backend_id"`
		ProbeStatus string `json:"probe_status"` // "ok" | "error"
		LatencyMS   int64  `json:"latency_ms,omitempty"`
		Error       string `json:"error,omitempty"`
	}

	results := make([]probeResult, 0, len(ids))
	probeCtx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	for _, id := range ids {
		latency, err := h.router.ProbeBackend(probeCtx, id)
		pr := probeResult{BackendID: id, LatencyMS: latency}
		if err != nil {
			pr.ProbeStatus = "error"
			pr.Error = err.Error()
		} else {
			pr.ProbeStatus = "ok"
		}
		results = append(results, pr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"probed_at": time.Now().UTC().Format(time.RFC3339),
		"version":   h.buildVersion,
		"results":   results,
	})
}

// quota handles GET /quota — per-backend quota and rate limit state.
func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	snaps := h.router.AllSnapshots()
	result := make(map[string]interface{}, len(snaps))
	for id, snap := range snaps {
		result[id] = map[string]interface{}{
			"status":                 string(snap.Status),
			"quota_exhausted":        snap.QuotaExhausted,
			"quota_reset_at":         timeOrNull(snap.QuotaResetAt),
			"quota_tokens_remaining": snap.QuotaTokensRemaining,
			"quota_tokens_total":     snap.QuotaTokensTotal,
			"rate_window_messages":   snap.RateWindowMessages,
			"rate_window_tokens_est": snap.RateWindowTokensEst,
			"rate_window_start":      timeOrNull(snap.RateWindowStart),
			"rate_limit_reset_at":    timeOrNull(snap.RateLimitResetAt),
			"session_cost_usd":       snap.SessionCostUSDEst,
			"total_cost_usd":         snap.TotalCostUSDEst,
			"total_calls":            snap.TotalCalls,
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// capacity handles GET /v1/capacity — per-backend capacity and last quota snapshot.
//
// last_quota_snapshot is null if no rate_limit_event has been received since
// daemon start for that backend (ephemeral; not restored from SQLite on restart).
func (h *Handler) capacity(w http.ResponseWriter, r *http.Request) {
	snaps := h.router.AllSnapshots()
	type backendCapacity struct {
		BackendID         string      `json:"backend_id"`
		Status            string      `json:"status"`
		QuotaExhausted    bool        `json:"quota_exhausted"`
		QuotaResetAt      interface{} `json:"quota_reset_at"`
		TotalCalls        int64       `json:"total_calls"`
		SessionCostUSD    float64     `json:"session_cost_usd"`
		LastQuotaSnapshot interface{} `json:"last_quota_snapshot"`
	}
	entries := make([]backendCapacity, 0, len(snaps))
	for id, snap := range snaps {
		var lastSnap interface{} = nil
		if snap.LastQuotaSnapshot != nil {
			lastSnap = map[string]interface{}{
				"status":          snap.LastQuotaSnapshot.Status,
				"rate_limit_type": snap.LastQuotaSnapshot.RateLimitType,
				"utilization":     snap.LastQuotaSnapshot.Utilization,
				"resets_at":       snap.LastQuotaSnapshot.ResetsAt.UTC().Format(time.RFC3339),
				"observed_at":     snap.LastQuotaSnapshot.ObservedAt.UTC().Format(time.RFC3339),
			}
		}
		entries = append(entries, backendCapacity{
			BackendID:         id,
			Status:            string(snap.Status),
			QuotaExhausted:    snap.QuotaExhausted,
			QuotaResetAt:      timeOrNull(snap.QuotaResetAt),
			TotalCalls:        snap.TotalCalls,
			SessionCostUSD:    snap.SessionCostUSDEst,
			LastQuotaSnapshot: lastSnap,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"backends": entries})
}

// metrics handles GET /metrics — Prometheus text format.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	snaps := h.router.AllSnapshots()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	for id, snap := range snaps {
		statusVal := 0
		if snap.Status == state.StatusHealthy {
			statusVal = 1
		}
		fmt.Fprintf(w, "router_backend_status{backend=%q} %d\n", id, statusVal)
		fmt.Fprintf(w, "router_backend_quota_exhausted{backend=%q} %d\n", id, boolToInt(snap.QuotaExhausted))
		fmt.Fprintf(w, "router_backend_consecutive_failures{backend=%q} %d\n", id, snap.ConsecutiveFailures)
		fmt.Fprintf(w, "router_backend_total_calls{backend=%q} %d\n", id, snap.TotalCalls)
		fmt.Fprintf(w, "router_backend_rate_window_messages{backend=%q} %d\n", id, snap.RateWindowMessages)
		fmt.Fprintf(w, "router_backend_session_cost_usd{backend=%q} %.6f\n", id, snap.SessionCostUSDEst)
		if snap.QuotaTokensRemaining >= 0 {
			fmt.Fprintf(w, "router_backend_quota_tokens_remaining{backend=%q} %d\n", id, snap.QuotaTokensRemaining)
		}
	}
}

// logs handles GET /logs — call log entries with optional date-range filtering.
//
// Query params:
//
//	backend=<id>   filter to one backend (optional)
//	limit=<n>      max rows, 1–500, default 50
//	from=<RFC3339> inclusive lower bound on call timestamp (optional)
//	to=<RFC3339>   exclusive upper bound on call timestamp (optional)
func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"rows": []interface{}{}})
		return
	}
	f, err := parseCallLogFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	rows, err := h.store.RecentCalls(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rows": rows})
}

// stats handles GET /stats — aggregated call statistics with optional date-range filtering.
//
// Query params: same as /logs (backend, from, to). limit is ignored.
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, http.StatusOK, store.CallStats{})
		return
	}
	f, err := parseCallLogFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	stats, err := h.store.CallStatsFor(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// parseCallLogFilter reads backend, limit, from, to query params from r.
func parseCallLogFilter(r *http.Request) (store.CallLogFilter, error) {
	var f store.CallLogFilter
	f.BackendID = r.URL.Query().Get("backend")
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n <= 0 {
			return f, fmt.Errorf("limit must be a positive integer")
		}
		f.Limit = n
	}
	if fromStr := r.URL.Query().Get("from"); fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return f, fmt.Errorf("from: %w", err)
		}
		f.From = t
	}
	if toStr := r.URL.Query().Get("to"); toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return f, fmt.Errorf("to: %w", err)
		}
		f.To = t
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.To.After(f.From) {
		return f, fmt.Errorf("to must be after from")
	}
	return f, nil
}

// rateLimitEvent handles POST /v1/internal/rate-limit.
//
// Accepts a rate-limit event from clagentic-console (Anthropic SDK rate_limit_event)
// and updates the named backend's state so the scorer can react before the next request.
func (h *Handler) rateLimitEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BackendID string `json:"backend_id"`
		LimitType string `json:"limit_type"`
		ResetsAt  string `json:"resets_at"`
		Status    string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("decode body: %v", err))
		return
	}
	if body.BackendID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "backend_id is required")
		return
	}
	if body.Status != "warning" && body.Status != "exhausted" {
		writeError(w, http.StatusBadRequest, "invalid_request", `status must be "warning" or "exhausted"`)
		return
	}

	var resetsAt time.Time
	if body.ResetsAt != "" {
		var err error
		resetsAt, err = time.Parse(time.RFC3339, body.ResetsAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("resets_at: must be RFC 3339 timestamp: %v", err))
			return
		}
	}

	if err := h.router.ApplyRateLimitEvent(body.BackendID, body.LimitType, body.Status, resetsAt); err != nil {
		// router returns "unknown backend" for missing IDs — treat as 404
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "applied",
		"backend_id": body.BackendID,
		"event":      body.Status,
	})
}

// backendReset handles POST /backends/{id}/reset.
func (h *Handler) backendReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.router.ForceReset(id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset", "backend_id": id})
}

// backendDisable handles POST /backends/{id}/disable.
func (h *Handler) backendDisable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.router.ForceDisable(id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled", "backend_id": id})
}

// backendEnable handles POST /backends/{id}/enable.
func (h *Handler) backendEnable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.router.ForceEnable(id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled", "backend_id": id})
}

// webhookCreate handles POST /webhooks.
func (h *Handler) webhookCreate(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no_store", "storage not configured")
		return
	}
	var body struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Secret string   `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "url is required")
		return
	}
	// Validate URL — block SSRF targets.
	if err := validateWebhookURL(body.URL, false); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	}
	// Validate that all requested event names are known.
	// An empty events list is intentionally accepted — it registers the endpoint
	// to receive all event types. The deliverer does not filter by events when the
	// list is empty, so the webhook fires on every state change.
	for _, ev := range body.Events {
		if _, ok := knownWebhookEvents[ev]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_event", fmt.Sprintf("unknown event type %q", ev))
			return
		}
	}
	id := uuid.NewString()
	eventsJSON, _ := json.Marshal(body.Events)
	h.store.SaveWebhook(id, body.URL, string(eventsJSON), body.Secret)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "url": body.URL})
}

// webhookDelete handles DELETE /webhooks/{id}.
func (h *Handler) webhookDelete(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "no_store", "storage not configured")
		return
	}
	id := r.PathValue("id")
	h.store.DeleteWebhook(id)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// webhookList handles GET /webhooks.
func (h *Handler) webhookList(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"webhooks": []interface{}{}})
		return
	}
	rows, err := h.store.ListWebhooks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Redact secret — callers learn only whether a secret is set, never its value.
	items := make([]webhookListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, webhookListItem{
			ID:        row.ID,
			URL:       row.URL,
			Events:    row.Events,
			HasSecret: row.Secret != "",
			CreatedAt: row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"webhooks": items})
}

// version handles GET /version.
func (h *Handler) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"product": "clagentic-router",
		"version": h.buildVersion,
	})
}

// writeSSEStream writes an OpenAI-compatible Server-Sent Events response for
// stream:true requests. Because backends return complete responses (not token
// streams), we emit:
//
//  1. A role-only delta chunk (signals start of assistant turn to the client)
//  2. A content delta chunk containing the full response text — emitted
//     unconditionally (even when resp.Content is empty), matching this
//     handler's pre-lr-add405 contract
//  3. A tool_calls delta chunk carrying every resp.ToolUses entry in one
//     chunk (lr-add405; only when resp.ToolUses is non-empty) — see
//     chunkToolCallDelta's doc for why this is one chunk, not true
//     incremental deltas
//  4. A finish chunk with finish_reason ("tool_calls" when ToolUses is
//     non-empty, "stop" otherwise — chatCompletionFinishReason) and empty
//     delta
//
// Followed by the required "data: [DONE]" sentinel.
//
// This is compatible with the OpenAI Python SDK, openai-node, and any client
// that correctly implements the SSE streaming protocol.
func writeSSEStream(w http.ResponseWriter, model string, resp *backend.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	id := "chatcmpl-" + uuid.NewString()[:8]
	now := time.Now().Unix()
	finishReason := chatCompletionFinishReason(resp)

	chunks := []chatCompletionChunk{
		// chunk 1: role delta — signals start of assistant turn
		{
			ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
			Choices: []chunkChoice{{
				Index: 0, Delta: chunkDelta{Role: "assistant"}, FinishReason: nil,
			}},
		},
	}

	// Content delta chunk: emitted unconditionally, even for an empty
	// string, matching this handler's pre-lr-add405 contract (an OpenAI
	// client's stream parser expects a content delta chunk in the normal
	// text-completion case; only a tool_calls-only turn adds a distinct
	// tool_calls chunk on top, it does not replace this one).
	chunks = append(chunks, chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
		Choices: []chunkChoice{{
			Index: 0, Delta: chunkDelta{Content: resp.Content}, FinishReason: nil,
		}},
	})

	if len(resp.ToolUses) > 0 {
		toolCallDeltas := make([]chunkToolCallDelta, len(resp.ToolUses))
		for i, tu := range resp.ToolUses {
			toolCallDeltas[i] = chunkToolCallDelta{Index: i, ID: tu.ID, Type: "function"}
			toolCallDeltas[i].Function.Name = tu.Name
			toolCallDeltas[i].Function.Arguments = string(tu.Input)
		}
		chunks = append(chunks, chatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
			Choices: []chunkChoice{{
				Index: 0, Delta: chunkDelta{ToolCalls: toolCallDeltas}, FinishReason: nil,
			}},
		})
	}

	// finish chunk — empty delta, finish_reason set
	chunks = append(chunks, chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: now, Model: model,
		Choices: []chunkChoice{{
			Index: 0, Delta: chunkDelta{}, FinishReason: &finishReason,
		}},
	})

	for _, chunk := range chunks {
		data, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}
}

// --- response helpers ---

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		// LastErrorType is populated only on chain-exhaustion (503) responses.
		// It reuses the same type-only, already-redacted state.ErrorType/
		// backend.ErrorType enum GET /v1/models publishes as "last_error_type"
		// per backend (lr-807319) — never raw error text, never a backend id.
		LastErrorType string `json:"last_error_type,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var resp errorResponse
	resp.Error.Code = code
	resp.Error.Message = message
	writeJSON(w, status, resp)
}

// writeChainExhaustedError writes the 503 backends_unavailable response,
// additionally carrying lastErrorType when known (empty = omitted). See
// errorResponse.LastErrorType's doc for the redaction contract this reuses.
func writeChainExhaustedError(w http.ResponseWriter, lastErrorType string) {
	var resp errorResponse
	resp.Error.Code = "backends_unavailable"
	resp.Error.Message = "no available backends in chain"
	resp.Error.LastErrorType = lastErrorType
	writeJSON(w, http.StatusServiceUnavailable, resp)
}

func timeOrNull(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
