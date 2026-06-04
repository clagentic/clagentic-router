// internal/backend/anthropic_api_test.go — unit tests for AnthropicAPIAdapter.
package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// anthropicSuccessBody builds a minimal Anthropic Messages API success response.
// blocks is a slice of content blocks; pass nil for a single text block.
func anthropicSuccessBody(blocks []map[string]interface{}) []byte {
	if blocks == nil {
		blocks = []map[string]interface{}{
			{"type": "text", "text": "Hello from Anthropic"},
		}
	}
	resp := map[string]interface{}{
		"id":    "msg_test",
		"type":  "message",
		"model": "claude-opus-4-8",
		"content": blocks,
		"usage": map[string]int{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	}
	data, _ := json.Marshal(resp)
	return data
}

// TestAnthropicAPI_HappyPath verifies a basic successful invocation.
func TestAnthropicAPI_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("unexpected x-api-key: %s", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from Anthropic" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if resp.PromptTokensEst != 10 {
		t.Errorf("unexpected prompt tokens: %d", resp.PromptTokensEst)
	}
	if resp.CompletionTokensEst != 5 {
		t.Errorf("unexpected completion tokens: %d", resp.CompletionTokensEst)
	}
}

// TestAnthropicAPI_OutputConfigEffort verifies that setting Effort populates output_config
// in the wire request.
func TestAnthropicAPI_OutputConfigEffort(t *testing.T) {
	var captured anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, EffortXHigh, ThinkingOff)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Think hard"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.OutputConfig == nil {
		t.Fatal("expected output_config to be set, got nil")
	}
	if captured.OutputConfig.Effort != "xhigh" {
		t.Errorf("expected output_config.effort=xhigh, got %q", captured.OutputConfig.Effort)
	}
	if captured.Thinking != nil {
		t.Errorf("expected thinking to be nil (ThinkingOff), got %+v", captured.Thinking)
	}
}

// TestAnthropicAPI_ThinkingAdaptive verifies that ThinkingAdaptive populates
// thinking.type="adaptive" with display="omitted".
func TestAnthropicAPI_ThinkingAdaptive(t *testing.T) {
	var captured anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, EffortHigh, ThinkingAdaptive)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Reason step by step"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Thinking == nil {
		t.Fatal("expected thinking to be set, got nil")
	}
	if captured.Thinking.Type != "adaptive" {
		t.Errorf("expected thinking.type=adaptive, got %q", captured.Thinking.Type)
	}
	if captured.Thinking.Display != "omitted" {
		t.Errorf("expected thinking.display=omitted, got %q", captured.Thinking.Display)
	}
	if captured.OutputConfig == nil || captured.OutputConfig.Effort != "high" {
		t.Errorf("expected output_config.effort=high, got %+v", captured.OutputConfig)
	}
}

// TestAnthropicAPI_NoEffortNoThinking verifies that omitting effort and thinking_mode
// produces no output_config or thinking fields in the wire request (backward compat).
func TestAnthropicAPI_NoEffortNoThinking(t *testing.T) {
	var captured anthropicRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	// Empty effort and ThinkingOff — the default, no-new-fields path.
	adapter := NewAnthropicAPIAdapter("test", "claude-sonnet-4-6", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.OutputConfig != nil {
		t.Errorf("expected output_config to be nil, got %+v", captured.OutputConfig)
	}
	if captured.Thinking != nil {
		t.Errorf("expected thinking to be nil, got %+v", captured.Thinking)
	}
}

// TestAnthropicAPI_SkipsThinkingBlocks verifies that the response parser skips
// content blocks with type="thinking" and extracts only type="text" blocks.
// This pins the behavior so a regression would be caught immediately.
func TestAnthropicAPI_SkipsThinkingBlocks(t *testing.T) {
	// Response contains a thinking block followed by a text block.
	// The adapter must return only the text content.
	blocks := []map[string]interface{}{
		{
			"type":    "thinking",
			"thinking": "Let me reason through this...",
		},
		{
			"type": "text",
			"text": "The answer is 42.",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(blocks))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, EffortHigh, ThinkingAdaptive)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "What is the answer?"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "The answer is 42." {
		t.Errorf("expected only text block content, got %q", resp.Content)
	}
}

// TestAnthropicAPI_RateLimitHeadersPopulated verifies that Anthropic rate-limit headers
// are harvested and stored in RateLimitInfo.
func TestAnthropicAPI_RateLimitHeadersPopulated(t *testing.T) {
	resetTime := "2026-05-28T20:00:00Z"
	wantResetAt, _ := time.Parse(time.RFC3339, resetTime)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-tokens-remaining", "45000")
		w.Header().Set("anthropic-ratelimit-tokens-reset", resetTime)
		w.Header().Set("anthropic-ratelimit-requests-remaining", "100")
		w.Header().Set("anthropic-ratelimit-requests-reset", resetTime)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RateLimitInfo.TokensRemaining != 45000 {
		t.Errorf("TokensRemaining = %d, want 45000", resp.RateLimitInfo.TokensRemaining)
	}
	if resp.RateLimitInfo.RequestsRemaining != 100 {
		t.Errorf("RequestsRemaining = %d, want 100", resp.RateLimitInfo.RequestsRemaining)
	}
	if !resp.RateLimitInfo.TokensResetAt.Equal(wantResetAt) {
		t.Errorf("TokensResetAt = %v, want %v", resp.RateLimitInfo.TokensResetAt, wantResetAt)
	}
	if !resp.RateLimitInfo.RequestsResetAt.Equal(wantResetAt) {
		t.Errorf("RequestsResetAt = %v, want %v", resp.RateLimitInfo.RequestsResetAt, wantResetAt)
	}
}

// TestAnthropicAPI_RateLimitHeadersAbsent verifies that absent headers produce zero-value
// RateLimitInfo (no panic, no spurious data).
func TestAnthropicAPI_RateLimitHeadersAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(anthropicSuccessBody(nil))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RateLimitInfo.TokensRemaining != 0 {
		t.Errorf("TokensRemaining = %d, want 0 (absent header)", resp.RateLimitInfo.TokensRemaining)
	}
	if !resp.RateLimitInfo.TokensResetAt.IsZero() {
		t.Errorf("TokensResetAt = %v, want zero time (absent header)", resp.RateLimitInfo.TokensResetAt)
	}
	if resp.RateLimitInfo.RequestsRemaining != 0 {
		t.Errorf("RequestsRemaining = %d, want 0 (absent header)", resp.RateLimitInfo.RequestsRemaining)
	}
	if !resp.RateLimitInfo.RequestsResetAt.IsZero() {
		t.Errorf("RequestsResetAt = %v, want zero time (absent header)", resp.RateLimitInfo.RequestsResetAt)
	}
}

