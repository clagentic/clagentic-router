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
// fields (metadata, top_k, etc.) are preserved via RawBody for the
// passthrough path, which forwards the original bytes unmodified.
type anthropicMsgRequest struct {
	Model     string                `json:"model"`
	Messages  []anthropicMsgMessage `json:"messages"`
	System    json.RawMessage       `json:"system,omitempty"`
	MaxTokens int                   `json:"max_tokens"`
	Stream    bool                  `json:"stream,omitempty"`
	// Tools is decoded only far enough to detect presence and, in routed
	// mode, into []anthropicMsgToolDef by decodeAnthropicTools (see
	// messagesRouted). Kept as json.RawMessage at THIS struct level
	// (rather than a typed slice) deliberately: this struct's json.Unmarshal
	// runs unconditionally for both passthrough and routed requests
	// (messages()), and passthrough must keep forwarding a malformed-per-our-
	// typed-shape (but validly-Anthropic) tools array unmodified rather than
	// 400ing on a decode error that only routed mode's stricter translation
	// needs to care about (lr-add405). Passthrough never decodes this field
	// at all — it forwards the original request bytes (including tools)
	// unmodified.
	Tools json.RawMessage `json:"tools,omitempty"`
	// WorkingDir is an opt-in absolute directory subprocess (CLI) adapters
	// use as their cmd.Dir, honored only in routed mode (messagesRouted).
	// Empty (the default) falls through to backend.DefaultWorkingDir with no
	// inference — never guessed from server-side state. Validated at the
	// routed-mode boundary via backend.ResolveWorkingDir. Passthrough mode
	// forwards the original request bytes unmodified and never reads this
	// field — it has no adapter, no subprocess, no cwd notion.
	WorkingDir string `json:"working_dir,omitempty"`
}

// anthropicMsgToolDef is the wire shape of one entry in the Anthropic
// Messages API "tools" array — same trio backend.ToolDef carries neutrally
// (name/description/input_schema), decoded here only far enough to
// translate into that neutral IR (toBackendToolDefs). Server-side tool
// types (Anthropic computer/text_editor) decode into this same shape
// (their name/description come from Anthropic's builtin catalog, not this
// request) but the router's translation does not special-case them — they
// are carried through as an ordinary ToolDef and rejected or accepted by
// the provider exactly as a client sending them directly would see.
type anthropicMsgToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// decodeAnthropicTools decodes req.Tools (json.RawMessage, presence-checked
// separately by hasTools) into []anthropicMsgToolDef for routed mode. Called
// only after hasTools(req.Tools) is true, so raw being empty/null/[] here
// would itself be a bug upstream — but this function still handles it
// gracefully (returns nil, nil) rather than assuming the caller's
// precondition holds.
func decodeAnthropicTools(raw json.RawMessage) ([]anthropicMsgToolDef, error) {
	if !hasTools(raw) {
		return nil, nil
	}
	var tools []anthropicMsgToolDef
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return tools, nil
}

// toBackendToolDefs translates decoded Anthropic tool definitions into the
// neutral backend.ToolDef IR. Returns nil for an empty input, matching the
// "absent means no tools" contract of backend.Request.Tools.
func toBackendToolDefs(tools []anthropicMsgToolDef) []backend.ToolDef {
	if len(tools) == 0 {
		return nil
	}
	out := make([]backend.ToolDef, len(tools))
	for i, t := range tools {
		out[i] = backend.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out
}

type anthropicMsgMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicMsgContentBlock is one element of a Messages API content array,
// covering "text", "tool_use", and "tool_result" block shapes. tool_use is
// the one this router actually produces in routed-mode responses and
// decodes on input (prior-turn history — see anthropicToBackendMessages);
// tool_result is decode-only, rendered as readable text on input since the
// router never executes a tool itself and has no InvokeError-equivalent
// carrier for "this was actually a tool result, not model prose" in the
// flat backend.Message.Content string single-shot carriage uses. Fields for
// a Type this block does not represent are omitted, matching the real
// Anthropic wire grammar.
type anthropicMsgContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use fields.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields (decode-only — this router never emits tool_result
	// blocks itself, only reads them out of caller-supplied history).
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
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

// toolUsesToAnthropicBlocks translates backend.ToolUse results into
// Anthropic "tool_use" content blocks for the routed-mode response.
func toolUsesToAnthropicBlocks(uses []backend.ToolUse) []anthropicMsgContentBlock {
	blocks := make([]anthropicMsgContentBlock, len(uses))
	for i, u := range uses {
		blocks[i] = anthropicMsgContentBlock{Type: "tool_use", ID: u.ID, Name: u.Name, Input: u.Input}
	}
	return blocks
}

// anthropicStopReason returns the Anthropic Messages API stop_reason for a
// routed-mode response: "tool_use" when the backend returned any tool_use
// blocks (lr-add405 — previously hardcoded to "end_turn" unconditionally,
// documented as a known limitation since tool calls were always dropped in
// translation), "end_turn" otherwise.
func anthropicStopReason(resp *backend.Response) string {
	if len(resp.ToolUses) > 0 {
		return "tool_use"
	}
	return "end_turn"
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

	reqHasTools := hasTools(req.Tools)
	var toolDefs []backend.ToolDef
	if reqHasTools {
		filtered, err := h.router.FilterChainForTools(chain)
		if err != nil {
			if err == router.ErrNoToolCapableBackend {
				// Refused before Route is ever called — Route's own call_log
				// writes never fire for this request, so record the refusal
				// explicitly (presence only; see LogToolRefusal doc).
				h.router.LogToolRefusal(r.Context(), chain, req.Model)
				writeAnthropicError(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("request carries tools but model %q resolves to no tool-capable backend in routed mode; "+
						"remove tools, or send this request to %s directly (passthrough forwards tools intact)",
						req.Model, "a non-role:/chain:/backend:-prefixed model"))
				return
			}
			writeAnthropicError(w, http.StatusBadRequest,
				fmt.Sprintf("model %q did not resolve to any configured backends", req.Model))
			return
		}
		chain = filtered

		anthropicTools, err := decodeAnthropicTools(req.Tools)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, err.Error())
			return
		}
		toolDefs = toBackendToolDefs(anthropicTools)
	}

	msgs, err := anthropicToBackendMessages(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}

	workingDir, err := backend.ResolveWorkingDir(req.WorkingDir)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}

	routerReq := &backend.Request{
		Messages:   msgs,
		MaxTokens:  req.MaxTokens,
		WorkingDir: workingDir,
		HasTools:   reqHasTools,
		Tools:      toolDefs,
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
		Content: anthropicResponseContentBlocks(resp),
		// tool_use when the backend returned tool_use blocks (lr-add405),
		// end_turn otherwise. max_tokens/stop_sequence are still never
		// surfaced — no adapter reports which stop condition it hit beyond
		// tool_use vs. a normal completion.
		StopReason: anthropicStopReason(resp),
		Usage: anthropicMsgUsage{
			InputTokens:  resp.PromptTokensEst,
			OutputTokens: resp.CompletionTokensEst,
		},
	})
}

