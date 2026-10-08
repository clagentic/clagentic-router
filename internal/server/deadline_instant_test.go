// internal/server/deadline_instant_test.go — one-instant deadline derivation
// for routed requests (Route expires a delivery margin before the write
// deadline) and handler-entry bounds for passthrough requests.
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/clagentic/clagentic-router/internal/router"
)

// deadlineRecorder is a ResponseWriter that supports http.ResponseController
// deadlines and records the write deadline it was given.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.writeDeadline = t
	return nil
}

func TestBeginRoutedRequestAt_RouteExpiresMarginBeforeWriteDeadline(t *testing.T) {
	// A start far in the past proves nothing re-reads the clock: every
	// deadline must be start-relative.
	start := time.Now().Add(-time.Hour)
	r := httptest.NewRequest(http.MethodPost, "/x", nil)

	t.Run("margin_reserved", func(t *testing.T) {
		d := 5 * time.Minute
		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx, cancel, deadline := beginRoutedRequestAt(w, r, start, d)
		defer cancel()

		wantWrite := start.Add(d)
		if !w.writeDeadline.Equal(wantWrite) || !deadline.Equal(wantWrite) {
			t.Fatalf("write deadline = %v / returned %v, want %v", w.writeDeadline, deadline, wantWrite)
		}
		got, ok := ctx.Deadline()
		if !ok {
			t.Fatal("route context has no deadline")
		}
		if want := wantWrite.Add(-router.DeliveryMargin); !got.Equal(want) {
			t.Fatalf("route deadline = %v, want write deadline - margin = %v", got, want)
		}
		if !got.Before(w.writeDeadline) {
			t.Fatal("route deadline is not strictly before the write deadline")
		}
	})

	t.Run("clamped_when_bound_not_above_margin", func(t *testing.T) {
		for _, d := range []time.Duration{router.DeliveryMargin, router.DeliveryMargin / 3, time.Second} {
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			ctx, cancel, _ := beginRoutedRequestAt(w, r, start, d)
			got, _ := ctx.Deadline()
			cancel()
			if !got.After(start) || !got.Before(w.writeDeadline) {
				t.Errorf("d=%s: route deadline %v must lie strictly between start %v and write deadline %v", d, got, start, w.writeDeadline)
			}
		}
	})
}

// A Route that finishes just inside its deadline still delivers a whole
// response; one that overruns it does not produce a 200.
func TestRoutedHandler_RouteJustBeforeDeadlineStillDelivers(t *testing.T) {
	const body = `{"model":"role:slow-chain","messages":[{"role":"user","content":"hi"}]}`
	// max_request_seconds=1 is below the delivery margin, so Route is clamped
	// to 500ms of the 1s write deadline.
	t.Run("inside", func(t *testing.T) {
		ts, _ := newDeadlineServer(t, 400*time.Millisecond, 1, "http://unused.invalid")
		resp, b, err := post(t, ts.URL+"/v1/chat/completions", body, bearer)
		if err != nil {
			t.Fatalf("response cut: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, b)
		}
	})
	t.Run("overrun", func(t *testing.T) {
		ts, _ := newDeadlineServer(t, 800*time.Millisecond, 1, "http://unused.invalid")
		start := time.Now()
		resp, _, err := post(t, ts.URL+"/v1/chat/completions", body, bearer)
		if err == nil && resp.StatusCode == http.StatusOK {
			t.Fatal("a Route past its deadline must not return 200")
		}
		if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
			t.Fatalf("Route ran %s, want it cancelled near 500ms", elapsed)
		}
	})
}

func TestBeginPassthroughRequestAt_BoundIsHandlerEntryRelative(t *testing.T) {
	start := time.Now().Add(-400 * time.Millisecond)
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	ctx, cancel := beginPassthroughRequestAt(w, r, start, time.Second)
	defer cancel()

	want := start.Add(time.Second)
	if got, _ := ctx.Deadline(); !got.Equal(want) {
		t.Fatalf("ctx deadline = %v, want entry+bound %v", got, want)
	}
	if !w.writeDeadline.Equal(want) {
		t.Fatalf("write deadline = %v, want the same instant %v", w.writeDeadline, want)
	}
}

// blockingUpstream never answers; it returns when the proxy abandons it. The
// request body is drained first because net/http only watches for a client
// disconnect (and so cancels the request context) once the body is consumed.
func blockingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(up.Close)
	return up
}

// Pre-Do work (credential resolution) counts against the bound: with a 1s
// bound and 400ms spent resolving credentials the upstream is abandoned at
// about 1s after handler entry, not 1.4s.
func TestBedrockPassthrough_PreDoWorkCountsAgainstBound(t *testing.T) {
	up := blockingUpstream(t)
	ts, srv := newDeadlineServer(t, 0, 1, "http://unused.invalid")
	srv.handler.bedrockRegion = "us-east-1"
	srv.handler.bedrockUpstreamBaseURL = up.URL
	srv.handler.bedrockCredentialsFn = func(context.Context) (aws.Credentials, error) {
		time.Sleep(400 * time.Millisecond)
		return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
	}

	start := time.Now()
	_, _, _ = post(t, ts.URL+"/model/anthropic.claude-x/invoke",
		`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, nil)
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond || elapsed > 1250*time.Millisecond {
		t.Fatalf("upstream abandoned after %s, want about 1s from handler entry (1.4s means the bound started at Do)", elapsed)
	}
}

// The messages passthrough shares the helper, so a stalled upstream is cut at
// the same handler-entry bound.
func TestMessagesPassthrough_StalledUpstreamCutAtBound(t *testing.T) {
	up := blockingUpstream(t)
	ts, _ := newDeadlineServer(t, 0, 1, up.URL)

	start := time.Now()
	_, _, _ = post(t, ts.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "client-key"})
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond || elapsed > 1250*time.Millisecond {
		t.Fatalf("stalled upstream cut after %s, want about 1s", elapsed)
	}
}
