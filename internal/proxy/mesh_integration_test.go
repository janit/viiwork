//go:build integration

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

const meshStaleAfter = 300 * time.Millisecond

var meshSecret = bytes.Repeat([]byte{7}, 32)

// meshNode is one in-process node: a handler on a real listener, a capacity
// poller following the other two, a router woken by its reports, and fake
// local backends in front of fake engines.
type meshNode struct {
	name     string
	srv      *httptest.Server
	h        atomic.Pointer[Handler]
	local    fakeLocalModels
	reports  *meshReports
	poller   *capacity.Poller
	router   *route.Router
	counters *Counters
	log      *activity.Log
}

// meshReports is the poller's reports as the router sees them. Freezing keeps
// the router on the reports it had (until they go stale), so a test can show a
// peer as free after it has filled up. Zeroing RTTs breaks a tie between peers
// by round-robin in node order instead of by measured latency.
type meshReports struct {
	p       *capacity.Poller
	zeroRTT bool

	mu     sync.Mutex
	frozen []capacity.Report
	freeze bool
}

func (m *meshReports) Reports() []capacity.Report {
	m.mu.Lock()
	var out []capacity.Report
	if m.freeze {
		out = append(out, m.frozen...)
	} else {
		out = m.p.Reports()
	}
	m.mu.Unlock()
	if m.zeroRTT {
		for i := range out {
			out[i].RTT = 0
		}
	}
	return out
}

func (m *meshReports) setFrozen(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.freeze = on
	if on {
		m.frozen = m.p.Reports()
	}
}

// fakeLocalCapacity computes a node's capacity from its fake backends.
type fakeLocalCapacity struct{ m *fakeLocalModels }

func (c fakeLocalCapacity) Capacity() []meshapi.ModelCapacity {
	c.m.mu.Lock()
	defer c.m.mu.Unlock()
	var out []meshapi.ModelCapacity
	for name, bs := range c.m.models {
		mc := meshapi.ModelCapacity{Name: name, Engine: "llamacpp", Backends: len(bs)}
		for _, b := range bs {
			if b.State() == supervisor.StateHealthy {
				mc.HealthyBackends++
				mc.Slots += b.Slots()
			}
			mc.Busy += b.InFlight()
		}
		out = append(out, mc)
	}
	return out
}

type meshSetup struct {
	queueTimeout time.Duration
	open         bool                           // open-mesh forward auth instead of secured
	zeroRTT      bool                           // see meshReports
	models       map[string]map[string][]string // node → model → engine addresses
}

var meshNames = []string{"A", "B", "C"}

func newMesh(t *testing.T, s meshSetup) map[string]*meshNode {
	t.Helper()
	nodes := map[string]*meshNode{}
	for _, name := range meshNames {
		n := &meshNode{name: name, counters: NewCounters(), log: activity.NewLog()}
		n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.h.Load().ServeHTTP(w, r)
		}))
		t.Cleanup(n.srv.Close)
		for model, addrs := range s.models[name] {
			n.local.add(model, addrs...)
		}
		nodes[name] = n
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, name := range meshNames {
		n := nodes[name]
		var members fakeMembers
		for _, other := range meshNames {
			m := memberAt(other, "127.0.0.1", meshapi.MemberAlive, other == name)
			m.Meta.API = nodes[other].srv.Listener.Addr().(*net.TCPAddr).Port
			members = append(members, m)
		}
		n.poller = capacity.NewPoller(capacity.Config{
			Self: name, Members: members, Interval: 50 * time.Millisecond,
			OnReport: func(string) { n.router.Wake() },
			Logf:     func(string, ...any) {},
		})
		n.reports = &meshReports{p: n.poller, zeroRTT: s.zeroRTT}
		n.router = route.New(route.Config{Self: name, Local: &n.local, Remote: n.reports, StaleAfter: meshStaleAfter, QueueMax: 64, QueueTimeout: s.queueTimeout})
		secret := meshSecret
		if s.open {
			secret = nil
		}
		n.h.Store(NewHandler(Deps{
			Self: name, Version: "test", Router: n.router, Reports: n.reports,
			Local: fakeLocalCapacity{&n.local}, Auth: mustAuth(t, name, secret, nil, members),
			Counters: n.counters, ForwardRetry: 1, Activity: n.log,
		}))
		go n.router.Run(ctx)
	}
	for _, name := range meshNames {
		go nodes[name].poller.Run(ctx)
	}
	waitFor(t, func() bool {
		now := time.Now()
		for _, n := range nodes {
			for _, other := range meshNames {
				if other == n.name {
					continue
				}
				if rep, ok := n.poller.Report(other); !ok || !capacity.Fresh(rep, now, meshStaleAfter) {
					return false
				}
			}
		}
		return true
	})
	return nodes
}

type meshResult struct {
	status int
	header http.Header
	body   string
	err    error
}

