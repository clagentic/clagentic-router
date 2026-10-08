// internal/backend/subprocess.go — the one place CLI adapters construct their
// subprocess, so the kill bound is the same for all of them.
package backend

import (
	"context"
	"os/exec"
	"time"
)

// SubprocessWaitDelay bounds how long cmd.Run waits for the subprocess's
// output pipes to close after its context is done.
//
// exec.CommandContext kills only the direct child. A CLI that forks helpers
// (a shell wrapper, an MCP server, a language runtime) leaves grandchildren
// holding the inherited stdout/stderr open, and without a WaitDelay cmd.Run
// blocks until they exit, which can be far past the deadline: the per-backend
// timeout Route applies would then be a no-op for exactly the adapters that
// need it. After this delay Run force-closes the pipes and returns.
//
// 3 s is long enough for a killed child's final buffered output to drain and
// short enough that a deadline overrun stays negligible next to the 180 s
// default timeout. Trade-off: a still-running grandchild is orphaned rather
// than reaped; this bounds the request, not the stray process.
//
// When the delay fires, Run returns an error (the kill's *exec.ExitError, or
// exec.ErrWaitDelay if the child had exited cleanly), so a deadline kill still
// reaches IsContextDeadlineKill as a non-nil error and classifies as timeout.
const SubprocessWaitDelay = 3 * time.Second

// newBoundedCommand is exec.CommandContext plus SubprocessWaitDelay. Every CLI
// adapter must build its subprocess here rather than calling
// exec.CommandContext directly.
func newBoundedCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = SubprocessWaitDelay
	return cmd
}
