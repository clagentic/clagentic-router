// internal/server/chain_exhausted_error_test.go — tests for the type-only
// last_error_type propagation on chain exhaustion (lr-807319).
//
// writeChainExhaustedError / writeAnthropicChainExhaustedError are pure
// response writers; tested directly against httptest.NewRecorder without a
// full Handler/router, mirroring handlers_test.go's existing pattern for
// other response-shape assertions.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteChainExhaustedError_CarriesLastErrorType(t *testing.T) {
	rw := httptest.NewRecorder()
	writeChainExhaustedError(rw, "auth")

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status: want %d, got %d", http.StatusServiceUnavailable, rw.Code)
	}

	var resp errorResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rw.Body.String())
	}
	if resp.Error.Code != "backends_unavailable" {
		t.Errorf("Error.Code = %q, want %q", resp.Error.Code, "backends_unavailable")
	}
	if resp.Error.LastErrorType != "auth" {
		t.Errorf("Error.LastErrorType = %q, want %q", resp.Error.LastErrorType, "auth")
	}
	// The generic message must remain unchanged — no raw backend detail leaks.
	if resp.Error.Message != "no available backends in chain" {
		t.Errorf("Error.Message = %q, want the unchanged generic message", resp.Error.Message)
	}
}

func TestWriteChainExhaustedError_EmptyTypeOmitted(t *testing.T) {
	rw := httptest.NewRecorder()
	writeChainExhaustedError(rw, "")

	body := rw.Body.String()
	if strings.Contains(body, "last_error_type") {
		t.Errorf("expected last_error_type to be omitted (omitempty) when unknown, body=%s", body)
	}
}

func TestWriteAnthropicChainExhaustedError_CarriesLastErrorType(t *testing.T) {
	rw := httptest.NewRecorder()
	writeAnthropicChainExhaustedError(rw, "quota")

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status: want %d, got %d", http.StatusServiceUnavailable, rw.Code)
	}

	var resp anthropicMsgError
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rw.Body.String())
	}
	if resp.Error.LastErrorType != "quota" {
		t.Errorf("Error.LastErrorType = %q, want %q", resp.Error.LastErrorType, "quota")
	}
	if resp.Error.Message != "no available backends in chain" {
		t.Errorf("Error.Message = %q, want the unchanged generic message", resp.Error.Message)
	}
}

func TestWriteAnthropicChainExhaustedError_EmptyTypeOmitted(t *testing.T) {
	rw := httptest.NewRecorder()
	writeAnthropicChainExhaustedError(rw, "")

	body := rw.Body.String()
	if strings.Contains(body, "last_error_type") {
		t.Errorf("expected last_error_type to be omitted (omitempty) when unknown, body=%s", body)
	}
}

// TestWriteChainExhaustedError_TimeoutCarriesLastErrorType covers AC4 for
// the OpenAI-shaped 503 response (lr-2f35bd): a chain exhausted by timeouts
// must carry last_error_type="timeout".
func TestWriteChainExhaustedError_TimeoutCarriesLastErrorType(t *testing.T) {
	rw := httptest.NewRecorder()
	writeChainExhaustedError(rw, "timeout")

	var resp errorResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rw.Body.String())
	}
	if resp.Error.LastErrorType != "timeout" {
		t.Errorf("Error.LastErrorType = %q, want %q (AC4)", resp.Error.LastErrorType, "timeout")
	}
}

// TestWriteAnthropicChainExhaustedError_TimeoutCarriesLastErrorType is the
// Anthropic-shaped analogue of the above, and additionally confirms the
// blanket overloaded_error status-based mapping is UNCHANGED for a timeout
// cause — only the new model_config category gets a different error.Type
// (see the next test).
func TestWriteAnthropicChainExhaustedError_TimeoutCarriesLastErrorType(t *testing.T) {
	rw := httptest.NewRecorder()
	writeAnthropicChainExhaustedError(rw, "timeout")

	var resp anthropicMsgError
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rw.Body.String())
	}
	if resp.Error.LastErrorType != "timeout" {
		t.Errorf("Error.LastErrorType = %q, want %q (AC4)", resp.Error.LastErrorType, "timeout")
	}
	if resp.Error.Type != "overloaded_error" {
		t.Errorf("Error.Type = %q, want %q — a timeout cause keeps the existing status-based mapping unchanged", resp.Error.Type, "overloaded_error")
	}
}

// TestWriteAnthropicChainExhaustedError_ModelConfig_NotOverloadedError
// covers B5's acceptance point directly: a chain exhausted because every
// candidate backend is offline for a locally misconfigured model identifier
// must NOT surface as error.type="overloaded_error" — that reads as
// transient capacity exhaustion ("retry later"), which is actively
// misleading for a fault only an operator editing router.yaml can fix.
func TestWriteAnthropicChainExhaustedError_ModelConfig_NotOverloadedError(t *testing.T) {
	rw := httptest.NewRecorder()
	writeAnthropicChainExhaustedError(rw, "model_config")

	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("status: want %d, got %d — chain exhaustion is still a 503 regardless of error.type", http.StatusServiceUnavailable, rw.Code)
	}

	var resp anthropicMsgError
	if err := json.Unmarshal(rw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rw.Body.String())
	}
	if resp.Error.Type == "overloaded_error" {
		t.Error("Error.Type = overloaded_error for a model_config chain exhaustion — must not read as transient capacity (B5)")
	}
	if resp.Error.LastErrorType != "model_config" {
		t.Errorf("Error.LastErrorType = %q, want %q", resp.Error.LastErrorType, "model_config")
	}
}
