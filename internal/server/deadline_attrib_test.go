// Class guard for deadline attribution. Earlier rounds fixed one mislabelled
// outcome at a time (a chain-budget expiry booked as a request deadline, a
// disconnect booked as a deadline because the report ran late) and each fix
// left a sibling with the same defect, because every site re-derived "who was
// at fault" from which context looked done afterwards or from the clock at
// report time. The invariant is that every deadline carries its cause and an
// outcome is read once from that cause. These tests exercise it through the
// real server-composed deadline (beginRoutedRequestAt, then Route) and through
// the passthrough timers, never a bare context.Background(), so a regression in
// how the server arms the cause is caught here and not only in router unit
// tests.
package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/state"
	"github.com/clagentic/clagentic-router/internal/store"
)

type fnAdapter struct {
	id string
	fn func(ctx context.Context) error
}

func (f *fnAdapter) ID() string { return f.id }
func (f *fnAdapter) Invoke(ctx context.Context, _ *backend.Request) (*backend.Response, error) {
	if err := f.fn(ctx); err != nil {
		return nil, err
	}
	return &backend.Response{Content: "ok"}, nil
}
func (f *fnAdapter) Capabilities() backend.Capabilities { return backend.Capabilities{} }

func hangUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRoutedAttribution_RealServerDeadline(t *testing.T) {
	// Only case e has an adapter that lingers after its context is done; it
	// is released after the client cancel so the cancel lands mid-Invoke.
	releaseE := make(chan struct{})
	type want struct {
		penalized  map[string]bool // backend id -> got a timeout health penalty
		outcome    string          // call_log outcome that must be present
		errType    string          // its error_type ("" = not checked)
		notOutcome string          // call_log outcome that must be absent
	}
	cases := []struct {
		name       string
		chain      string
		tiers      map[string][]string
		timeouts   map[string]int
		fns        map[string]func(context.Context) error
		maxRequest int
		cancelAt   time.Duration // client cancel after this long; 0 = never
		release    chan struct{} // closed shortly after the client cancel, if set
		want       want
	}{
		{
			name:  "a_uncapped_single_tier_hangs",
			chain: "a", timeouts: map[string]int{"a": 1},
			fns:  map[string]func(context.Context) error{"a": hangUntilDone},
			want: want{penalized: map[string]bool{"a": true}, outcome: "fallback", errType: "timeout", notOutcome: "request_deadline"},
		},
		{
			// Tier a is killed at 1s but only returns ~200ms later; tier b then
			// starts with its own 1s, but the chain budget (2s) ends first.
			name:  "b_last_tier_after_slow_kill_return",
			chain: "a,b", timeouts: map[string]int{"a": 1, "b": 1},
			fns: map[string]func(context.Context) error{
				"a": func(ctx context.Context) error { <-ctx.Done(); time.Sleep(200 * time.Millisecond); return ctx.Err() },
				"b": hangUntilDone,
			},
			want: want{penalized: map[string]bool{"a": true, "b": true}, outcome: "fallback", errType: "timeout", notOutcome: "request_deadline"},
		},
		{
			name:  "c_request_cap_below_backend_timeout",
			chain: "a", timeouts: map[string]int{"a": 10}, maxRequest: 1,
			fns:  map[string]func(context.Context) error{"a": hangUntilDone},
			want: want{penalized: map[string]bool{"a": false}, outcome: "request_deadline", notOutcome: "fallback"},
		},
		{
			name:  "d_client_cancel_before_backend_deadline",
			chain: "a", timeouts: map[string]int{"a": 10}, cancelAt: 200 * time.Millisecond,
			fns:  map[string]func(context.Context) error{"a": hangUntilDone},
			want: want{penalized: map[string]bool{"a": false}, outcome: "cancelled", notOutcome: "fallback"},
		},
		{
			// The tier's chain budget is its slowest candidate (z, 2s), so a's own
			// 1s deadline fires first; the client leaves at 1.2s while a is still
			// draining. First event wins: a is charged.
			name:  "e_client_cancel_after_backend_deadline_fired",
			chain: "mixed", tiers: map[string][]string{"mixed": {"a", "z"}},
			timeouts: map[string]int{"a": 1, "z": 2}, cancelAt: 1200 * time.Millisecond, release: releaseE,
			fns: map[string]func(context.Context) error{
				"a": func(ctx context.Context) error { <-ctx.Done(); <-releaseE; return ctx.Err() },
				"z": hangUntilDone,
			},
			want: want{penalized: map[string]bool{"a": true}, outcome: "fallback", errType: "timeout", notOutcome: "cancelled"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })

			cfg := &config.Config{
				Backends: map[string]*config.BackendConfig{},
				Tiers:    tc.tiers,
				Routing: config.RoutingConfig{
					Strategy: "ordered", HealthProbeIntervalSeconds: 3600,
					DegradedFailureThreshold: 3, OfflineFailureThreshold: 6,
				},
				Proxy: config.ProxyConfig{MaxRequestSeconds: tc.maxRequest},
			}
			adapters := map[string]backend.Adapter{}
			for id, fn := range tc.fns {
				cfg.Backends[id] = &config.BackendConfig{Adapter: "stub", CostWeight: 1, TimeoutSeconds: tc.timeouts[id]}
				adapters[id] = &fnAdapter{id: id, fn: fn}
			}
			rt := router.New(cfg, adapters, st, nil)
			chain := strings.Split(tc.chain, ",")

			clientCtx, cancelClient := context.WithCancel(context.Background())
			defer cancelClient()
			if tc.cancelAt > 0 {
				time.AfterFunc(tc.cancelAt, func() {
					cancelClient()
					if tc.release != nil {
						time.Sleep(50 * time.Millisecond)
						close(tc.release)
					}
				})
			}
			req := httptest.NewRequest(http.MethodPost, "/x", nil).WithContext(clientCtx)
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

			t0 := time.Now()
			budget, capped := rt.RequestDeadline(chain)
			routeCtx, cancelRoute, _ := beginRoutedRequestAt(w, req, t0, budget, capped)
			defer cancelRoute()
			_, _, _ = rt.Route(routeCtx, &backend.Request{Messages: []backend.Message{{Role: "user", Content: "hi"}}}, chain)

			for id, wantPenalty := range tc.want.penalized {
				snap, _ := rt.StateSnapshot(id)
				got := snap.LastErrorType == state.ErrTypeTimeout && snap.ConsecutiveFailures > 0
				if got != wantPenalty {
					t.Errorf("backend %s timeout penalty = %v, want %v (snapshot %+v)", id, got, wantPenalty, snap)
				}
				if !wantPenalty && (snap.ConsecutiveFailures != 0 || snap.LastErrorType != "") {
					t.Errorf("backend %s wrongly penalized: %+v", id, snap)
				}
			}

			rows, err := st.RecentCalls(store.CallLogFilter{Limit: 20})
			if err != nil {
				t.Fatalf("RecentCalls: %v", err)
			}
			var foundWant bool
			for _, row := range rows {
				if row.Outcome == tc.want.notOutcome {
					t.Errorf("call_log has outcome %q, which must be absent: %+v", row.Outcome, rows)
				}
				if row.Outcome == tc.want.outcome && (tc.want.errType == "" || row.ErrorType == tc.want.errType) {
					foundWant = true
				}
			}
			if !foundWant {
				t.Errorf("call_log missing outcome %q error_type %q: %+v", tc.want.outcome, tc.want.errType, rows)
			}
		})
	}
}

