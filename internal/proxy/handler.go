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
	"strings"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/logging"
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
	evictOnHardFailure bool
	cors               *CORS
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
		statusHandler:  NewStatusHandler(reg.NodeID(), reg.LocalModel(), reg.Backends(), reg.Power(), reg.Cost(), StatusLocation{Hostname: reg.Hostname(), ListenAddr: reg.ListenAddr()}),
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

// SetEvictOnHardFailure enables proxy-path eviction: when set, a hard socket
// failure (EOF, connection refused) on a backend request flips that backend
// to unhealthy immediately rather than waiting for the health-check ladder.
func (h *Handler) SetEvictOnHardFailure(enabled bool) {
	h.evictOnHardFailure = enabled
}

// SetCORS enables cross-origin access for the listed origins. Leave it unset
// and no CORS header is ever sent, which is the pre-existing behaviour: the
// API is then reachable only from a server-side caller or a page served by the
// node itself.
func (h *Handler) SetCORS(c *CORS) {
	h.cors = c
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

	// CORS runs before routing for two reasons: the allow header has to land on
	// every response including the SSE streams and the error paths, and a
	// preflight has to be answered here or not at all — the switch below
	// matches only GET and POST, so an OPTIONS would fall through to 404.
	if h.cors != nil && h.cors.apply(w, r) {
		return
	}

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
	case r.URL.Path == "/mesh" && r.Method == "GET":
		w.Header().Set("Content-Type", "text/html")
		w.Write(web.MeshHTML)
	case r.URL.Path == "/v1/mesh/stream" && r.Method == "GET":
		h.handleMeshStream(w, r)
	case r.URL.Path == "/prompt" && r.Method == "GET":
		// A full page rather than the dashboard's old in-place modal: the point
		// is that each row is a real link, so a middle- or cmd-click opens one
		// in a background tab and a batch can be triaged side by side.
		w.Header().Set("Content-Type", "text/html")
		w.Write(web.PromptHTML)
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
	case r.URL.Path == "/v1/prompts" && r.Method == "GET":
		h.handlePromptLookup(w, r)
	case r.URL.Path == "/v1/mesh/prompt" && r.Method == "GET":
		h.handleMeshPrompt(w, r)
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

// presizeCap bounds how much readBodyPresized will trust Content-Length for.
// 2 MB comfortably covers a 100K-token prompt; anything larger grows normally.
const presizeCap = 2 << 20

// HeaderTask is a fallback for clients whose SDKs forbid non-standard JSON fields.
const HeaderTask = "X-Viiwork-Task"

// maxTaskIDLen caps the task tag length — the dashboard badge needs to stay readable.
const maxTaskIDLen = 32

// sanitizeTaskID trims whitespace, strips non-printable runes, and truncates to maxTaskIDLen.
func sanitizeTaskID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b = append(b, r)
		}
	}
	if len(b) > maxTaskIDLen {
		b = b[:maxTaskIDLen]
	}
	return strings.TrimSpace(string(b))
}

