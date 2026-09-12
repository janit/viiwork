package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

const engineJSON = `{"choices":[{"message":{"role":"assistant","content":"hi there"}}],"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`

// recEngine is an engine or peer that records what it receives.
type recEngine struct {
	srv     *httptest.Server
	hits    atomic.Int64
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
}

// newRecEngine answers with engineJSON unless serve is given.
func newRecEngine(t *testing.T, serve http.HandlerFunc) *recEngine {
	t.Helper()
	e := &recEngine{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, string(body))
		e.headers = append(e.headers, r.Header.Clone())
		e.mu.Unlock()
		if serve != nil {
			serve(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, engineJSON)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *recEngine) addr() string { return e.srv.Listener.Addr().String() }

func (e *recEngine) last() (string, http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.bodies) == 0 {
		return "", nil
	}
	return e.bodies[len(e.bodies)-1], e.headers[len(e.headers)-1]
}

// closedAddr is an address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

type fakeLocalModels struct {
	mu     sync.Mutex
	models map[string][]*fakeLocalBackend
}

// add configures model with one single-slot healthy backend per address.
func (f *fakeLocalModels) add(model string, addrs ...string) []*fakeLocalBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.models == nil {
		f.models = map[string][]*fakeLocalBackend{}
	}
	var bs []*fakeLocalBackend
	for i, a := range addrs {
		bs = append(bs, &fakeLocalBackend{id: fmt.Sprintf("%s/%d", model, i), addr: a, state: supervisor.StateHealthy, slots: 1})
	}
	f.models[model] = bs
	return bs
}

func (f *fakeLocalModels) Backends(model string) ([]route.LocalBackend, bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bs, ok := f.models[model]
	if !ok {
		return nil, false, false
	}
	out := make([]route.LocalBackend, len(bs))
	for i, b := range bs {
		out[i] = b
	}
	return out, true, true
}

type fakeReports struct {
	mu   sync.Mutex
	list []capacity.Report
}

func (f *fakeReports) Reports() []capacity.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capacity.Report(nil), f.list...)
}

func (f *fakeReports) add(node, addr string, received time.Time, models ...meshapi.ModelCapacity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = append(f.list, capacity.Report{Node: node, APIAddr: addr, Received: received, Models: models})
}

func peerModel(name string, slots, busy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{Name: name, Engine: "llamacpp", Slots: slots, Busy: busy, Backends: 1, HealthyBackends: 1}
}

type fakeCapacity []meshapi.ModelCapacity

func (f fakeCapacity) Capacity() []meshapi.ModelCapacity { return f }

type handlerFx struct {
	t            *testing.T
	local        fakeLocalModels
	reports      fakeReports
	capacity     fakeCapacity
	members      fakeMembers
	queueTimeout time.Duration
	queueMax     int
	forwardRetry int
	resolve      Resolver
	extra        ModelLister
	pipelines    *PipelineResolver
	exec         *pipeline.Executor

	counters *Counters
	log      *activity.Log
	router   *route.Router
	h        *Handler
}

func newHandlerFx(t *testing.T) *handlerFx {
	return &handlerFx{t: t, queueTimeout: time.Second, queueMax: 4, forwardRetry: 1}
}

func (f *handlerFx) build() *handlerFx {
	f.t.Helper()
	f.counters = NewCounters()
	f.log = activity.NewLog()
	f.router = route.New(route.Config{Self: "self", Local: &f.local, Remote: &f.reports, StaleAfter: time.Minute, QueueMax: f.queueMax, QueueTimeout: f.queueTimeout})
	ctx, cancel := context.WithCancel(context.Background())
	f.t.Cleanup(cancel)
	go f.router.Run(ctx)
	f.h = NewHandler(Deps{
		Self:         "self",
		Version:      "v-test",
		Router:       f.router,
		Reports:      &f.reports,
		Local:        f.capacity,
		Auth:         mustAuth(f.t, "self", nil, nil, f.members),
		Counters:     f.counters,
		ForwardRetry: f.forwardRetry,
		Activity:     f.log,
		Pipelines:    f.pipelines,
		PipelineExec: f.exec,
		Resolve:      f.resolve,
		ExtraModels:  f.extra,
	})
	return f
}

