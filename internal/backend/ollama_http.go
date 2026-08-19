// internal/backend/ollama_http.go — adapter for the Ollama HTTP API.
//
// Invokes: POST <url>/api/chat with a JSON body (OpenAI-compatible messages).
// No authentication required by default (Ollama is local-first).
// stream=false for synchronous responses.
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

// ollamaMessage mirrors the Ollama API message shape.
type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// ollamaRequest is the POST body for /api/chat.
type ollamaRequest struct {
	Model    string           `json:"model"`
	Messages []ollamaMessage  `json:"messages"`
	Stream   bool             `json:"stream"`
	Tools    []ollamaToolSpec `json:"tools,omitempty"`
}

// ollamaToolSpec is the Ollama /api/chat "tools" entry envelope — Ollama
// mirrors the OpenAI function-calling wire shape
// ({"type":"function","function":{name,description,parameters}}), per the
// Ollama API docs.
type ollamaToolSpec struct {
	Type     string             `json:"type"`
	Function ollamaToolSpecFunc `json:"function"`
}

type ollamaToolSpecFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ollamaToolCall is one entry of message.tool_calls in an Ollama /api/chat
// response. Unlike OpenAI, Ollama's function.arguments is a JSON object
// value, not a JSON-encoded string — so Arguments is json.RawMessage here,
// carried through to ToolUse.Input unmodified (no string-decode step, unlike
// the OpenAI adapter's Arguments string field).
type ollamaToolCall struct {
	Function ollamaToolCallFunc `json:"function"`
}

type ollamaToolCallFunc struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toOllamaTools translates the neutral backend.ToolDef slice into the Ollama
// wire envelope. Returns nil for an empty input so "tools" is omitted
// entirely (omitempty) rather than sent as [].
func toOllamaTools(defs []ToolDef) []ollamaToolSpec {
	if len(defs) == 0 {
		return nil
	}
	out := make([]ollamaToolSpec, len(defs))
	for i, d := range defs {
		out[i] = ollamaToolSpec{
			Type: "function",
			Function: ollamaToolSpecFunc{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.InputSchema,
			},
		}
	}
	return out
}

// ollamaResponse is the response from /api/chat (stream=false).
type ollamaResponse struct {
	Model   string        `json:"model"`
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

// OllamaHTTPAdapter calls an Ollama server over HTTP.
type OllamaHTTPAdapter struct {
	id     string
	url    string
	model  string
	client *http.Client
}

// NewOllamaHTTPAdapter creates an adapter for the given Ollama server.
// url should be the base URL (e.g. "http://localhost:11434").
func NewOllamaHTTPAdapter(id, url, model string, timeout time.Duration) *OllamaHTTPAdapter {
	return &OllamaHTTPAdapter{
		id:    id,
		url:   strings.TrimRight(url, "/"),
		model: model,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (a *OllamaHTTPAdapter) ID() string { return a.id }

// Capabilities reports the Ollama HTTP adapter's wire protocol support.
// Invoke marshals req.Tools into the Ollama "tools" field (toOllamaTools,
// same function-calling envelope as OpenAI's) and parses
// message.tool_calls back out of the response (see Invoke below), so
// SupportsTools is true. Images remain unsupported: this adapter's request
// body still carries plain-text message content only.
func (a *OllamaHTTPAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: true, SupportsStreaming: false, SupportsImages: false}
}

// Invoke sends a chat request to the Ollama API.
func (a *OllamaHTTPAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	msgs := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	body := ollamaRequest{
		Model:    a.model,
		Messages: msgs,
		Stream:   false,
		Tools:    toOllamaTools(req.Tools),
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("marshal request: %v", err)}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url+"/api/chat", bytes.NewReader(data))
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeNetwork, Raw: fmt.Sprintf("build request: %v", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		errType := ErrTypeNetwork
		if strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "timeout") {
			errType = ErrTypeTimeout
		}
		return nil, &InvokeError{Type: errType, Raw: truncate(err.Error(), 500)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &InvokeError{Type: ErrTypeNetwork, Raw: fmt.Sprintf("read response: %v", err)}
	}

	if resp.StatusCode != http.StatusOK {
		errType := ErrTypeUnknown
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			errType = ErrTypeAuth
		case http.StatusTooManyRequests:
			errType = ErrTypeRateLimit
		case http.StatusServiceUnavailable, 529:
			errType = ErrTypeRateLimit
		}
		return nil, &InvokeError{
			Type: errType,
			Raw:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(respBody), 300)),
		}
	}

	var out ollamaResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: fmt.Sprintf("parse response: %v", err)}
	}

	content := strings.TrimSpace(out.Message.Content)

	var toolUses []ToolUse
	for _, tc := range out.Message.ToolCalls {
		toolUses = append(toolUses, ToolUse{Name: tc.Function.Name, Input: tc.Function.Arguments})
	}

	// A tool_calls-only turn legitimately carries no text content — only
	// treat empty content as a schema error when there is also no tool call
	// to report.
	if content == "" && len(toolUses) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "empty content in Ollama response"}
	}

	slog.Debug("ollama_http invoke ok", "backend", a.id, "model", a.model, "content_len", len(content),
		"tool_uses", len(toolUses), "request_id", RequestIDFromCtx(ctx))

	return &Response{
		Content:             content,
		PromptTokensEst:     EstimateTokens(req.Messages),
		CompletionTokensEst: len(content) / 4,
		ToolUses:            toolUses,
	}, nil
}
