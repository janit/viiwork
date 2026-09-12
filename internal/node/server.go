package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
	"github.com/janit/viiwork/v2/web"
)

// maxPromptResponseBytes caps a proxied member prompt response. The store
// truncates prompts well below this, so the slack only covers JSON overhead.
const maxPromptResponseBytes = 1 << 20

// promptClient is /v1/mesh/prompt's outbound client: the dial and the whole
// request are bounded, so a member that accepts the connection but never
// answers cannot pin a handler goroutine (as v1).
var promptClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	},
}

type ServerDeps struct {
	Self           string
	Version        string
	Started        time.Time
	StreamCtx      context.Context // cancelled at shutdown; ends every SSE handler
	Inference      http.Handler    // *proxy.Handler
	Aliases        http.Handler    // alias.NewHandler
	AliasInfo      func() meshapi.AliasesResponse
	Status         func() meshapi.NodeStatus
	Cluster        func() meshapi.ClusterResponse
	Members        func() []mesh.Member
	Activity       *activity.Log
	GPUHistory     *gpu.History
	GPUBroadcaster *gpu.Broadcaster
	GPUAvailable   func() bool
	PowerControl   *power.Controller // nil = power endpoints answer 503
	CORS           *CORS             // nil = no CORS headers
	Health         func() (healthy, total int, models int)
}

type server struct {
	d ServerDeps
}

