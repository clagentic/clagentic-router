// internal/backend/cache_usage_test.go — table-driven tests for per-family
// cache-token capture (lr-718af0).
//
// Fixtures below are built from REAL captured shapes, not invented ones:
//   - anthropicUsage: Anthropic Messages API "usage" object per public docs
//     (cache_creation_input_tokens/cache_read_input_tokens), same fields
//     anthropic_api.go now parses.
//   - openaiUsage: Chat Completions API "usage.prompt_tokens_details.cached_tokens"
//     per public docs, same field openai_api.go now parses.
//   - types.TokenUsage (bedrock): the vendored SDK struct fields, confirmed
//     live via a reflection probe against the actual pinned SDK version
//     (github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.57.1) before
//     this code was written — CacheReadInputTokens/CacheWriteInputTokens
//     are real *int32 fields on that struct.
//   - geminiOutput: this package's own live-verified capture (see
//     gemini_cli.go's package doc, lr-c98c investigation) of
//     `gemini --output-format json` — no cache field exists in that shape.
//
// Every case explicitly asserts the nil-vs-zero distinction: a family that
// reports real data must never leave CacheUsage nil, and a family that
// cannot report must never fabricate a zero-valued *CacheUsage.
package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestAnthropicAPI_CacheUsage_Reported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":      "msg_test",
			"type":    "message",
			"model":   "claude-opus-4-8",
			"content": []map[string]interface{}{{"type": "text", "text": "hi"}},
			"usage": map[string]int{
				"input_tokens":                100,
				"output_tokens":               20,
				"cache_creation_input_tokens": 30,
				"cache_read_input_tokens":     70,
			},
		}
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL, 10*time.Second, "", ThinkingOff)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage == nil {
		t.Fatal("expected non-nil CacheUsage for anthropic_api (family reports real cache data)")
	}
	if resp.CacheUsage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", resp.CacheUsage.InputTokens)
	}
	if resp.CacheUsage.CacheReadTokens != 70 {
		t.Errorf("CacheReadTokens = %d, want 70", resp.CacheUsage.CacheReadTokens)
	}
	if resp.CacheUsage.CacheWriteTokens != 30 {
		t.Errorf("CacheWriteTokens = %d, want 30", resp.CacheUsage.CacheWriteTokens)
	}
}

// TestAnthropicAPI_CacheUsage_GenuineMiss verifies that a response with no
// cache activity at all still yields a non-nil CacheUsage with zero fields
// — a real reported miss, not "unsupported". This is the exact distinction
// this task's acceptance criteria hinges on.
func TestAnthropicAPI_CacheUsage_GenuineMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":      "msg_test",
			"type":    "message",
			"model":   "claude-opus-4-8",
			"content": []map[string]interface{}{{"type": "text", "text": "hi"}},
			"usage":   map[string]int{"input_tokens": 100, "output_tokens": 20},
		}
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	adapter := NewAnthropicAPIAdapter("test", "claude-opus-4-8", "test-key", srv.URL, 10*time.Second, "", ThinkingOff)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage == nil {
		t.Fatal("expected non-nil CacheUsage (a genuine miss is still reported data, not unsupported)")
	}
	if resp.CacheUsage.CacheReadTokens != 0 || resp.CacheUsage.CacheWriteTokens != 0 {
		t.Errorf("expected zero cache fields for a genuine miss, got %+v", resp.CacheUsage)
	}
}

func TestOpenAIAPI_CacheUsage_Reported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"role": "assistant", "content": "hi"}},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     100,
				"completion_tokens": 20,
				"prompt_tokens_details": map[string]int{
					"cached_tokens": 64,
				},
			},
		}
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	adapter := NewOpenAIAPIAdapter("test", "gpt-5", "test-key", srv.URL, 10*time.Second)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage == nil {
		t.Fatal("expected non-nil CacheUsage for openai_api (family reports real cache-read data)")
	}
	if resp.CacheUsage.CacheReadTokens != 64 {
		t.Errorf("CacheReadTokens = %d, want 64", resp.CacheUsage.CacheReadTokens)
	}
	// OpenAI has no cache-write concept — this is a real, documented zero,
	// not a signal that the family is unsupported (CacheUsage is non-nil).
	if resp.CacheUsage.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0 (OpenAI has no write-side cache accounting)", resp.CacheUsage.CacheWriteTokens)
	}
}

func TestBedrockAPI_CacheUsage_Reported(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role:    types.ConversationRoleAssistant,
						Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "hi"}},
					},
				},
				Usage: &types.TokenUsage{
					InputTokens:           aws.Int32(100),
					OutputTokens:          aws.Int32(20),
					CacheReadInputTokens:  aws.Int32(70),
					CacheWriteInputTokens: aws.Int32(30),
				},
			}, nil
		},
	}

	adapter := newBedrockAPIAdapterWithClient("test", "anthropic.claude-sonnet-4-6", client)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage == nil {
		t.Fatal("expected non-nil CacheUsage for bedrock_api (verified SDK fields exist)")
	}
	if resp.CacheUsage.CacheReadTokens != 70 {
		t.Errorf("CacheReadTokens = %d, want 70", resp.CacheUsage.CacheReadTokens)
	}
	if resp.CacheUsage.CacheWriteTokens != 30 {
		t.Errorf("CacheWriteTokens = %d, want 30", resp.CacheUsage.CacheWriteTokens)
	}
}

// TestBedrockAPI_CacheUsage_NilUsageBlock verifies the defensive path when
// Bedrock's response omits the usage block entirely (typed as a pointer in
// the SDK) — CacheUsage must stay nil here, distinguishing "no usage data
// on this response at all" from a reported zero.
func TestBedrockAPI_CacheUsage_NilUsageBlock(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role:    types.ConversationRoleAssistant,
						Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "hi"}},
					},
				},
				Usage: nil,
			}, nil
		},
	}

	adapter := newBedrockAPIAdapterWithClient("test", "anthropic.claude-sonnet-4-6", client)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage != nil {
		t.Errorf("expected nil CacheUsage when Usage block is absent, got %+v", resp.CacheUsage)
	}
}

// TestGeminiCLI_CacheUsage_Unsupported verifies gemini_cli's documented,
// live-verified no-op: CacheUsage must stay nil, never a fabricated zero,
// because the real `gemini --output-format json` shape (geminiOutput) has
// no cache-accounting field at all.
func TestGeminiCLI_CacheUsage_Unsupported(t *testing.T) {
	dir := t.TempDir()
	payload := geminiSuccessPayload("hello from gemini", "gemini-2.5-flash", 10, 5)
	binPath := writeFakeBin(t, dir, "gemini", string(payload))

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "ping"}}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.CacheUsage != nil {
		t.Errorf("expected nil CacheUsage for gemini_cli (documented unsupported), got %+v", resp.CacheUsage)
	}
}

// TestOllamaHTTP_CacheUsage_Unsupported verifies ollama_http's documented
// no-op: Ollama's /api/chat has no cache-accounting concept, so
// CacheUsage must stay nil, never a fabricated zero.
func TestOllamaHTTP_CacheUsage_Unsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"model":   "llama3",
			"message": map[string]string{"role": "assistant", "content": "hi"},
			"done":    true,
		}
		data, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer srv.Close()

	adapter := NewOllamaHTTPAdapter("test", srv.URL, "llama3", 10*time.Second)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CacheUsage != nil {
		t.Errorf("expected nil CacheUsage for ollama_http (no cache concept in the API), got %+v", resp.CacheUsage)
	}
}
