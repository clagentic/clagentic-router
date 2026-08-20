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
