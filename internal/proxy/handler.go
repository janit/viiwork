package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/meshapi"
)

// Error types the handler writes besides meshapi's.
const (
	errTypeInvalidRequest = "invalid_request"
	errTypeNotFound       = "not_found"
	errTypeServer         = "server_error"
)

// queueRetryAfter is the Retry-After, in seconds, on a 429 from the queue.
const queueRetryAfter = "2"

// ResolveError is written as-is by the handler: status, error type, message, and
// Retry-After seconds when non-zero. P5's alias resolver returns it.
type ResolveError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter int
}

func (e *ResolveError) Error() string { return e.Message }

// Resolver maps the requested model name to the real model and the alias it
// was reached through ("" for a real name).
type Resolver func(requested string) (model, alias string, err error)

// ModelLister adds entries to /v1/models.
type ModelLister func() []meshapi.ModelEntry

// LocalCapacity is this node's per-model occupancy; *supervisor.Supervisor
// satisfies it.
type LocalCapacity interface {
	Capacity() []meshapi.ModelCapacity
}

type Deps struct {
	Self         string
	Version      string
	Router       *route.Router
	Reports      route.Reports // *capacity.Poller
	Local        LocalCapacity // *supervisor.Supervisor
	Auth         *ForwardAuth
	Counters     *Counters
	ForwardRetry int
	Activity     *activity.Log // nil = no events, no prompt history
	Pipelines    *PipelineResolver
	PipelineExec *pipeline.Executor
	Resolve      Resolver    // nil = identity
	ExtraModels  ModelLister // nil = none
}

// Handler serves inference, /v1/models and /v1/capacity. The node server
// wraps it with panic recovery, CORS, /health and the dashboards.
type Handler struct {
	d Deps
}

func NewHandler(d Deps) *Handler { return &Handler{d: d} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, errTypeInvalidRequest, "method not allowed")
			return
		}
		h.handleInference(w, r)
	case meshapi.PathModels:
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.handleModels(w)
	case meshapi.PathCapacity:
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.handleCapacity(w)
	default:
		http.NotFound(w, r)
	}
}

