// internal/webhook/deliverer_test.go — unit tests for webhook delivery.
package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clagentic/clagentic-router/internal/state"
)

func testEvent(event, backendID string) DeliveryEvent {
	return DeliveryEvent{
		Event:     event,
		BackendID: backendID,
		Snapshot: state.Snapshot{
			BackendID: backendID,
			Status:    state.StatusHealthy,
		},
		At: time.Now().UTC(),
	}
}

func fastConfig() Config {
	return Config{MaxRetry: 2, InitialBackoffMs: 10, TimeoutSeconds: 5}
}

func TestDeliverer_DeliversToEndpoint(t *testing.T) {
	var received atomic.Int32
	var capturedBody []byte
	var capturedEvent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		capturedEvent = r.Header.Get("X-Clagentic-Event")
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL, Events: nil}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("backend_offline", "test-backend"))
	time.Sleep(100 * time.Millisecond)

	if received.Load() == 0 {
		t.Fatal("expected delivery, got none")
	}
	if capturedEvent != "backend_offline" {
		t.Errorf("X-Clagentic-Event: want %q, got %q", "backend_offline", capturedEvent)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if body["event"] != "backend_offline" {
		t.Errorf("body event: want %q, got %v", "backend_offline", body["event"])
	}
	if body["backend_id"] != "test-backend" {
		t.Errorf("body backend_id: want %q, got %v", "test-backend", body["backend_id"])
	}
}

func TestDeliverer_SetsDeliveryIDHeader(t *testing.T) {
	var deliveryID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryID = r.Header.Get("X-Clagentic-Delivery")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("quota_low", "b1"))
	time.Sleep(100 * time.Millisecond)

	if deliveryID == "" {
		t.Error("X-Clagentic-Delivery header not set")
	}
}

func TestDeliverer_HMACSig_WithSecret(t *testing.T) {
	var sigHeader string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get("X-Clagentic-Signature")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL, Secret: "test-secret"}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("auth_failure", "b1"))
	time.Sleep(100 * time.Millisecond)

	if sigHeader == "" {
		t.Fatal("X-Clagentic-Signature not set")
	}
	want := computeSignature(body, "test-secret")
	if sigHeader != want {
		t.Errorf("signature mismatch: want %q, got %q", want, sigHeader)
	}
}

func TestDeliverer_NoSignatureHeaderWhenNoSecret(t *testing.T) {
	var sigHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigHeader = r.Header.Get("X-Clagentic-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL, Secret: ""}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("backend_degraded", "b1"))
	time.Sleep(100 * time.Millisecond)

	if sigHeader != "" {
		t.Errorf("expected no signature header, got %q", sigHeader)
	}
}

func TestDeliverer_EventFiltering_Subscribed(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Only subscribed to "quota_low"
	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL, Events: []string{"quota_low"}}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("quota_low", "b1"))
	d.Enqueue(testEvent("backend_offline", "b1")) // should NOT deliver
	time.Sleep(150 * time.Millisecond)

	if count.Load() != 1 {
		t.Errorf("expected 1 delivery (only quota_low), got %d", count.Load())
	}
}

func TestDeliverer_EventFiltering_EmptySubscribesAll(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{{URL: srv.URL, Events: nil}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("quota_low", "b1"))
	d.Enqueue(testEvent("backend_offline", "b1"))
	d.Enqueue(testEvent("auth_failure", "b1"))
	time.Sleep(150 * time.Millisecond)

	if count.Load() != 3 {
		t.Errorf("expected 3 deliveries (all events), got %d", count.Load())
	}
}

func TestDeliverer_RetryOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := New(Config{MaxRetry: 3, InitialBackoffMs: 20, TimeoutSeconds: 5}, nil,
		[]StaticEndpoint{{URL: srv.URL}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("backend_degraded", "b1"))
	time.Sleep(300 * time.Millisecond)

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts (retry on 500), got %d", attempts.Load())
	}
}

func TestDeliverer_MaxRetryExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := New(Config{MaxRetry: 3, InitialBackoffMs: 10, TimeoutSeconds: 5}, nil,
		[]StaticEndpoint{{URL: srv.URL}})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("backend_offline", "b1"))
	time.Sleep(500 * time.Millisecond)

	if attempts.Load() != 3 {
		t.Errorf("expected exactly 3 attempts (MaxRetry=3), got %d", attempts.Load())
	}
}

func TestDeliverer_MultipleEndpoints(t *testing.T) {
	var count1, count2 atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count1.Add(1); w.WriteHeader(200) }))
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count2.Add(1); w.WriteHeader(200) }))
	defer srv1.Close()
	defer srv2.Close()

	d := New(fastConfig(), nil, []StaticEndpoint{
		{URL: srv1.URL},
		{URL: srv2.URL},
	})
	d.Start()
	defer d.Stop()

	d.Enqueue(testEvent("backend_recovered", "b1"))
	time.Sleep(150 * time.Millisecond)

	if count1.Load() != 1 || count2.Load() != 1 {
		t.Errorf("expected 1 delivery to each endpoint, got srv1=%d srv2=%d",
			count1.Load(), count2.Load())
	}
}

func TestComputeSignature(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	secret := "my-secret"
	sig := computeSignature(body, secret)
	if len(sig) == 0 || sig[:7] != "sha256=" {
		t.Errorf("unexpected signature format: %q", sig)
	}
	// Deterministic
	if sig2 := computeSignature(body, secret); sig != sig2 {
		t.Error("signature not deterministic")
	}
	// Different secret → different sig
	if sig3 := computeSignature(body, "other-secret"); sig == sig3 {
		t.Error("different secrets should produce different signatures")
	}
}

func TestMatchesEvent(t *testing.T) {
	cases := []struct {
		sub   []string
		event string
		want  bool
	}{
		{nil, "quota_low", true},        // empty = all events
		{[]string{}, "quota_low", true}, // empty = all events
		{[]string{"quota_low"}, "quota_low", true},
		{[]string{"backend_offline"}, "quota_low", false},
		{[]string{"quota_low", "backend_offline"}, "backend_offline", true},
		{[]string{"*"}, "anything", true}, // wildcard
	}
	for _, tc := range cases {
		if got := matchesEvent(tc.sub, tc.event); got != tc.want {
			t.Errorf("matchesEvent(%v, %q) = %v, want %v", tc.sub, tc.event, got, tc.want)
		}
	}
}
