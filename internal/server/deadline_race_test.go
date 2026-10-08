// Two delivery guards for the passthrough path:
//   - the work deadline C firing between the upstream call returning and the
//     response being committed must produce the error response, never a 200
//     whose body is cut right after the status line;
//   - streamed chunks must reach the client as they arrive, through the real
//     logging middleware, not only when the handler returns.
package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Injects the race deterministically: the upstream answers at once, and the
// seam holds the handler until C has really fired, so commit runs with the
// context already cancelled. Both passthrough handlers share relay/commit; each
// is exercised so a handler that bypassed the shared helper fails here.
func TestPassthroughCommit_WorkDeadlineFiredBeforeCommit_ErrorResponseNotTruncated200(t *testing.T) {
	const (
		bedrockBody = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`
		msgsBody    = `{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	)
	cases := []struct {
		name, path, body string
		hdr              map[string]string
		bedrock          bool
	}{
		{name: "messages", path: "/v1/messages", body: msgsBody, hdr: map[string]string{"x-api-key": "client-key"}},
		{name: "bedrock", path: "/model/anthropic.claude-x/invoke", body: bedrockBody, bedrock: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"upstream":"complete body that must not be relayed"}`))
			}))
			t.Cleanup(up.Close)

			ts, srv := newDeadlineServer(t, 0, 1, up.URL)
			if tc.bedrock {
				srv.handler.bedrockRegion = "us-east-1"
				srv.handler.bedrockUpstreamBaseURL = up.URL
				srv.handler.bedrockCredentialsFn = func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
				}
			}
			srv.handler.beforePassthroughCommit = func(pt *passthroughRequest) {
				<-pt.Context().Done()
			}

			resp, body, err := post(t, ts.URL+tc.path, tc.body, tc.hdr)
			if err != nil {
				t.Fatalf("client got a broken reply instead of the error response: %v (%q)", err, body)
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status %d, want 502 (a 200 here is the truncated-relay race): %q", resp.StatusCode, body)
			}
			if len(bytes.TrimSpace(body)) == 0 {
				t.Fatal("error status with an empty body")
			}
			if bytes.Contains(body, []byte("must not be relayed")) {
				t.Fatalf("upstream body relayed after the work deadline fired: %q", body)
			}
		})
	}
}

// The client must observe chunk-0 while the upstream is still holding chunk-1
// back. Without the logging wrapper's Flush the chunk sits in the server's
// buffer until the handler returns, and this test times out waiting for it.
func TestPassthroughStream_ChunksReachClientIncrementally(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: chunk-0\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: chunk-1\n\n")
	}))
	t.Cleanup(up.Close)
	ts, _ := newDeadlineServer(t, 0, 5, up.URL)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case first := <-got:
		if !strings.Contains(first, "chunk-0") {
			t.Fatalf("first read = %q, want chunk-0", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chunk-0 not delivered while the upstream held chunk-1 back: stream is buffered until the handler returns")
	}

	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(rest, []byte("chunk-1")) {
		t.Fatalf("chunk-1 missing after release: %q", rest)
	}
}
