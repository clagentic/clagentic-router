// internal/backend/anthropic_api.go — adapter for the Anthropic Messages API.
//
// Uses the Anthropic API directly (ANTHROPIC_API_KEY or config api_key).
// This is an alternative to claude_cli for environments where an API key
// is available (e.g. CI, containers without OAuth).
//
// API reference: https://docs.anthropic.com/en/api/messages
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const anthropicDefaultURL = "https://api.anthropic.com"
const anthropicAPIVersion = "2023-06-01"

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicOutputConfig carries the output_config object (Opus 4.8+).
// Populated only when Effort is configured; omitted entirely otherwise.
type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// anthropicThinking carries the thinking object (Opus 4.7+/4.8+).
// Populated only when ThinkingMode == "adaptive"; omitted entirely otherwise.
// Display defaults to "omitted": thinking tokens are consumed regardless of display
// setting; omitting display keeps the response lean and avoids parsing overhead.
type anthropicThinking struct {
	Type    string `json:"type"`
	Display string `json:"display,omitempty"`
}

type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       string                 `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	// Tools carries backend.ToolDef translated to the Anthropic Messages API
	// envelope (input_schema). Omitted entirely when the request carries no
	// tools — matches the neutral IR contract in adapter.go.
	Tools []anthropicToolDef `json:"tools,omitempty"`
}

// anthropicToolDef is the Anthropic Messages API's tool definition envelope
// — same name/description/schema trio as backend.ToolDef, wire field named
// "input_schema" per the Anthropic API reference.
type anthropicToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// toAnthropicTools translates the neutral backend.ToolDef slice into the
// Anthropic wire envelope. Returns nil for an empty input so the "tools"
// field is omitted entirely (omitempty) rather than sent as [].
func toAnthropicTools(defs []ToolDef) []anthropicToolDef {
	if len(defs) == 0 {
		return nil
	}
	out := make([]anthropicToolDef, len(defs))
	for i, d := range defs {
		out[i] = anthropicToolDef{Name: d.Name, Description: d.Description, InputSchema: d.InputSchema}
	}
	return out
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use fields — present only when Type == "tool_use".
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
	// Error fields
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AnthropicAPIAdapter calls the Anthropic Messages API with an API key.
type AnthropicAPIAdapter struct {
	id           string
	model        string
	apiKey       string
	apiURL       string
	effort       EffortLevel
	thinkingMode ThinkingMode
	client       *http.Client
}

// NewAnthropicAPIAdapter creates a new adapter.
// apiKey is the Anthropic API key (already resolved from env: references by config).
// apiURL is the base URL; empty defaults to https://api.anthropic.com.
// effort and thinkingMode are passed from BackendConfig fields; empty values are safe
// (they cause the corresponding fields to be omitted from wire requests).
func NewAnthropicAPIAdapter(id, model, apiKey, apiURL string, timeout time.Duration, effort EffortLevel, thinkingMode ThinkingMode) *AnthropicAPIAdapter {
	if apiURL == "" {
		apiURL = anthropicDefaultURL
	}
	return &AnthropicAPIAdapter{
		id:           id,
		model:        model,
		apiKey:       apiKey,
		apiURL:       strings.TrimRight(apiURL, "/"),
		effort:       effort,
		thinkingMode: thinkingMode,
		client:       &http.Client{Timeout: timeout},
	}
}

func (a *AnthropicAPIAdapter) ID() string { return a.id }

// Capabilities reports the Anthropic Messages API adapter's wire protocol
// support. Invoke marshals req.Tools into the Anthropic "tools" field
// (toAnthropicTools) and parses "tool_use" content blocks back out of the
// response (see Invoke below), so SupportsTools is true. Images remain
// unsupported: anthropicMessage.Content is still a plain string, so no
// multimodal content block is ever marshaled or parsed.
func (a *AnthropicAPIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: true, SupportsStreaming: false, SupportsImages: false}
}

// Invoke sends a request to the Anthropic Messages API.
func (a *AnthropicAPIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	if a.apiKey == "" {
		return nil, &InvokeError{Type: ErrTypeAuth, Raw: "anthropic_api: no api_key configured"}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Extract system message and build messages array (no system role in Anthropic API)
	var system string
	var msgs []anthropicMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: m.Role, Content: m.Content})
	}
	if len(msgs) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "no user messages after filtering"}
	}

	body := anthropicRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  msgs,
		Tools:     toAnthropicTools(req.Tools),
	}
	if a.effort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: string(a.effort)}
	}
	if a.thinkingMode == ThinkingAdaptive {
		body.Thinking = &anthropicThinking{Type: "adaptive", Display: "omitted"}
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("marshal: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeNetwork, Raw: fmt.Sprintf("build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		errType := ErrTypeNetwork
		if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "timeout") {
			errType = ErrTypeTimeout
		}
		return nil, &InvokeError{Type: errType, Raw: truncate(err.Error(), 500)}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		errType := httpStatusToErrorType(resp.StatusCode, string(respBody))
		return nil, &InvokeError{
			Type: errType,
			Raw:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300)),
		}
	}

	var out anthropicResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("parse response: %v", err)}
	}

	if out.Error.Message != "" {
		return nil, &InvokeError{
			Type: ClassifyError(out.Error.Type+": "+out.Error.Message, 0),
			Raw:  truncate(out.Error.Message, 500),
		}
	}

	var content strings.Builder
	var toolUses []ToolUse
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			toolUses = append(toolUses, ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	text := strings.TrimSpace(content.String())
	// A tool_use-only turn legitimately carries no text block (out.StopReason
	// == "tool_use") — only treat empty text as a schema error when there is
	// also no tool_use to report, matching the "no usable content at all"
	// case this check exists to catch.
	if text == "" && len(toolUses) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty content blocks in Anthropic response"}
	}

	slog.Debug("anthropic_api invoke ok", "backend", a.id, "model", a.model, "content_len", len(text),
		"tool_uses", len(toolUses), "stop_reason", out.StopReason, "request_id", RequestIDFromCtx(ctx))

	rl := RateLimitInfo{
		TokensRemaining:   parseIntHeader(resp.Header, "anthropic-ratelimit-tokens-remaining"),
		TokensResetAt:     parseRFC3339Header(resp.Header, "anthropic-ratelimit-tokens-reset"),
		RequestsRemaining: parseIntHeader(resp.Header, "anthropic-ratelimit-requests-remaining"),
		RequestsResetAt:   parseRFC3339Header(resp.Header, "anthropic-ratelimit-requests-reset"),
	}

	return &Response{
		Content:             text,
		PromptTokensEst:     out.Usage.InputTokens,
		CompletionTokensEst: out.Usage.OutputTokens,
		RateLimitInfo:       rl,
		ToolUses:            toolUses,
	}, nil
}

// httpStatusToErrorType maps HTTP status codes to ErrorType.
func httpStatusToErrorType(status int, body string) ErrorType {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrTypeAuth
	case http.StatusTooManyRequests:
		if strings.Contains(strings.ToLower(body), "credit") ||
			strings.Contains(strings.ToLower(body), "quota") {
			return ErrTypeQuota
		}
		return ErrTypeRateLimit
	case 529:
		return ErrTypeRateLimit
	case http.StatusServiceUnavailable:
		return ErrTypeNetwork
	case http.StatusPaymentRequired:
		return ErrTypeQuota
	}
	// 400 bad request — e.g. invalid field for this model (wrong thinking.type);
	// ErrTypeUnknown is correct here.
	return ErrTypeUnknown
}
