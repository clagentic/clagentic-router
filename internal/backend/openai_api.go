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
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Messages  []openaiMessage `json:"messages"`
}

type openaiChoice struct {
	Message openaiMessage `json:"message"`
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
// protocol support. Like AnthropicAPIAdapter, this declares the adapter's
// protocol capacity (the OpenAI API supports tool calling and vision);
// today's router-level translation on the routed-mode path does not yet
// forward tools/images through any adapter — see AnthropicAPIAdapter's
// Capabilities doc for the same caveat.
func (a *OpenAIAPIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: true, SupportsStreaming: false, SupportsImages: true}
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

	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "openai_api: empty content in response"}
	}

	slog.Debug("openai_api invoke ok", "backend", a.id, "model", a.model, "content_len", len(text),
		"request_id", RequestIDFromCtx(ctx))

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
	}, nil
}
