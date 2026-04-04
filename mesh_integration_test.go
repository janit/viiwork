//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/proxy"
)

func TestMeshCrossNodeRouting(t *testing.T) {
	// Node 1's backend: serves "model-alpha"
	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"from alpha"}}]}`))
		}
	}))
	defer backend1.Close()

	// Node 2's backend: serves "model-beta"
	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"from beta"}}]}`))
		}
	}))
	defer backend2.Close()

	// Set up node 2 as a full viiwork (with /v1/status and /v1/chat/completions)
	state2 := balancer.NewBackendState(0, backend2.Listener.Addr().String())
	state2.SetStatus(balancer.StatusHealthy)
	bal2 := balancer.New([]*balancer.BackendState{state2}, 7, 4)
	reg2 := peer.NewRegistry("viiwork-node2", "model-beta", []*balancer.BackendState{state2}, nil, 3*time.Second)
	handler2 := proxy.NewMeshHandler(bal2, reg2, 30*time.Second)
	node2 := httptest.NewServer(handler2)
	defer node2.Close()

	// Set up node 1 with node 2 as a peer
	state1 := balancer.NewBackendState(0, backend1.Listener.Addr().String())
	state1.SetStatus(balancer.StatusHealthy)
	bal1 := balancer.New([]*balancer.BackendState{state1}, 7, 4)
	peers1 := []*peer.PeerState{peer.NewPeerState(node2.Listener.Addr().String())}
	reg1 := peer.NewRegistry("viiwork-node1", "model-alpha", []*balancer.BackendState{state1}, peers1, 3*time.Second)
	reg1.PollOnce(context.Background())
	handler1 := proxy.NewMeshHandler(bal1, reg1, 30*time.Second)

	t.Run("local model routes locally", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"model-alpha","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler1.ServeHTTP(w, req)
		if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
		if !strings.Contains(w.Body.String(), "from alpha") { t.Errorf("expected local response, got %s", w.Body.String()) }
	})

	t.Run("peer model routes to peer", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"model-beta","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler1.ServeHTTP(w, req)
		if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
		if !strings.Contains(w.Body.String(), "from beta") { t.Errorf("expected peer response, got %s", w.Body.String()) }
	})

	t.Run("unknown model returns 404", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"nonexistent","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler1.ServeHTTP(w, req)
		if w.Code != 404 { t.Errorf("expected 404, got %d", w.Code) }
	})

	t.Run("models endpoint shows both models", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		w := httptest.NewRecorder()
		handler1.ServeHTTP(w, req)
		var resp proxy.ModelsResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if len(resp.Data) != 2 { t.Errorf("expected 2 models, got %d: %+v", len(resp.Data), resp.Data) }
	})
}
