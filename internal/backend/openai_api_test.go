// internal/backend/openai_api_test.go — unit tests for OpenAIAPIAdapter.
package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// openaiSuccessBody builds a minimal OpenAI Chat Completions success response.
func openaiSuccessBody(content string, promptTokens, completionTokens int) []byte {
	resp := map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"message": map[string]string{
					"role":    "assistant",
					"content": content,
				},
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
		},
	}
	data, _ := json.Marshal(resp)
	return data
}

// openaiErrorBody builds a minimal OpenAI error response body.
func openaiErrorBody(errType, message, code string) []byte {
	resp := map[string]interface{}{
		"error": map[string]string{
			"type":    errType,
			"message": message,
			"code":    code,
		},
	}
	data, _ := json.Marshal(resp)
	return data
}

func TestOpenAIAPIAdapter_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected Content-Type: %s", r.Header.Get("Content-Type"))
		}

		var reqBody openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if reqBody.Model != "gpt-4o" {
			t.Errorf("unexpected model: %s", reqBody.Model)
		}
		if reqBody.MaxTokens != 4096 {
			t.Errorf("unexpected max_tokens: %d (want 4096 default)", reqBody.MaxTokens)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiSuccessBody("Hello from OpenAI", 10, 5))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from OpenAI" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if resp.PromptTokensEst != 10 {
		t.Errorf("unexpected prompt tokens: %d", resp.PromptTokensEst)
	}
	if resp.CompletionTokensEst != 5 {
		t.Errorf("unexpected completion tokens: %d", resp.CompletionTokensEst)
	}
}

func TestOpenAIAPIAdapter_SystemMessagePassthrough(t *testing.T) {
	// OpenAI accepts system role directly in messages array — verify it is passed through.
	var capturedMessages []openaiMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody openaiRequest
		json.NewDecoder(r.Body).Decode(&reqBody)
		capturedMessages = reqBody.Messages
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiSuccessBody("ok", 15, 3))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedMessages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(capturedMessages))
	}
	if capturedMessages[0].Role != "system" || capturedMessages[0].Content != "You are helpful." {
		t.Errorf("system message not passed through: %+v", capturedMessages[0])
	}
}

func TestOpenAIAPIAdapter_401AuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(openaiErrorBody("invalid_api_key", "Incorrect API key provided.", "invalid_api_key"))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "bad-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	invErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if invErr.Type != ErrTypeAuth {
		t.Errorf("expected ErrTypeAuth, got %q", invErr.Type)
	}
}

func TestOpenAIAPIAdapter_429RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(openaiErrorBody("rate_limit_exceeded", "Rate limit reached for requests.", "rate_limit_exceeded"))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	invErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if invErr.Type != ErrTypeRateLimit {
		t.Errorf("expected ErrTypeRateLimit, got %q", invErr.Type)
	}
}

func TestOpenAIAPIAdapter_429InsufficientQuota(t *testing.T) {
	// 429 with "insufficient_quota" code/body → ErrTypeQuota (not ErrTypeRateLimit).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(openaiErrorBody("insufficient_quota", "You exceeded your current quota.", "insufficient_quota"))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	invErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if invErr.Type != ErrTypeQuota {
		t.Errorf("expected ErrTypeQuota, got %q", invErr.Type)
	}
}

func TestOpenAIAPIAdapter_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	invErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if invErr.Type != ErrTypeSchema {
		t.Errorf("expected ErrTypeSchema, got %q", invErr.Type)
	}
}

func TestOpenAIAPIAdapter_NoAPIKey(t *testing.T) {
	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "", "https://api.openai.com", 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	invErr, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if invErr.Type != ErrTypeAuth {
		t.Errorf("expected ErrTypeAuth, got %q", invErr.Type)
	}
}

func TestOpenAIAPIAdapter_RateLimitHeadersPopulated(t *testing.T) {
	// Verify that rate-limit response headers are harvested into RateLimitInfo.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-tokens", "8000")
		w.Header().Set("x-ratelimit-reset-tokens", "6m0s")
		w.Header().Set("x-ratelimit-remaining-requests", "50")
		w.Header().Set("x-ratelimit-reset-requests", "1m0s")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiSuccessBody("ok", 10, 5))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RateLimitInfo.TokensRemaining != 8000 {
		t.Errorf("TokensRemaining = %d, want 8000", resp.RateLimitInfo.TokensRemaining)
	}
	if resp.RateLimitInfo.RequestsRemaining != 50 {
		t.Errorf("RequestsRemaining = %d, want 50", resp.RateLimitInfo.RequestsRemaining)
	}
	if resp.RateLimitInfo.TokensResetAt.IsZero() {
		t.Error("TokensResetAt is zero, expected a future time")
	}
	if resp.RateLimitInfo.RequestsResetAt.IsZero() {
		t.Error("RequestsResetAt is zero, expected a future time")
	}
	// Reset times should be in the future
	if !resp.RateLimitInfo.TokensResetAt.After(time.Now()) {
		t.Errorf("TokensResetAt %v is not in the future", resp.RateLimitInfo.TokensResetAt)
	}
}

func TestOpenAIAPIAdapter_RateLimitHeadersAbsent(t *testing.T) {
	// When rate-limit headers are not present, RateLimitInfo fields are zero value.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiSuccessBody("ok", 10, 5))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RateLimitInfo.TokensRemaining != 0 {
		t.Errorf("TokensRemaining = %d, want 0 (absent header)", resp.RateLimitInfo.TokensRemaining)
	}
	if resp.RateLimitInfo.RequestsRemaining != 0 {
		t.Errorf("RequestsRemaining = %d, want 0 (absent header)", resp.RateLimitInfo.RequestsRemaining)
	}
	if !resp.RateLimitInfo.TokensResetAt.IsZero() {
		t.Errorf("TokensResetAt = %v, want zero time (absent header)", resp.RateLimitInfo.TokensResetAt)
	}
	if !resp.RateLimitInfo.RequestsResetAt.IsZero() {
		t.Errorf("RequestsResetAt = %v, want zero time (absent header)", resp.RateLimitInfo.RequestsResetAt)
	}
}

func TestOpenAIAPIAdapter_MaxTokensDefault(t *testing.T) {
	// Confirm MaxTokens defaults to 4096 when req.MaxTokens <= 0.
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody openaiRequest
		json.NewDecoder(r.Body).Decode(&reqBody)
		capturedMaxTokens = reqBody.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiSuccessBody("ok", 5, 2))
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test-backend", "gpt-4o", "test-key", srv.URL, 10*time.Second)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages:  []Message{{Role: "user", Content: "Hi"}},
		MaxTokens: 0,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedMaxTokens != 4096 {
		t.Errorf("expected max_tokens=4096, got %d", capturedMaxTokens)
	}
}
