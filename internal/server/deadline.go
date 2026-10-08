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
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// defaultBackstopWriteTimeout bounds every endpoint that does not set its own
// deadline (health, admin, metrics, version).
const defaultBackstopWriteTimeout = 300 * time.Second

// Option customizes New.
type Option func(*serverOptions)

type serverOptions struct {
	backstopWriteTimeout time.Duration
}

// withBackstopWriteTimeout overrides the http.Server WriteTimeout. Unexported:
// it exists so tests can use millisecond-scale backstops, not as an operator
// knob (the operator knob is proxy.max_request_seconds).
func withBackstopWriteTimeout(d time.Duration) Option {
	return func(o *serverOptions) { o.backstopWriteTimeout = d }
}

// extendWriteDeadline moves this request's connection write deadline to
// now+d. The deadline is absolute from now, so an actively-flowing stream is
// cut only when d elapses, never by the 300 s backstop. A ResponseWriter that
// cannot set deadlines (e.g. httptest.ResponseRecorder) is left as-is: there
// is no deadline to extend.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration, requestID string) {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
	if err != nil && !isNotSupported(err) {
		slog.Warn("server: could not set per-request write deadline", "err", err, "request_id", requestID)
	}
}

func isNotSupported(err error) bool {
	return errors.Is(err, http.ErrNotSupported)
}

// deliveryWriter records the first error returned while writing the response
// so a handler can report a failed delivery after a successful Route.
type deliveryWriter struct {
	http.ResponseWriter
	err error
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
		slog.Warn("server: response delivery failed after successful route",
			"err", d.err, "request_id", requestID, "backend", backendID,
			"elapsed_ms", time.Since(start).Milliseconds())
	}
}
