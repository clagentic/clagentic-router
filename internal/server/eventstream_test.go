// internal/server/eventstream_test.go — tests for AWS event-stream framing
// used by the routed-mode Bedrock streaming response.
package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
)

func TestWriteBedrockEventStream_ContentType(t *testing.T) {
	resp := &backend.Response{Content: "hello world"}
	rw := httptest.NewRecorder()
	if err := writeBedrockEventStream(rw, "test-model", resp); err != nil {
		t.Fatalf("writeBedrockEventStream: %v", err)
	}

	if ct := rw.Header().Get("Content-Type"); ct != "application/vnd.amazon.eventstream" {
		t.Errorf("Content-Type: want %q, got %q", "application/vnd.amazon.eventstream", ct)
	}
	if rw.Code != 200 {
		t.Errorf("status: want 200, got %d", rw.Code)
	}
}

// TestWriteBedrockEventStream_FramesDecodeRoundTrip verifies the encoded
// frames can be decoded back by the same AWS SDK eventstream codec — this is
// the deterministic verification the task calls for in place of live-Bedrock
// verification: the framing itself round-trips through the real library, not
// just our own encoder's byte output.
func TestWriteBedrockEventStream_FramesDecodeRoundTrip(t *testing.T) {
	resp := &backend.Response{Content: "the response text", PromptTokensEst: 12, CompletionTokensEst: 34}
	rw := httptest.NewRecorder()
	if err := writeBedrockEventStream(rw, "test-model", resp); err != nil {
		t.Fatalf("writeBedrockEventStream: %v", err)
	}

	payloads, err := decodeBedrockEventStream(rw.Body.Bytes())
	if err != nil {
		t.Fatalf("decodeBedrockEventStream: %v", err)
	}

	// message_start, content_block_start, content_block_delta,
	// content_block_stop, message_delta, message_stop.
	const wantFrames = 6
	if len(payloads) != wantFrames {
		t.Fatalf("frame count: want %d, got %d", wantFrames, len(payloads))
	}

	var types []string
	for i, p := range payloads {
		var evt struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(p, &evt); err != nil {
			t.Fatalf("frame %d: unmarshal payload: %v (raw: %s)", i, err, p)
		}
		types = append(types, evt.Type)
	}

	wantTypes := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	for i, want := range wantTypes {
		if types[i] != want {
			t.Errorf("frame %d type: want %q, got %q", i, want, types[i])
		}
	}
}

func TestWriteBedrockEventStream_ContentDeltaCarriesFullText(t *testing.T) {
	const text = "the full response text"
	resp := &backend.Response{Content: text}
	rw := httptest.NewRecorder()
	if err := writeBedrockEventStream(rw, "m", resp); err != nil {
		t.Fatalf("writeBedrockEventStream: %v", err)
	}

	payloads, err := decodeBedrockEventStream(rw.Body.Bytes())
	if err != nil {
		t.Fatalf("decodeBedrockEventStream: %v", err)
	}
	if len(payloads) < 3 {
		t.Fatalf("expected at least 3 frames, got %d", len(payloads))
	}

	var delta struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(payloads[2], &delta); err != nil {
		t.Fatalf("unmarshal content_block_delta: %v", err)
	}
	if delta.Type != "content_block_delta" {
		t.Fatalf("frame 2 type: want content_block_delta, got %q", delta.Type)
	}
	if delta.Delta.Text != text {
		t.Errorf("delta text: want %q, got %q", text, delta.Delta.Text)
	}
}

func TestWriteBedrockEventStream_UsageInMessageStart(t *testing.T) {
	resp := &backend.Response{Content: "x", PromptTokensEst: 42}
	rw := httptest.NewRecorder()
	if err := writeBedrockEventStream(rw, "m", resp); err != nil {
		t.Fatalf("writeBedrockEventStream: %v", err)
	}

	payloads, err := decodeBedrockEventStream(rw.Body.Bytes())
	if err != nil {
		t.Fatalf("decodeBedrockEventStream: %v", err)
	}

	var start struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payloads[0], &start); err != nil {
		t.Fatalf("unmarshal message_start: %v", err)
	}
	if start.Message.Usage.InputTokens != 42 {
		t.Errorf("input_tokens: want 42, got %d", start.Message.Usage.InputTokens)
	}
}