func (f *handlerFx) do(method, target, body string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for _, e := range edit {
		e(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// requestEvents is the activity log's request events.
func (f *handlerFx) requestEvents() []activity.Event {
	var out []activity.Event
	for _, ev := range f.log.Recent() {
		if ev.Type == meshapi.EventRequest {
			out = append(out, ev)
		}
	}
	return out
}

// holdSlot takes a slot for model with a test lease.
func (f *handlerFx) holdSlot(model string) *route.Lease {
	f.t.Helper()
	l, err := f.router.Pick(route.Request{Model: model})
	if err != nil {
		f.t.Fatalf("holding a slot: %v", err)
	}
	f.t.Cleanup(l.Release)
	return l
}

func fromMember(origin string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set(meshapi.HeaderForwarded, origin)
		r.RemoteAddr = "127.0.0.1:40000"
	}
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) meshapi.ErrorBody {
	t.Helper()
	var e meshapi.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return e.Error
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const chatReq = `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`

func TestHandlerMethodAndBodyErrors(t *testing.T) {
	f := newHandlerFx(t).build()
	if rec := f.do(http.MethodGet, "/v1/chat/completions", ""); rec.Code != 405 || errorOf(t, rec).Message != "method not allowed" {
		t.Errorf("H1: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPost, "/v1/chat/completions", `{"messages":[]}`); rec.Code != 400 || errorOf(t, rec).Message != "model is required" {
		t.Errorf("H2: %d %q", rec.Code, rec.Body.String())
	}
	big := `{"model":"m","x":"` + strings.Repeat("a", 33<<20) + `"}`
	if rec := f.do(http.MethodPost, "/v1/chat/completions", big); rec.Code != 413 {
		t.Errorf("H3: %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/v1/nosuch", ""); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

func TestHandlerLocalServed(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
	h := rec.Header()
	if rec.Code != 200 || h.Get(meshapi.HeaderNode) != "self" || h.Get(meshapi.HeaderGPUBackend) != "m/0" || h.Get(meshapi.HeaderModel) != "m" {
		t.Fatalf("H4: %d %v %q", rec.Code, h, rec.Body.String())
	}
	if h.Get(meshapi.HeaderQueuedMs) != "" {
		t.Errorf("H4: a request that did not queue has %s %q", meshapi.HeaderQueuedMs, h.Get(meshapi.HeaderQueuedMs))
	}
	if got := f.counters.Get("m"); got != (ModelCounters{Requests: 1, Tokens: 7}) {
		t.Errorf("H4: counters = %+v", got)
	}
	evs := f.requestEvents()
	if len(evs) != 2 || !strings.Contains(evs[0].Message, "m → m/0") || !meshapi.IsRequestTerminal(evs[1].Message) {
		t.Fatalf("H4: request events = %+v", evs)
	}
	entry, ok := f.log.GetPrompt(evs[0].RequestID)
	if !ok || !strings.Contains(entry.Prompt, "hello there") || !strings.Contains(entry.Output, "hi there") || entry.Model != "m" {
		t.Errorf("H4: prompt entry = %+v, %v", entry, ok)
	}
}

func TestHandlerTaskStripped(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.build()
	rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"m","task":"t1","messages":[]}`)
	body, hdr := eng.last()
	if rec.Code != 200 || strings.Contains(body, "task") || hdr.Get(HeaderTask) != "t1" {
		t.Errorf("H5: code=%d engine body=%q task header=%q", rec.Code, body, hdr.Get(HeaderTask))
	}
	if evs := f.requestEvents(); len(evs) == 0 || evs[0].TaskID != "t1" {
		t.Errorf("H5: events carry no task: %+v", evs)
	}
}

func TestHandlerQueue(t *testing.T) {
	t.Run("H6 queued then served", func(t *testing.T) {
		f := newHandlerFx(t)
		f.local.add("m", newRecEngine(t, nil).addr())
		f.build()
		lease := f.holdSlot("m")
		time.AfterFunc(150*time.Millisecond, lease.Release)
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		ms, err := strconv.Atoi(rec.Header().Get(meshapi.HeaderQueuedMs))
		if rec.Code != 200 || err != nil || ms < 150 {
			t.Errorf("code=%d queued=%q", rec.Code, rec.Header().Get(meshapi.HeaderQueuedMs))
		}
	})

	t.Run("H7 queue full", func(t *testing.T) {
		f := newHandlerFx(t)
		f.local.add("m", newRecEngine(t, nil).addr())
		f.build()
		f.holdSlot("m")
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if l, _, err := f.router.Acquire(ctx, route.Request{Model: "m"}); err == nil {
					l.Release()
				}
			}()
		}
		waitFor(t, func() bool { return f.router.QueueLen("m") == 4 })
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		cancel()
		wg.Wait()
		if e := errorOf(t, rec); rec.Code != 429 || !strings.Contains(e.Message, "is full") || e.Type != meshapi.ErrTypeRateLimit || rec.Header().Get("Retry-After") != "2" {
			t.Errorf("code=%d body=%q retry=%q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
		}
	})

	t.Run("H8 queue timeout", func(t *testing.T) {
		f := newHandlerFx(t)
		f.local.add("m", newRecEngine(t, nil).addr())
		f.build()
		f.holdSlot("m")
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		if e := errorOf(t, rec); rec.Code != 429 || e.Message != `no free slot for "m" within 1s` || rec.Header().Get("Retry-After") != "2" {
			t.Errorf("code=%d body=%q retry=%q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
		}
	})
}

func TestHandlerRoutingErrors(t *testing.T) {
	f := newHandlerFx(t)
	f.local.add("m", newRecEngine(t, nil).addr())
	f.reports.add("P", "127.0.0.1:1", time.Now(), peerModel("x", 1, 0))
	f.build()
	if rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"nosuch"}`); rec.Code != 404 || errorOf(t, rec).Message != `model "nosuch" not found` {
		t.Errorf("H9: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPost, "/v1/chat/completions?host=a%20b", chatReq); rec.Code != 400 || errorOf(t, rec).Message != "invalid host parameter" {
		t.Errorf("H10: %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodPost, "/v1/chat/completions?host=P", chatReq); rec.Code != 404 || errorOf(t, rec).Message != `model "m" is not served by host "P"` {
		t.Errorf("H11: %d %q", rec.Code, rec.Body.String())
	}
}

func TestHandlerForwards(t *testing.T) {
	gb1 := memberAt("gb1", "127.0.0.1", meshapi.MemberAlive, false)

	t.Run("H12 unproven claim is a client request", func(t *testing.T) {
		f := newHandlerFx(t)
		eng := newRecEngine(t, nil)
		f.local.add("m", eng.addr())
		f.members = fakeMembers{gb1}
		f.build()
		lease := f.holdSlot("m")
		time.AfterFunc(100*time.Millisecond, lease.Release)
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, func(r *http.Request) {
			r.Header.Set(meshapi.HeaderForwarded, "gb9")
		})
		_, hdr := eng.last()
		if rec.Code != 200 || rec.Header().Get(meshapi.HeaderQueuedMs) == "" {
			t.Errorf("not queued like a client request: %d %v", rec.Code, rec.Header())
		}
		if hdr.Get(meshapi.HeaderForwarded) != "" {
			t.Errorf("the engine saw %s", meshapi.HeaderForwarded)
		}
		if len(f.requestEvents()) == 0 {
			t.Error("no prompt history for a client request")
		}
	})

	t.Run("H13 valid forward", func(t *testing.T) {
		f := newHandlerFx(t)
		eng := newRecEngine(t, nil)
		f.local.add("m", eng.addr())
		f.members = fakeMembers{gb1}
		f.build()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"))
		_, hdr := eng.last()
		if rec.Code != 200 || rec.Header().Get(meshapi.HeaderNode) != "self" || f.counters.Get("m").Requests != 1 {
			t.Errorf("code=%d headers=%v counters=%+v", rec.Code, rec.Header(), f.counters.Get("m"))
		}
		if evs := f.requestEvents(); len(evs) != 0 {
			t.Errorf("a forward recorded activity: %+v", evs)
		}
		if hdr.Get(meshapi.HeaderForwarded) != "" {
			t.Errorf("the engine saw %s", meshapi.HeaderForwarded)
		}
	})

	t.Run("H14 forward never queues", func(t *testing.T) {
		f := newHandlerFx(t)
		f.local.add("m", newRecEngine(t, nil).addr())
		f.members = fakeMembers{gb1}
		f.queueTimeout = 5 * time.Second
		f.build()
		f.holdSlot("m")
		start := time.Now()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"))
		if e := errorOf(t, rec); rec.Code != 429 || e.Message != "no free slot" || time.Since(start) > 200*time.Millisecond {
			t.Errorf("code=%d body=%q after %s", rec.Code, rec.Body.String(), time.Since(start))
		}
	})

	t.Run("H15 forward for a peer-only model", func(t *testing.T) {
		f := newHandlerFx(t)
		f.members = fakeMembers{gb1}
		f.reports.add("P", newRecEngine(t, nil).addr(), time.Now(), peerModel("m", 1, 0))
		f.build()
		if rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1")); rec.Code != 404 {
			t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
		}
	})
}

func TestHandlerRetry(t *testing.T) {
	t.Run("H16 local retry after a hard failure", func(t *testing.T) {
		f := newHandlerFx(t)
		eng := newRecEngine(t, nil)
		bs := f.local.add("m", closedAddr(t), eng.addr())
		f.build()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		if rec.Code != 200 || rec.Header().Get(meshapi.HeaderGPUBackend) != "m/1" || bs[0].hard.Load() != 1 {
			t.Errorf("code=%d backend=%q hard=%d", rec.Code, rec.Header().Get(meshapi.HeaderGPUBackend), bs[0].hard.Load())
		}
		if got := f.counters.Get("m"); got != (ModelCounters{Requests: 1, Tokens: 7}) {
			t.Errorf("counters = %+v", got)
		}
	})

	t.Run("H17 peer 429 then another peer", func(t *testing.T) {
		f := newHandlerFx(t)
		p := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) })
		q := newRecEngine(t, nil)
		f.reports.add("P", p.addr(), time.Now(), peerModel("m", 2, 0))
		f.reports.add("Q", q.addr(), time.Now(), peerModel("m", 1, 0))
		f.build()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		if rec.Code != 200 || rec.Header().Get(meshapi.HeaderNode) != "Q" || p.hits.Load() != 1 || q.hits.Load() != 1 {
			t.Errorf("code=%d node=%q P=%d Q=%d", rec.Code, rec.Header().Get(meshapi.HeaderNode), p.hits.Load(), q.hits.Load())
		}
		if got := f.counters.Get("m"); got != (ModelCounters{}) {
			t.Errorf("a peer-served request was counted here: %+v", got)
		}
	})

	t.Run("H18 peer cut after the first byte", func(t *testing.T) {
		f := newHandlerFx(t)
		p := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: chunk1\n\n")
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
		})
		q := newRecEngine(t, nil)
		f.reports.add("P", p.addr(), time.Now(), peerModel("m", 2, 0))
		f.reports.add("Q", q.addr(), time.Now(), peerModel("m", 1, 0))
		f.build()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "chunk1") || q.hits.Load() != 0 {
			t.Errorf("code=%d body=%q Q=%d", rec.Code, rec.Body.String(), q.hits.Load())
		}
	})

	t.Run("H19 final acquisition after retries are used up", func(t *testing.T) {
		f := newHandlerFx(t)
		eng := newRecEngine(t, nil)
		bs := f.local.add("m", closedAddr(t), eng.addr())
		f.forwardRetry = 0
		f.build()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		starts := 0
		for _, ev := range f.requestEvents() {
			if !meshapi.IsRequestTerminal(ev.Message) {
				starts++
			}
		}
		if rec.Code != 200 || rec.Header().Get(meshapi.HeaderGPUBackend) != "m/1" || starts != 2 || bs[0].hard.Load() != 1 || eng.hits.Load() != 1 {
			t.Errorf("code=%d backend=%q dispatches=%d hard=%d b1 hits=%d", rec.Code, rec.Header().Get(meshapi.HeaderGPUBackend), starts, bs[0].hard.Load(), eng.hits.Load())
		}
	})

	t.Run("H27 the final acquisition has only the budget left", func(t *testing.T) {
		f := newHandlerFx(t)
		bs := f.local.add("m", closedAddr(t))
		f.queueTimeout = 400 * time.Millisecond
		f.forwardRetry = 0
		f.build()
		b := bs[0]
		b.mu.Lock()
		b.onRelease = func(n int) {
			if n == 2 { // the request's failed attempt: a second test lease takes the slot
				b.inFlight++
			}
		}
		b.mu.Unlock()
		lease := f.holdSlot("m") // release 1
		time.AfterFunc(300*time.Millisecond, lease.Release)
		start := time.Now()
		rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
		elapsed := time.Since(start)
		b.Release() // the second test lease
		if rec.Code != 429 || !strings.Contains(errorOf(t, rec).Message, "within 400ms") {
			t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
		}
		if elapsed < 380*time.Millisecond || elapsed >= 650*time.Millisecond {
			t.Errorf("answered after %s, want about 400ms (the full timeout again would be about 700ms)", elapsed)
		}
	})
}

