package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/peer"
)

func TestModelsEndpoint(t *testing.T) {
	h := NewModelsHandler("/models/gpt-oss-20b-Q4_K_M.gguf")
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Errorf("expected 200, got %d", w.Code) }
	var resp ModelsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil { t.Fatalf("decode error: %v", err) }
	if len(resp.Data) != 1 { t.Fatalf("expected 1 model, got %d", len(resp.Data)) }
	if resp.Data[0].ID != "gpt-oss-20b-Q4_K_M" {
		t.Errorf("expected model id gpt-oss-20b-Q4_K_M, got %s", resp.Data[0].ID)
	}
}

func TestModelIDFromPath(t *testing.T) {
	tests := []struct{ path, expected string }{
		{"/models/gpt-oss-20b-Q4_K_M.gguf", "gpt-oss-20b-Q4_K_M"},
		{"/models/subfolder/model.v2.gguf", "model.v2"},
		{"model.gguf", "model"},
	}
	for _, tc := range tests {
		got := ModelIDFromPath(tc.path)
		if got != tc.expected { t.Errorf("ModelIDFromPath(%q) = %q, want %q", tc.path, got, tc.expected) }
	}
}

func TestProxyRoutesToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-123","choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer backend.Close()
	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Errorf("expected 200, got %d", w.Code) }
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "hello") {
		t.Errorf("expected response to contain 'hello', got %s", body)
	}
	if w.Header().Get("X-GPU-Backend") == "" { t.Error("expected X-GPU-Backend header") }
}

func TestProxy503WhenAllUnhealthy(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9999")
	state.SetStatus(balancer.StatusUnhealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 { t.Errorf("expected 503, got %d", w.Code) }
}

func TestProxy429OnBackpressure(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9999")
	state.SetStatus(balancer.StatusHealthy)
	for range 4 { state.IncrInFlight() }
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 429 { t.Errorf("expected 429, got %d", w.Code) }
	if w.Header().Get("Retry-After") != "2" { t.Errorf("expected Retry-After: 2, got %s", w.Header().Get("Retry-After")) }
}

func TestRoutesToPeerForUnknownModel(t *testing.T) {
	localBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"local"}}]}`))
	}))
	defer localBackend.Close()

	peerBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			json.NewEncoder(w).Encode(peer.StatusResponse{
				NodeID: "viiwork-peer", Models: []string{"peer-model"},
				HealthyBackends: 1, TotalBackends: 1,
			})
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"from peer"}}]}`))
	}))
	defer peerBackend.Close()

	localState := balancer.NewBackendState(0, localBackend.Listener.Addr().String())
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}

	peers := []*peer.PeerState{peer.NewPeerState(peerBackend.Listener.Addr().String())}
	reg := peer.NewRegistry("viiwork-local", "local-model", backends, peers, 3*time.Second)
	reg.PollOnce(context.Background())

	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"peer-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 { t.Errorf("expected 200, got %d", w.Code) }
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "from peer") { t.Errorf("expected peer response, got %s", body) }
}

func TestModelNotFound404(t *testing.T) {
	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	reg := peer.NewRegistry("viiwork-test", "local-model", backends, nil, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"nonexistent","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 { t.Errorf("expected 404, got %d", w.Code) }
}

func TestForwardedRequestLocalOnly(t *testing.T) {
	localBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"local"}}]}`))
	}))
	defer localBackend.Close()

	localState := balancer.NewBackendState(0, localBackend.Listener.Addr().String())
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	reg := peer.NewRegistry("viiwork-test", "local-model", backends, nil, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"local-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Viiwork-Forwarded", "viiwork-other")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Errorf("expected 200, got %d", w.Code) }
}

func TestForwardedRequestModelNotLocal(t *testing.T) {
	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	reg := peer.NewRegistry("viiwork-test", "local-model", backends, nil, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Viiwork-Forwarded", "viiwork-other")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 { t.Errorf("expected 404, got %d", w.Code) }
}

func TestDynamicModelsEndpoint(t *testing.T) {
	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}

	peerState := peer.NewPeerState("192.168.1.10:8080")
	peerState.Update(peer.StatusResponse{NodeID: "viiwork-peer", Models: []string{"peer-model"}})

	reg := peer.NewRegistry("viiwork-test", "local-model", backends, []*peer.PeerState{peerState}, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp ModelsResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Data) != 2 { t.Fatalf("expected 2 models, got %d", len(resp.Data)) }
}

func TestMetricsEndpoint(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9001")
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	hist := gpu.NewHistory(10)
	hist.Record(gpu.GPUSample{GPUID: 0, Utilization: 85, VRAMUsedMB: 14200, VRAMTotalMB: 16368, Timestamp: 1000})
	bcast := gpu.NewBroadcaster()
	h.SetMetrics(hist, bcast, func() bool { return true })

	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["available"] != true { t.Error("expected available true") }
}

func TestMetricsEndpointUnavailable(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9001")
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["available"] != false { t.Error("expected available false") }
}

func TestEmbeddingsEndpoint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("expected path /v1/embeddings, got %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer backend.Close()
	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"test","input":"hello world"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "embedding") {
		t.Errorf("expected embedding in response, got %s", body)
	}
	if w.Header().Get("X-GPU-Backend") == "" {
		t.Error("expected X-GPU-Backend header")
	}
}

func TestEmbeddings503WhenAllUnhealthy(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9999")
	state.SetStatus(balancer.StatusUnhealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"test","input":"hello"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestEmbeddings429OnBackpressure(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9999")
	state.SetStatus(balancer.StatusHealthy)
	for range 4 {
		state.IncrInFlight()
	}
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"test","input":"hello"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 429 {
		t.Errorf("expected 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "2" {
		t.Errorf("expected Retry-After: 2, got %s", w.Header().Get("Retry-After"))
	}
}

func TestHealthEndpointHealthy(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9001")
	state.SetStatus(balancer.StatusHealthy)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" { t.Errorf("expected status 'ok', got %v", resp["status"]) }
	if resp["backends_healthy"] != float64(1) { t.Errorf("expected 1 healthy, got %v", resp["backends_healthy"]) }
	if resp["version"] == nil { t.Error("expected version field") }
	if resp["uptime_seconds"] == nil { t.Error("expected uptime_seconds field") }
}

func TestHealthEndpointUnhealthy(t *testing.T) {
	state := balancer.NewBackendState(0, "localhost:9001")
	state.SetStatus(balancer.StatusDead)
	bal := balancer.New([]*balancer.BackendState{state}, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 503 { t.Fatalf("expected 503, got %d", w.Code) }
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "unhealthy" { t.Errorf("expected status 'unhealthy', got %v", resp["status"]) }
}

func TestHealthEndpointWithMesh(t *testing.T) {
	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	reg := peer.NewRegistry("viiwork-test", "local-model", backends, nil, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["node_id"] != "viiwork-test" { t.Errorf("expected node_id, got %v", resp["node_id"]) }
	if resp["model"] != "local-model" { t.Errorf("expected model, got %v", resp["model"]) }
	if resp["peers_total"] == nil { t.Error("expected peers_total field") }
}
