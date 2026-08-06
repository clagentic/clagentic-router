// internal/server/messages.go — POST /v1/messages: Anthropic Messages API surface.
//
// ANTHROPIC_BASE_URL is process-global in Claude Code — pointing it at the
// router routes the WHOLE session (orchestrator included) through this
// endpoint. It therefore has two modes, keyed on the request's "model" field:
//
//  1. Passthrough (default, any model NOT prefixed role:/chain:/backend:):
//     transparent reverse proxy to a configurable upstream (default
//     https://api.anthropic.com). Request body and response (including
//     streaming SSE) are forwarded byte-for-byte — tools, prompt caching,
//     and multimodal content pass through untouched. This is what keeps an
//     interactive Claude Code session fully functional while pointed here.
//
//  2. Routed (role:*/chain:*/backend:* models): translated to the internal
//     backend.Request, sent through the existing router chain machinery,
//     and translated back to Anthropic Messages response/SSE shape. This
//     path is lossy — see messagesLimitation in README — and is intended
//     for one-shot review/audit roles, not tool-using builders.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/router"
)

// --- Anthropic Messages API wire types (request/response subset) ---

// anthropicMsgRequest is decoded just enough to make the passthrough-vs-routed
// decision and, for routed mode, to translate into backend.Request. Unknown
// fields (tools, metadata, top_k, etc.) are preserved via RawBody for the
// passthrough path, which forwards the original bytes unmodified.
type anthropicMsgRequest struct {
	Model     string                `json:"model"`
	Messages  []anthropicMsgMessage `json:"messages"`
	System    json.RawMessage       `json:"system,omitempty"`
	MaxTokens int                   `json:"max_tokens"`
	Stream    bool                  `json:"stream,omitempty"`
}

type anthropicMsgMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicMsgContentBlock is one element of a Messages API content array.
type anthropicMsgContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicMsgUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthropicMsgResponse is the routed-mode response shape.
type anthropicMsgResponse struct {
	ID           string                     `json:"id"`
	Type         string                     `json:"type"`
	Role         string                     `json:"role"`
	Model        string                     `json:"model"`
	Content      []anthropicMsgContentBlock `json:"content"`
	StopReason   string                     `json:"stop_reason"`
	StopSequence *string                    `json:"stop_sequence"`
	Usage        anthropicMsgUsage          `json:"usage"`
}

// anthropicMsgError is the Anthropic-format error envelope.
type anthropicMsgError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicErrorTypeForStatus maps an HTTP status to the Anthropic API's
// error.type taxonomy so routed-mode errors look like real Anthropic errors
// to any client (Claude Code included) that inspects the error type.
func anthropicErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError writes a well-formed Anthropic-format error response.
func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	var resp anthropicMsgError
	resp.Type = "error"
	resp.Error.Type = anthropicErrorTypeForStatus(status)
	resp.Error.Message = message
	writeJSON(w, status, resp)
}

// isRoutedModel reports whether model uses router chain-selection syntax
// (role:/chain:/backend:) rather than a plain upstream model name.
func isRoutedModel(model string) bool {
	return strings.HasPrefix(model, "role:") ||
		strings.HasPrefix(model, "chain:") ||
		strings.HasPrefix(model, "backend:")
}

// messages handles POST /v1/messages.
//
// Auth matrix (deliberately mode-dependent — see task design notes):
//   - Routed mode (role:/chain:/backend: models): requires the router's own
//     inference token via x-api-key OR Authorization: Bearer, checked in
//     messagesRouted. This is a real router-owned invocation (metering,
//     chain selection) and must be gated like /v1/chat/completions.
//   - Passthrough mode (any other model): the router does NOT check its own
//     token here. The security boundary is the client's own Anthropic
//     credential, which travels through to the upstream Anthropic API
//     unchanged (or is substituted by upstream_api_key when configured) —
//     Anthropic authenticates the call, not the router. Gating passthrough
//     on the router's token as well would break the "point ANTHROPIC_BASE_URL
//     at the router and the session just works" requirement for any client
//     that only knows its Anthropic key, not a separate router token.
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	// Same defensive cap as /v1/chat/completions; passthrough bodies can carry
	// large tool definitions/results so this is generous headroom, not a tight fit.
	const maxBodyBytes = 8 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
		return
	}

	var req anthropicMsgRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required")
		return
	}

	if isRoutedModel(req.Model) {
		h.messagesRouted(w, r, &req)
		return
	}
	h.messagesPassthrough(w, r, rawBody)
}

// --- Passthrough mode ---

// messagesPassthrough forwards the request to the configured upstream
// unmodified, streaming the response (including SSE) back byte-for-byte.
func (h *Handler) messagesPassthrough(w http.ResponseWriter, r *http.Request, rawBody []byte) {
	upstreamURL := h.anthropicUpstreamURL + "/v1/messages"

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(rawBody))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err))
		return
	}

	// Forward content-type/anthropic-* headers (anthropic-version, anthropic-beta, etc.)
	// verbatim — the router does not know the full set of client-negotiated features
	// and must not silently drop them.
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "anthropic-") {
			for _, v := range vals {
				upReq.Header.Add(k, v)
			}
		}
	}
	if upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	// Auth: forward the client's own credential by default. When
	// upstream_api_key is configured, substitute it instead — the router
	// then owns the upstream credential and clients need only the router's
	// own inference token to pass the inbound auth() check.
	if h.anthropicUpstreamAPIKey != "" {
		upReq.Header.Set("x-api-key", h.anthropicUpstreamAPIKey)
		upReq.Header.Del("Authorization")
	} else {
		if v := r.Header.Get("x-api-key"); v != "" {
			upReq.Header.Set("x-api-key", v)
		}
		if v := r.Header.Get("Authorization"); v != "" {
			upReq.Header.Set("Authorization", v)
		}
	}

	upResp, err := h.anthropicHTTPClient().Do(upReq)
	if err != nil {
		slog.Error("messages: passthrough upstream error", "err", err, "request_id", RequestID(r.Context()))
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer upResp.Body.Close()

	// Mirror upstream headers (content-type, anthropic-*, rate-limit headers) so
	// a client inspecting response headers sees the same thing it would from
	// api.anthropic.com directly.
	for k, vals := range upResp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Router-Mode", "passthrough")
	w.WriteHeader(upResp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := upResp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				slog.Warn("messages: passthrough stream read error", "err", rerr, "request_id", RequestID(r.Context()))
			}
			return
		}
	}
}

