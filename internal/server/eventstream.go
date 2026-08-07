// internal/server/eventstream.go — AWS event-stream (vnd.amazon.eventstream)
// framing for the routed-mode POST /model/{modelId}/invoke-with-response-stream
// response.
//
// This is a THIRD response-framing scheme alongside plain SSE
// (writeSSEStream, OpenAI grammar) and Anthropic Messages SSE
// (writeAnthropicSSEStream). AWS Bedrock's InvokeModelWithResponseStream API
// does not use text/event-stream at all — it frames each chunk as a binary
// AWS event-stream message (4-byte total length, 4-byte headers length,
// 4-byte prelude CRC, headers, payload, 4-byte message CRC), and the JSON
// payload of each frame carries the *same* event shape the direct Anthropic
// Messages streaming API sends over SSE (message_start, content_block_delta,
// etc.) — just wrapped in {"bytes": base64(json)} rather than an
// "event: <type>\ndata: <json>\n\n" line.
//
// Framing itself is delegated to the AWS SDK's own eventstream codec
// (github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream, already a
// transitive dependency via bedrockruntime) rather than hand-rolled, per the
// task's guidance to check the real wire format library before reinventing it.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/backend"
)

// bedrockEventStreamPayload is the envelope every Bedrock InvokeModelWithResponseStream
// frame payload carries: the actual Anthropic-grammar event JSON, base64-decoded
// implicitly by being raw bytes inside the eventstream payload (base64 is an
// eventstream *header-value-type* concern, not applied to the payload itself —
// the payload bytes are the raw JSON, matching AWS SDK client behavior of
// reading PayloadPart.Bytes directly as the chunk JSON).
type bedrockEventStreamPayload = json.RawMessage

// writeBedrockEventStream emits a routed-mode Bedrock InvokeModelWithResponseStream
// response for a complete (non-token-streamed) backend response. Mirrors
// writeAnthropicSSEStream's event sequence (message_start ->
// content_block_start -> content_block_delta -> content_block_stop ->
// message_delta -> message_stop) but frames each event as an AWS event-stream
// "chunk" message instead of an SSE line, per the Bedrock wire contract.
//
// Like the other routed-mode streaming paths, this is NOT true token
// streaming — backends return complete responses, so the full text is
// emitted in one content_block_delta frame. Documented routed-mode limitation,
// consistent with writeSSEStream/writeAnthropicSSEStream.
func writeBedrockEventStream(w http.ResponseWriter, model string, resp *backend.Response) error {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.Header().Set("X-Amzn-Bedrock-Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	enc := eventstream.NewEncoder()
	id := "msg_" + uuid.NewString()[:24]

	writeChunk := func(event map[string]interface{}) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("bedrock eventstream: marshal event: %w", err)
		}
		msg := eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":event-type", Value: eventstream.StringValue("chunk")},
				{Name: ":content-type", Value: eventstream.StringValue("application/json")},
				{Name: ":message-type", Value: eventstream.StringValue("event")},
			},
			Payload: payload,
		}
		if err := enc.Encode(w, msg); err != nil {
			return fmt.Errorf("bedrock eventstream: encode frame: %w", err)
		}
		if canFlush {
			flusher.Flush()
		}
		return nil
	}

	if err := writeChunk(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": resp.PromptTokensEst, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}

	if err := writeChunk(map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}); err != nil {
		return err
	}

	if err := writeChunk(map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": resp.Content},
	}); err != nil {
		return err
	}

	if err := writeChunk(map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}); err != nil {
		return err
	}

	if err := writeChunk(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": resp.CompletionTokensEst},
	}); err != nil {
		return err
	}

	return writeChunk(map[string]interface{}{
		"type": "message_stop",
	})
}

// decodeBedrockEventStream reads all frames from an AWS event-stream body and
// returns their raw JSON payloads in order. Used by tests to verify framing
// round-trips through the real AWS SDK decoder rather than only asserting on
// our own encoder's output.
func decodeBedrockEventStream(body []byte) ([]bedrockEventStreamPayload, error) {
	dec := eventstream.NewDecoder()
	r := bytes.NewReader(body)
	var payloads []bedrockEventStreamPayload
	for {
		msg, err := dec.Decode(r, nil)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return payloads, fmt.Errorf("bedrock eventstream: decode frame: %w", err)
		}
		payloads = append(payloads, append(json.RawMessage(nil), msg.Payload...))
	}
	return payloads, nil
}
