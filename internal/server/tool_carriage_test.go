// internal/server/tool_carriage_test.go — end-to-end tests for single-shot
// tool carriage (lr-add405): a routed-mode response carrying backend.ToolUse
// entries must surface as tool_use content blocks (Anthropic Messages /
// Bedrock InvokeModel) or tool_calls (OpenAI Chat Completions), with the
// correct stop_reason/finish_reason, in both the non-streaming JSON response
// and the streaming SSE/event-stream grammar.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

// newToolCarriageTestServer builds a Server with one tool-capable stub
// backend that returns resp verbatim, registered under both "backend:id"
// (direct) and chain "reviewer-chain" (role:) selection.
func newToolCarriageTestServer(t *testing.T, resp *backend.Response) (*httptest.Server, func()) {
	t.Helper()
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"tool-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Chains: map[string][]string{
			"reviewer-chain": {"tool-backend"},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"tool-backend": &stubAdapter{id: "tool-backend", supportsTools: true, response: resp},
	}
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "http://unused.invalid", "", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	return ts, func() { ts.Close() }
}

// --- POST /v1/messages (Anthropic Messages API) ---

func TestMessagesRouted_ToolUseResponse_EmitsToolUseBlockAndStopReason(t *testing.T) {
	toolResp := &backend.Response{
		Content:  "",
		ToolUses: []backend.ToolUse{{ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Boston"}`)}},
	}
	ts, cleanup := newToolCarriageTestServer(t, toolResp)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"tools":      []map[string]interface{}{{"name": "get_weather", "input_schema": map[string]interface{}{"type": "object"}}},
		"messages":   []map[string]string{{"role": "user", "content": "weather in Boston?"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.StopReason != "tool_use" {
		t.Errorf("stop_reason: want tool_use, got %q", body.StopReason)
	}
	if len(body.Content) != 1 {
		t.Fatalf("content blocks: want 1 (tool_use only, no empty text block), got %d: %+v", len(body.Content), body.Content)
	}
	block := body.Content[0]
	if block.Type != "tool_use" {
		t.Errorf("content[0].type: want tool_use, got %q", block.Type)
	}
	if block.ID != "tu_1" || block.Name != "get_weather" {
		t.Errorf("unexpected tool_use block: %+v", block)
	}
	if string(block.Input) != `{"city":"Boston"}` {
		t.Errorf("input: want {\"city\":\"Boston\"}, got %s", block.Input)
	}
}

func TestMessagesRouted_TextAndToolUse_BothBlocksPresent(t *testing.T) {
	toolResp := &backend.Response{
		Content:  "Let me check that for you.",
		ToolUses: []backend.ToolUse{{ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{}`)}},
	}
	ts, cleanup := newToolCarriageTestServer(t, toolResp)
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"tools":      []map[string]interface{}{{"name": "get_weather"}},
		"messages":   []map[string]string{{"role": "user", "content": "weather?"}},
	})
	defer resp.Body.Close()

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Content) != 2 {
		t.Fatalf("content blocks: want 2 (text + tool_use), got %d: %+v", len(body.Content), body.Content)
	}
	if body.Content[0].Type != "text" || body.Content[0].Text != "Let me check that for you." {
		t.Errorf("content[0]: unexpected text block: %+v", body.Content[0])
	}
	if body.Content[1].Type != "tool_use" {
		t.Errorf("content[1].type: want tool_use, got %q", body.Content[1].Type)
	}
}

func TestMessagesRouted_NoToolUse_StopReasonEndTurn(t *testing.T) {
	ts, cleanup := newToolCarriageTestServer(t, &backend.Response{Content: "plain answer"})
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.StopReason != "end_turn" {
		t.Errorf("stop_reason: want end_turn, got %q", body.StopReason)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "text" {
		t.Fatalf("unexpected content: %+v", body.Content)
	}
}

// TestMessagesRouted_StreamToolUse_EmitsInputJSONDeltaGrammar verifies the
// SSE writer emits tool_use blocks in correct Anthropic event grammar
// (content_block_start{type:tool_use} -> content_block_delta{input_json_delta}
// -> content_block_stop), with stop_reason:"tool_use" in message_delta.
func TestMessagesRouted_StreamToolUse_EmitsInputJSONDeltaGrammar(t *testing.T) {
	toolResp := &backend.Response{
		ToolUses: []backend.ToolUse{{ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Boston"}`)}},
	}
	ts, cleanup := newToolCarriageTestServer(t, toolResp)
	defer cleanup()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/messages",
		bytes.NewReader(mustMarshal(t, map[string]interface{}{
			"model":      "role:reviewer-chain",
			"max_tokens": 100,
			"stream":     true,
			"tools":      []map[string]interface{}{{"name": "get_weather"}},
			"messages":   []map[string]string{{"role": "user", "content": "weather?"}},
		})))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var events []string
	var sawInputJSONDelta bool
	var sawToolUseStart bool
	var sawToolUseStopReason bool
	scanner := bufio.NewScanner(resp.Body)
	var pendingEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			pendingEvent = strings.TrimPrefix(line, "event: ")
			events = append(events, pendingEvent)
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			switch pendingEvent {
			case "content_block_start":
				if strings.Contains(data, `"type":"tool_use"`) {
					sawToolUseStart = true
				}
			case "content_block_delta":
				if strings.Contains(data, `"input_json_delta"`) {
					sawInputJSONDelta = true
				}
			case "message_delta":
				if strings.Contains(data, `"stop_reason":"tool_use"`) {
					sawToolUseStopReason = true
				}
			}
		}
	}

	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(events) != len(want) {
		t.Fatalf("event count: want %d %v, got %d %v", len(want), want, len(events), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d]: want %q, got %q", i, want[i], events[i])
		}
	}
	if !sawToolUseStart {
		t.Error("expected a content_block_start event with type:tool_use")
	}
	if !sawInputJSONDelta {
		t.Error("expected a content_block_delta event with input_json_delta")
	}
	if !sawToolUseStopReason {
		t.Error("expected message_delta stop_reason to be tool_use")
	}
}