// --- passthrough attribution ---

type ptRoute struct {
	name, path, body string
	hdr              map[string]string
	bedrock          bool
}

var ptRoutes = []ptRoute{
	{name: "messages", path: "/v1/messages",
		body: `{"model":"claude-sonnet-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		hdr:  map[string]string{"x-api-key": "client-key"}},
	{name: "bedrock", path: "/model/anthropic.claude-x/invoke-with-response-stream", bedrock: true,
		body: `{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[{"role":"user","content":"."}]}`},
}

// runPassthrough drives one passthrough request against up and returns the
// Warn+ log it produced. cancelAfter > 0 makes the client disconnect.
func runPassthrough(t *testing.T, p ptRoute, up *httptest.Server, cancelAfter time.Duration) string {
	t.Helper()
	buf := captureWarnLog(t)
	ts, srv := newDeadlineServer(t, 0, 1, up.URL)
	if p.bedrock {
		srv.handler.bedrockRegion = "us-east-1"
		srv.handler.bedrockUpstreamBaseURL = up.URL
		srv.handler.bedrockCredentialsFn = func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIAEXAMPLESTUBKEY", SecretAccessKey: "stub"}, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if cancelAfter > 0 {
		time.AfterFunc(cancelAfter, cancel)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+p.path, strings.NewReader(p.body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range p.hdr {
		req.Header.Set(k, v)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Close waits for the handler to finish, so its log is complete and the
	// buffer is safe to read.
	ts.Close()
	return buf.String()
}

func TestPassthroughAttribution_DistinctCausePerEvent(t *testing.T) {
	allCauses := []string{"cause=work_deadline", "cause=write_deadline", "cause=client_cancelled"}
	scenarios := []struct {
		name        string
		up          func(t *testing.T) *httptest.Server
		cancelAfter time.Duration
		want        string
	}{
		{"work_deadline_C_timer", blockingUpstream, 0, "cause=work_deadline"},
		{"client_disconnect", blockingUpstream, 150 * time.Millisecond, "cause=client_cancelled"},
		{"write_deadline_W_timer", func(t *testing.T) *httptest.Server { return chunkUpstream(t, 1000, 50*time.Millisecond) }, 0, "cause=write_deadline"},
	}
	for _, p := range ptRoutes {
		for _, sc := range scenarios {
			t.Run(p.name+"/"+sc.name, func(t *testing.T) {
				out := runPassthrough(t, p, sc.up(t), sc.cancelAfter)
				if !strings.Contains(out, sc.want) {
					t.Fatalf("log missing %q:\n%s", sc.want, out)
				}
				for _, other := range allCauses {
					if other != sc.want && strings.Contains(out, other) {
						t.Errorf("log carries %q for a %s event:\n%s", other, sc.name, out)
					}
				}
			})
		}
	}
}