func meshPost(url, body string) meshResult {
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		return meshResult{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return meshResult{status: resp.StatusCode, header: resp.Header, body: string(b), err: err}
}

// heldEngine answers engineJSON once released, after max, or never if the
// request is cancelled first.
func heldEngine(t *testing.T, max time.Duration) (*recEngine, func()) {
	release := make(chan struct{})
	var once sync.Once
	e := newRecEngine(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(max):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, engineJSON)
	})
	releaseFn := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseFn)
	return e, releaseFn
}

func TestMeshM1SpreadAndQueue(t *testing.T) {
	ea, _ := heldEngine(t, 200*time.Millisecond)
	eb, _ := heldEngine(t, 200*time.Millisecond)
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, models: map[string]map[string][]string{
		"A": {"m": {ea.addr()}}, "B": {"m": {eb.addr()}},
	}})
	results := make([]meshResult, 6)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = meshPost(nodes["A"].srv.URL, chatReq)
		}(i)
	}
	wg.Wait()
	servedBy := map[string]int{}
	queued := 0
	for i, r := range results {
		if r.err != nil || r.status != 200 {
			t.Fatalf("request %d: %d %q %v", i, r.status, r.body, r.err)
		}
		servedBy[r.header.Get(meshapi.HeaderNode)]++
		if r.header.Get(meshapi.HeaderQueuedMs) != "" {
			queued++
		}
	}
	if servedBy["A"] == 0 || servedBy["B"] == 0 || queued < 4 || ea.hits.Load()+eb.hits.Load() != 6 {
		t.Errorf("served by %v, %d queued, engine hits A=%d B=%d", servedBy, queued, ea.hits.Load(), eb.hits.Load())
	}
}

func TestMeshM2QueueTimeout(t *testing.T) {
	ea, release := heldEngine(t, 2*time.Second)
	nodes := newMesh(t, meshSetup{queueTimeout: 300 * time.Millisecond, models: map[string]map[string][]string{
		"A": {"m": {ea.addr()}},
	}})
	first := make(chan meshResult, 1)
	go func() { first <- meshPost(nodes["A"].srv.URL, chatReq) }()
	waitFor(t, func() bool { return ea.hits.Load() == 1 })
	start := time.Now()
	r := meshPost(nodes["A"].srv.URL, chatReq)
	elapsed := time.Since(start)
	release()
	if r.status != 429 || r.header.Get("Retry-After") != "2" || elapsed < 300*time.Millisecond {
		t.Errorf("second request: %d %q Retry-After=%q after %s", r.status, r.body, r.header.Get("Retry-After"), elapsed)
	}
	if f := <-first; f.status != 200 {
		t.Errorf("first request: %d %q %v", f.status, f.body, f.err)
	}
}

func TestMeshM3ForwardRefusedThenRetried(t *testing.T) {
	eb, release := heldEngine(t, 5*time.Second)
	ec := newRecEngine(t, nil)
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, zeroRTT: true, models: map[string]map[string][]string{
		"B": {"m": {eb.addr()}}, "C": {"m": {ec.addr()}},
	}})
	nodes["A"].reports.setFrozen(true) // A keeps seeing B as free
	direct := make(chan meshResult, 1)
	go func() { direct <- meshPost(nodes["B"].srv.URL, chatReq) }()
	waitFor(t, func() bool { return eb.hits.Load() == 1 })

	r := meshPost(nodes["A"].srv.URL, chatReq)
	release()
	if r.status != 200 || r.header.Get(meshapi.HeaderNode) != "C" {
		t.Errorf("via A: %d %q node=%q", r.status, r.body, r.header.Get(meshapi.HeaderNode))
	}
	if ec.hits.Load() != 1 || eb.hits.Load() != 1 {
		t.Errorf("engine hits B=%d C=%d, want 1 and 1", eb.hits.Load(), ec.hits.Load())
	}
	if d := <-direct; d.status != 200 {
		t.Errorf("direct request to B: %d %q", d.status, d.body)
	}
}

func TestMeshM4StreamingIsUnbuffered(t *testing.T) {
	signal := make(chan struct{})
	eb := newRecEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunkOne)
		w.(http.Flusher).Flush()
		select {
		case <-signal:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, models: map[string]map[string][]string{
		"B": {"m": {eb.addr()}},
	}})
	resp, err := http.Post(nodes["A"].srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if first := readEvent(t, reader, 3*time.Second); !strings.Contains(first, "one") {
		t.Fatalf("first event = %q", first)
	}
	close(signal)
	if rest, _ := io.ReadAll(reader); !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("rest = %q", rest)
	}
}

func TestMeshM5CutStreamIsNotRetried(t *testing.T) {
	eb := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: chunk1\n\n")
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
	ec := newRecEngine(t, nil)
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, zeroRTT: true, models: map[string]map[string][]string{
		"B": {"m": {eb.addr()}}, "C": {"m": {ec.addr()}},
	}})
	r := meshPost(nodes["A"].srv.URL, `{"model":"m","stream":true}`)
	if r.status != 200 || !strings.Contains(r.body, "chunk1") || strings.Contains(r.body, "[DONE]") || ec.hits.Load() != 0 {
		t.Errorf("%d %q %v; C hits=%d", r.status, r.body, r.err, ec.hits.Load())
	}
}