// handleInference runs the inference flow of P4 Task 8: parse, verify a
// forward, resolve, then dispatch with retries over the router.
func (h *Handler) handleInference(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	body, err := readBodyPresized(r.Body, r.ContentLength)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, errTypeInvalidRequest, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "failed to read request")
		return
	}

	var fields struct {
		Model string `json:"model"`
		Think *bool  `json:"think"`
		Task  string `json:"task"`
	}
	if model, ok := extractModelFast(body); ok {
		// The fast path proved think and task absent, so the zero values are right.
		fields.Model = model
	} else {
		json.Unmarshal(body, &fields)
	}
	if fields.Model == "" {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "model is required")
		return
	}
	thinkDisabled := fields.Think == nil || !*fields.Think

	// The body's task wins over the header. Engines never see the field, and
	// peers get the task as the header.
	taskID := sanitizeTaskID(fields.Task)
	if taskID == "" {
		taskID = sanitizeTaskID(r.Header.Get(HeaderTask))
	}
	if fields.Task != "" {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(body, &generic); err == nil {
			if _, present := generic["task"]; present {
				delete(generic, "task")
				if rewritten, err := json.Marshal(generic); err == nil {
					body = rewritten
				}
			}
		}
	}
	if taskID != "" {
		r.Header.Set(HeaderTask, taskID)
	}

	_, forwarded := h.d.Auth.Verify(r, body)
	stripMeshHeaders(r.Header)

	if !forwarded && h.d.Pipelines != nil {
		if p, locale, localeKey, ok := h.d.Pipelines.Resolve(fields.Model); ok {
			sourceText := pipelineSourceText(body)
			if sourceText == "" {
				writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "no user message found")
				return
			}
			h.handlePipeline(w, r, p, locale, localeKey, sourceText, fields.Model, taskID)
			return
		}
		if name, matched := h.d.Pipelines.MatchesPipelinePrefix(fields.Model); matched {
			msg := fmt.Sprintf("unknown locale in model '%s', available: %v", fields.Model, h.d.Pipelines.AvailableLocales(name))
			writeError(w, http.StatusBadRequest, errTypeInvalidRequest, msg)
			return
		}
	}

	// A forward carries the real name, resolved once on the origin.
	model, alias := fields.Model, ""
	if !forwarded && h.d.Resolve != nil {
		m, a, err := h.d.Resolve(fields.Model)
		if err != nil {
			var re *ResolveError
			if errors.As(err, &re) {
				if re.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(re.RetryAfter))
				}
				writeError(w, re.Status, re.Type, re.Message)
				return
			}
			log.Printf("proxy: resolving model %q: %v", fields.Model, err)
			writeError(w, http.StatusInternalServerError, errTypeServer, "model resolution failed")
			return
		}
		model, alias = m, a
		if alias != "" {
			// Engines and peers see the name they serve; a receiver never
			// resolves. Only aliased requests pay for the rewrite.
			body = rewriteModel(body, model)
		}
	}

	// The pin is compared against member names by the router and never
	// dialled. A forward ignores it: it already reached the pinned node.
	// Guarded on RawQuery so the common request parses nothing.
	var host string
	if !forwarded && r.URL.RawQuery != "" {
		pin, ok := sanitizeHost(r.URL.Query().Get(meshapi.QueryHost))
		if !ok {
			writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "invalid host parameter")
			return
		}
		host = pin
	}

	w, capture := newCaptureWriter(w)
	// Forwards leave no prompt history: the origin already has it (Decision 12).
	record := !forwarded && h.d.Activity != nil
	var rid int64
	if record {
		// The prompt history names both the model and the alias it was asked
		// for (P5 Decision 17); events and counters keep the real model.
		historyModel := model
		if alias != "" {
			historyModel = model + " (alias " + alias + ")"
		}
		rid = activity.NewRequestID()
		h.d.Activity.StorePrompt(rid, historyModel, extractPromptText(body))
		defer func() {
			h.d.Activity.StoreOutput(rid, historyModel, capture.Output(), time.Since(start).Milliseconds())
		}()
	}

	lease, queued, err := h.d.Router.Acquire(r.Context(), route.Request{Model: model, Host: host, Forwarded: forwarded})
	if err != nil {
		h.writeAcquireError(w, err, model, host)
		return
	}
	var (
		tried   map[string]bool
		retries int
		final   bool // the last dispatch: the one after the final acquisition
	)
	for {
		t := lease.Target()
		if queued > 0 {
			w.Header().Set(meshapi.HeaderQueuedMs, strconv.FormatInt(queued.Milliseconds(), 10))
		}
		if alias != "" {
			w.Header().Set(meshapi.HeaderAlias, alias)
		}
		label := t.BackendID
		if !t.Local {
			label = meshapi.PeerLabel(t.Node)
		}
		if record {
			h.d.Activity.EmitRequestTask(rid, -1, taskID, "%s", meshapi.RequestStarted(model, label))
		}

		var res execResult
		if t.Local {
			res = serveLocal(w, r, body, lease.Backend(), model, h.d.Self, thinkDisabled)
		} else {
			res = forwardToPeer(w, r, body, t, h.d.Auth, h.d.Self)
			if res.Outcome == outcomeRetryable {
				lease.Refused() // before Release: its wake must not hand the peer back (Decision 18)
			}
		}
		lease.Release()

		if res.Outcome == outcomeServed {
			if record {
				elapsed := time.Since(start).Round(time.Millisecond)
				msg := meshapi.RequestDone(model, label, elapsed)
				if res.Aborted {
					msg = meshapi.RequestAborted(model, label, elapsed)
				}
				h.d.Activity.EmitRequestTask(rid, -1, taskID, "%s", msg)
			}
			// Counted where the request ran, so a forward is counted by its
			// receiver and not twice (spec).
			if t.Local {
				tokens, _ := capture.CompletionTokens()
				h.d.Counters.Add(model, tokens)
			}
			return
		}

		if logging.DebugEnabled() {
			log.Printf("[debug] %s: dispatch not served: %s", model, res.Reason)
		}
		if forwarded {
			// One dispatch only: the origin picks again (Decision 16).
			writeError(w, http.StatusServiceUnavailable, meshapi.ErrTypeUnavailable, "backend failed before responding")
			return
		}
		if final {
			log.Printf("proxy: %s: no route could serve the request; last: %s", model, res.Reason)
			h.endUnserved(rid, taskID, model, label, start, record)
			writeError(w, http.StatusBadGateway, errTypeServer, "no route could serve the request")
			return
		}

		if tried == nil {
			tried = map[string]bool{}
		}
		tried[t.Key()] = true
		if retries < h.d.ForwardRetry {
			retries++
			if next, err := h.d.Router.Pick(route.Request{Model: model, Host: host, Exclude: tried}); err == nil {
				lease = next
				continue
			}
		}

		// The final acquisition may queue, but only for what is left of the
		// request's queue budget (Decision 17).
		budget := h.d.Router.QueueTimeout() - queued
		if budget <= 0 {
			budget = -1
		}
		var waited time.Duration
		lease, waited, err = h.d.Router.Acquire(r.Context(), route.Request{Model: model, Host: host, QueueBudget: budget})
		queued += waited
		if err != nil {
			h.endUnserved(rid, taskID, model, label, start, record)
			h.writeAcquireError(w, err, model, host)
			return
		}
		final = true
	}
}

