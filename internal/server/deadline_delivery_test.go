// A response can fail to deliver after Route has already succeeded, and call_log
// rightly keeps recording the backend's "pass". These tests guard that the
// failure is still surfaced at Warn, and that its cause is read from the error
// captured at the moment of failure: a report that runs later (it is deferred)
// must not relabel an early failure as a deadline expiry.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var errClientGone = errors.New("write: broken pipe")

// failingWriter is a ResponseWriter whose body writes always fail, standing in
// for a connection whose write deadline already expired.
type failingWriter struct{ header http.Header }

func (f *failingWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errClientGone }
func (f *failingWriter) WriteHeader(int)           {}

func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestDeliveryWriter_CapturesWriteFailure(t *testing.T) {
	dw := &deliveryWriter{ResponseWriter: &failingWriter{}}

	if _, err := dw.Write([]byte("x")); !errors.Is(err, errClientGone) {
		t.Fatalf("Write error = %v, want the underlying error passed through", err)
	}
	if !errors.Is(dw.err, errClientGone) {
		t.Fatalf("captured err = %v, want %v", dw.err, errClientGone)
	}

	// Only the first failure is kept.
	if _, err := dw.Write([]byte("y")); err == nil {
		t.Fatal("second write unexpectedly succeeded")
	}
	if !errors.Is(dw.err, errClientGone) {
		t.Fatalf("first captured err was overwritten: %v", dw.err)
	}
}

func TestDeliveryWriter_ReportDeliveryWarnsOnFailure(t *testing.T) {
	buf := captureWarnLog(t)
	dw := &deliveryWriter{ResponseWriter: &failingWriter{}}
	_, _ = dw.Write([]byte("x"))

	dw.reportDelivery("req-123", "backend-9", time.Now().Add(-50*time.Millisecond))

	out := buf.String()
	for _, want := range []string{
		"response delivery failed",
		"request_id=req-123",
		"backend=backend-9",
		"elapsed_ms=",
		"broken pipe",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warn log missing %q:\n%s", want, out)
		}
	}
}

// The cause is fixed by the error captured at the first failure. A write that
// fails before the write deadline W must stay write_failed even when the report
// runs after W (reportDelivery is deferred, so it always runs later than the
// failure it reports).
func TestDeliveryWriter_FailureBeforeW_ReportedAfterW_IsWriteFailed(t *testing.T) {
	buf := captureWarnLog(t)
	w := time.Now().Add(40 * time.Millisecond)
	dw := newDeliveryWriter(&failingWriter{})

	_, _ = dw.Write([]byte("x")) // fails now, before W
	time.Sleep(60 * time.Millisecond)
	if !time.Now().After(w) {
		t.Fatal("test setup: report must run after W")
	}
	dw.reportDelivery("req-1", "b", time.Now())

	if !strings.Contains(buf.String(), "cause=write_failed") {
		t.Errorf("a pre-W failure reported after W must be write_failed:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "write_deadline") {
		t.Errorf("report-time clock leaked into attribution:\n%s", buf.String())
	}
}

func TestWriteFailureCause_FromErrorIdentity(t *testing.T) {
	if got := writeFailureCause(os.ErrDeadlineExceeded); got != "write_deadline" {
		t.Errorf("os.ErrDeadlineExceeded -> %q, want write_deadline", got)
	}
	if got := writeFailureCause(fmt.Errorf("write tcp: %w", os.ErrDeadlineExceeded)); got != "write_deadline" {
		t.Errorf("wrapped os.ErrDeadlineExceeded -> %q, want write_deadline", got)
	}
	if got := writeFailureCause(errClientGone); got != "write_failed" {
		t.Errorf("broken pipe -> %q, want write_failed", got)
	}
}

// With a real http.Server, a write that fails because the connection's write
// deadline W expired must surface as os.ErrDeadlineExceeded (verifying that
// net/http propagates the net.Conn error unchanged) and be reported as
// write_deadline.
func TestDeliveryWriter_RealServer_FailureAtW_IsWriteDeadline(t *testing.T) {
	buf := captureWarnLog(t)
	reported := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(reported)
		t0 := time.Now()
		setWriteDeadline(w, t0.Add(100*time.Millisecond), "req-w")
		dw := newDeliveryWriter(w)
		time.Sleep(250 * time.Millisecond) // work outlives W
		dw.WriteHeader(http.StatusOK)
		_, _ = dw.Write([]byte("late"))
		dw.reportDelivery("req-w", "b", t0)
		if !errors.Is(dw.err, os.ErrDeadlineExceeded) {
			t.Errorf("captured err = %v, want one matching os.ErrDeadlineExceeded", dw.err)
		}
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err == nil {
		resp.Body.Close()
	}
	<-reported

	if !strings.Contains(buf.String(), "cause=write_deadline") {
		t.Errorf("write failure at W must be write_deadline:\n%s", buf.String())
	}
}

func TestDeliveryWriter_ReportDeliverySilentOnSuccess(t *testing.T) {
	buf := captureWarnLog(t)
	rec := &okWriter{}
	dw := &deliveryWriter{ResponseWriter: rec}
	_, _ = dw.Write([]byte("x"))

	dw.reportDelivery("req-1", "b", time.Now())

	if buf.Len() != 0 {
		t.Errorf("unexpected warn on successful delivery: %s", buf.String())
	}
}

type okWriter struct{ failingWriter }

func (o *okWriter) Write(p []byte) (int, error) { return len(p), nil }
