// internal/backend/bedrock_api.go — adapter for AWS Bedrock's Converse API.
//
// Uses the AWS Bedrock Runtime Converse API, which is uniform across model
// families for text-only, non-streaming invocation (both the Anthropic and
// OpenAI families hosted on Bedrock use the same Converse call shape). This
// is why one adapter covers both families with no model-family-specific
// config — see CLAUDE.md's "Design constraint" note for bedrock_api.
//
// Credentials come exclusively from the standard AWS SDK credential chain
// (env vars, shared config/credentials files, web identity, ECS, IMDS) via
// config.LoadDefaultConfig. There is no api_key-style config field — Bedrock
// requests are SigV4-signed by the SDK, not bearer-token authenticated, and
// credentials must never be readable from router.yaml.
//
// Region has no SDK default and is a required config key; its absence fails
// adapter construction (and thus startup) loudly rather than silently
// picking an arbitrary region.
//
// Model ID semantics: bare model IDs (e.g. anthropic.claude-...) are
// region-specific. Region-prefixed inference profile IDs (us.*, eu.*) add
// cross-region routing with failover, and some models are reachable only
// via an inference profile. The two forms are NOT interchangeable — check
// the Bedrock console for the correct form for your target model/region.
//
// API reference: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html
package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// bedrockConverseClient is the subset of *bedrockruntime.Client used by this
// adapter. Interfacing it allows unit tests to substitute a mock instead of
// making live AWS calls.
type bedrockConverseClient interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// BedrockAPIAdapter calls the AWS Bedrock Converse API.
type BedrockAPIAdapter struct {
	id     string
	model  string
	client bedrockConverseClient
}

// NewBedrockAPIAdapter creates a new adapter using the standard AWS SDK
// credential chain (env -> web identity -> shared creds -> shared config ->
// ECS -> IMDS). region is required — Bedrock has no default region, and an
// empty region is rejected here rather than deferred to the first failed
// call. profile is optional; when set it selects a named profile from the
// shared AWS config/credentials files.
func NewBedrockAPIAdapter(ctx context.Context, id, model, region, profile string) (*BedrockAPIAdapter, error) {
	if region == "" {
		return nil, fmt.Errorf("bedrock_api: region is required (no SDK default exists for Bedrock)")
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bedrock_api: load AWS config: %w", err)
	}

	return &BedrockAPIAdapter{
		id:     id,
		model:  model,
		client: bedrockruntime.NewFromConfig(awsCfg),
	}, nil
}

// newBedrockAPIAdapterWithClient constructs an adapter around a caller-supplied
// client, bypassing credential resolution. Used by tests to inject a mock.
func newBedrockAPIAdapterWithClient(id, model string, client bedrockConverseClient) *BedrockAPIAdapter {
	return &BedrockAPIAdapter{id: id, model: model, client: client}
}

func (a *BedrockAPIAdapter) ID() string { return a.id }

// Capabilities reports the Bedrock Converse API adapter's wire protocol
// support. Invoke marshals req.Tools into a Converse ToolConfiguration
// (toBedrockToolConfig) and parses ToolUseBlock members back out of the
// response (see Invoke and bedrockOutputContent), so SupportsTools is true.
// Images remain unsupported: Invoke still builds only
// ContentBlockMemberText members, never an image content block.
func (a *BedrockAPIAdapter) Capabilities() Capabilities {
	return Capabilities{SupportsTools: true, SupportsStreaming: false, SupportsImages: false}
}

