// internal/server/deadline_delivery_test.go — delivery-failure capture and the
// post-Route Warn emitted by deliveryWriter.
package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
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
