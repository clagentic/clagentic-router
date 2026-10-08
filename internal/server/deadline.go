// internal/server/deadline.go — per-request write deadlines and delivery
// observability for the LLM endpoints.
//
// http.Server.WriteTimeout is an absolute connection write deadline armed when
// request headers are read; it does not cancel r.Context(), so a handler that
// outlives it keeps running, finishes its backend call, and then fails to
// write the response (the client sees an empty reply). One global value cannot
// be right for both /health and a multi-tier reviewer chain, so the server
// keeps WriteTimeout as a backstop for non-LLM endpoints and every LLM handler
// replaces it for its own connection via http.ResponseController.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/clagentic/clagentic-router/internal/router"
)

// defaultBackstopWriteTimeout bounds every endpoint that does not set its own
// deadline (health, admin, metrics, version).
const defaultBackstopWriteTimeout = 300 * time.Second

// setWriteDeadline moves this request's connection write deadline to the
// absolute instant at. An actively-flowing stream is therefore cut only when
// at passes, never by the 300 s backstop. A ResponseWriter that cannot set
// deadlines (e.g. httptest.ResponseRecorder) is left as-is: there is no
// deadline to extend.
func setWriteDeadline(w http.ResponseWriter, at time.Time, requestID string) {
	err := http.NewResponseController(w).SetWriteDeadline(at)
	if err != nil && !isNotSupported(err) {
		slog.Warn("server: could not set per-request write deadline", "err", err, "request_id", requestID)
	}
}

func isNotSupported(err error) bool {
	return errors.Is(err, http.ErrNotSupported)
}

// routedDeadlines derives both deadlines of a routed request from one instant:
// the write deadline is start+d, and Route must finish a delivery margin
// earlier so a Route that succeeds always has time to write its response.
// When d is not larger than the margin the margin cannot be reserved in full;
// the Route deadline is then clamped to the midpoint of the request so it is
// never at or before start (an immediately-expired Route) nor at the write
// deadline.
func routedDeadlines(start time.Time, d time.Duration) (route, write time.Time) {
	write = start.Add(d)
	if d > router.DeliveryMargin {
		return write.Add(-router.DeliveryMargin), write
	}
	return start.Add(d / 2), write
}

// beginRoutedRequest arms the per-request write deadline and returns a context
// for Router.Route that expires a delivery margin before it, so work stops
// while a response can still be written instead of finishing one that is
// thrown away. The returned deadline is the write deadline; callers pass it to
// newDeliveryWriter so a late write failure is attributed correctly. The
// caller must call cancel.
func beginRoutedRequest(w http.ResponseWriter, r *http.Request, d time.Duration) (ctx context.Context, cancel context.CancelFunc, deadline time.Time) {
	return beginRoutedRequestAt(w, r, time.Now(), d)
}

// beginRoutedRequestAt is beginRoutedRequest with the handler-entry instant
// supplied, so every deadline derives from that one reading of the clock.
func beginRoutedRequestAt(w http.ResponseWriter, r *http.Request, start time.Time, d time.Duration) (ctx context.Context, cancel context.CancelFunc, deadline time.Time) {
	route, write := routedDeadlines(start, d)
	setWriteDeadline(w, write, RequestID(r.Context()))
	ctx, cancel = context.WithDeadline(r.Context(), route)
	return ctx, cancel, write
}

// beginPassthroughRequest bounds a passthrough request by d measured from
// handler entry. The write deadline and the returned context share that one
// instant, so pre-Do work (credential resolution, signing) and the upstream
// call are bounded together and the upstream cannot outlive the write
// deadline. Use the returned context for everything upstream. The caller must
// call cancel.
func beginPassthroughRequest(w http.ResponseWriter, r *http.Request, d time.Duration) (ctx context.Context, cancel context.CancelFunc) {
	return beginPassthroughRequestAt(w, r, time.Now(), d)
}

func beginPassthroughRequestAt(w http.ResponseWriter, r *http.Request, start time.Time, d time.Duration) (context.Context, context.CancelFunc) {
	deadline := start.Add(d)
	setWriteDeadline(w, deadline, RequestID(r.Context()))
	return context.WithDeadline(r.Context(), deadline)
}

// deliveryWriter records the first error returned while writing the response
// so a handler can report a failed delivery after a successful Route.
type deliveryWriter struct {
	http.ResponseWriter
	err error
	// deadline is the request deadline shared with the write deadline; zero
	// means unknown.
	deadline time.Time
}

func newDeliveryWriter(w http.ResponseWriter, deadline time.Time) *deliveryWriter {
	return &deliveryWriter{ResponseWriter: w, deadline: deadline}
}

func (d *deliveryWriter) Write(p []byte) (int, error) {
	n, err := d.ResponseWriter.Write(p)
	if err != nil && d.err == nil {
		d.err = err
	}
	return n, err
}

// Flush satisfies http.Flusher (the SSE writers type-assert for it) while
// capturing flush errors.
func (d *deliveryWriter) Flush() {
	if err := http.NewResponseController(d.ResponseWriter).Flush(); err != nil && !isNotSupported(err) && d.err == nil {
		d.err = err
	}
}

// Unwrap lets http.NewResponseController reach the real connection.
func (d *deliveryWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

// reportDelivery forces any buffered bytes onto the wire (a small response is
// otherwise only flushed after the handler returns, where a failure is
// invisible) and logs at Warn when the write failed. Backend outcome and
// delivery outcome are different facts: call_log keeps recording the former.
func (d *deliveryWriter) reportDelivery(requestID, backendID string, start time.Time) {
	d.Flush()
	if d.err != nil {
		// A failure at or past the request deadline is the router's own
		// per-request bound firing (max_request_seconds below the work done),
		// not a client disconnect or network fault.
		cause := "write_failed"
		if !d.deadline.IsZero() && !time.Now().Before(d.deadline) {
			cause = "request_deadline"
		}
		slog.Warn("server: response delivery failed after successful route",
			"err", d.err, "cause", cause, "request_id", requestID, "backend", backendID,
			"elapsed_ms", time.Since(start).Milliseconds())
	}
}