// NewServer is the node's one HTTP API: inference, aliases, status, the
// dashboards and their streams.
func NewServer(d ServerDeps) http.Handler {
	if d.StreamCtx == nil {
		d.StreamCtx = context.Background()
	}
	return &server{d: d}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rv := recover(); rv != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			log.Printf("[PANIC] %s %s: %v\n%s", r.Method, r.URL.Path, rv, buf[:n])
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()

	// CORS runs before routing for two reasons: the allow header has to land on
	// every response including the SSE streams and the error paths, and a
	// preflight has to be answered here or not at all, since the routes below
	// match only GET and POST.
	if s.d.CORS != nil && s.d.CORS.apply(w, r) {
		return
	}

	path, get := r.URL.Path, r.Method == http.MethodGet
	switch {
	case path == meshapi.PathHealth && get:
		s.handleHealth(w)
	case path == meshapi.PathModels, path == meshapi.PathCapacity,
		path == meshapi.PathChatCompletions, path == meshapi.PathCompletions, path == meshapi.PathEmbeddings:
		s.d.Inference.ServeHTTP(w, r)
	case path == meshapi.PathAliases || strings.HasPrefix(path, meshapi.PathAliases+"/"):
		s.d.Aliases.ServeHTTP(w, r)
	case path == meshapi.PathStatus && get:
		writeJSON(w, s.d.Status())
	case path == meshapi.PathCluster && get:
		writeJSON(w, s.d.Cluster())
	case path == "/" && get:
		writePage(w, web.DashboardHTML)
	case path == "/mesh" && get:
		writePage(w, web.MeshHTML)
	case path == "/chat" && get:
		writePage(w, web.ChatHTML)
	case path == "/prompt" && get:
		// A full page rather than a modal: each row is a real link, so a batch
		// can be opened in background tabs and read side by side.
		writePage(w, web.PromptHTML)
	case path == "/v1/metrics" && get:
		s.handleMetrics(w)
	case path == "/v1/metrics/stream" && get:
		s.handleMetricsStream(w, r)
	case path == meshapi.PathActivity && get:
		s.handleActivity(w)
	case path == meshapi.PathActivityStream && get:
		s.handleActivityStream(w, r)
	case path == meshapi.PathPrompts && get:
		s.handlePromptLookup(w, r)
	case path == meshapi.PathMeshPrompt && get:
		s.handleMeshPrompt(w, r)
	case path == meshapi.PathMeshStream && get:
		s.handleMeshStream(w, r)
	case path == meshapi.PathPower && r.Method == http.MethodPost:
		s.handlePower(w, r)
	case path == meshapi.PathMeshPower && r.Method == http.MethodPost:
		s.handleMeshPower(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writePage(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html")
	w.Write(page)
}

// handleHealth answers 503 only when models are configured and not one
// backend is healthy (P6 Decision 5): a node with no models is a router, and
// healthy.
func (s *server) handleHealth(w http.ResponseWriter) {
	healthy, total, models := 0, 0, 0
	if s.d.Health != nil {
		healthy, total, models = s.d.Health()
	}
	resp := map[string]any{
		"status":           "ok",
		"version":          s.d.Version,
		"node":             s.d.Self,
		"uptime_seconds":   int(time.Since(s.d.Started).Seconds()),
		"backends_healthy": healthy,
		"backends_total":   total,
	}
	w.Header().Set("Content-Type", "application/json")
	if models > 0 && healthy == 0 {
		resp["status"] = "unhealthy"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *server) handleMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if s.d.GPUHistory == nil || s.d.GPUAvailable == nil || !s.d.GPUAvailable() {
		json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}
	all := s.d.GPUHistory.AllGPUSamples()
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

func (s *server) handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if s.d.GPUBroadcaster == nil {
		http.Error(w, "metrics not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.d.GPUBroadcaster.Subscribe()
	defer s.d.GPUBroadcaster.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.d.StreamCtx.Done():
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

func (s *server) handleActivity(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	if s.d.Activity == nil {
		json.NewEncoder(w).Encode(map[string]any{"events": []struct{}{}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"events": s.d.Activity.Recent()})
}

func (s *server) handleActivityStream(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if s.d.Activity == nil {
		http.Error(w, "activity log not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe before reading the backlog, never after: an event landing
	// between the two would otherwise fall in the gap and be delivered by
	// neither. The overlap this creates instead — an event in both the backlog
	// and the live feed — is the safe direction, and consumers deduplicate.
	ch := s.d.Activity.Subscribe()
	defer s.d.Activity.Unsubscribe(ch)

	for _, ev := range s.d.Activity.Backlog() {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	f.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.d.StreamCtx.Done():
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

// handlePromptLookup serves this node's own stored prompt for a request id.
// It is also what handleMeshPrompt proxies to on the member that actually owns
// a given rid — request ids are a per-process counter, not cluster-wide, so a
// lookup only ever makes sense against the node that minted it.
func (s *server) handlePromptLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rid, err := strconv.ParseInt(r.URL.Query().Get("rid"), 10, 64)
	if err != nil || s.d.Activity == nil {
		http.NotFound(w, r)
		return
	}
	entry, ok := s.d.Activity.GetPrompt(rid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(entry)
}

// handleMeshPrompt is the fan-out entry point the mesh dashboard's prompt page
// calls. An empty addr means the request originated on whichever node the
// browser's /v1/mesh/stream connection landed on (mirroring how the mesh
// stream leaves MeshEvent.Addr unset for its own local events), so it is served
// from this node's own store. A non-empty addr names a member, and the browser
// may not be able to reach it directly, so this proxies server-side instead,
// the same reasoning as the rest of the mesh fan-out.
func (s *server) handleMeshPrompt(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		s.handlePromptLookup(w, r)
		return
	}
	// addr is attacker-controllable: it arrives as a query parameter, and this
	// handler fetches it and echoes the response back. Forwarding it verbatim
	// would turn any node into an SSRF probe for its own network — the mesh is
	// on a LAN alongside IPMI and management interfaces. Only alive members'
	// API addresses are allowed (P6 Decision 9); the dashboard never needs any
	// other.
	if !s.isMemberAddr(addr) {
		http.Error(w, `{"error":{"message":"unknown peer","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	// Re-serialise rid from the parsed integer rather than passing the raw
	// string through, so nothing can smuggle extra query parameters or path
	// segments into the member request.
	rid, err := strconv.ParseInt(r.URL.Query().Get("rid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	url := "http://" + addr + meshapi.PathPrompts + "?rid=" + strconv.FormatInt(rid, 10)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, `{"error":{"message":"bad peer address","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	resp, err := promptClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":{"message":"peer unreachable","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	// Bound the copy: the body is a prompt entry from a member, and a member
	// that is compromised or simply wrong should not be able to stream
	// unbounded data through this node into the browser.
	io.Copy(w, io.LimitReader(resp.Body, maxPromptResponseBytes))
}

// isMemberAddr reports whether addr is an alive member's API address. Matching
// is exact: resolving or normalising here would reintroduce the SSRF this
// guards against.
func (s *server) isMemberAddr(addr string) bool {
	if s.d.Members == nil {
		return false
	}
	for _, m := range s.d.Members() {
		if m.State == meshapi.MemberAlive && m.APIAddr() == addr {
			return true
		}
	}
	return false
}
