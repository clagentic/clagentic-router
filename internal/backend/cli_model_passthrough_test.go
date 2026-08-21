// internal/backend/cli_model_passthrough_test.go — tests that CLI adapters pass model
// strings through to the underlying CLI verbatim, without router-level transformation.
//
// This matters because model alias resolution (e.g. "claude-sonnet" → current default
// Sonnet version) is delegated entirely to the provider CLIs. The router must not
// intercept, normalize, or cache model strings — it hands them through as-is.
//
// Tests use a fake binary written to a temp dir that records its argv to a file,
// then exits with a synthetic success payload the adapter can parse.
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBin writes a shell script that records its own argv to argsFile
// and then writes output to stdout, then makes it executable.
// For claude_cli: output is a valid claude JSON response.
// For codex_cli: output is plain text (codex writes plain stdout).
func writeFakeBin(t *testing.T, dir, name, stdout string) string {
	t.Helper()
	argsFile := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"printf '%s' " + shellQuote(stdout) + "\n"
	binPath := filepath.Join(dir, name)
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return binPath
}

// shellQuote wraps s in single quotes, escaping any single quotes inside.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// readArgs reads the args file written by writeFakeBin. Each arg is on its own line.
func readArgs(t *testing.T, dir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".args"))
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var args []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			args = append(args, line)
		}
	}
	return args
}

// findFlag finds the value following flag in args, or returns "".
func findFlag(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestClaudeCLI_ModelPassthrough verifies that ClaudeCLIAdapter passes the model
// string to --model verbatim — both a pinned version and a family alias.
func TestClaudeCLI_ModelPassthrough(t *testing.T) {
	claudeSuccess := func() []byte {
		out := claudeOutput{
			Type:    "result",
			Result:  "hello",
			CostUSD: 0.001,
		}
		data, _ := json.Marshal(out)
		return data
	}

	cases := []struct {
		name  string
		model string
	}{
		{"pinned version", "claude-sonnet-4-6"},
		{"family alias sonnet", "claude-sonnet"},
		{"family alias haiku", "claude-haiku"},
		{"family alias opus", "claude-opus"},
		// Empty model omits --model flag — also pass-through (no forced default).
		{"empty model", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

			adapter := NewClaudeCLIAdapter("test", tc.model, binPath, "", ThinkingOff, 0)
			req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

			resp, err := adapter.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.Content != "hello" {
				t.Errorf("unexpected content: %q", resp.Content)
			}

			args := readArgs(t, dir, "claude")

			// --verbose must always be present when using --output-format stream-json
			// with --print (required since claude CLI 2.1.173). (lr-1994)
			hasVerbose := false
			for _, a := range args {
				if a == "--verbose" {
					hasVerbose = true
					break
				}
			}
			if !hasVerbose {
				t.Error("--verbose flag missing; required since claude 2.1.173 for stream-json+print mode")
			}

			got := findFlag(args, "--model")
			if tc.model == "" {
				// --model flag should be absent when model is empty
				for _, a := range args {
					if a == "--model" {
						t.Error("--model flag present but model is empty")
					}
				}
			} else {
				if got != tc.model {
					t.Errorf("--model %q passed to CLI, want %q", got, tc.model)
				}
			}
		})
	}
}

// TestCodexCLI_ModelPassthrough verifies that CodexCLIAdapter passes the model
// string to --model verbatim, delegating any resolution to the codex CLI.
func TestCodexCLI_ModelPassthrough(t *testing.T) {
	cases := []struct {
		name  string
		model string
	}{
		{"pinned o4-mini", "o4-mini"},
		{"pinned o3", "o3"},
		{"pinned o3-pro", "o3-pro"},
		{"empty model", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// codex writes plain text stdout
			binPath := writeFakeBin(t, dir, "codex", "response from codex")

			adapter := NewCodexCLIAdapter("test", tc.model, "", "", "", binPath)
			req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

			resp, err := adapter.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.Content != "response from codex" {
				t.Errorf("unexpected content: %q", resp.Content)
			}

			args := readArgs(t, dir, "codex")

			got := findFlag(args, "--model")
			if tc.model == "" {
				for _, a := range args {
					if a == "--model" {
						t.Error("--model flag present but model is empty")
					}
				}
			} else {
				if got != tc.model {
					t.Errorf("--model %q passed to CLI, want %q", got, tc.model)
				}
			}

			// --json is required for the in-band failure surface and
			// cache/token metrics wiring (lr-a40da5) — must be present on
			// every invocation, not conditional on any flag.
			hasJSON := false
			for _, a := range args {
				if a == "--json" {
					hasJSON = true
					break
				}
			}
			if !hasJSON {
				t.Error("--json flag missing; required for in-band failure detection and cache metrics (lr-a40da5)")
			}
		})
	}
}
