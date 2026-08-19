// internal/backend/openai_api.go — adapter for the OpenAI Chat Completions API.
//
// Uses the OpenAI API directly (OPENAI_API_KEY or config api_key).
// This is an alternative to codex_cli for environments where an API key
// is available (e.g. CI, containers without OAuth).
//
// API reference: https://platform.openai.com/docs/api-reference/chat
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

const openaiDefaultURL = "https://api.openai.com"

type openaiMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []openaiMessage  `json:"messages"`
	Tools     []openaiToolSpec `json:"tools,omitempty"`
}

// openaiToolSpec is the OpenAI Chat Completions "tools" entry envelope —
// same name/description/schema trio as backend.ToolDef, wrapped in a
// {"type":"function","function":{...}} wire shape per the OpenAI API
// reference (function.parameters carries the JSON Schema).
type openaiToolSpec struct {
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// openaiToolCall is one entry of message.tool_calls in a Chat Completions
// response — the OpenAI wire shape for a model-requested tool invocation.
type openaiToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openaiToolCallFunction `json:"function"`
}

type openaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toOpenAITools translates the neutral backend.ToolDef slice into the OpenAI
// wire envelope. Returns nil for an empty input so "tools" is omitted
// entirely (omitempty) rather than sent as [].
func toOpenAITools(defs []ToolDef) []openaiToolSpec {
	if len(defs) == 0 {
		return nil
	}
	out := make([]openaiToolSpec, len(defs))
	for i, d := range defs {
		out[i] = openaiToolSpec{
			Type: "function",
			Function: openaiToolFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.InputSchema,
			},
		}
	}
	return out
}

type openaiChoice struct {
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openaiResponse struct {
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
	// Error fields (returned in 4xx/5xx bodies)
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

// OpenAIAPIAdapter calls the OpenAI Chat Completions API with an API key.
type OpenAIAPIAdapter struct {
	id     string
	model  string
	apiKey string
	apiURL string
	client *http.Client
}

// NewOpenAIAPIAdapter creates a new adapter.
// apiKey is the OpenAI API key (already resolved from env: references by config).
// apiURL is the base URL; empty defaults to https://api.openai.com.
func NewOpenAIAPIAdapter(id, model, apiKey, apiURL string, timeout time.Duration) *OpenAIAPIAdapter {
	if apiURL == "" {
		apiURL = openaiDefaultURL
	}
	return &OpenAIAPIAdapter{
		id:     id,
		model:  model,
		apiKey: apiKey,
		apiURL: strings.TrimRight(apiURL, "/"),
		client: &http.Client{Timeout: timeout},
	}
}

func (a *OpenAIAPIAdapter) ID() string { return a.id }

// Capabilities reports the OpenAI Chat Completions API adapter's wire
// protocol support. Invoke marshals req.Tools into the OpenAI "tools" field
// (toOpenAITools) and parses message.tool_calls back out of the first
// choice (see Invoke below), so SupportsTools is true. Images remain
// unsupported: openaiMessage.Content is still a plain string, so no vision
// content part is ever marshaled or parsed.
func (a *OpenAIAPIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: true, SupportsStreaming: false, SupportsImages: false}
}

// Invoke sends a request to the OpenAI Chat Completions API.
func (a *OpenAIAPIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	if a.apiKey == "" {
		return nil, &InvokeError{Type: ErrTypeAuth, Raw: "openai_api: no api_key configured"}
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Pass all messages through — OpenAI supports system role directly in messages array.
	msgs := make([]openaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, openaiMessage{Role: m.Role, Content: m.Content})
	}
	if len(msgs) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "openai_api: no messages provided"}
	}

	body := openaiRequest{
		Model:     a.model,
		MaxTokens: maxTokens,
		Messages:  msgs,
		Tools:     toOpenAITools(req.Tools),
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("marshal: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeNetwork, Raw: fmt.Sprintf("build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

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

	var out openaiResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("parse response: %v", err)}
	}

	if out.Error.Message != "" {
		return nil, &InvokeError{
			Type: ClassifyError(out.Error.Type+": "+out.Error.Message, 0),
			Raw:  truncate(out.Error.Message, 500),
		}
	}

	if len(out.Choices) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "openai_api: empty choices in response"}
	}

	msg := out.Choices[0].Message
	text := strings.TrimSpace(msg.Content)

	var toolUses []ToolUse
	for _, tc := range msg.ToolCalls {
		toolUses = append(toolUses, ToolUse{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: json.RawMessage(tc.Function.Arguments),
		})
	}

	// A tool_calls-only turn legitimately carries no text content
	// (finish_reason == "tool_calls") — only treat empty content as a
	// schema error when there is also no tool call to report.
	if text == "" && len(toolUses) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "openai_api: empty content in response"}
	}

	slog.Debug("openai_api invoke ok", "backend", a.id, "model", a.model, "content_len", len(text),
		"tool_uses", len(toolUses), "finish_reason", out.Choices[0].FinishReason, "request_id", RequestIDFromCtx(ctx))

	rl := RateLimitInfo{
		TokensRemaining:   parseIntHeader(resp.Header, "x-ratelimit-remaining-tokens"),
		TokensResetAt:     parseDurationResetHeader(resp.Header, "x-ratelimit-reset-tokens"),
		RequestsRemaining: parseIntHeader(resp.Header, "x-ratelimit-remaining-requests"),
		RequestsResetAt:   parseDurationResetHeader(resp.Header, "x-ratelimit-reset-requests"),
	}

	return &Response{
		Content:             text,
		PromptTokensEst:     out.Usage.PromptTokens,
		CompletionTokensEst: out.Usage.CompletionTokens,
		RateLimitInfo:       rl,
		ToolUses:            toolUses,
	}, nil
}