func TestMeshM6CountersAndHistory(t *testing.T) {
	eb := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunkOne)
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":11,\"total_tokens\":13}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, models: map[string]map[string][]string{
		"B": {"m": {eb.addr()}},
	}})
	r := meshPost(nodes["A"].srv.URL, `{"model":"m","stream":true,"messages":[{"role":"user","content":"count me"}]}`)
	if r.status != 200 {
		t.Fatalf("%d %q %v", r.status, r.body, r.err)
	}
	waitFor(t, func() bool { return nodes["B"].counters.Get("m").Requests == 1 })
	if got := nodes["B"].counters.Get("m"); got != (ModelCounters{Requests: 1, Tokens: 11}) {
		t.Errorf("B's counters = %+v", got)
	}
	if got := nodes["A"].counters.Get("m"); got != (ModelCounters{}) {
		t.Errorf("A's counters = %+v", got)
	}
	aEvents := requestEventsOf(nodes["A"].log)
	if len(aEvents) == 0 {
		t.Fatal("A has no request events")
	}
	if entry, ok := nodes["A"].log.GetPrompt(aEvents[0].RequestID); !ok || !strings.Contains(entry.Prompt, "count me") {
		t.Errorf("A's prompt entry = %+v, %v", entry, ok)
	}
	if bEvents := requestEventsOf(nodes["B"].log); len(bEvents) != 0 {
		t.Errorf("B recorded a forward: %+v", bEvents)
	}
}

func requestEventsOf(l *activity.Log) []activity.Event {
	var out []activity.Event
	for _, ev := range l.Recent() {
		if ev.Type == meshapi.EventRequest {
			out = append(out, ev)
		}
	}
	return out
}

func TestMeshM7CapacityShowsQueued(t *testing.T) {
	ea, release := heldEngine(t, 2*time.Second)
	nodes := newMesh(t, meshSetup{queueTimeout: 2 * time.Second, models: map[string]map[string][]string{
		"A": {"m": {ea.addr()}},
	}})
	done := make(chan meshResult, 2)
	go func() { done <- meshPost(nodes["A"].srv.URL, chatReq) }()
	waitFor(t, func() bool { return ea.hits.Load() == 1 })
	go func() { done <- meshPost(nodes["A"].srv.URL, chatReq) }()
	waitFor(t, func() bool {
		resp, err := http.Get(nodes["A"].srv.URL + meshapi.PathCapacity)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var c meshapi.CapacityResponse
		if json.NewDecoder(resp.Body).Decode(&c) != nil {
			return false
		}
		for _, m := range c.Models {
			if m.Name == "m" && m.Queued >= 1 {
				return true
			}
		}
		return false
	})
	release()
	for i := 0; i < 2; i++ {
		if r := <-done; r.status != 200 {
			t.Errorf("request: %d %q", r.status, r.body)
		}
	}
}

func TestMeshM8OpenMesh(t *testing.T) {
	eb, release := heldEngine(t, 5*time.Second)
	nodes := newMesh(t, meshSetup{queueTimeout: 5 * time.Second, open: true, models: map[string]map[string][]string{
		"B": {"m": {eb.addr()}},
	}})
	a, b := nodes["A"], nodes["B"]

	// A → B works in an open mesh.
	release()
	if r := meshPost(a.srv.URL, chatReq); r.status != 200 || r.header.Get(meshapi.HeaderNode) != "B" {
		t.Fatalf("A → B: %d %q node=%q", r.status, r.body, r.header.Get(meshapi.HeaderNode))
	}
	if evs := requestEventsOf(b.log); len(evs) != 0 {
		t.Fatalf("B did not treat A's request as a forward: %+v", evs)
	}

	// B full: A's forward is refused at once, and A queues the request.
	eb2, release2 := heldEngine(t, 5*time.Second)
	b.local.add("m", eb2.addr())
	waitFor(t, func() bool {
		rep, ok := a.poller.Report("B")
		mc, has := rep.Model("m")
		return ok && has && mc.Busy == 0 && capacity.Fresh(rep, time.Now(), meshStaleAfter)
	})
	a.reports.setFrozen(true)
	direct := make(chan meshResult, 1)
	go func() { direct <- meshPost(b.srv.URL, chatReq) }()
	waitFor(t, func() bool { return eb2.hits.Load() == 1 })
	viaA := make(chan meshResult, 1)
	go func() { viaA <- meshPost(a.srv.URL, chatReq) }()
	waitFor(t, func() bool { return a.router.QueueLen("m") == 1 })
	if eb2.hits.Load() != 1 {
		t.Errorf("the refused forward reached B's engine: hits=%d", eb2.hits.Load())
	}
	a.reports.setFrozen(false)
	release2()
	if r := <-viaA; r.status != 200 || r.header.Get(meshapi.HeaderQueuedMs) == "" {
		t.Errorf("via A: %d %q queued=%q", r.status, r.body, r.header.Get(meshapi.HeaderQueuedMs))
	}
	if d := <-direct; d.status != 200 {
		t.Errorf("direct: %d %q", d.status, d.body)
	}
}