// H28: a peer that refused is not asked again on the same report; the request
// queues until a newer report, instead of failing twice and getting a 502
// (Decision 18).
func TestHandlerRefusedPeerQueues(t *testing.T) {
	f := newHandlerFx(t)
	var calls atomic.Int64
	p := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, engineJSON)
	})
	f.reports.add("P", p.addr(), time.Now(), peerModel("m", 1, 0))
	f.build()
	go func() {
		for f.router.QueueLen("m") == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		f.reports.mu.Lock()
		f.reports.list[0].Received = time.Now()
		f.reports.mu.Unlock()
		f.router.Wake()
	}()
	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
	if rec.Code != 200 || rec.Header().Get(meshapi.HeaderQueuedMs) == "" || p.hits.Load() != 2 {
		t.Errorf("code=%d body=%q queued=%q P hits=%d", rec.Code, rec.Body.String(), rec.Header().Get(meshapi.HeaderQueuedMs), p.hits.Load())
	}
}

func TestHandlerRetryExhausted(t *testing.T) {
	f := newHandlerFx(t)
	f.local.add("m", closedAddr(t))
	f.forwardRetry = 0
	f.build()
	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq)
	if e := errorOf(t, rec); rec.Code != 502 || e.Message != "no route could serve the request" || e.Type != "server_error" {
		t.Errorf("client: code=%d body=%q", rec.Code, rec.Body.String())
	}
	evs := f.requestEvents()
	if len(evs) == 0 || !meshapi.IsRequestTerminal(evs[len(evs)-1].Message) {
		t.Errorf("the dashboard row is never cleared: %+v", evs)
	}

	g := newHandlerFx(t)
	g.local.add("m", closedAddr(t))
	g.members = fakeMembers{memberAt("gb1", "127.0.0.1", meshapi.MemberAlive, false)}
	g.build()
	rec = g.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"))
	if e := errorOf(t, rec); rec.Code != 503 || e.Message != "backend failed before responding" || e.Type != meshapi.ErrTypeUnavailable {
		t.Errorf("forward: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandlerResolve(t *testing.T) {
	f := newHandlerFx(t)
	f.local.add("m", newRecEngine(t, nil).addr())
	f.resolve = func(requested string) (string, string, error) {
		switch requested {
		case "stable":
			return "m", "stable", nil
		case "gone":
			return "", "", &ResolveError{Status: 503, Type: meshapi.ErrTypeUnavailable, Message: "alias stable: no node serves m", RetryAfter: 5}
		}
		return requested, "", nil
	}
	f.build()
	rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"stable"}`)
	if rec.Code != 200 || rec.Header().Get(meshapi.HeaderAlias) != "stable" || rec.Header().Get(meshapi.HeaderModel) != "m" {
		t.Errorf("H20: code=%d headers=%v", rec.Code, rec.Header())
	}
	rec = f.do(http.MethodPost, "/v1/chat/completions", `{"model":"gone"}`)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || strings.TrimSpace(rec.Body.String()) != `{"error":{"message":"alias stable: no node serves m","type":"unavailable"}}` {
		t.Errorf("H21: code=%d body=%q retry=%q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
}

func TestHandlerAliasedBody(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.resolve = func(requested string) (string, string, error) {
		if requested == "stable" {
			return "m", "stable", nil
		}
		return requested, "", nil
	}
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"stable","messages":[{"role":"user","content":"hello there"}]}`)
	body, _ := eng.last()
	var sent struct {
		Model string `json:"model"`
	}
	if rec.Code != 200 || json.Unmarshal([]byte(body), &sent) != nil || sent.Model != "m" {
		t.Errorf("H25: code=%d engine body=%q", rec.Code, body)
	}
	evs := f.requestEvents()
	if len(evs) == 0 {
		t.Fatal("H25: no request events")
	}
	if entry, ok := f.log.GetPrompt(evs[0].RequestID); !ok || entry.Model != "m (alias stable)" {
		t.Errorf("H25: prompt entry = %+v, %v", entry, ok)
	}
	if !strings.Contains(evs[0].Message, "m → m/0") {
		t.Errorf("H25: activity uses %q, want the real model", evs[0].Message)
	}
	if got := f.counters.Get("m"); got.Requests != 1 {
		t.Errorf("H25: counters = %+v", got)
	}

	rec = f.do(http.MethodPost, "/v1/chat/completions", chatReq)
	if body, _ := eng.last(); rec.Code != 200 || body != chatReq {
		t.Errorf("H26: an unaliased body was rewritten: %q", body)
	}
}

