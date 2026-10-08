// exec.CommandContext kills only the direct child. When a CLI leaves a
// grandchild holding its stdout open, cmd.Run blocks until that grandchild
// exits, so the per-backend deadline Route applies does not actually return
// control for the CLI adapters. These tests give every adapter such a fixture
// and require Invoke to come back within deadline + SubprocessWaitDelay, still
// classified as a timeout. Driving each real adapter (rather than asserting a
// field on a command) is what proves all four use the shared bound.
package backend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// writeGrandchildBin writes a script that backgrounds a long sleep (the
// grandchild inherits stdout/stderr) and waits on it. Killing the script's
// own PID leaves the sleep holding the pipes open for 6 s, longer than the
// deadline plus SubprocessWaitDelay plus slack used below.
func writeGrandchildBin(t *testing.T, dir, name string) string {
	t.Helper()
	script := "#!/bin/sh\nsleep 6 &\nwait\n"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatalf("write grandchild bin: %v", err)
	}
	return p
}

func TestCLIAdapters_GrandchildHoldingPipes_InvokeReturnsWithinWaitDelay(t *testing.T) {
	const deadline = 150 * time.Millisecond
	const slack = 1500 * time.Millisecond
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	cases := []struct {
		name  string
		build func(bin string) Adapter
	}{
		{"claude_cli", func(bin string) Adapter {
			return NewClaudeCLIAdapter("claude-test", "claude-sonnet-4-6", bin, "", ThinkingOff, 0)
		}},
		{"codex_cli", func(bin string) Adapter {
			return NewCodexCLIAdapter("codex-test", "o4-mini", "", "", "", bin)
		}},
		{"codex_subagent", func(bin string) Adapter {
			return NewCodexSubagentAdapter("codex-subagent-test", "flagship", bin, 0)
		}},
		{"gemini_cli", func(bin string) Adapter {
			return NewGeminiCLIAdapter("gemini-test", "gemini-2.5-flash", bin)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := writeGrandchildBin(t, t.TempDir(), tc.name)
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()

			start := time.Now()
			_, err := tc.build(bin).Invoke(ctx, req)
			elapsed := time.Since(start)

			if elapsed > deadline+SubprocessWaitDelay+slack {
				t.Fatalf("Invoke returned after %s, want within deadline %s + WaitDelay %s + slack (grandchild held the pipes)",
					elapsed, deadline, SubprocessWaitDelay)
			}
			ie, ok := err.(*InvokeError)
			if !ok || ie.Type != ErrTypeTimeout {
				t.Fatalf("err = %#v, want *InvokeError{Type: timeout}", err)
			}
		})
	}
}

// A WaitDelay expiry returns a non-nil error (ExitError after a kill,
// ErrWaitDelay after a clean exit), so a deadline kill keeps classifying as a
// timeout instead of being mistaken for success.
func TestIsContextDeadlineKill_TrueForWaitDelayErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	for _, err := range []error{context.DeadlineExceeded, exec.ErrWaitDelay} {
		if !IsContextDeadlineKill(ctx, err) {
			t.Errorf("IsContextDeadlineKill(expired ctx, %v) = false, want true", err)
		}
	}
}
