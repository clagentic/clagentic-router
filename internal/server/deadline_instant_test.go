// Guards the single-instant invariant of the request deadline model: every
// deadline derives from one start instant and one budget, so the routed and
// passthrough paths cannot drift apart, nothing re-reads the clock mid-request,
// and work that must end in a written error response always stops a delivery
// margin before the connection's write deadline. A regression here shows up as
// an empty reply (the error had no budget left to be written) rather than a
// failed assertion in the handler under test.
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
		ctx, cancel, deadline := beginRoutedRequestAt(w, r, start, d, false)
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
			ctx, cancel, _ := beginRoutedRequestAt(w, r, start, d, false)
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

// The passthrough bound is two-phase: phase-1 work is cancelled at the work
// deadline C (a margin before the write deadline W, so an error response can
// still be written), and committing the response re-arms the cancel to W.
func TestBeginPassthroughRequestAt_WorkEndsBeforeWriteDeadline(t *testing.T) {
	t0 := time.Now().Add(-100 * time.Millisecond)
	d := time.Second // below the margin, so C is the midpoint t0+500ms
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	pt := beginPassthroughRequestAt(w, r, t0, d)
	defer pt.Close()

	wantWork, wantWrite := requestDeadlines(t0, d)
	if !w.writeDeadline.Equal(wantWrite) {
		t.Fatalf("write deadline = %v, want %v", w.writeDeadline, wantWrite)
	}
	if !wantWork.Before(wantWrite) {
		t.Fatalf("work deadline %v must be before write deadline %v", wantWork, wantWrite)
	}

	select {
	case <-pt.Context().Done():
		t.Fatal("phase-1 context cancelled immediately")
	case <-time.After(time.Until(wantWork) - 100*time.Millisecond):
	}
	select {
	case <-pt.Context().Done():
	case <-time.After(time.Until(wantWork) + 150*time.Millisecond):
		t.Fatalf("phase-1 context not cancelled at the work deadline %v", wantWork)
	}
	if time.Now().After(wantWrite) {
		t.Fatal("work deadline fired at or after the write deadline")
	}
}

// requestDeadlines is the one derivation both the routed and the passthrough
// helpers use; they must agree with it for the same (t0, d).
func TestRequestDeadlines_SingleSourceForRoutedAndPassthrough(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	r := httptest.NewRequest(http.MethodPost, "/x", nil)

	for _, d := range []time.Duration{5 * time.Minute, router.DeliveryMargin, time.Second} {
		work, write := requestDeadlines(t0, d)
		if !write.Equal(t0.Add(d)) {
			t.Errorf("d=%s: write = %v, want t0+d", d, write)
		}
		if d > router.DeliveryMargin {
			if !work.Equal(write.Add(-router.DeliveryMargin)) {
				t.Errorf("d=%s: work = %v, want write-margin", d, work)
			}
		} else if !work.Equal(t0.Add(d / 2)) {
			t.Errorf("d=%s: work = %v, want clamped midpoint", d, work)
		}

		w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		ctx, cancel, got := beginRoutedRequestAt(w, r, t0, d, false)
		routedWork, _ := ctx.Deadline()
		cancel()
		if !routedWork.Equal(work) || !got.Equal(write) || !w.writeDeadline.Equal(write) {
			t.Errorf("d=%s: routed helper (%v, %v) disagrees with requestDeadlines (%v, %v)", d, routedWork, got, work, write)
		}

		pw := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		pt := beginPassthroughRequestAt(pw, r, t0, d)
		pt.Close()
		if !pw.writeDeadline.Equal(write) {
			t.Errorf("d=%s: passthrough write deadline %v, want %v", d, pw.writeDeadline, write)
		}
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

// Pre-Do work (credential resolution) counts against the work deadline: with a
// 1s bound (work deadline 500ms) and 200ms spent resolving credentials the
// stalled upstream is abandoned at about 500ms and the client RECEIVES the
// 502, not an empty reply.
func TestBedrockPassthrough_PreDoWorkCountsAgainstBound(t *testing.T) {
	up := blockingUpstream(t)
	ts, srv := newDeadlineServer(t, 0, 1, "http://unused.invalid")
	srv.handler.bedrockRegion = "us-east-1"
	srv.handler.bedrockUpstreamBaseURL = up.URL
	srv.handler.bedrockCredentialsFn = func(context.Context) (aws.Credentials, error) {
		time.Sleep(200 * time.Millisecond)
		return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
	}

	start := time.Now()
	resp, body, err := post(t, ts.URL+"/model/anthropic.claude-x/invoke",
		`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("empty reply after %s: the error response had no budget to be written: %v", elapsed, err)
	}
	if resp.StatusCode != http.StatusBadGateway || len(body) == 0 {
		t.Fatalf("status %d body %q, want 502 with a body", resp.StatusCode, body)
	}
	if elapsed < 450*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("upstream abandoned after %s, want about 500ms (work deadline), well before the 1s write deadline", elapsed)
	}
}

// The messages passthrough shares the helper, so a stalled upstream is cut at
// the work deadline and the client receives the error response.
func TestMessagesPassthrough_StalledUpstreamCutAtBound(t *testing.T) {
	up := blockingUpstream(t)
	ts, _ := newDeadlineServer(t, 0, 1, up.URL)

	start := time.Now()
	resp, body, err := post(t, ts.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "client-key"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("empty reply after %s: the error response had no budget to be written: %v", elapsed, err)
	}
	if resp.StatusCode != http.StatusBadGateway || len(body) == 0 {
		t.Fatalf("status %d body %q, want 502 with a body", resp.StatusCode, body)
	}
	if elapsed < 450*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatalf("stalled upstream cut after %s, want about 500ms", elapsed)
	}
}
