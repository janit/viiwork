package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/web"
)

type Handler struct {
	balancer           *balancer.Balancer
	registry           *peer.Registry
	modelsHandler      http.Handler
	latencyWindow      time.Duration
	statusHandler      http.Handler
	clusterHandler     http.Handler
	metricsHistory     *gpu.History
	metricsBroadcaster *gpu.Broadcaster
	metricsAvailable   func() bool
}

// NewHandler creates a standalone handler (no mesh). Preserved for backward compatibility.
func NewHandler(bal *balancer.Balancer, modelPath string, latencyWindow time.Duration) *Handler {
	return &Handler{
		balancer:      bal,
		modelsHandler: NewModelsHandler(modelPath),
		latencyWindow: latencyWindow,
	}
}

// NewMeshHandler creates a handler with mesh routing support.
func NewMeshHandler(bal *balancer.Balancer, reg *peer.Registry, latencyWindow time.Duration) *Handler {
	return &Handler{
		balancer:       bal,
		registry:       reg,
		latencyWindow:  latencyWindow,
		statusHandler:  NewStatusHandler(reg.NodeID(), reg.LocalModel(), reg.Backends(), reg.Power(), reg.Cost()),
		clusterHandler: NewClusterHandler(reg),
	}
}

func (h *Handler) SetMetrics(history *gpu.History, broadcaster *gpu.Broadcaster, available func() bool) {
	h.metricsHistory = history
	h.metricsBroadcaster = broadcaster
	h.metricsAvailable = available
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/models" && r.Method == "GET":
		h.handleModels(w, r)
	case r.URL.Path == "/v1/status" && r.Method == "GET":
		if h.statusHandler != nil {
			h.statusHandler.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	case r.URL.Path == "/v1/cluster" && r.Method == "GET":
		if h.clusterHandler != nil {
			h.clusterHandler.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	case r.URL.Path == "/" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/html")
		w.Write(web.DashboardHTML)
	case r.URL.Path == "/chat" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/html")
		w.Write(web.ChatHTML)
	case r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/completions":
		h.handleProxy(w, r)
	case r.URL.Path == "/v1/metrics" && r.Method == "GET":
		h.handleMetrics(w, r)
	case r.URL.Path == "/v1/metrics/stream" && r.Method == "GET":
		h.handleMetricsStream(w, r)
	case r.URL.Path == "/v1/embeddings" && r.Method == "POST":
		h.handleProxy(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if h.registry != nil {
		resp := ModelsResponse{Object: "list", Data: h.registry.AllModels()}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}
	// Standalone mode: delegate to static models handler
	if h.modelsHandler != nil {
		h.modelsHandler.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ModelsResponse{Object: "list"})
}

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	// No registry = standalone mode, use local balancer directly
	if h.registry == nil {
		h.handleLocalProxy(w, r)
		return
	}

	// Read and buffer body to extract model
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to read request","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	var reqBody struct {
		Model string `json:"model"`
	}
	json.Unmarshal(bodyBytes, &reqBody)

	isForwarded := r.Header.Get(HeaderForwarded) != ""

	if isForwarded {
		// Forwarded request: only use local backends
		if reqBody.Model != h.registry.LocalModel() {
			http.Error(w, `{"error":{"message":"model not found","type":"not_found"}}`, http.StatusNotFound)
			return
		}
		h.handleLocalProxy(w, r)
		return
	}

	// Find routes for the requested model
	routes := h.registry.FindRoutesForModel(reqBody.Model)
	if len(routes) == 0 {
		http.Error(w, `{"error":{"message":"model not found","type":"not_found"}}`, http.StatusNotFound)
		return
	}

	route, err := peer.PickRoute(routes, h.balancer.MaxInFlightPerGPU())
	if err != nil {
		switch err {
		case balancer.ErrBackpressure:
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":{"message":"all backends at capacity","type":"rate_limit"}}`, http.StatusTooManyRequests)
		default:
			http.Error(w, `{"error":{"message":"no route available","type":"server_error"}}`, http.StatusServiceUnavailable)
		}
		return
	}

	if route.Type == peer.RouteLocal {
		proxyRequest(w, r, route.Backend, h.latencyWindow)
	} else {
		proxyToPeer(w, r, route.Addr, h.registry.NodeID())
	}
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.metricsHistory == nil || h.metricsAvailable == nil || !h.metricsAvailable() {
		json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}
	all := h.metricsHistory.AllGPUSamples()
	gpus := make(map[string][]gpu.GPUSample, len(all))
	for id, samples := range all {
		gpus[strconv.Itoa(id)] = samples
	}
	json.NewEncoder(w).Encode(map[string]any{
		"available":        true,
		"interval_seconds": 5,
		"max_samples":      720,
		"gpus":             gpus,
	})
}

func (h *Handler) handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if h.metricsBroadcaster == nil {
		http.Error(w, "metrics not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.metricsBroadcaster.Subscribe()
	defer h.metricsBroadcaster.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()
		}
	}
}

func (h *Handler) handleLocalProxy(w http.ResponseWriter, r *http.Request) {
	backend, err := h.balancer.Pick()
	if err != nil {
		switch err {
		case balancer.ErrNoHealthyBackend:
			w.Header().Set("Retry-After", "10")
			http.Error(w, `{"error":{"message":"no healthy backend","type":"server_error"}}`, http.StatusServiceUnavailable)
		case balancer.ErrBackpressure:
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":{"message":"all backends at capacity","type":"rate_limit"}}`, http.StatusTooManyRequests)
		default:
			http.Error(w, `{"error":{"message":"internal error","type":"server_error"}}`, http.StatusInternalServerError)
		}
		return
	}
	proxyRequest(w, r, backend, h.latencyWindow)
}
