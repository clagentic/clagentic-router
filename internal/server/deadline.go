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
	"io"
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

// requestDeadlines is the single place every LLM handler path derives its
// deadlines (the request invariant): from one start instant t0 and one budget d
// it returns the write deadline W = t0+d and the work deadline C = W minus the
// delivery margin.
//
//   - Any work whose failure the handler reports by WRITING a response must be
//     bounded by C, never W, so the error response still has the margin to be
//     written before the connection's write deadline kills it.
//   - Only after the response status line is committed may work run until W
//     (copying a passthrough stream body): no error response is possible then.
//
// When d is not larger than the margin the margin cannot be reserved in full;
// C is then clamped to the midpoint t0+d/2 so it is never at or before t0 (an
// immediately-expired request) nor at W.
//
// t0 is read once per request by the caller, after the request body has been
// read (that read is bounded by the server's ReadTimeout), not at literal
// handler entry. The same t0 feeds elapsed logging and reportDelivery.
func requestDeadlines(t0 time.Time, d time.Duration) (work, write time.Time) {
	write = t0.Add(d)
	if d > router.DeliveryMargin {
		return write.Add(-router.DeliveryMargin), write
	}
	return t0.Add(d / 2), write
}

// beginRoutedRequestAt arms the per-request write deadline W and returns a
// context for Router.Route that expires at the work deadline C, so work stops
// while a response can still be written instead of finishing one that is
// thrown away. The returned deadline is W; callers pass it to newDeliveryWriter
// so a late write failure is attributed correctly. The caller must call
// cancel.
func beginRoutedRequestAt(w http.ResponseWriter, r *http.Request, t0 time.Time, d time.Duration) (ctx context.Context, cancel context.CancelFunc, deadline time.Time) {
	work, write := requestDeadlines(t0, d)
	setWriteDeadline(w, write, RequestID(r.Context()))
	ctx, cancel = context.WithDeadline(r.Context(), work)
	return ctx, cancel, write
}

// passthroughRequest is the shared two-phase bound for passthrough handlers
// (messages and bedrock). Phase 1 covers everything whose failure is reported
// by writing an error response (credential resolution, signing, the upstream
// call up to response headers): its context is cancelled at the work deadline
// C. Once the upstream headers arrive the response is committed, so phase 2
// (relaying the body) re-arms the cancel to the write deadline W.
type passthroughRequest struct {
	ctx    context.Context
	cancel context.CancelFunc
	write  time.Time
	timer  *time.Timer
}

// beginPassthroughRequestAt arms the write deadline W and returns the
// two-phase passthrough bound derived from t0 and d via requestDeadlines. Use
// ctx for everything upstream and defer Close.
func beginPassthroughRequestAt(w http.ResponseWriter, r *http.Request, t0 time.Time, d time.Duration) *passthroughRequest {
	work, write := requestDeadlines(t0, d)
	setWriteDeadline(w, write, RequestID(r.Context()))
	ctx, cancel := context.WithCancel(r.Context())
	return &passthroughRequest{
		ctx:    ctx,
		cancel: cancel,
		write:  write,
		timer:  time.AfterFunc(time.Until(work), cancel),
	}
}

// Context is the phase-1 context for upstream work.
func (p *passthroughRequest) Context() context.Context { return p.ctx }

// Close releases the timer and cancels the context.
func (p *passthroughRequest) Close() {
	p.timer.Stop()
	p.cancel()
}

// commit moves the cancel from C to W; call it once the upstream response
// headers are in hand and the status line is about to be written.
func (p *passthroughRequest) commit() {
	p.timer.Stop()
	p.timer = time.AfterFunc(time.Until(p.write), p.cancel)
}

// relay writes the upstream status line and headers, then copies the body
// until EOF, a write failure, or W. Shared by every passthrough handler so the
// phase transition lives in one place. label names the endpoint in logs.
func (p *passthroughRequest) relay(w http.ResponseWriter, up *http.Response, requestID, label string) {
	p.commit()
	for k, vals := range up.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Router-Mode", "passthrough")
	w.WriteHeader(up.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := up.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				slog.Warn(label+": passthrough stream read error", "err", rerr, "request_id", requestID)
			}
			return
		}
	}
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