// anthropicHTTPClient returns the HTTP client used for upstream passthrough
// calls. A package-level default is used since passthrough has no per-backend
// timeout configuration — long streaming responses are expected.
func (h *Handler) anthropicHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

// --- Routed mode ---

// messagesRouted translates the Anthropic Messages request into a
// backend.Request, routes it through the existing chain machinery, and
// translates the response back to Anthropic Messages format.
func (h *Handler) messagesRouted(w http.ResponseWriter, r *http.Request, req *anthropicMsgRequest) {
	if !h.anthropicTokenPresented(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "invalid or missing x-api-key/bearer token")
		return
	}

	chain := h.router.ResolveModel(req.Model)
	if len(chain) == 0 {
		writeAnthropicError(w, http.StatusBadRequest,
			fmt.Sprintf("model %q did not resolve to any configured backends", req.Model))
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages is required")
		return
	}

	msgs, err := anthropicToBackendMessages(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}

	routerReq := &backend.Request{
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
	}

	resp, meta, err := h.router.Route(r.Context(), routerReq, chain)
	if err != nil {
		if err == router.ErrAllFailed || err == router.ErrNoChain {
			writeAnthropicError(w, http.StatusServiceUnavailable, "no available backends in chain")
			return
		}
		slog.Error("messages: routed backend error", "err", err, "request_id", RequestID(r.Context()))
		writeAnthropicError(w, http.StatusBadGateway, "upstream backend failed")
		return
	}

	w.Header().Set("X-Router-Mode", "routed")
	w.Header().Set("X-Router-Backend", meta.BackendID)
	if meta.FallbackReason != "" {
		w.Header().Set("X-Router-Fallback-Reason", meta.FallbackReason)
	}

	if req.Stream {
		writeAnthropicSSEStream(w, req.Model, resp)
		return
	}

	writeJSON(w, http.StatusOK, anthropicMsgResponse{
		ID:      "msg_" + uuid.NewString()[:24],
		Type:    "message",
		Role:    "assistant",
		Model:   req.Model,
		Content: []anthropicMsgContentBlock{{Type: "text", Text: resp.Content}},
		// end_turn is the general-purpose Anthropic stop_reason for a normal
		// completion; the router's backends never surface a distinct reason
		// (tool_use, max_tokens, stop_sequence) since tool calls are dropped
		// in translation — documented as a known limitation.
		StopReason: "end_turn",
		Usage: anthropicMsgUsage{
			InputTokens:  resp.PromptTokensEst,
			OutputTokens: resp.CompletionTokensEst,
		},
	})
}

// anthropicToBackendMessages converts Anthropic Messages request fields
// (system + messages, with content as either a plain string or a content
// block array) into backend.Message. Only "text" content blocks are kept —
// tool_use/tool_result/image blocks are dropped, which is the documented
// routed-mode limitation (no tool-calling, no multimodal).
func anthropicToBackendMessages(req *anthropicMsgRequest) ([]backend.Message, error) {
	var out []backend.Message

	if len(req.System) > 0 {
		sysText, err := decodeAnthropicContent(req.System)
		if err != nil {
			return nil, fmt.Errorf("system: %w", err)
		}
		if sysText != "" {
			out = append(out, backend.Message{Role: "system", Content: sysText})
		}
	}

	for i, m := range req.Messages {
		text, err := decodeAnthropicContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, backend.Message{Role: m.Role, Content: text})
	}
	return out, nil
}

// decodeAnthropicContent decodes an Anthropic "content" field, which is
// either a plain JSON string or an array of content blocks. Non-text blocks
// (tool_use, tool_result, image, etc.) are skipped.
func decodeAnthropicContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	// Fall back to content block array.
	var blocks []anthropicMsgContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("content: expected string or block array: %w", err)
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), nil
}

// --- Routed-mode SSE (Anthropic event grammar) ---

// writeAnthropicSSEStream emits a canned Anthropic Messages SSE stream for a
// complete (non-token-streamed) backend response, mirroring the OpenAI
// handler's writeSSEStream approach but in Anthropic event grammar:
// message_start -> content_block_start -> content_block_delta ->
// content_block_stop -> message_delta -> message_stop.
//
// Like the OpenAI streaming path, this is NOT true token streaming — backends
// return complete responses, so the full text is emitted in one delta event.
// This is the documented routed-mode limitation.
func writeAnthropicSSEStream(w http.ResponseWriter, model string, resp *backend.Response) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	id := "msg_" + uuid.NewString()[:24]

	writeEvent := func(event string, data interface{}) {
		payload, err := json.Marshal(data)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		if canFlush {
			flusher.Flush()
		}
	}

	writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": resp.PromptTokensEst, "output_tokens": 0},
		},
	})

	writeEvent("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})

	writeEvent("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": resp.Content},
	})

	writeEvent("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})

	writeEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": resp.CompletionTokensEst},
	})

	writeEvent("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}