// readBodyPresized buffers a request body, sizing the destination from
// Content-Length when the client supplied a usable one.
//
// io.ReadAll starts at 512 bytes and grows by repeated append, so a large chat
// completion body is reallocated and copied ~a dozen times on the way in. Chat
// clients always send Content-Length (the body is a fully-built JSON document,
// not a stream), so the size is known up front in practice.
//
// The length is treated as a HINT, never as truth: it is ignored when absent
// (-1), when implausible, and it does not bound how much is read. The caller
// has already wrapped the body in http.MaxBytesReader, which remains the only
// thing enforcing the size limit. A lying Content-Length therefore costs at
// most one wasted allocation, never a truncated or over-large read.
func readBodyPresized(r io.Reader, contentLength int64) ([]byte, error) {
	if contentLength <= 0 || contentLength > maxRequestBodySize {
		return io.ReadAll(r)
	}
	// Content-Length is CLIENT-CONTROLLED, so it must not size an allocation
	// without a bound. Sending "Content-Length: 32MB" with a one-byte body
	// would otherwise force a 32 MB allocation per request — cheap for the
	// attacker, and multiplied by concurrency an easy way to push a 62 GB host
	// into swap. io.ReadAll never had this exposure because it only ever
	// allocated what it actually read.
	//
	// Capping costs almost nothing: real chat bodies sit far below this, and a
	// genuinely larger one just grows from the cap in a few doublings instead
	// of from 512 bytes in a dozen.
	if contentLength > presizeCap {
		contentLength = presizeCap
	}
	// The headroom is bytes.MinRead, not +1: Buffer.ReadFrom asks grow() for
	// MinRead free bytes before EVERY read, including the final one that just
	// returns io.EOF. Sizing to exactly Content-Length therefore triggers one
	// last doubling and allocates more than io.ReadAll did — measured, not
	// theorised (251 KB/op vs 202 KB before this line was corrected).
	buf := bytes.NewBuffer(make([]byte, 0, contentLength+bytes.MinRead))
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Byte-level keys used to gate the fast field extraction below.
var (
	keyModelJSON = []byte(`"model"`)
	keyThinkJSON = []byte(`"think"`)
	keyTaskJSON  = []byte(`"task"`)
	escapePrefix = []byte(`\u`)
)

// promptExtract pulls just enough of a chat/completions body to recover the
// user-facing prompt text for the dashboard's prompt history. It mirrors the
// same last-user-message convention handlePipeline already uses for
// sourceText, plus the legacy /v1/completions "prompt" string field.
type promptExtract struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Prompt string `json:"prompt"`
}

// extractPromptText is best-effort: a body with multimodal content parts (an
// array instead of a plain string) fails to decode into Content for that one
// message, same as elsewhere in this file, and simply yields no text there
// rather than an error the caller has to handle.
func extractPromptText(body []byte) string {
	var p promptExtract
	json.Unmarshal(body, &p)
	for i := len(p.Messages) - 1; i >= 0; i-- {
		if p.Messages[i].Role == "user" && p.Messages[i].Content != "" {
			return p.Messages[i].Content
		}
	}
	return p.Prompt
}

