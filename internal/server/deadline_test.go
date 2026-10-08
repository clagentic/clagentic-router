// The server-wide WriteTimeout is an absolute connection deadline that does not
// cancel the handler, so a request outliving it completes its backend call and
// then fails to deliver (the client sees an empty reply). LLM endpoints
// therefore replace it per request. Each test drives a real net/http server
// whose backstop is far shorter than the work being done, because only a real
// connection reproduces the empty-reply failure; durations are millisecond
// scale to keep that affordable.
package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
)

const testBackstop = 150 * time.Millisecond

// sleepAdapter takes a fixed time to answer, standing in for a slow LLM call.
type sleepAdapter struct {
	id    string
	delay time.Duration
}

func (s *sleepAdapter) ID() string { return s.id }
func (s *sleepAdapter) Invoke(ctx context.Context, _ *backend.Request) (*backend.Response, error) {
	select {
	case <-time.After(s.delay):
		return &backend.Response{Content: "slow-ok"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *sleepAdapter) Capabilities() backend.Capabilities { return backend.Capabilities{} }

// newDeadlineServer starts a server over one slow backend. maxRequestSeconds
// is proxy.max_request_seconds (0 = default).
func newDeadlineServer(t *testing.T, delay time.Duration, maxRequestSeconds int, upstreamURL string) (*httptest.Server, *Server) {
	t.Helper()
	cfg := &config.Config{
		Backends: map[string]*config.BackendConfig{
			"slow": {Adapter: "stub", CostWeight: 1.0, TimeoutSeconds: 10},
		},
		Chains: map[string][]string{"slow-chain": {"slow"}},
		Routing: config.RoutingConfig{
			Strategy:                   "ordered",
			HealthProbeIntervalSeconds: 3600,
			DegradedFailureThreshold:   3,
			OfflineFailureThreshold:    6,
		},
		Proxy: config.ProxyConfig{MaxRequestSeconds: maxRequestSeconds},
	}
	r := router.New(cfg, map[string]backend.Adapter{"slow": &sleepAdapter{id: "slow", delay: delay}}, nil, nil)
	srv := New(":0", "secret", "secret", false, r, nil, upstreamURL, "", "", "", false, "", "test")
	srv.httpServer.WriteTimeout = testBackstop

	ts := httptest.NewUnstartedServer(srv.httpServer.Handler)
	ts.Config.WriteTimeout = srv.httpServer.WriteTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, srv
}

func post(t *testing.T, url, body string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

var bearer = map[string]string{"Authorization": "Bearer secret"}

// A routed handler that outlives the backstop but is within its chain-derived
// deadline must deliver a complete response on every routed endpoint.
func TestRoutedHandlers_OutliveBackstopWithinChainDeadline(t *testing.T) {
	ts, _ := newDeadlineServer(t, 3*testBackstop, 0, "http://unused.invalid")

	cases := []struct {
		name, path, body string
	}{
		{"chat_completions", "/v1/chat/completions",
			`{"model":"role:slow-chain","messages":[{"role":"user","content":"hi"}]}`},
		{"messages", "/v1/messages",
			`{"model":"role:slow-chain","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"bedrock_invoke", "/model/role:slow-chain/invoke",
			`{"anthropic_version":"bedrock-2023-05-31","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"chat_completions_stream", "/v1/chat/completions",
			`{"model":"role:slow-chain","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			resp, body, err := post(t, ts.URL+tc.path, tc.body, bearer)
			if err != nil {
				t.Fatalf("empty/truncated reply after %s (backstop leaked into LLM endpoint): %v", time.Since(start), err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if !bytes.Contains(body, []byte("slow-ok")) {
				t.Errorf("incomplete body: %s", body)
			}
			if time.Since(start) < 2*testBackstop {
				t.Fatalf("handler finished in %s, test did not outlive the backstop", time.Since(start))
			}
		})
	}
}

// An explicit proxy.max_request_seconds below the handler's runtime wins: the
// per-request deadline is real, not just a removal of the backstop, and it
// bounds the routed work as well as the write. The chain-derived deadline would
// be ~10s, so only the explicit 1s bound can stop a 1.5s backend; a response
// inside 1s but past the backstop must still arrive, which a leaked backstop
// would break.
func TestRoutedHandler_ExplicitMaxRequestWins(t *testing.T) {
	const body = `{"model":"role:slow-chain","messages":[{"role":"user","content":"hi"}]}`

	t.Run("cut_after_bound", func(t *testing.T) {
		delay := 1500 * time.Millisecond
		ts, _ := newDeadlineServer(t, delay, 1, "http://unused.invalid")
		start := time.Now()
		resp, _, err := post(t, ts.URL+"/v1/chat/completions", body, bearer)
		// The request deadline also bounds Route, so the work stops at the
		// bound instead of finishing a response nobody can receive. The client
		// sees either a dead connection or an error status, never the
		// backend's completion.
		if err == nil && resp.StatusCode == http.StatusOK {
			t.Fatal("expected the 1s max_request_seconds deadline to prevent a 200 from a 1.5s backend")
		}
		elapsed := time.Since(start)
		// A 1s bound is below the delivery margin, so Route is clamped to the
		// midpoint (500ms) of the 1s write deadline.
		if elapsed < 450*time.Millisecond || elapsed >= delay {
			t.Fatalf("stopped after %s, want about the clamped 500ms Route deadline and before the backend's %s", elapsed, delay)
		}
	})

	t.Run("delivered_within_bound_past_backstop", func(t *testing.T) {
		delay := 3 * testBackstop
		ts, _ := newDeadlineServer(t, delay, 1, "http://unused.invalid")
		start := time.Now()
		resp, b, err := post(t, ts.URL+"/v1/chat/completions", body, bearer)
		if err != nil {
			t.Fatalf("response within the 1s bound was cut (backstop leaked): %v", err)
		}
		if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte("slow-ok")) {
			t.Fatalf("status %d body %s", resp.StatusCode, b)
		}
		if elapsed := time.Since(start); elapsed <= testBackstop || elapsed >= time.Second {
			t.Fatalf("elapsed %s, want between backstop %s and 1s bound", elapsed, testBackstop)
		}
	})
}

// slowStreamUpstream emits n SSE-ish chunks, one every gap, flushing each.
func slowStreamUpstream(t *testing.T, n int, gap time.Duration) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(gap)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)
	return up
}

func requireCompleteStream(t *testing.T, body []byte, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if !bytes.Contains(body, []byte(fmt.Sprintf("chunk-%d", i))) {
			t.Fatalf("stream cut before chunk-%d: %q", i, body)
		}
	}
	if !bytes.Contains(body, []byte("[DONE]")) {
		t.Fatalf("stream missing terminator: %q", body)
	}
}

// A passthrough stream lasting longer than the backstop, within
// proxy.max_request_seconds, is delivered whole.
func TestMessagesPassthroughStream_OutlivesBackstop(t *testing.T) {
	up := slowStreamUpstream(t, 8, 50*time.Millisecond) // ~400ms > 150ms backstop
	ts, _ := newDeadlineServer(t, 0, 5, up.URL)

	resp, body, err := post(t, ts.URL+"/v1/messages",
		`{"model":"claude-sonnet-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": "client-key"})
	if err != nil {
		t.Fatalf("stream cut: %v (body so far %q)", err, body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	requireCompleteStream(t, body, 8)
}

func TestBedrockPassthroughStream_OutlivesBackstop(t *testing.T) {
	up := slowStreamUpstream(t, 8, 50*time.Millisecond)
	ts, srv := newDeadlineServer(t, 0, 5, "http://unused.invalid")
	srv.handler.bedrockRegion = "us-east-1"
	srv.handler.bedrockUpstreamBaseURL = up.URL
	srv.handler.bedrockCredentialsFn = func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
	}

	resp, body, err := post(t, ts.URL+"/model/anthropic.claude-x/invoke-with-response-stream",
		`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`, nil)
	if err != nil {
		t.Fatalf("stream cut: %v (body so far %q)", err, body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	requireCompleteStream(t, body, 8)
}

// Non-LLM endpoints keep a write deadline: the backstop is set (default 300s,
// never disabled) and LLM handlers are the only ones that replace it.
func TestNonLLMEndpoints_KeepBackstopWriteTimeout(t *testing.T) {
	r := router.New(&config.Config{
		Backends: map[string]*config.BackendConfig{"b": {Adapter: "stub"}},
		Routing:  config.RoutingConfig{HealthProbeIntervalSeconds: 3600},
	}, map[string]backend.Adapter{"b": &stubAdapter{id: "b"}}, nil, nil)

	def := New(":0", "secret", "secret", false, r, nil, "https://api.anthropic.com", "", "", "", false, "", "test")
	if got := def.httpServer.WriteTimeout; got != defaultBackstopWriteTimeout || got <= 0 {
		t.Errorf("default server WriteTimeout = %s, want %s (non-zero backstop)", got, defaultBackstopWriteTimeout)
	}
}