// toBedrockToolConfig translates the neutral backend.ToolDef slice into a
// Bedrock Converse ToolConfiguration. Returns nil for an empty input so
// ConverseInput.ToolConfig is left unset entirely, matching the neutral IR
// contract (omit rather than send an empty tool list).
//
// InputSchema is carried as json.RawMessage in the neutral type but the
// Converse API's ToolInputSchema wants a smithy document.Interface, not raw
// bytes — document.NewLazyDocument (this service's own document package,
// not smithy-go's, since ToolInputSchema's union member is sealed to the
// bedrockruntime-generated Interface) accepts the raw JSON bytes directly
// and round-trips them losslessly (smithy documents are this SDK's typed
// wrapper for provider-agnostic JSON payloads), so no schema information is
// lost or reinterpreted in the translation.
func toBedrockToolConfig(defs []ToolDef) *types.ToolConfiguration {
	if len(defs) == 0 {
		return nil
	}
	tools := make([]types.Tool, 0, len(defs))
	for _, d := range defs {
		spec := types.ToolSpecification{
			Name: aws.String(d.Name),
		}
		if d.Description != "" {
			spec.Description = aws.String(d.Description)
		}
		if len(d.InputSchema) > 0 {
			spec.InputSchema = &types.ToolInputSchemaMemberJson{
				Value: document.NewLazyDocument(d.InputSchema),
			}
		}
		tools = append(tools, &types.ToolMemberToolSpec{Value: spec})
	}
	return &types.ToolConfiguration{Tools: tools}
}

// Invoke sends a request to the Bedrock Converse API.
func (a *BedrockAPIAdapter) Invoke(ctx context.Context, req *Request) (*Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	var systemPrompts []types.SystemContentBlock
	var messages []types.Message
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemPrompts = append(systemPrompts, &types.SystemContentBlockMemberText{Value: m.Content})
			continue
		}
		role := types.ConversationRoleUser
		if m.Role == "assistant" {
			role = types.ConversationRoleAssistant
		}
		messages = append(messages, types.Message{
			Role:    role,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
		})
	}
	if len(messages) == 0 {
		return nil, &InvokeError{Type: ErrTypeSchema, Raw: "bedrock_api: no user/assistant messages after filtering"}
	}

	input := &bedrockruntime.ConverseInput{
		ModelId:  aws.String(a.model),
		Messages: messages,
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens: aws.Int32(int32(maxTokens)),
		},
	}
	if len(systemPrompts) > 0 {
		input.System = systemPrompts
	}
	if toolCfg := toBedrockToolConfig(req.Tools); toolCfg != nil {
		input.ToolConfig = toolCfg
	}

	out, err := a.client.Converse(ctx, input)
	if err != nil {
		return nil, &InvokeError{Type: classifyBedrockError(err), Raw: truncate(err.Error(), 500)}
	}

	text, toolUses, textErr := bedrockOutputContent(out)
	if textErr != nil {
		return nil, textErr
	}

	var promptTokens, completionTokens int
	if out.Usage != nil {
		if out.Usage.InputTokens != nil {
			promptTokens = int(*out.Usage.InputTokens)
		}
		if out.Usage.OutputTokens != nil {
			completionTokens = int(*out.Usage.OutputTokens)
		}
	}

	slog.Debug("bedrock_api invoke ok", "backend", a.id, "model", a.model, "content_len", len(text),
		"tool_uses", len(toolUses), "request_id", RequestIDFromCtx(ctx))

	return &Response{
		Content:             text,
		PromptTokensEst:     promptTokens,
		CompletionTokensEst: completionTokens,
		ToolUses:            toolUses,
	}, nil
}