// extractModelFast returns the value of a top-level "model" key without parsing
// the rest of the body, reporting false when it cannot do so safely.
//
// The motivation: handleProxy needs three small scalars, but json.Unmarshal must
// lex the entire document to produce them — including a prompt that can run to
// megabytes. Routing a 16K-token request cost ~762us of pure lexing before this.
//
// Three guards keep it honest, and any of them failing means the caller falls
// back to the full unmarshal:
//
//  1. "think" and "task" must be absent from the raw bytes. They are viiwork
//     extensions and almost never present; if either string appears anywhere,
//     even inside prompt text, we take the slow path rather than guess.
//  2. "model" must appear exactly once. json.Unmarshal resolves duplicate keys
//     to the LAST occurrence while an early-stopping scan would take the first,
//     so a body with two "model" keys must not use this path.
//  3. Scanning stops at the first non-scalar value. Skipping over a nested
//     array with the decoder would cost what we are trying to avoid, so if
//     "model" does not appear before "messages" there is nothing to win.
//
// Correctness rests on encoding/json's own lexer — this does not hand-roll JSON
// parsing, it just stops reading early.
func extractModelFast(body []byte) (string, bool) {
	if bytes.Contains(body, keyThinkJSON) || bytes.Contains(body, keyTaskJSON) {
		return "", false
	}
	// JSON permits unicode escapes in KEYS, so {"\u0074hink":true} is a valid
	// spelling of "think" that the byte scan above cannot see. Early-stopping
	// cannot rule out a later key either — by the time the decoder reaches an
	// escaped "think" we have already returned on "model". The byte scan is
	// therefore the only thing proving absence, and it must not be defeatable,
	// so any escape sequence anywhere disqualifies the fast path.
	//
	// Cost of being this strict: Python's json.dumps defaults to
	// ensure_ascii=True and escapes every non-ASCII character, so clients
	// sending non-English prompts fall back to the full unmarshal. That is the
	// pre-existing behaviour and always correct — just not faster.
	if bytes.Contains(body, escapePrefix) {
		return "", false
	}
	if bytes.Count(body, keyModelJSON) != 1 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := keyTok.(string)
		// The byte-level guards above cannot see keys written with JSON unicode
		// escapes — {"\u0074hink":true} is a valid spelling of "think" that
		// bytes.Contains will miss, and taking the fast path there would drop a
		// think/task the client really sent. dec.Token() has already decoded the
		// escape, so re-checking the decoded key closes the hole for free.
		if key == "think" || key == "task" {
			return "", false
		}
		valTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, isDelim := valTok.(json.Delim); isDelim {
			// Nested object or array: skipping it is the expense we are avoiding.
			_ = d
			return "", false
		}
		if key == "model" {
			s, ok := valTok.(string)
			return s, ok
		}
	}
	return "", false
}

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Read and buffer body to extract model and think parameters
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	bodyBytes, err := readBodyPresized(r.Body, r.ContentLength)
	if err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, `{"error":{"message":"request body too large","type":"invalid_request"}}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":{"message":"failed to read request","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}

	var reqBody struct {
		Model string `json:"model"`
		Think *bool  `json:"think"`
		Task  string `json:"task"`
	}
	if model, ok := extractModelFast(bodyBytes); ok {
		// Fast path proved think/task absent, so the zero values are correct.
		reqBody.Model = model
	} else {
		json.Unmarshal(bodyBytes, &reqBody)
	}
	thinkDisabled := reqBody.Think == nil || !*reqBody.Think

	// Resolve task ID: body "task" wins, else X-Viiwork-Task header.
	taskID := sanitizeTaskID(reqBody.Task)
	if taskID == "" {
		taskID = sanitizeTaskID(r.Header.Get(HeaderTask))
	}

	// Strip "task" from the body before forwarding so backends never see it.
	if reqBody.Task != "" {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &generic); err == nil {
			if _, present := generic["task"]; present {
				delete(generic, "task")
				if rewritten, err := json.Marshal(generic); err == nil {
					bodyBytes = rewritten
				}
			}
		}
	}
	// Propagate task to peers via header (body has been stripped).
	if taskID != "" {
		r.Header.Set(HeaderTask, taskID)
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))

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
			h.handlePipeline(w, r, p, locale, localeKey, sourceText, reqBody.Model, taskID)
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
	if h.activity != nil {
		h.activity.StorePrompt(rid, model, extractPromptText(bodyBytes))
		// Capture the response on its way back to the client so the dashboard
		// can show what came out, not only what went in. Wrapping here covers
		// both branches below at once, local and peer-routed alike, and the
		// deferred store runs after whichever one ran has finished writing.
		var capw *captureWriter
		w, capw = newCaptureWriter(w)
		defer func() {
			h.activity.StoreOutput(rid, model, capw.Output(), time.Since(start).Milliseconds())
		}()
	}
	if route.Type == peer.RouteLocal {
		if logging.DebugEnabled() {
			log.Printf("[debug] %s → gpu-%d (in_flight=%d)", model, route.Backend.GPUID, route.Backend.InFlight())
		}
		if h.activity != nil {
			h.activity.EmitRequestTask(rid, route.Backend.GPUID, taskID, "%s → %s", model, route.Backend.Label())
		}
		aborted := proxyRequest(w, r, route.Backend, h.latencyWindow, thinkDisabled, h.evictOnHardFailure)
		elapsed := time.Since(start).Round(time.Millisecond)
		if logging.DebugEnabled() {
			log.Printf("[debug] %s → gpu-%d finished (elapsed=%s aborted=%v in_flight=%d)", model, route.Backend.GPUID, elapsed, aborted, route.Backend.InFlight())
		}
		if h.activity != nil {
			if aborted {
				h.activity.EmitRequestTask(rid, route.Backend.GPUID, taskID, "%s → %s aborted by client (%s)", model, route.Backend.Label(), elapsed)
			} else {
				h.activity.EmitRequestTask(rid, route.Backend.GPUID, taskID, "%s → %s done (%s)", model, route.Backend.Label(), elapsed)
			}
		}
	} else {
		if logging.DebugEnabled() {
			log.Printf("[debug] %s → peer %s", model, route.Addr)
		}
		if h.activity != nil {
			h.activity.EmitRequestTask(rid, -1, taskID, "%s → peer %s", model, route.Addr)
		}
		// Write-through in-flight: subsequent picks on this node see the
		// dispatch immediately, before the next poll of /v1/status updates
		// the peer's reported total.
		if route.Peer != nil {
			route.Peer.IncLocalInFlight()
		}
		proxyToPeer(w, r, route.Addr, h.registry.NodeID(), thinkDisabled)
		if route.Peer != nil {
			route.Peer.DecLocalInFlight()
		}
		elapsed := time.Since(start).Round(time.Millisecond)
		if logging.DebugEnabled() {
			log.Printf("[debug] %s → peer %s finished (elapsed=%s)", model, route.Addr, elapsed)
		}
		if h.activity != nil {
			h.activity.EmitRequestTask(rid, -1, taskID, "%s → peer %s done (%s)", model, route.Addr, elapsed)
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
	proxyRequest(w, r, backend, h.latencyWindow, thinkDisabled, h.evictOnHardFailure)
}

func (h *Handler) handleActivity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if h.activity == nil {
		json.NewEncoder(w).Encode(map[string]any{"events": []struct{}{}})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"events": h.activity.Recent()})
}

// handlePromptLookup serves this node's own stored prompt for a request id.
// It is also what handleMeshPrompt proxies to on the peer that actually owns
// a given rid — request ids are a per-process counter, not cluster-wide, so
// a lookup only ever makes sense against the node that minted it.
func (h *Handler) handlePromptLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	rid, err := strconv.ParseInt(r.URL.Query().Get("rid"), 10, 64)
	if err != nil || h.activity == nil {
		http.NotFound(w, r)
		return
	}
	entry, ok := h.activity.GetPrompt(rid)
	if !ok {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(entry)
}

// handleMeshPrompt is the fan-out entry point the mesh dashboard's prompt
// modal calls. An empty addr means the request originated on whichever node
// the browser's /v1/mesh/stream connection landed on (mirroring how
// handleMeshStream leaves MeshEvent.Addr unset for its own local events), so
// it is served from this node's own store. A non-empty addr names a peer, and
// the browser may not be able to reach it directly — LAN-addressed peers,
// possibly tunnelled to only one node — so this proxies server-side instead,
// the same reasoning as the rest of the mesh fan-out.
func (h *Handler) handleMeshPrompt(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("addr")
	if addr == "" {
		h.handlePromptLookup(w, r)
		return
	}
	// addr is attacker-controllable: it arrives as a query parameter, and this
	// handler fetches it and echoes the response back. Forwarding it verbatim
	// would turn any node into an SSRF probe for its own network — the mesh is
	// on a LAN alongside IPMI and management interfaces. Only addresses this
	// node already peers with are allowed; the dashboard never needs any other.
	if !h.isPeerAddr(addr) {
		http.Error(w, `{"error":{"message":"unknown peer","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	// Re-serialise rid from the parsed integer rather than passing the raw
	// string through, so nothing can smuggle extra query parameters or path
	// segments into the peer request.
	rid, err := strconv.ParseInt(r.URL.Query().Get("rid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	url := "http://" + addr + "/v1/prompts?rid=" + strconv.FormatInt(rid, 10)
	req, err := http.NewRequestWithContext(r.Context(), "GET", url, nil)
	if err != nil {
		http.Error(w, `{"error":{"message":"bad peer address","type":"invalid_request"}}`, http.StatusBadRequest)
		return
	}
	// peerClient carries a timeout; http.DefaultClient does not, and a peer
	// that accepts the connection but never answers would otherwise pin this
	// goroutine and its response writer indefinitely.
	resp, err := peerClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":{"message":"peer unreachable","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	// Bound the copy: the body is a prompt entry from a peer, and a peer that
	// is compromised or simply wrong should not be able to stream unbounded
	// data through this node into the browser.
	io.Copy(w, io.LimitReader(resp.Body, maxPromptResponseBytes))
}

// maxPromptResponseBytes caps a proxied peer prompt response. The store
// truncates prompts well below this, so the slack only covers JSON overhead.
const maxPromptResponseBytes = 1 << 20

// isPeerAddr reports whether addr is one of this node's configured peers.
// Matching is exact against the configured host:port — the peer list comes
// from config, so no normalisation or DNS resolution is involved, and none
// should be: resolving here would reintroduce the SSRF this guards against.
func (h *Handler) isPeerAddr(addr string) bool {
	if h.registry == nil {
		return false
	}
	for _, p := range h.registry.Peers() {
		if p.Addr == addr {
			return true
		}
	}
	return false
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
