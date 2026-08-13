// internal/server/handlers_test.go — handler-level tests for SSE streaming
// and the /logs, /stats date-range endpoints.
//
// These tests exercise the HTTP layer without a real router or store — they
// use httptest.NewRecorder and minimal stubs.
package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
)

// --- writeSSEStream ---

func TestWriteSSEStream_ContentType(t *testing.T) {
	resp := &backend.Response{Content: "hello world"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "test-model", resp)

	ct := rw.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type: want %q, got %q", "text/event-stream", ct)
	}
	if rw.Code != http.StatusOK {
		t.Errorf("status: want 200, got %d", rw.Code)
	}
}

func TestWriteSSEStream_DoneTerminator(t *testing.T) {
	resp := &backend.Response{Content: "hello"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	body := rw.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("SSE body missing [DONE] terminator:\n%s", body)
	}
}

func TestWriteSSEStream_ChunkCount(t *testing.T) {
	resp := &backend.Response{Content: "hello"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	// Expect exactly 4 data: lines: role chunk, content chunk, finish chunk, [DONE]
	var dataLines []string
	scanner := bufio.NewScanner(strings.NewReader(rw.Body.String()))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, line)
		}
	}
	if len(dataLines) != 4 {
		t.Errorf("expected 4 data: lines, got %d:\n%v", len(dataLines), dataLines)
	}
}

func TestWriteSSEStream_RoleDelta(t *testing.T) {
	resp := &backend.Response{Content: "hi"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	chunks := parseSSEChunks(t, rw.Body.String())
	if len(chunks) < 1 {
		t.Fatal("no chunks parsed")
	}
	// First chunk must have role="assistant", no content
	first := chunks[0]
	if len(first.Choices) == 0 {
		t.Fatal("first chunk has no choices")
	}
	if first.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk role: want %q, got %q", "assistant", first.Choices[0].Delta.Role)
	}
	if first.Choices[0].Delta.Content != "" {
		t.Errorf("first chunk should have no content, got %q", first.Choices[0].Delta.Content)
	}
}

func TestWriteSSEStream_ContentDelta(t *testing.T) {
	content := "the full response text"
	resp := &backend.Response{Content: content}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	chunks := parseSSEChunks(t, rw.Body.String())
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// Second chunk carries the content
	second := chunks[1]
	if len(second.Choices) == 0 {
		t.Fatal("second chunk has no choices")
	}
	if second.Choices[0].Delta.Content != content {
		t.Errorf("content delta: want %q, got %q", content, second.Choices[0].Delta.Content)
	}
}

func TestWriteSSEStream_FinishChunk(t *testing.T) {
	resp := &backend.Response{Content: "done"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	chunks := parseSSEChunks(t, rw.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(chunks))
	}
	// Third chunk is the finish chunk
	finish := chunks[2]
	if len(finish.Choices) == 0 {
		t.Fatal("finish chunk has no choices")
	}
	if finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason: want %q, got %v", "stop", finish.Choices[0].FinishReason)
	}
}

func TestWriteSSEStream_ConsistentID(t *testing.T) {
	resp := &backend.Response{Content: "test"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	chunks := parseSSEChunks(t, rw.Body.String())
	if len(chunks) < 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	id := chunks[0].ID
	if id == "" {
		t.Fatal("chunk ID is empty")
	}
	for i, c := range chunks {
		if c.ID != id {
			t.Errorf("chunk %d ID mismatch: want %q, got %q", i, id, c.ID)
		}
	}
}

func TestWriteSSEStream_ObjectField(t *testing.T) {
	resp := &backend.Response{Content: "x"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "my-model", resp)

	chunks := parseSSEChunks(t, rw.Body.String())
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object: want %q, got %q", i, "chat.completion.chunk", c.Object)
		}
		if c.Model != "my-model" {
			t.Errorf("chunk %d model: want %q, got %q", i, "my-model", c.Model)
		}
	}
}

func TestWriteSSEStream_EmptyContent(t *testing.T) {
	resp := &backend.Response{Content: ""}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	// Must still emit all 4 data: lines
	chunks := parseSSEChunks(t, rw.Body.String())
	if len(chunks) != 3 {
		t.Errorf("expected 3 parsed chunks (not counting [DONE]), got %d", len(chunks))
	}
	body := rw.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("missing [DONE] terminator")
	}
}

func TestWriteSSEStream_XAccelBufferingHeader(t *testing.T) {
	resp := &backend.Response{Content: "hi"}
	rw := httptest.NewRecorder()
	writeSSEStream(rw, "m", resp)

	if got := rw.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering: want %q, got %q", "no", got)
	}
}

// --- parseCallLogFilter ---

func TestParseCallLogFilter_Empty(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs", nil)
	f, err := parseCallLogFilter(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.BackendID != "" || !f.From.IsZero() || !f.To.IsZero() {
		t.Errorf("expected zero filter, got %+v", f)
	}
}

func TestParseCallLogFilter_BackendAndLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs?backend=b1&limit=25", nil)
	f, err := parseCallLogFilter(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.BackendID != "b1" {
		t.Errorf("BackendID: want %q, got %q", "b1", f.BackendID)
	}
	if f.Limit != 25 {
		t.Errorf("Limit: want 25, got %d", f.Limit)
	}
}

func TestParseCallLogFilter_FromTo(t *testing.T) {
	from := "2026-01-01T00:00:00Z"
	to := "2026-01-02T00:00:00Z"
	r := httptest.NewRequest("GET", "/logs?from="+from+"&to="+to, nil)
	f, err := parseCallLogFilter(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.From.IsZero() {
		t.Error("From should not be zero")
	}
	if f.To.IsZero() {
		t.Error("To should not be zero")
	}
	if !f.To.After(f.From) {
		t.Errorf("To (%v) should be after From (%v)", f.To, f.From)
	}
}

func TestParseCallLogFilter_InvalidFrom(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs?from=not-a-time", nil)
	_, err := parseCallLogFilter(r)
	if err == nil {
		t.Error("expected error for invalid from, got nil")
	}
}

func TestParseCallLogFilter_InvalidTo(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs?to=bad", nil)
	_, err := parseCallLogFilter(r)
	if err == nil {
		t.Error("expected error for invalid to, got nil")
	}
}

func TestParseCallLogFilter_ToBeforeFrom(t *testing.T) {
	from := "2026-01-02T00:00:00Z"
	to := "2026-01-01T00:00:00Z" // earlier
	r := httptest.NewRequest("GET", "/logs?from="+from+"&to="+to, nil)
	_, err := parseCallLogFilter(r)
	if err == nil {
		t.Error("expected error when to <= from, got nil")
	}
}

func TestParseCallLogFilter_InvalidLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs?limit=notanumber", nil)
	_, err := parseCallLogFilter(r)
	if err == nil {
		t.Error("expected error for non-integer limit, got nil")
	}
}

func TestParseCallLogFilter_NegativeLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/logs?limit=-5", nil)
	_, err := parseCallLogFilter(r)
	if err == nil {
		t.Error("expected error for negative limit, got nil")
	}
}

// --- helpers ---

// parseSSEChunks scans an SSE body and returns all parsed chatCompletionChunk
// objects (skipping the [DONE] line).
func parseSSEChunks(t *testing.T, body string) []chatCompletionChunk {
	t.Helper()
	var chunks []chatCompletionChunk
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var c chatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("unmarshal SSE chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks
}
