// internal/server/bedrock_invoke.go — POST /model/{modelId}/invoke and
// POST /model/{modelId}/invoke-with-response-stream: AWS Bedrock Runtime
// InvokeModel wire shape.
//
// CLAUDE_CODE_USE_BEDROCK=1 makes Claude Code speak this shape instead of the
// Anthropic Messages API — ANTHROPIC_BEDROCK_BASE_URL redirects that traffic
// here. Unlike POST /v1/messages, the model identifier is carried entirely
// by the URL path segment ({modelId}), never in the request body. That path
// segment is the routing key for both modes below:
//
//  1. Passthrough (default, any modelId NOT prefixed role:/chain:/backend:):
//     SigV4-signed reverse proxy to the real AWS Bedrock Runtime endpoint for
//     the configured region, mirroring messagesPassthrough's shape but
//     targeting bedrock-runtime.<region>.amazonaws.com instead of
//     api.anthropic.com and substituting SigV4 signing for a forwarded
//     bearer/x-api-key credential (Bedrock does not accept either).
//
//  2. Routed (role:*/chain:*/backend: modelId): translated to the internal
//     backend.Request, sent through the existing router chain machinery, and
//     translated back into the Bedrock InvokeModel response envelope — which,
//     per the AWS Bedrock Anthropic-model contract, is byte-identical in
//     shape to the direct Anthropic Messages response (no extra Bedrock
//     wrapper). Streaming uses AWS event-stream framing (see eventstream.go),
//     a third response scheme distinct from both plain SSE and the Anthropic
//     Messages SSE grammar messagesRouted already produces.
//
// TRADE-OFF: the passthrough path is implemented against the documented wire
// format (captured via a throwaway listener, no live Bedrock account
// available at authoring time) and is NOT end-to-end verified against real
// AWS Bedrock. Framing (path extraction, request/response translation,
// event-stream encode/decode) is unit-tested deterministically; the SigV4
// signing call shape matches the AWS SDK v4.Signer API exactly, but the
// actual signed-request-accepted-by-AWS claim is unverified. See PR body.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/router"
)

// --- Bedrock InvokeModel wire types (request/response subset) ---

// bedrockInvokeRequest is decoded just enough to translate routed-mode
// requests into backend.Request. Unlike anthropicMsgRequest there is no
// Model field — the model comes exclusively from the URL path. Passthrough
// forwards rawBody unmodified, so unknown fields (thinking, anthropic_beta,
// metadata, etc.) never need explicit representation here.
type bedrockInvokeRequest struct {
	AnthropicVersion string                `json:"anthropic_version"`
	Messages         []anthropicMsgMessage `json:"messages"`
	System           json.RawMessage       `json:"system,omitempty"`
	MaxTokens        int                   `json:"max_tokens"`
	// Tools is decoded only far enough to detect presence — routed mode
	// (bedrockRouted) does not translate tool definitions to the backend
	// (see anthropicToBackendMessages, reused from messages.go). A
	// tools-bearing request must be refused when the resolved chain has no
	// tool-capable backend rather than silently dropped; see bedrockRouted's
	// tool-capability check. Passthrough mode is unaffected: it forwards
	// the original request bytes (including tools) unmodified, never
	// decoding this field.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// bedrockErrorEnvelope is the Bedrock Runtime error response shape — distinct
// from anthropicMsgError (Bedrock uses a bare "message" field, not the
// Anthropic {"type":"error","error":{...}} envelope).
type bedrockErrorEnvelope struct {
	Message string `json:"message"`
}

// writeBedrockError writes a Bedrock-Runtime-format error response.
func writeBedrockError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, bedrockErrorEnvelope{Message: message})
}

// bedrockInvoke handles POST /model/{modelId}/invoke.
func (h *Handler) bedrockInvoke(w http.ResponseWriter, r *http.Request) {
	h.bedrockDispatch(w, r, false)
}

// bedrockInvokeStream handles POST /model/{modelId}/invoke-with-response-stream.
func (h *Handler) bedrockInvokeStream(w http.ResponseWriter, r *http.Request) {
	h.bedrockDispatch(w, r, true)
}

