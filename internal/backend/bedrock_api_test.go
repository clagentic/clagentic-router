// internal/backend/bedrock_api_test.go — unit tests for BedrockAPIAdapter.
//
// All tests run against a mocked bedrockConverseClient — no live AWS calls.
package backend

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// mockConverseClient is a bedrockConverseClient stub driven by a caller-supplied
// function, allowing each test to control the response/error and to inspect
// the request that was sent.
type mockConverseClient struct {
	fn func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

func (m *mockConverseClient) Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	return m.fn(ctx, params, optFns...)
}

func TestBedrockAPI_HappyPath(t *testing.T) {
	var captured *bedrockruntime.ConverseInput
	client := &mockConverseClient{
		fn: func(_ context.Context, params *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			captured = params
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role:    types.ConversationRoleAssistant,
						Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Hello from Bedrock"}},
					},
				},
				Usage: &types.TokenUsage{
					InputTokens:  aws.Int32(10),
					OutputTokens: aws.Int32(5),
				},
			}, nil
		},
	}

	adapter := newBedrockAPIAdapterWithClient("test", "anthropic.claude-sonnet-4-6", client)
	resp, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "Hello"},
		},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "Hello from Bedrock" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
	if resp.PromptTokensEst != 10 {
		t.Errorf("unexpected prompt tokens: %d", resp.PromptTokensEst)
	}
	if resp.CompletionTokensEst != 5 {
		t.Errorf("unexpected completion tokens: %d", resp.CompletionTokensEst)
	}

	if captured == nil {
		t.Fatal("client was not called")
	}
	if captured.ModelId == nil || *captured.ModelId != "anthropic.claude-sonnet-4-6" {
		t.Errorf("unexpected model id: %v", captured.ModelId)
	}
	if captured.InferenceConfig == nil || captured.InferenceConfig.MaxTokens == nil || *captured.InferenceConfig.MaxTokens != 512 {
		t.Errorf("unexpected max tokens: %+v", captured.InferenceConfig)
	}
	if len(captured.System) != 1 {
		t.Fatalf("expected 1 system prompt, got %d", len(captured.System))
	}
	sysBlock, ok := captured.System[0].(*types.SystemContentBlockMemberText)
	if !ok || sysBlock.Value != "be terse" {
		t.Errorf("unexpected system prompt: %+v", captured.System[0])
	}
	if len(captured.Messages) != 1 {
		t.Fatalf("expected 1 message after system extraction, got %d", len(captured.Messages))
	}
}

func TestBedrockAPI_DefaultMaxTokens(t *testing.T) {
	var captured *bedrockruntime.ConverseInput
	client := &mockConverseClient{
		fn: func(_ context.Context, params *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			captured = params
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "ok"}}},
				},
			}, nil
		},
	}
	adapter := newBedrockAPIAdapterWithClient("test", "m", client)
	_, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *captured.InferenceConfig.MaxTokens != 4096 {
		t.Errorf("expected default max tokens 4096, got %d", *captured.InferenceConfig.MaxTokens)
	}
}

func TestBedrockAPI_NoMessagesAfterFilter(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			t.Fatal("client should not be called when there are no messages")
			return nil, nil
		},
	}
	adapter := newBedrockAPIAdapterWithClient("test", "m", client)
	_, err := adapter.Invoke(context.Background(), &Request{
		Messages: []Message{{Role: "system", Content: "only a system prompt"}},
	})
	var ie *InvokeError
	if !errors.As(err, &ie) || ie.Type != ErrTypeSchema {
		t.Fatalf("expected ErrTypeSchema, got %v", err)
	}
}

// TestBedrockAPI_NonTextBlockSkipped verifies that a response containing a
// non-text content block (union member the adapter doesn't recognize) does
// not panic and, when combined with a text block, still returns the text.
func TestBedrockAPI_NonTextBlockSkipped(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Content: []types.ContentBlock{
							&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{}},
							&types.ContentBlockMemberText{Value: "the real answer"},
						},
					},
				},
			}, nil
		},
	}
	adapter := newBedrockAPIAdapterWithClient("test", "m", client)
	resp, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("unexpected error (should skip unknown block, not panic): %v", err)
	}
	if resp.Content != "the real answer" {
		t.Errorf("unexpected content: %q", resp.Content)
	}
}

// TestBedrockAPI_EmptyTextContent verifies an all-non-text response (e.g. tool
// use only) is a schema error, not a panic or silent empty success.
func TestBedrockAPI_EmptyTextContent(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Content: []types.ContentBlock{&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{}}},
					},
				},
			}, nil
		},
	}
	adapter := newBedrockAPIAdapterWithClient("test", "m", client)
	_, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	var ie *InvokeError
	if !errors.As(err, &ie) || ie.Type != ErrTypeSchema {
		t.Fatalf("expected ErrTypeSchema, got %v", err)
	}
}

func TestBedrockAPI_ErrorClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorType
	}{
		{"throttling", &types.ThrottlingException{}, ErrTypeRateLimit},
		{"model_not_ready", &types.ModelNotReadyException{}, ErrTypeRateLimit},
		{"access_denied", &types.AccessDeniedException{}, ErrTypeAuth},
		{"access_denied_model_not_enabled", &types.AccessDeniedException{Message: aws.String("You don't have access to the model")}, ErrTypeAuth},
		{"validation", &types.ValidationException{}, ErrTypeSchema},
		{"resource_not_found", &types.ResourceNotFoundException{}, ErrTypeSchema},
		{"model_timeout", &types.ModelTimeoutException{}, ErrTypeTimeout},
		{"model_error", &types.ModelErrorException{}, ErrTypeNetwork},
		{"internal_server", &types.InternalServerException{}, ErrTypeNetwork},
		{"service_unavailable", &types.ServiceUnavailableException{}, ErrTypeNetwork},
		{"unclassified", errors.New("some other error"), ErrTypeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBedrockError(tc.err)
			if got != tc.want {
				t.Errorf("classifyBedrockError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestBedrockAPI_InvokeSurfacesClassifiedError(t *testing.T) {
	client := &mockConverseClient{
		fn: func(_ context.Context, _ *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return nil, &types.ThrottlingException{Message: aws.String("too many requests")}
		},
	}
	adapter := newBedrockAPIAdapterWithClient("test", "m", client)
	_, err := adapter.Invoke(context.Background(), &Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	var ie *InvokeError
	if !errors.As(err, &ie) {
		t.Fatalf("expected *InvokeError, got %T: %v", err, err)
	}
	if ie.Type != ErrTypeRateLimit {
		t.Errorf("expected ErrTypeRateLimit, got %v", ie.Type)
	}
}

func TestNewBedrockAPIAdapter_RequiresRegion(t *testing.T) {
	_, err := NewBedrockAPIAdapter(context.Background(), "test", "m", "", "")
	if err == nil {
		t.Fatal("expected error when region is empty")
	}
}