func TestHandlerModels(t *testing.T) {
	f := newHandlerFx(t)
	f.capacity = fakeCapacity{{Name: "m"}}
	f.reports.add("P", "127.0.0.1:1", time.Now().Add(-time.Hour), peerModel("m", 1, 0), peerModel("x", 1, 0))
	f.pipelines = NewPipelineResolver([]*pipeline.Pipeline{testPipeline(map[string]string{}, "fi")})
	f.extra = func() []meshapi.ModelEntry {
		return []meshapi.ModelEntry{{ID: "stable", OwnedBy: meshapi.OwnedByAlias, Target: "m"}}
	}
	f.build()
	rec := f.do(http.MethodGet, "/v1/models", "")
	var resp meshapi.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Object != "list" {
		t.Fatalf("H22: %q: %v", rec.Body.String(), err)
	}
	var got []string
	for _, e := range resp.Data {
		if e.Object != "model" {
			t.Errorf("H22: %s has object %q", e.ID, e.Object)
		}
		got = append(got, e.ID+"("+e.OwnedBy+")")
	}
	if want := "m(local) stable(alias) tr-fi(pipeline) x(peer)"; strings.Join(got, " ") != want {
		t.Errorf("H22: models = %v, want %s", got, want)
	}
}

func TestHandlerCapacity(t *testing.T) {
	f := newHandlerFx(t)
	f.local.add("m", newRecEngine(t, nil).addr())
	f.capacity = fakeCapacity{{Name: "m", Engine: "llamacpp", Slots: 2, Busy: 1, Backends: 2, HealthyBackends: 2}}
	f.build()
	f.holdSlot("m")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, _, err := f.router.Acquire(ctx, route.Request{Model: "m"}); err == nil {
				l.Release()
			}
		}()
	}
	waitFor(t, func() bool { return f.router.QueueLen("m") == 2 })
	rec := f.do(http.MethodGet, "/v1/capacity", "")
	cancel()
	wg.Wait()
	var resp meshapi.CapacityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("H23: %q: %v", rec.Body.String(), err)
	}
	want := meshapi.ModelCapacity{Name: "m", Engine: "llamacpp", Slots: 2, Busy: 1, Queued: 2, Backends: 2, HealthyBackends: 2}
	if resp.Node != "self" || resp.Ver != "v-test" || len(resp.Models) != 1 || resp.Models[0] != want {
		t.Errorf("H23: %+v", resp)
	}
}