// bedrockDispatch reads the request body, extracts {modelId} from the path,
// and dispatches to passthrough or routed mode based on isRoutedModel —
// mirroring the messages() split (server.go / messages.go).
func (h *Handler) bedrockDispatch(w http.ResponseWriter, r *http.Request, stream bool) {
	// Same defensive cap as /v1/messages; Bedrock bodies carry the same
	// tool-definition/system-prompt payloads.
	const maxBodyBytes = 8 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	modelID := r.PathValue("modelId")
	if modelID == "" {
		writeBedrockError(w, http.StatusBadRequest, "modelId path segment is required")
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeBedrockError(w, http.StatusBadRequest, fmt.Sprintf("read body: %v", err))
		return
	}

	if isRoutedModel(modelID) {
		var req bedrockInvokeRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			writeBedrockError(w, http.StatusBadRequest, fmt.Sprintf("decode body: %v", err))
			return
		}
		h.bedrockRouted(w, r, modelID, &req, stream)
		return
	}
	h.bedrockPassthrough(w, r, modelID, rawBody, stream)
}

// --- Routed mode ---

// bedrockRouted translates the Bedrock InvokeModel request into a
// backend.Request, routes it through the existing chain machinery, and
// translates the response back into the Bedrock InvokeModel response
// envelope (or AWS event-stream frames, for the streaming variant).
func (h *Handler) bedrockRouted(w http.ResponseWriter, r *http.Request, modelID string, req *bedrockInvokeRequest, stream bool) {
	if !h.anthropicTokenPresented(r) {
		writeBedrockError(w, http.StatusUnauthorized, "invalid or missing x-api-key/bearer token")
		return
	}

	chain := h.router.ResolveModel(modelID)
	if len(chain) == 0 {
		writeBedrockError(w, http.StatusBadRequest,
			fmt.Sprintf("model %q did not resolve to any configured backends", modelID))
		return
	}
	if len(req.Messages) == 0 {
		writeBedrockError(w, http.StatusBadRequest, "messages is required")
		return
	}

	if hasTools(req.Tools) {
		filtered, err := h.router.FilterChainForTools(chain)
		if err != nil {
			if err == router.ErrNoToolCapableBackend {
				writeBedrockError(w, http.StatusUnprocessableEntity,
					fmt.Sprintf("request carries tools but model %q resolves to no tool-capable backend in routed mode; "+
						"remove tools, or send this request to a real Bedrock model/inference-profile ID directly (passthrough forwards tools intact)",
						modelID))
				return
			}
			writeBedrockError(w, http.StatusBadRequest,
				fmt.Sprintf("model %q did not resolve to any configured backends", modelID))
			return
		}
		chain = filtered
	}

	// Reuse anthropicToBackendMessages (messages.go) — the Bedrock and direct
	// Anthropic Messages request shapes share the same messages/system content
	// grammar (string or content-block array), only the envelope differs.
	msgs, err := anthropicToBackendMessages(&anthropicMsgRequest{
		Messages: req.Messages,
		System:   req.System,
	})
	if err != nil {
		writeBedrockError(w, http.StatusBadRequest, err.Error())
		return
	}

	routerReq := &backend.Request{
		Messages:  msgs,
		MaxTokens: req.MaxTokens,
	}

	resp, meta, err := h.router.Route(r.Context(), routerReq, chain)
	if err != nil {
		if err == router.ErrAllFailed || err == router.ErrNoChain {
			writeBedrockError(w, http.StatusServiceUnavailable, "no available backends in chain")
			return
		}
		slog.Error("bedrock invoke: routed backend error", "err", err, "request_id", RequestID(r.Context()))
		writeBedrockError(w, http.StatusBadGateway, "upstream backend failed")
		return
	}

	w.Header().Set("X-Router-Mode", "routed")
	w.Header().Set("X-Router-Backend", meta.BackendID)
	if meta.FallbackReason != "" {
		w.Header().Set("X-Router-Fallback-Reason", meta.FallbackReason)
	}

	if stream {
		if err := writeBedrockEventStream(w, modelID, resp); err != nil {
			slog.Error("bedrock invoke: eventstream write failed", "err", err, "request_id", RequestID(r.Context()))
		}
		return
	}

	// The Bedrock InvokeModel response body for Anthropic models is the same
	// shape as the direct Anthropic Messages response — no Bedrock-specific
	// wrapper — so anthropicMsgResponse (messages.go) is reused as-is.
	writeJSON(w, http.StatusOK, anthropicMsgResponse{
		ID:      "msg_" + uuid.NewString()[:24],
		Type:    "message",
		Role:    "assistant",
		Model:   modelID,
		Content: []anthropicMsgContentBlock{{Type: "text", Text: resp.Content}},
		// Same documented limitation as messagesRouted: backends never surface
		// a distinct stop reason, since tool calls are dropped in translation.
		StopReason: "end_turn",
		Usage: anthropicMsgUsage{
			InputTokens:  resp.PromptTokensEst,
			OutputTokens: resp.CompletionTokensEst,
		},
	})
}

