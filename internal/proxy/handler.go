package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/pipeline"
	"github.com/janit/viiwork/web"
)

var startTime = time.Now()

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
	activity           *activity.Log
	pipelineResolver   *PipelineResolver
	pipelineExecutor   *pipeline.Executor
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

func (h *Handler) SetActivity(actLog *activity.Log) {
	h.activity = actLog
}


func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rv := recover(); rv != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			log.Printf("[PANIC] %s %s: %v\n%s", r.Method, r.URL.Path, rv, buf[:n])
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()

	switch {
	case r.URL.Path == "/health" && r.Method == "GET":
		h.handleHealth(w, r)
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
		if r.Method != "POST" {
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request"}}`, http.StatusMethodNotAllowed)
			return
		}
		h.handleProxy(w, r)
	case r.URL.Path == "/v1/metrics" && r.Method == "GET":
		h.handleMetrics(w, r)
	case r.URL.Path == "/v1/metrics/stream" && r.Method == "GET":
		h.handleMetricsStream(w, r)
	case r.URL.Path == "/v1/activity" && r.Method == "GET":
		h.handleActivity(w, r)
	case r.URL.Path == "/v1/activity/stream" && r.Method == "GET":
		h.handleActivityStream(w, r)
	case r.URL.Path == "/v1/embeddings" && r.Method == "POST":
		h.handleProxy(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if h.registry != nil {
		resp := ModelsResponse{Object: "list", Data: h.registry.AllModels()}
		if h.pipelineResolver != nil {
			resp.Data = append(resp.Data, h.pipelineResolver.VirtualModels()...)
		}
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

// maxRequestBodySize limits inference request bodies to 32 MB.
const maxRequestBodySize = 32 << 20

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Read and buffer body to extract model and think parameters
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, `{"error":{"message":"request body too large","type":"invalid_request"}}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":{"message":"failed to read request","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	var reqBody struct {
		Model string `json:"model"`
		Think *bool  `json:"think"`
	}
	json.Unmarshal(bodyBytes, &reqBody)
	thinkDisabled := reqBody.Think == nil || !*reqBody.Think

	// Pipeline interception
	if h.pipelineResolver != nil {
		if p, locale, localeKey, ok := h.pipelineResolver.Resolve(reqBody.Model); ok {
			var fullReq struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			json.Unmarshal(bodyBytes, &fullReq)
			sourceText := ""
			for i := len(fullReq.Messages) - 1; i >= 0; i-- {
				if fullReq.Messages[i].Role == "user" {
					sourceText = fullReq.Messages[i].Content
					break
				}
			}
			if sourceText == "" {
				http.Error(w, `{"error":{"message":"no user message found","type":"invalid_request"}}`, http.StatusBadRequest)
				return
			}
			h.handlePipeline(w, r, p, locale, localeKey, sourceText, reqBody.Model)
			return
		}
		// Unknown locale in a pipeline model name
		if pName, matched := h.pipelineResolver.MatchesPipelinePrefix(reqBody.Model); matched {
			avail := h.pipelineResolver.AvailableLocales(pName)
			msg := fmt.Sprintf("unknown locale in model '%s', available: %v", reqBody.Model, avail)
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": msg, "type": "invalid_request"},
			})
			return
		}
	}

	// No registry = standalone mode, use local balancer directly
	if h.registry == nil {
		h.handleLocalProxy(w, r, thinkDisabled)
		return
	}

	forwardedBy := r.Header.Get(HeaderForwarded)
	isForwarded := forwardedBy != "" && h.registry.IsKnownPeer(forwardedBy)

	if isForwarded {
		// Forwarded request from a known peer: only use local backends
		if reqBody.Model != h.registry.LocalModel() {
			http.Error(w, `{"error":{"message":"model not found","type":"not_found"}}`, http.StatusNotFound)
			return
		}
		h.handleLocalProxy(w, r, thinkDisabled)
		return
	}

	// Find routes for the requested model
	routes := h.registry.FindRoutesForModel(reqBody.Model)
	if len(routes) == 0 {
		log.Printf("[debug] no routes for model %q", reqBody.Model)
		http.Error(w, `{"error":{"message":"model not found","type":"not_found"}}`, http.StatusNotFound)
		return
	}

	route, err := peer.PickRoute(routes, h.balancer.MaxInFlightPerGPU())
	if err != nil {
		// Log in-flight state for all backends when routing fails
		for _, bs := range h.balancer.Backends() {
			log.Printf("[debug] backpressure: gpu-%d status=%s in_flight=%d", bs.GPUID, bs.Status(), bs.InFlight())
		}
		switch err {
		case balancer.ErrBackpressure:
			log.Printf("[debug] 429 backpressure for model %q — all backends at capacity", reqBody.Model)
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":{"message":"all backends at capacity","type":"rate_limit"}}`, http.StatusTooManyRequests)
		default:
			log.Printf("[debug] 503 no route for model %q: %v", reqBody.Model, err)
			http.Error(w, `{"error":{"message":"no route available","type":"server_error"}}`, http.StatusServiceUnavailable)
		}
		return
	}

	model := reqBody.Model
	start := time.Now()
	rid := activity.NewRequestID()
	if route.Type == peer.RouteLocal {
		log.Printf("[debug] %s → gpu-%d (in_flight=%d)", model, route.Backend.GPUID, route.Backend.InFlight())
		if h.activity != nil {
			h.activity.EmitRequest(rid, route.Backend.GPUID, "%s → gpu-%d", model, route.Backend.GPUID)
		}
		aborted := proxyRequest(w, r, route.Backend, h.latencyWindow, thinkDisabled)
		elapsed := time.Since(start).Round(time.Millisecond)
		log.Printf("[debug] %s → gpu-%d finished (elapsed=%s aborted=%v in_flight=%d)", model, route.Backend.GPUID, elapsed, aborted, route.Backend.InFlight())
		if h.activity != nil {
			if aborted {
				h.activity.EmitRequest(rid, route.Backend.GPUID, "%s → gpu-%d aborted by client (%s)", model, route.Backend.GPUID, elapsed)
			} else {
				h.activity.EmitRequest(rid, route.Backend.GPUID, "%s → gpu-%d done (%s)", model, route.Backend.GPUID, elapsed)
			}
		}
	} else {
		log.Printf("[debug] %s → peer %s", model, route.Addr)
		if h.activity != nil {
			h.activity.EmitRequest(rid, -1, "%s → peer %s", model, route.Addr)
		}
		proxyToPeer(w, r, route.Addr, h.registry.NodeID(), thinkDisabled)
		elapsed := time.Since(start).Round(time.Millisecond)
		log.Printf("[debug] %s → peer %s finished (elapsed=%s)", model, route.Addr, elapsed)
		if h.activity != nil {
			h.activity.EmitRequest(rid, -1, "%s → peer %s done (%s)", model, route.Addr, elapsed)
		}
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

func (h *Handler) handleLocalProxy(w http.ResponseWriter, r *http.Request, thinkDisabled bool) {
	backend, err := h.balancer.Pick()
	if err != nil {
		for _, bs := range h.balancer.Backends() {
			log.Printf("[debug] local pick failed: gpu-%d status=%s in_flight=%d", bs.GPUID, bs.Status(), bs.InFlight())
		}
		switch err {
		case balancer.ErrNoHealthyBackend:
			log.Printf("[debug] 503 no healthy backend")
			w.Header().Set("Retry-After", "10")
			http.Error(w, `{"error":{"message":"no healthy backend","type":"server_error"}}`, http.StatusServiceUnavailable)
		case balancer.ErrBackpressure:
			log.Printf("[debug] 429 local backpressure — all backends at capacity")
			w.Header().Set("Retry-After", "2")
			http.Error(w, `{"error":{"message":"all backends at capacity","type":"rate_limit"}}`, http.StatusTooManyRequests)
		default:
			log.Printf("[debug] 500 balancer error: %v", err)
			http.Error(w, `{"error":{"message":"internal error","type":"server_error"}}`, http.StatusInternalServerError)
		}
		return
	}
	proxyRequest(w, r, backend, h.latencyWindow, thinkDisabled)
}

func (h *Handler) handleActivity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.activity == nil {
		json.NewEncoder(w).Encode(map[string]any{"events": []struct{}{}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"events": h.activity.Recent()})
}

func (h *Handler) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if h.activity == nil {
		http.Error(w, "activity log not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.activity.Subscribe()
	defer h.activity.Unsubscribe(ch)

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

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	backends := h.balancer.Backends()
	healthy, total, totalInFlight := 0, len(backends), int64(0)
	for _, b := range backends {
		if b.Status() == balancer.StatusHealthy {
			healthy++
		}
		totalInFlight += b.InFlight()
	}

	resp := map[string]any{
		"status":           "ok",
		"version":          Version,
		"uptime_seconds":   int(time.Since(startTime).Seconds()),
		"backends_healthy": healthy,
		"backends_total":   total,
	}

	if h.registry != nil {
		resp["node_id"] = h.registry.NodeID()
		resp["model"] = h.registry.LocalModel()
		peers := h.registry.Peers()
		reachable := 0
		for _, p := range peers {
			if p.Status() == peer.StatusReachable {
				reachable++
			}
		}
		resp["peers_reachable"] = reachable
		resp["peers_total"] = len(peers)
	}

	if healthy == 0 {
		resp["status"] = "unhealthy"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}
	json.NewEncoder(w).Encode(resp)
}
