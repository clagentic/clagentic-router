// internal/backend/ollama_poller_test.go — unit tests for OllamaPoller.
package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaPoller_PollModelsLoaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3:8b", "size_vram": 4 * 1024 * 1024 * 1024},
				{"name": "mistral:7b", "size_vram": 3 * 1024 * 1024 * 1024},
			},
		})
	}))
	defer srv.Close()

	totalVRAM := int64(16 * 1024 * 1024 * 1024) // 16 GB
	p := NewOllamaPoller("test", srv.URL, "llama3:8b", totalVRAM, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if snap.BackendID != "test" {
		t.Errorf("BackendID = %q, want %q", snap.BackendID, "test")
	}
	wantVRAMUsed := int64(7 * 1024 * 1024 * 1024)
	if snap.VRAMUsed != wantVRAMUsed {
		t.Errorf("VRAMUsed = %d, want %d", snap.VRAMUsed, wantVRAMUsed)
	}
	if snap.VRAMTotal != totalVRAM {
		t.Errorf("VRAMTotal = %d, want %d", snap.VRAMTotal, totalVRAM)
	}
	wantHeadroom := totalVRAM - wantVRAMUsed
	if snap.VRAMHeadroom != wantHeadroom {
		t.Errorf("VRAMHeadroom = %d, want %d", snap.VRAMHeadroom, wantHeadroom)
	}
	if !snap.ModelHot {
		t.Errorf("expected ModelHot=true for target model in response")
	}
	if snap.TargetModel != "llama3:8b" {
		t.Errorf("TargetModel = %q, want %q", snap.TargetModel, "llama3:8b")
	}
}

func TestOllamaPoller_PollTargetModelNotLoaded(t *testing.T) {
	// Target model absent from /api/ps — ModelHot should be false.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "mistral:7b", "size_vram": 3 * 1024 * 1024 * 1024},
			},
		})
	}))
	defer srv.Close()

	p := NewOllamaPoller("test", srv.URL, "llama3:8b", 0, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if snap.ModelHot {
		t.Errorf("expected ModelHot=false when target model not in response")
	}
}

func TestOllamaPoller_PollNoModels(t *testing.T) {
	// Empty models list — no VRAM used, target model not hot.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []interface{}{},
		})
	}))
	defer srv.Close()

	totalVRAM := int64(8 * 1024 * 1024 * 1024)
	p := NewOllamaPoller("test", srv.URL, "llama3:8b", totalVRAM, 0, nil)
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if snap.VRAMUsed != 0 {
		t.Errorf("VRAMUsed = %d, want 0 for empty models list", snap.VRAMUsed)
	}
	if snap.VRAMHeadroom != totalVRAM {
		t.Errorf("VRAMHeadroom = %d, want %d for empty models list", snap.VRAMHeadroom, totalVRAM)
	}
	if snap.ModelHot {
		t.Errorf("ModelHot should be false for empty models list")
	}
}

func TestOllamaPoller_VRAMUnknownWhenTotalZero(t *testing.T) {
	// When TotalVRAM is 0, VRAMHeadroom must be -1 (unknown).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3:8b", "size_vram": 4 * 1024 * 1024 * 1024},
			},
		})
	}))
	defer srv.Close()

	p := NewOllamaPoller("test", srv.URL, "llama3:8b", 0, 0, nil) // TotalVRAM=0
	snap, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll returned error: %v", err)
	}
	if snap.VRAMHeadroom != -1 {
		t.Errorf("VRAMHeadroom = %d, want -1 when TotalVRAM is unknown", snap.VRAMHeadroom)
	}
}

func TestOllamaPoller_DefaultInterval(t *testing.T) {
	p := NewOllamaPoller("test", "http://localhost:11434", "llama3:8b", 0, 0, nil)
	if p.Interval != 7*time.Second {
		t.Errorf("default interval = %v, want 7s", p.Interval)
	}
}

func TestOllamaPoller_OnUpdateCallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3:8b", "size_vram": 4 * 1024 * 1024 * 1024},
			},
		})
	}))
	defer srv.Close()

	var received OllamaCapacity
	called := make(chan struct{}, 1)
	p := NewOllamaPoller("test", srv.URL, "llama3:8b", 0, 50*time.Millisecond, func(snap OllamaCapacity) {
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

	if !received.ModelHot {
		t.Errorf("expected ModelHot=true in callback")
	}
}