// --- Passthrough mode ---

// bedrockHTTPClient returns the HTTP client used for upstream Bedrock
// passthrough calls, mirroring anthropicHTTPClient's long timeout rationale.
func (h *Handler) bedrockHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

// bedrockPassthrough forwards the request to the real AWS Bedrock Runtime
// endpoint for the configured region, SigV4-signing it with credentials from
// the standard AWS SDK credential chain (same chain backend.NewBedrockAPIAdapter
// uses for the outbound Converse-API direction — see bedrock_api.go). The
// response (including AWS event-stream framing for the streaming variant) is
// forwarded back byte-for-byte, unmodified, since it is already in the exact
// wire shape the client expects.
func (h *Handler) bedrockPassthrough(w http.ResponseWriter, r *http.Request, modelID string, rawBody []byte, stream bool) {
	if h.bedrockRegion == "" {
		writeBedrockError(w, http.StatusServiceUnavailable,
			"bedrock passthrough is not configured (bedrock.region is unset) — only role:/chain:/backend: model IDs are routable")
		return
	}

	credsFn := h.bedrockCredentialsFn
	if credsFn == nil {
		credsFn = h.resolveBedrockCredentials
	}
	creds, err := credsFn(r.Context())
	if err != nil {
		slog.Error("bedrock invoke: credential resolution failed", "err", err, "request_id", RequestID(r.Context()))
		writeBedrockError(w, http.StatusBadGateway, "failed to resolve AWS credentials for passthrough")
		return
	}

	action := "invoke"
	if stream {
		action = "invoke-with-response-stream"
	}
	base := h.bedrockUpstreamBaseURL
	if base == "" {
		base = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", h.bedrockRegion)
	}
	upstreamURL := fmt.Sprintf("%s/model/%s/%s", base, modelID, action)

	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(rawBody))
	if err != nil {
		writeBedrockError(w, http.StatusBadGateway, fmt.Sprintf("build upstream request: %v", err))
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	if v := r.Header.Get("Accept"); v != "" {
		upReq.Header.Set("Accept", v)
	}
	// anthropic-version/anthropic-beta travel in the body for Bedrock (not as
	// headers, unlike the direct Anthropic API) — forwarding rawBody unmodified
	// already carries them; no header translation needed here.

	sum := sha256.Sum256(rawBody)
	payloadHash := hex.EncodeToString(sum[:])
	signer := v4.NewSigner()
	if err := signer.SignHTTP(r.Context(), creds, upReq, payloadHash, "bedrock", h.bedrockRegion, time.Now()); err != nil {
		slog.Error("bedrock invoke: SigV4 signing failed", "err", err, "request_id", RequestID(r.Context()))
		writeBedrockError(w, http.StatusBadGateway, "failed to sign upstream request")
		return
	}

	upResp, err := h.bedrockHTTPClient().Do(upReq)
	if err != nil {
		slog.Error("bedrock invoke: passthrough upstream error", "err", err, "request_id", RequestID(r.Context()))
		writeBedrockError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer upResp.Body.Close()

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
				slog.Warn("bedrock invoke: passthrough stream read error", "err", rerr, "request_id", RequestID(r.Context()))
			}
			return
		}
	}
}

// resolveBedrockCredentials loads AWS credentials via the standard SDK chain
// (env -> web identity -> shared creds -> shared config -> ECS -> IMDS),
// honoring h.bedrockProfile when set. Extracted as its own method so it can
// be exercised independently of a live HTTP round trip in tests that stub
// credential resolution.
func (h *Handler) resolveBedrockCredentials(ctx context.Context) (aws.Credentials, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(h.bedrockRegion),
	}
	if h.bedrockProfile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(h.bedrockProfile))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("load AWS config: %w", err)
	}
	return awsCfg.Credentials.Retrieve(ctx)
}