// --- POST /v1/chat/completions (OpenAI-compatible) ---

func TestChatCompletions_ToolUseResponse_EmitsToolCallsAndFinishReason(t *testing.T) {
	toolResp := &backend.Response{
		ToolUses: []backend.ToolUse{{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Boston"}`)}},
	}
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"tool-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"tool-backend": &stubAdapter{id: "tool-backend", supportsTools: true, response: toolResp},
	}
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:tool-backend",
		"max_tokens": 100,
		"tools":      []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "get_weather"}}},
		"messages":   []map[string]string{{"role": "user", "content": "weather?"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	var body chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Choices) != 1 {
		t.Fatalf("choices: want 1, got %d", len(body.Choices))
	}
	choice := body.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason: want tool_calls, got %q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls: want 1, got %d", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"Boston"}` {
		t.Errorf("unexpected tool_call: %+v", tc)
	}
}

func TestChatCompletions_NoToolUse_FinishReasonStop(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()

	var body chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: want stop, got %q", body.Choices[0].FinishReason)
	}
	if len(body.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("tool_calls: want none, got %+v", body.Choices[0].Message.ToolCalls)
	}
}

// --- POST /model/{modelId}/invoke (Bedrock InvokeModel) ---

// TestBedrockRouted_ToolUseResponse_EmitsToolUseBlockAndStopReason mirrors
// TestMessagesRouted_ToolUseResponse_EmitsToolUseBlockAndStopReason for the
// Bedrock InvokeModel routed path, which reuses the same
// anthropicResponseContentBlocks/anthropicStopReason helpers.
func TestBedrockRouted_ToolUseResponse_EmitsToolUseBlockAndStopReason(t *testing.T) {
	toolResp := &backend.Response{
		ToolUses: []backend.ToolUse{{ID: "tu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Boston"}`)}},
	}
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"tool-backend": {Adapter: "stub", CostWeight: 1.0},
		},
		Chains: map[string][]string{
			"reviewer-chain": {"tool-backend"},
		},
		Routing: config.RoutingConfig{
			Strategy:                   "scored",
			QuotaWarningThreshold:      0.2,
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
	}
	adapters := map[string]backend.Adapter{
		"tool-backend": &stubAdapter{id: "tool-backend", supportsTools: true, response: toolResp},
	}
	r := router.New(cfg, adapters, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "")
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp := doBedrockInvoke(t, ts, "/model/role:reviewer-chain/invoke", "x-api-key", "secret", map[string]interface{}{
		"max_tokens":        100,
		"anthropic_version": "bedrock-2023-05-31",
		"tools":             []map[string]interface{}{{"name": "get_weather"}},
		"messages":          []map[string]string{{"role": "user", "content": "weather?"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}

	var body anthropicMsgResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.StopReason != "tool_use" {
		t.Errorf("stop_reason: want tool_use, got %q", body.StopReason)
	}
	if len(body.Content) != 1 || body.Content[0].Type != "tool_use" {
		t.Fatalf("unexpected content: %+v", body.Content)
	}
	if body.Content[0].ID != "tu_1" || body.Content[0].Name != "get_weather" {
		t.Errorf("unexpected tool_use block: %+v", body.Content[0])
	}
}

// --- decodeAnthropicContent: input translation no longer drops tool_use/tool_result ---

func TestDecodeAnthropicContent_ToolUseBlock_RenderedAsMarker(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Boston"}}]`)
	text, err := decodeAnthropicContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text == "" {
		t.Fatal("expected a non-empty rendered marker, got empty string (tool_use silently dropped)")
	}
	if !strings.Contains(text, "get_weather") || !strings.Contains(text, "Boston") {
		t.Errorf("rendered marker missing tool name/input: %q", text)
	}
}

func TestDecodeAnthropicContent_ToolResultBlock_RenderedAsMarker(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_result","tool_use_id":"tu_1","content":"72 degrees and sunny"}]`)
	text, err := decodeAnthropicContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text == "" {
		t.Fatal("expected a non-empty rendered marker, got empty string (tool_result silently dropped)")
	}
	if !strings.Contains(text, "tu_1") || !strings.Contains(text, "72 degrees and sunny") {
		t.Errorf("rendered marker missing tool_use_id/content: %q", text)
	}
}

func TestDecodeAnthropicContent_TextAndToolUse_BothPreserved(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"checking now"},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{}}]`)
	text, err := decodeAnthropicContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(text, "checking now") {
		t.Errorf("expected text block preserved: %q", text)
	}
	if !strings.Contains(text, "get_weather") {
		t.Errorf("expected tool_use marker preserved: %q", text)
	}
}

func TestDecodeAnthropicContent_ImageBlock_StillDropped(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"see image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)
	text, err := decodeAnthropicContent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "see image" {
		t.Errorf("expected only the text block to survive (images out of scope), got %q", text)
	}
}
