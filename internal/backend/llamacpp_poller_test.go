// internal/backend/llamacpp_poller_test.go — unit tests for LlamaCppPoller.
package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLlamaCppPoller_PollHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "ok",
			"slots_idle":       3,
			"slots_processing": 1,
		})
	}))
	defer srv.Close()

	p := NewLlamaCppPoller("test", srv.URL, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if !snap.Healthy {
		t.Errorf("expected Healthy=true, got false")
	}
	if snap.SlotsIdle != 3 {
		t.Errorf("SlotsIdle = %d, want 3", snap.SlotsIdle)
	}
	if snap.SlotsProcessing != 1 {
		t.Errorf("SlotsProcessing = %d, want 1", snap.SlotsProcessing)
	}
	if snap.TotalSlots != 4 {
		t.Errorf("TotalSlots = %d, want 4", snap.TotalSlots)
	}
	if snap.BackendID != "test" {
		t.Errorf("BackendID = %q, want %q", snap.BackendID, "test")
	}
	if snap.PolledAt.IsZero() {
		t.Errorf("PolledAt should not be zero")
	}
}

func TestLlamaCppPoller_Poll503Loading(t *testing.T) {
	// HTTP 503 means model is loading — Healthy must be false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := NewLlamaCppPoller("test", srv.URL, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned unexpected error: %v", err)
	}
	if snap.Healthy {
		t.Errorf("expected Healthy=false for 503, got true")
	}
	if snap.SlotsIdle != 0 || snap.SlotsProcessing != 0 || snap.TotalSlots != 0 {
		t.Errorf("expected all slot fields 0 for 503, got idle=%d proc=%d total=%d",
			snap.SlotsIdle, snap.SlotsProcessing, snap.TotalSlots)
	}
}

func TestLlamaCppPoller_PollSlotsFullHealthy(t *testing.T) {
	// HTTP 200 with slots_idle=0 means loaded but saturated: Healthy=true, SlotsIdle=0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "ok",
			"slots_idle":       0,
			"slots_processing": 4,
		})
	}))
	defer srv.Close()

	p := NewLlamaCppPoller("test", srv.URL, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if !snap.Healthy {
		t.Errorf("expected Healthy=true for slots-full 200 response, got false")
	}
	if snap.SlotsIdle != 0 {
		t.Errorf("SlotsIdle = %d, want 0", snap.SlotsIdle)
	}
	if snap.TotalSlots != 4 {
		t.Errorf("TotalSlots = %d, want 4", snap.TotalSlots)
	}
}

func TestLlamaCppPoller_DefaultInterval(t *testing.T) {
	p := NewLlamaCppPoller("test", "http://localhost:8080", 0, nil)
	if p.Interval != 4*time.Second {
		t.Errorf("default interval = %v, want 4s", p.Interval)
	}
}

func TestLlamaCppPoller_CustomInterval(t *testing.T) {
	p := NewLlamaCppPoller("test", "http://localhost:8080", 10*time.Second, nil)
	if p.Interval != 10*time.Second {
		t.Errorf("custom interval = %v, want 10s", p.Interval)
	}
}

func TestLlamaCppPoller_OnUpdateCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":           "ok",
			"slots_idle":       2,
			"slots_processing": 2,
		})
	}))
	defer srv.Close()

	var received LlamaCppCapacity
	called := make(chan struct{}, 1)
	p := NewLlamaCppPoller("test", srv.URL, 50*time.Millisecond, func(snap LlamaCppCapacity) {
		received = snap
		select {
		case called <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go p.Start(ctx)

	select {
	case <-called:
	case <-ctx.Done():
		t.Fatal("OnUpdate was not called within timeout")
	}

	if received.SlotsIdle != 2 {
		t.Errorf("received SlotsIdle = %d, want 2", received.SlotsIdle)
	}
}
