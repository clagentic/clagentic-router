// internal/backend/gemini_cli_test.go — unit tests for GeminiCLIAdapter.
//
// Uses a fake gemini binary (shell script) written to a temp dir that records
// its argv to a file and returns a synthetic success or failure payload.
// Same pattern as cli_model_passthrough_test.go.
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// geminiSuccess builds a valid gemini --output-format json success payload.
func geminiSuccessPayload(response, model string, inputTokens, candidateTokens int) []byte {
	out := geminiOutput{
		SessionID: "test-session",
		Response:  response,
		Stats: geminiOutputStats{
			Models: map[string]geminiModelStats{
				model: {
					Tokens: geminiTokenCounts{
						Input:      inputTokens,
						Candidates: candidateTokens,
						Total:      inputTokens + candidateTokens,
					},
				},
			},
		},
	}
	data, _ := json.Marshal(out)
	return data
}

// TestGeminiCLI_ModelPassthrough verifies that GeminiCLIAdapter passes the model
// string to -m verbatim and the prompt to -p.
func TestGeminiCLI_ModelPassthrough(t *testing.T) {
	cases := []struct {
		name  string
		model string
	}{
		{"full name flash", "gemini-2.5-flash"},
		{"full name pro", "gemini-2.5-pro"},
		{"short alias flash", "flash"},
		{"short alias pro", "pro"},
		{"empty model", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			modelKey := tc.model
			if modelKey == "" {
				modelKey = "gemini-2.5-flash" // stats key when model unspecified
			}
			payload := geminiSuccessPayload("hello from gemini", modelKey, 10, 5)
			binPath := writeFakeBin(t, dir, "gemini", string(payload))

			adapter := NewGeminiCLIAdapter("test", tc.model, binPath)
			req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

			resp, err := adapter.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.Content != "hello from gemini" {
				t.Errorf("unexpected content: %q", resp.Content)
			}

			args := readArgs(t, dir, "gemini")

			// -p flag must be present with the prompt
			promptVal := findFlag(args, "-p")
			if promptVal == "" {
				t.Error("-p flag missing or has no value")
			}
			if !strings.Contains(promptVal, "ping") {
				t.Errorf("-p value %q does not contain the prompt", promptVal)
			}

			// --output-format json must always be present
			fmtVal := findFlag(args, "--output-format")
			if fmtVal != "json" {
				t.Errorf("--output-format %q, want json", fmtVal)
			}

			// -m flag
			gotModel := findFlag(args, "-m")
			if tc.model == "" {
				for _, a := range args {
					if a == "-m" {
						t.Error("-m flag present but model is empty")
					}
				}
			} else {
				if gotModel != tc.model {
					t.Errorf("-m %q, want %q", gotModel, tc.model)
				}
			}
		})
	}
}

// TestGeminiCLI_TokenCounts verifies token counts are read from stats.models map.
func TestGeminiCLI_TokenCounts(t *testing.T) {
	dir := t.TempDir()
	payload := geminiSuccessPayload("the answer", "gemini-2.5-flash", 42, 17)
	binPath := writeFakeBin(t, dir, "gemini", string(payload))

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "what is 6x7?"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.PromptTokensEst != 42 {
		t.Errorf("PromptTokensEst = %d, want 42", resp.PromptTokensEst)
	}
	if resp.CompletionTokensEst != 17 {
		t.Errorf("CompletionTokensEst = %d, want 17", resp.CompletionTokensEst)
	}
}

// TestGeminiCLI_EmptyResponse verifies that an empty response field returns ErrTypeSchema.
func TestGeminiCLI_EmptyResponse(t *testing.T) {
	dir := t.TempDir()
	out := geminiOutput{
		SessionID: "s",
		Response:  "",
		Stats:     geminiOutputStats{Models: map[string]geminiModelStats{}},
	}
	payload, _ := json.Marshal(out)
	binPath := writeFakeBin(t, dir, "gemini", string(payload))

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "hi"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty response, got nil")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if ie.Type != ErrTypeSchema {
		t.Errorf("error type = %q, want %q", ie.Type, ErrTypeSchema)
	}
}

// TestGeminiCLI_NonZeroExit verifies that a non-zero exit code is classified correctly.
func TestGeminiCLI_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	// Write a fake binary that exits 1 with an error message on stderr.
	argsFile := filepath.Join(dir, "gemini.args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"printf 'rate limit exceeded\\n' >&2\n" +
		"exit 1\n"
	binPath := filepath.Join(dir, "gemini")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "hi"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("expected *InvokeError, got %T", err)
	}
	if ie.Type != ErrTypeRateLimit {
		t.Errorf("error type = %q, want %q", ie.Type, ErrTypeRateLimit)
	}
}

// TestGeminiCLI_SystemPromptPrepended verifies that a system message is prepended
// to the user prompt in the -p flag value.
//
// This test writes a custom fake binary that captures the -p value to a dedicated
// file rather than using writeFakeBin/readArgs, because the prompt contains embedded
// newlines (\n\n separator) that the per-arg newline-split pattern cannot handle.
func TestGeminiCLI_SystemPromptPrepended(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	payload := geminiSuccessPayload("ok", "gemini-2.5-flash", 5, 2)

	// Script: iterate args looking for -p, write its value to promptFile, then emit payload.
	// Uses a loop so argument order does not matter.
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"-p\" ]; then\n" +
		"    printf '%s' \"$2\" > " + promptFile + "\n" +
		"    shift 2\n" +
		"  else\n" +
		"    shift\n" +
		"  fi\n" +
		"done\n" +
		"printf '%s' " + shellQuote(string(payload)) + "\n"
	binPath := filepath.Join(dir, "gemini")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "Hello"},
	}}

	_, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	promptBytes, readErr := os.ReadFile(promptFile)
	if readErr != nil {
		t.Fatalf("read prompt file: %v", readErr)
	}
	promptVal := string(promptBytes)

	if !strings.Contains(promptVal, "System: You are a helpful assistant.") {
		t.Errorf("-p value %q missing system prefix", promptVal)
	}
	if !strings.Contains(promptVal, "Hello") {
		t.Errorf("-p value %q missing user prompt", promptVal)
	}
	// System prefix must come before user content.
	sysIdx := strings.Index(promptVal, "System:")
	userIdx := strings.Index(promptVal, "Hello")
	if sysIdx > userIdx {
		t.Errorf("system prefix appears after user prompt in -p value")
	}
}

// TestGeminiCLI_MultiModelStats verifies that token counts are summed across
// multiple model entries in the stats.models map.
func TestGeminiCLI_MultiModelStats(t *testing.T) {
	dir := t.TempDir()
	out := geminiOutput{
		SessionID: "s",
		Response:  "multi model answer",
		Stats: geminiOutputStats{
			Models: map[string]geminiModelStats{
				"gemini-2.5-flash": {
					Tokens: geminiTokenCounts{Input: 30, Candidates: 10, Total: 40},
				},
				"gemini-2.5-flash-thinking": {
					Tokens: geminiTokenCounts{Input: 5, Candidates: 3, Total: 8},
				},
			},
		},
	}
	payload, _ := json.Marshal(out)
	binPath := writeFakeBin(t, dir, "gemini", string(payload))

	adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "hi"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.PromptTokensEst != 35 {
		t.Errorf("PromptTokensEst = %d, want 35 (30+5)", resp.PromptTokensEst)
	}
	if resp.CompletionTokensEst != 13 {
		t.Errorf("CompletionTokensEst = %d, want 13 (10+3)", resp.CompletionTokensEst)
	}
}
