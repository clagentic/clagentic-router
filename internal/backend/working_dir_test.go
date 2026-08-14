// internal/backend/working_dir_test.go — cross-adapter tests for
// Request.WorkingDir handling (lr-009423).
//
// Covers, for all four subprocess adapters (claude_cli, codex_cli,
// codex_subagent, gemini_cli):
//   - WorkingDir present: cmd.Dir is set to it (subprocess actually runs
//     with that cwd, verified via `pwd`).
//   - WorkingDir absent: cmd.Dir defaults to DefaultWorkingDir ("/").
//
// Uses the fake-binary-argv pattern from cli_model_passthrough_test.go, with
// an added `pwd` line so the fake binary also records its own cwd.
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeBinWithCwd is writeFakeBin plus a line that records the
// subprocess's working directory (via `pwd`) to cwdFile.
func writeFakeBinWithCwd(t *testing.T, dir, name, stdout, cwdFile string) string {
	t.Helper()
	argsFile := filepath.Join(dir, name+".args")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"pwd > " + cwdFile + "\n" +
		"printf '%s' " + shellQuote(stdout) + "\n"
	binPath := filepath.Join(dir, name)
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return binPath
}

// readCwd reads the cwd recorded by writeFakeBinWithCwd, trimming the
// trailing newline `pwd` emits.
func readCwd(t *testing.T, cwdFile string) string {
	t.Helper()
	data, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("read cwd file: %v", err)
	}
	s := string(data)
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return s
}

func TestClaudeCLI_WorkingDir(t *testing.T) {
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}

	t.Run("present sets cmd.Dir", func(t *testing.T) {
		dir := t.TempDir()
		wantCwd := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "claude", string(claudeSuccess()), cwdFile)

		adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, nil)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: wantCwd}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != resolveSymlinks(t, wantCwd) {
			t.Errorf("subprocess cwd = %q, want %q", got, wantCwd)
		}
	})

	t.Run("absent defaults to /", func(t *testing.T) {
		dir := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "claude", string(claudeSuccess()), cwdFile)

		adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, nil)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != "/" {
			t.Errorf("subprocess cwd = %q, want /", got)
		}
	})
}

func TestCodexCLI_WorkingDir(t *testing.T) {
	t.Run("present sets cmd.Dir", func(t *testing.T) {
		dir := t.TempDir()
		wantCwd := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "codex", "response from codex", cwdFile)

		adapter := NewCodexCLIAdapter("test", "", "", "", "", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: wantCwd}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != resolveSymlinks(t, wantCwd) {
			t.Errorf("subprocess cwd = %q, want %q", got, wantCwd)
		}
	})

	t.Run("absent defaults to /", func(t *testing.T) {
		dir := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "codex", "response from codex", cwdFile)

		adapter := NewCodexCLIAdapter("test", "", "", "", "", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != "/" {
			t.Errorf("subprocess cwd = %q, want / (previously this adapter inherited the daemon cwd — this is the fix)", got)
		}
	})
}

func TestCodexSubagent_WorkingDir(t *testing.T) {
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}

	t.Run("present sets cmd.Dir", func(t *testing.T) {
		dir := t.TempDir()
		wantCwd := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "claude", string(claudeSuccess()), cwdFile)

		adapter := NewCodexSubagentAdapter("test", "flagship", binPath, nil)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: wantCwd}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != resolveSymlinks(t, wantCwd) {
			t.Errorf("subprocess cwd = %q, want %q", got, wantCwd)
		}
	})

	t.Run("absent defaults to /", func(t *testing.T) {
		dir := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		binPath := writeFakeBinWithCwd(t, dir, "claude", string(claudeSuccess()), cwdFile)

		adapter := NewCodexSubagentAdapter("test", "flagship", binPath, nil)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != "/" {
			t.Errorf("subprocess cwd = %q, want /", got)
		}
	})
}

func TestGeminiCLI_WorkingDir(t *testing.T) {
	t.Run("present sets cmd.Dir", func(t *testing.T) {
		dir := t.TempDir()
		wantCwd := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		payload := geminiSuccessPayload("hello from gemini", "gemini-2.5-flash", 10, 5)
		binPath := writeFakeBinWithCwd(t, dir, "gemini", string(payload), cwdFile)

		adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}, WorkingDir: wantCwd}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != resolveSymlinks(t, wantCwd) {
			t.Errorf("subprocess cwd = %q, want %q", got, wantCwd)
		}
	})

	t.Run("absent defaults to /", func(t *testing.T) {
		dir := t.TempDir()
		cwdFile := filepath.Join(dir, "cwd.txt")
		payload := geminiSuccessPayload("hello from gemini", "gemini-2.5-flash", 10, 5)
		binPath := writeFakeBinWithCwd(t, dir, "gemini", string(payload), cwdFile)

		adapter := NewGeminiCLIAdapter("test", "gemini-2.5-flash", binPath)
		req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

		if _, err := adapter.Invoke(context.Background(), req); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		got := readCwd(t, cwdFile)
		if got != "/" {
			t.Errorf("subprocess cwd = %q, want / (previously this adapter inherited the daemon cwd — this is the fix)", got)
		}
	})
}

// resolveSymlinks resolves symlinks in path (e.g. macOS /tmp -> /private/tmp,
// or Linux systems where t.TempDir() lands under a symlinked base) so the
// comparison against the subprocess's `pwd` output (which shells resolve to
// the real path) is exact rather than string-literal.
func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}
