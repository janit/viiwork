//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/proxy"
)

func TestFullProxyFlow(t *testing.T) {
	var mockServers []*httptest.Server
	var states []*balancer.BackendState

	for i := range 3 {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health":
				w.WriteHeader(200)
			case "/v1/chat/completions":
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"test response"},"finish_reason":"stop"}]}`))
			default:
				w.WriteHeader(404)
			}
		}))
		mockServers = append(mockServers, srv)

		state := balancer.NewBackendState(i, srv.Listener.Addr().String())
		state.SetStatus(balancer.StatusHealthy)
		states = append(states, state)
	}
	defer func() {
		for _, s := range mockServers { s.Close() }
	}()

	bal := balancer.New(states, 7, 20)
	handler := proxy.NewHandler(bal, "/models/test-model.gguf", 30*time.Second)

	t.Run("models endpoint", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
		var resp proxy.ModelsResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Data[0].ID != "test-model" {
			t.Errorf("expected model id test-model, got %s", resp.Data[0].ID)
		}
	})

	t.Run("load balancing", func(t *testing.T) {
		states[0].RecordLatency(100*time.Millisecond, 30*time.Second)
		states[1].RecordLatency(50*time.Millisecond, 30*time.Second)
		states[2].RecordLatency(150*time.Millisecond, 30*time.Second)

		gpusSeen := make(map[string]bool)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for range 30 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Errorf("expected 200, got %d", w.Code)
					return
				}
				gpu := w.Header().Get("X-GPU-Backend")
				mu.Lock()
				gpusSeen[gpu] = true
				mu.Unlock()
			}()
		}
		wg.Wait()

		if len(gpusSeen) < 2 {
			t.Errorf("expected requests spread across backends, only saw %v", gpusSeen)
		}
	})
}
