// Class guards for the deadline invariant that a context expiry reported by
// writing an error response must leave budget to write it. Each earlier fix
// patched one handler branch and the next review found a sibling branch with
// the same defect, so these tests enumerate every branch (routed and
// passthrough, before and after the response is committed) instead of one:
// a new handler path that forgets the work/write split fails here as an empty
// reply, and a relay that outruns the write deadline fails the cut-time check.
package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Walks every handler branch where a context expiry becomes an error response.
// With max_request_seconds=1 the work deadline is 500ms and the write deadline
// 1s; the client must RECEIVE the error status and a non-empty body, never an
// empty reply, and must get it before the write deadline.
func TestCtxExpiryBranches_ClientReceivesErrorResponse(t *testing.T) {
	const (
		bedrockBody = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`
		msgsBody    = `{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	)
	stubCreds := func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
	}
	cases := []struct {
		name, path, body string
		hdr              map[string]string
		// wantStatus 0 means any 4xx/5xx.
		wantStatus int
		setup      func(srv *Server, upstreamURL string)
	}{
		{name: "chat_routed", path: "/v1/chat/completions", hdr: bearer,
			body: `{"model":"role:slow-chain","messages":[{"role":"user","content":"hi"}]}`},
		{name: "messages_routed", path: "/v1/messages", hdr: bearer,
			body: `{"model":"role:slow-chain","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "bedrock_routed", path: "/model/role:slow-chain/invoke", hdr: bearer, body: bedrockBody},
		{name: "bedrock_passthrough_credential_failure", path: "/model/anthropic.claude-x/invoke",
			body: bedrockBody, wantStatus: http.StatusBadGateway,
			setup: func(srv *Server, up string) {
				srv.handler.bedrockRegion = "us-east-1"
				srv.handler.bedrockUpstreamBaseURL = up
				srv.handler.bedrockCredentialsFn = func(ctx context.Context) (aws.Credentials, error) {
					<-ctx.Done()
					return aws.Credentials{}, ctx.Err()
				}
			}},
		{name: "bedrock_passthrough_stalled_do", path: "/model/anthropic.claude-x/invoke",
			body: bedrockBody, wantStatus: http.StatusBadGateway,
			setup: func(srv *Server, up string) {
				srv.handler.bedrockRegion = "us-east-1"
				srv.handler.bedrockUpstreamBaseURL = up
				srv.handler.bedrockCredentialsFn = stubCreds
			}},
		{name: "messages_passthrough_stalled_do", path: "/v1/messages",
			body: msgsBody, hdr: map[string]string{"x-api-key": "client-key"}, wantStatus: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := blockingUpstream(t)
			// Routed cases need a backend slower than the 500ms work deadline.
			ts, srv := newDeadlineServer(t, 5*time.Second, 1, up.URL)
			if tc.setup != nil {
				tc.setup(srv, up.URL)
			}

			start := time.Now()
			resp, body, err := post(t, ts.URL+tc.path, tc.body, tc.hdr)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("empty reply after %s (no budget left to write the error): %v", elapsed, err)
			}
			if tc.wantStatus != 0 && resp.StatusCode != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %q)", resp.StatusCode, tc.wantStatus, body)
			}
			if resp.StatusCode < 400 {
				t.Fatalf("status %d, want an error status (body %q)", resp.StatusCode, body)
			}
			if len(bytes.TrimSpace(body)) == 0 {
				t.Fatalf("status %d with an empty body", resp.StatusCode)
			}
			if elapsed < 450*time.Millisecond || elapsed >= time.Second {
				t.Fatalf("error delivered after %s, want at the ~500ms work deadline, before the 1s write deadline", elapsed)
			}
		})
	}
}

// chunkUpstream sends headers at once, then chunk-i every gap for total
// chunks, then [DONE]; it stops when the proxy hangs up.
func chunkUpstream(t *testing.T, total int, gap time.Duration) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := 0; i < total; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	return up
}

// Two-phase relay: a body still flowing after the work deadline (500ms) is
// delivered whole while within the write deadline (1s), and one still flowing
// at the write deadline is cut there.
func TestPassthroughRelay_RunsToWriteDeadlineNotWorkDeadline(t *testing.T) {
	const req = `{"model":"claude-sonnet-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"x-api-key": "client-key"}

	t.Run("flowing_past_work_deadline_delivered", func(t *testing.T) {
		up := chunkUpstream(t, 8, 80*time.Millisecond) // ~640ms: past 500ms, within 1s
		ts, _ := newDeadlineServer(t, 0, 1, up.URL)
		start := time.Now()
		resp, body, err := post(t, ts.URL+"/v1/messages", req, hdr)
		if err != nil {
			t.Fatalf("stream cut after %s (work deadline leaked into the relay): %v (%q)", time.Since(start), err, body)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		requireCompleteStream(t, body, 8)
		if elapsed := time.Since(start); elapsed < 550*time.Millisecond {
			t.Fatalf("finished in %s, test did not outlive the 500ms work deadline", elapsed)
		}
	})

	t.Run("flowing_at_write_deadline_cut", func(t *testing.T) {
		up := chunkUpstream(t, 1000, 50*time.Millisecond)
		ts, _ := newDeadlineServer(t, 0, 1, up.URL)
		start := time.Now()
		// The cut surfaces as a read error; the partial body is what is asserted.
		_, body, _ := post(t, ts.URL+"/v1/messages", req, hdr)
		elapsed := time.Since(start)
		if bytes.Contains(body, []byte("[DONE]")) {
			t.Fatal("stream completed; expected a cut at the write deadline")
		}
		if elapsed < 900*time.Millisecond || elapsed > 1400*time.Millisecond {
			t.Fatalf("stream cut after %s, want about the 1s write deadline", elapsed)
		}
		if !bytes.Contains(body, []byte("chunk-0")) {
			t.Fatalf("no bytes delivered before the cut: %q", body)
		}
	})
}