// bedrockOutputContent extracts the text and tool_use content from a
// ConverseOutput. Output.Content is a union type (types.ContentBlock);
// member types this adapter does not carry (reasoning, image, etc.) are
// skipped rather than causing a panic on a failed type assertion — image
// content is out of scope for this adapter but must not crash the router if
// the model emits it.
func bedrockOutputContent(out *bedrockruntime.ConverseOutput) (string, []ToolUse, error) {
	if out == nil || out.Output == nil {
		return "", nil, &InvokeError{Type: ErrTypeSchema, Raw: "bedrock_api: empty output in response"}
	}
	msgMember, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", nil, &InvokeError{Type: ErrTypeSchema, Raw: "bedrock_api: unexpected output member type"}
	}

	var sb strings.Builder
	var toolUses []ToolUse
	for _, block := range msgMember.Value.Content {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			sb.WriteString(b.Value)
		case *types.ContentBlockMemberToolUse:
			toolUses = append(toolUses, bedrockToolUseBlockToToolUse(b.Value))
		}
		// Unknown/other block types (reasoning, image, etc.) are intentionally
		// skipped — Invoke never panics on an unrecognized union member.
	}
	text := strings.TrimSpace(sb.String())
	if text == "" && len(toolUses) == 0 {
		return "", nil, &InvokeError{Type: ErrTypeSchema, Raw: "bedrock_api: no text or tool_use content blocks in response"}
	}
	return text, toolUses, nil
}

// bedrockToolUseBlockToToolUse translates a Converse ToolUseBlock into the
// neutral backend.ToolUse type. Input is a smithy document.Interface;
// MarshalSmithyDocument round-trips it back to the same JSON bytes that
// went in via toBedrockToolConfig's document.NewLazyDocument, so this is a
// lossless translation, not a reinterpretation of the payload.
func bedrockToolUseBlockToToolUse(b types.ToolUseBlock) ToolUse {
	tu := ToolUse{Name: aws.ToString(b.Name)}
	if b.ToolUseId != nil {
		tu.ID = aws.ToString(b.ToolUseId)
	}
	if b.Input != nil {
		if raw, err := b.Input.MarshalSmithyDocument(); err == nil {
			tu.Input = json.RawMessage(raw)
		}
	}
	return tu
}

// classifyBedrockError maps typed Bedrock Runtime SDK errors onto the
// router's existing ErrorType taxonomy using errors.As, never string
// matching. The router's ErrorType enum (backend/adapter.go) has no
// separate "downstream" or "validation" category, so the Bedrock-specific
// taxonomy in the task brief is folded onto the closest existing type:
//
//	ThrottlingException, ModelNotReadyException   -> ErrTypeRateLimit (throttled)
//	AccessDeniedException                         -> ErrTypeAuth
//	ValidationException, ResourceNotFoundException -> ErrTypeSchema (bad request)
//	ModelTimeoutException                          -> ErrTypeTimeout
//	ModelErrorException, InternalServerException,
//	ServiceUnavailableException                    -> ErrTypeNetwork (retriable
//	                                                   downstream/server-side failure)
//
// AccessDeniedException covers both missing/expired credentials and a
// model that exists but is not enabled for the account (the latter carries
// a "You don't have access to the model" message). Both classify as auth;
// distinguishing them would require message substring matching, which is
// fragile and not required here.
func classifyBedrockError(err error) ErrorType {
	var throttling *types.ThrottlingException
	if errors.As(err, &throttling) {
		return ErrTypeRateLimit
	}
	var modelNotReady *types.ModelNotReadyException
	if errors.As(err, &modelNotReady) {
		return ErrTypeRateLimit
	}
	var accessDenied *types.AccessDeniedException
	if errors.As(err, &accessDenied) {
		return ErrTypeAuth
	}
	var validation *types.ValidationException
	if errors.As(err, &validation) {
		return ErrTypeSchema
	}
	var notFound *types.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return ErrTypeSchema
	}
	var modelTimeout *types.ModelTimeoutException
	if errors.As(err, &modelTimeout) {
		return ErrTypeTimeout
	}
	var modelError *types.ModelErrorException
	if errors.As(err, &modelError) {
		return ErrTypeNetwork
	}
	var internalErr *types.InternalServerException
	if errors.As(err, &internalErr) {
		return ErrTypeNetwork
	}
	var serviceUnavailable *types.ServiceUnavailableException
	if errors.As(err, &serviceUnavailable) {
		return ErrTypeNetwork
	}
	return ErrTypeUnknown
}