// rewriteModel sets the body's model field, keeping every other field as it
// was. A body that does not decode as an object is left alone.
func rewriteModel(body []byte, model string) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	name, err := json.Marshal(model)
	if err != nil {
		return body
	}
	generic["model"] = name
	rewritten, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return rewritten
}

// endUnserved clears the dashboard row a started event opened for a request
// that ends without being served. The grammar has no failed form, so it is
// logged as done, which is what v1 logged for a failed peer forward.
func (h *Handler) endUnserved(rid int64, taskID, model, label string, start time.Time, record bool) {
	if !record {
		return
	}
	h.d.Activity.EmitRequestTask(rid, -1, taskID, "%s", meshapi.RequestDone(model, label, time.Since(start).Round(time.Millisecond)))
}

func (h *Handler) writeAcquireError(w http.ResponseWriter, err error, model, host string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client is gone; there is no one to answer.
	case errors.Is(err, route.ErrModelNotFound):
		writeError(w, http.StatusNotFound, errTypeNotFound, fmt.Sprintf("model %q not found", model))
	case errors.Is(err, route.ErrHostNotServing):
		writeError(w, http.StatusNotFound, errTypeNotFound, fmt.Sprintf("model %q is not served by host %q", model, host))
	case errors.Is(err, route.ErrNoFreeSlot):
		writeError(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, "no free slot")
	case errors.Is(err, route.ErrQueueFull):
		w.Header().Set("Retry-After", queueRetryAfter)
		writeError(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, fmt.Sprintf("queue for %q is full", model))
	case errors.Is(err, route.ErrQueueTimeout):
		w.Header().Set("Retry-After", queueRetryAfter)
		writeError(w, http.StatusTooManyRequests, meshapi.ErrTypeRateLimit, fmt.Sprintf("no free slot for %q within %s", model, h.d.Router.QueueTimeout()))
	default:
		log.Printf("proxy: routing %q: %v", model, err)
		writeError(w, http.StatusInternalServerError, errTypeServer, "routing failed")
	}
}

// handleModels lists local models, then peers', pipelines' and the extra
// entries; a duplicate id keeps its first entry in that order.
func (h *Handler) handleModels(w http.ResponseWriter) {
	seen := map[string]bool{}
	data := []meshapi.ModelEntry{}
	add := func(e meshapi.ModelEntry) {
		if e.ID == "" || seen[e.ID] {
			return
		}
		seen[e.ID] = true
		e.Object = "model"
		data = append(data, e)
	}
	if h.d.Local != nil {
		for _, mc := range h.d.Local.Capacity() {
			add(meshapi.ModelEntry{ID: mc.Name, OwnedBy: meshapi.OwnedByLocal})
		}
	}
	if h.d.Reports != nil {
		// Whatever the report's age: a model listed once stays listed while its
		// member is known (Decision 4).
		for _, rep := range h.d.Reports.Reports() {
			if rep.Node == h.d.Self {
				continue
			}
			for _, mc := range rep.Models {
				add(meshapi.ModelEntry{ID: mc.Name, OwnedBy: meshapi.OwnedByPeer})
			}
		}
	}
	if h.d.Pipelines != nil {
		for _, e := range h.d.Pipelines.VirtualModels() {
			add(e)
		}
	}
	if h.d.ExtraModels != nil {
		for _, e := range h.d.ExtraModels() {
			add(e)
		}
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	writeJSON(w, http.StatusOK, meshapi.ModelsResponse{Object: "list", Data: data})
}

func (h *Handler) handleCapacity(w http.ResponseWriter) {
	var local []meshapi.ModelCapacity
	if h.d.Local != nil {
		local = h.d.Local.Capacity()
	}
	models := make([]meshapi.ModelCapacity, len(local))
	for i, m := range local {
		m.Queued = h.d.Router.QueueLen(m.Name)
		models[i] = m
	}
	writeJSON(w, http.StatusOK, meshapi.CapacityResponse{Node: h.d.Self, Ver: h.d.Version, Models: models})
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: message, Type: typ}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
