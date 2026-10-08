// internal/router/causes.go — the sentinel causes every deadline or cancel in
// the request path is created with.
//
// Outcomes are attributed once, from context.Cause of the innermost context
// (the first event wins, and the cause is recorded at the event), never from
// which context is observed done afterwards or from the clock at report time.
// Each sentinel names exactly one deadline owner, so a failure can be charged
// to the right party: the backend, the chain, the operator's request cap, or
// the passthrough work/write phases.
package router

import (
	"context"
	"errors"
)

// deadlineCause is a cause whose context expired by deadline rather than by
// explicit cancel. It reports Is(context.DeadlineExceeded) so callers that only
// care "was this a deadline" keep working, while errors.Is against a specific
// sentinel still discriminates between owners.
type deadlineCause string

func (e deadlineCause) Error() string { return string(e) }

// Is makes every deadline cause match context.DeadlineExceeded.
func (e deadlineCause) Is(target error) bool { return target == context.DeadlineExceeded }

var (
	// ErrBackendTimeout: one backend's timeout_seconds elapsed. The backend's
	// fault: health penalty, and the chain may advance if the request is live.
	ErrBackendTimeout error = deadlineCause("backend timeout_seconds exceeded")

	// ErrChainBudget: the request's chain-derived work deadline elapsed while
	// the backend was running. The backend ran out the budget the chain gave
	// it, so it is charged as a timeout, but nothing is left for a next tier.
	ErrChainBudget error = deadlineCause("chain budget exhausted")

	// ErrRequestCap: proxy.max_request_seconds capped the request below what
	// the chain needed. The operator's bound, not the backend's fault: no
	// penalty.
	ErrRequestCap error = deadlineCause("proxy.max_request_seconds reached")

	// ErrWorkDeadline: a passthrough request's phase-1 work deadline (a delivery
	// margin before the write deadline) elapsed.
	ErrWorkDeadline error = deadlineCause("request work deadline reached")

	// ErrWriteDeadline: a passthrough request's write deadline elapsed while the
	// body was being relayed.
	ErrWriteDeadline error = deadlineCause("request write deadline reached")
)

// CauseLabel renders a context cause as a stable, greppable label for logs, so
// the router's own deadlines are distinguishable from upstream failure and
// client disconnect.
func CauseLabel(cause error) string {
	switch {
	case cause == nil:
		return "none"
	case errors.Is(cause, ErrBackendTimeout):
		return "backend_timeout"
	case errors.Is(cause, ErrChainBudget):
		return "chain_budget"
	case errors.Is(cause, ErrRequestCap):
		return "request_cap"
	case errors.Is(cause, ErrWorkDeadline):
		return "work_deadline"
	case errors.Is(cause, ErrWriteDeadline):
		return "write_deadline"
	case errors.Is(cause, context.Canceled):
		return "client_cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return cause.Error()
	}
}
