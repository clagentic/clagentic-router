// internal/server/working_dir_test.go — boundary validation tests for the
// working_dir wire field on both POST /v1/chat/completions and
// POST /v1/messages (routed mode) (lr-009423).
//
// Invalid values (relative, nonexistent, file-not-directory) must be
// rejected with 4xx at the server boundary — never silently ignored and
// never left to fail opaquely inside a subprocess exec later. A valid
// absolute directory must be accepted (200).
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestChatCompletions_WorkingDir_ValidAbsoluteDir_Accepted(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	dir := t.TempDir()
	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":       "backend:test-backend",
		"max_tokens":  100,
		"working_dir": dir,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestChatCompletions_WorkingDir_Absent_Accepted(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	// No working_dir field at all — must not be treated as invalid; falls
	// through to the adapter-level default.
	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":      "backend:test-backend",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestChatCompletions_WorkingDir_RelativePath_Rejected(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":       "backend:test-backend",
		"max_tokens":  100,
		"working_dir": "relative/path",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
	var errBody errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Code != "invalid_request" {
		t.Errorf("error.code: want invalid_request, got %q", errBody.Error.Code)
	}
}

func TestChatCompletions_WorkingDir_NonexistentPath_Rejected(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":       "backend:test-backend",
		"max_tokens":  100,
		"working_dir": missing,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestChatCompletions_WorkingDir_FileNotDirectory_Rejected(t *testing.T) {
	ts, cleanup := newChatCompletionsTestServer(t, false)
	defer cleanup()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	resp := doChatCompletionsRequest(t, ts, "secret", map[string]interface{}{
		"model":       "backend:test-backend",
		"max_tokens":  100,
		"working_dir": filePath,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
}

// --- POST /v1/messages (routed mode) ---

func TestMessagesRouted_WorkingDir_ValidAbsoluteDir_Accepted(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	dir := t.TempDir()
	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":       "role:reviewer-chain",
		"max_tokens":  100,
		"working_dir": dir,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMessagesRouted_WorkingDir_Absent_Accepted(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":      "role:reviewer-chain",
		"max_tokens": 100,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
}

func TestMessagesRouted_WorkingDir_RelativePath_Rejected(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":       "role:reviewer-chain",
		"max_tokens":  100,
		"working_dir": "relative/path",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
	var errBody anthropicMsgError
	if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error.Type != "invalid_request_error" {
		t.Errorf("error.type: want invalid_request_error, got %q", errBody.Error.Type)
	}
}

func TestMessagesRouted_WorkingDir_NonexistentPath_Rejected(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":       "role:reviewer-chain",
		"max_tokens":  100,
		"working_dir": missing,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestMessagesRouted_WorkingDir_FileNotDirectory_Rejected(t *testing.T) {
	ts, cleanup := newMessagesTestServer(t, "http://unused.invalid")
	defer cleanup()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":       "role:reviewer-chain",
		"max_tokens":  100,
		"working_dir": filePath,
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", resp.StatusCode)
	}
}

// TestMessagesPassthrough_WorkingDir_Ignored verifies that passthrough mode
// (non role:/chain:/backend: model) never reads working_dir — the field is
// forwarded as part of the raw request bytes, not decoded or validated,
// matching the existing Tools passthrough contract.
func TestMessagesPassthrough_WorkingDir_Ignored(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer upstream.Close()

	ts, cleanup := newMessagesTestServer(t, upstream.URL)
	defer cleanup()

	// A working_dir value that would be REJECTED in routed mode must sail
	// through untouched in passthrough mode, since passthrough never
	// decodes/validates it — only forwards raw bytes upstream.
	resp := doMessagesRequest(t, ts, "x-api-key", "secret", map[string]interface{}{
		"model":       "claude-sonnet-4-6",
		"max_tokens":  100,
		"working_dir": "relative/path/would-be-rejected-in-routed-mode",
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()
	// Passthrough forwards raw bytes and never decodes working_dir, so the
	// request must succeed via the stub upstream — a 400 here would mean the
	// field leaked into routed-mode validation.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200 (passthrough must not validate working_dir), got %d", resp.StatusCode)
	}
}