// TestAnthropicAPI_HTTP400Response verifies that an HTTP 400 response from the server
// is classified as ErrTypeUnknown. Anthropic returns 400 for invalid field values
// (e.g. wrong thinking.type); the current httpStatusToErrorType falls through to
// ErrTypeUnknown for that status, and this test pins that behavior.
func TestAnthropicAPI_HTTP400Response(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"thinking.type must be adaptive"}}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error for HTTP 400, got nil")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
	if ie.Type != ErrTypeUnknown {
		t.Errorf("expected ErrTypeUnknown for 400, got %q", ie.Type)
	}
}

// TestAnthropicAPI_InBodyError verifies that an HTTP 200 response containing an error
// object in the JSON body is surfaced as an *InvokeError. The Anthropic API may return
// 200 with {"type":"error",...} when the request reaches the server but fails at the
// model layer.
func TestAnthropicAPI_InBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`))
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL,
		10*time.Second, "", ThinkingOff)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error for in-body error response, got nil")
	}
	if _, ok := err.(*InvokeError); !ok {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
}

// TestAnthropicAPI_SerializeOutputConfigJSON verifies that anthropicRequest serializes
// output_config and thinking correctly when fields are set, and that the zero-value
// produces clean JSON with no extra keys.
func TestAnthropicAPI_SerializeOutputConfigJSON(t *testing.T) {
	t.Run("effort_and_thinking_set", func(t *testing.T) {
		req := anthropicRequest{
			Model:     "claude-opus-4-8",
			MaxTokens: 1024,
			Messages:  []anthropicMessage{{Role: "user", Content: "hi"}},
			OutputConfig: &anthropicOutputConfig{Effort: "xhigh"},
			Thinking:     &anthropicThinking{Type: "adaptive", Display: "omitted"},
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]interface{}
		json.Unmarshal(data, &m)
		oc, ok := m["output_config"].(map[string]interface{})
		if !ok {
			t.Fatalf("output_config missing or wrong type: %v", m["output_config"])
		}
		if oc["effort"] != "xhigh" {
			t.Errorf("output_config.effort=%v, want xhigh", oc["effort"])
		}
		th, ok := m["thinking"].(map[string]interface{})
		if !ok {
			t.Fatalf("thinking missing or wrong type: %v", m["thinking"])
		}
		if th["type"] != "adaptive" {
			t.Errorf("thinking.type=%v, want adaptive", th["type"])
		}
		if th["display"] != "omitted" {
			t.Errorf("thinking.display=%v, want omitted", th["display"])
		}
	})

	t.Run("no_effort_no_thinking", func(t *testing.T) {
		req := anthropicRequest{
			Model:     "claude-sonnet-4-6",
			MaxTokens: 1024,
			Messages:  []anthropicMessage{{Role: "user", Content: "hi"}},
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]interface{}
		json.Unmarshal(data, &m)
		if _, exists := m["output_config"]; exists {
			t.Error("output_config present but should be omitted")
		}
		if _, exists := m["thinking"]; exists {
			t.Error("thinking present but should be omitted")
		}
	})
}