// anthropicResponseContentBlocks builds the routed-mode response content
// array: a text block when resp.Content is non-empty, followed by any
// tool_use blocks (lr-add405). A tool_use-only turn (resp.Content=="")
// omits the text block entirely rather than emitting an empty one, matching
// real Anthropic API behavior.
func anthropicResponseContentBlocks(resp *backend.Response) []anthropicMsgContentBlock {
	blocks := make([]anthropicMsgContentBlock, 0, 1+len(resp.ToolUses))
	if resp.Content != "" {
		blocks = append(blocks, anthropicMsgContentBlock{Type: "text", Text: resp.Content})
	}
	blocks = append(blocks, toolUsesToAnthropicBlocks(resp.ToolUses)...)
	return blocks
}

// anthropicToBackendMessages converts Anthropic Messages request fields
// (system + messages, with content as either a plain string or a content
// block array) into backend.Message. "text" content blocks are kept as-is;
// tool_use/tool_result blocks are rendered as a readable text marker (see
// decodeAnthropicContent) rather than silently dropped (lr-add405) — a
// caller replaying prior-turn history that includes a tool call/result
// still gets that turn represented in the flattened prompt, even though the
// router itself never executes tools or accepts a fresh tool_result. image
// blocks remain out of scope and are still dropped — multimodal carriage is
// not part of this task.
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
// either a plain JSON string or an array of content blocks, into a single
// flattened text string (the shape every backend.Message.Content is).
//
//   - "text" blocks are appended verbatim.
//   - "tool_use" blocks are rendered as a "[Tool call: <name>(<input>)]"
//     marker (lr-add405) — the model's own prior tool call is real
//     conversational content a backend should see when replaying history,
//     even though this router cannot re-issue it as a structured block on
//     a text-only CLI backend or reconstruct the original response's exact
//     content-array shape.
//   - "tool_result" blocks are rendered as a "[Tool result for <id>:
//     <content>]" marker, same rationale.
//   - "image" blocks are still skipped entirely — multimodal carriage is
//     out of scope for this task (single-shot TOOL carriage only).
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
	appendPart := func(part string) {
		if part == "" {
			return
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(part)
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			appendPart(b.Text)
		case "tool_use":
			appendPart(fmt.Sprintf("[Tool call: %s(%s)]", b.Name, string(b.Input)))
		case "tool_result":
			appendPart(fmt.Sprintf("[Tool result for %s: %s]", b.ToolUseID, string(b.Content)))
			// image and any other block type are intentionally skipped.
		}
	}
	return sb.String(), nil
}

// --- Routed-mode SSE (Anthropic event grammar) ---

// writeAnthropicSSEStream emits a canned Anthropic Messages SSE stream for a
// complete (non-token-streamed) backend response, mirroring the OpenAI
// handler's writeSSEStream approach but in Anthropic event grammar:
// message_start -> content_block_start -> content_block_delta ->
// content_block_stop (repeated per content block) -> message_delta ->
// message_stop.
//
// Like the OpenAI streaming path, this is NOT true token streaming — backends
// return complete responses, so each block's full content is emitted in one
// delta event. This is the documented routed-mode limitation.
//
// Content blocks (lr-add405): a text block (if resp.Content is non-empty) at
// index 0, followed by one tool_use block per resp.ToolUses at incrementing
// indices — same block set and ordering as the non-streaming response
// (anthropicResponseContentBlocks), just each one framed as its own
// start/delta/stop event triplet instead of a single JSON array. A tool_use
// block's delta uses Anthropic's "input_json_delta" shape (partial_json is
// the JSON-encoded input as one string chunk, matching the "NOT true token
// streaming" constraint above — the whole input arrives in one delta).
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

	index := 0
	if resp.Content != "" {
		writeEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{"type": "text_delta", "text": resp.Content},
		})
		writeEvent("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": index,
		})
		index++
	}

	for _, tu := range resp.ToolUses {
		writeEvent("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]interface{}{
				"type": "tool_use", "id": tu.ID, "name": tu.Name, "input": map[string]interface{}{},
			},
		})
		writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(tu.Input)},
		})
		writeEvent("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": index,
		})
		index++
	}

	writeEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": anthropicStopReason(resp), "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": resp.CompletionTokensEst},
	})

	writeEvent("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}
