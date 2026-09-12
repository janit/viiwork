package alias

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// proxyBackend is a local backend in front of an httptest engine.
type proxyBackend struct {
	addr     string
	mu       sync.Mutex
	state    supervisor.State
	inFlight int
}

func (b *proxyBackend) ID() string   { return "m/0" }
func (b *proxyBackend) Addr() string { return b.addr }
func (b *proxyBackend) Slots() int   { return 1 }
func (b *proxyBackend) State() supervisor.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
func (b *proxyBackend) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}
func (b *proxyBackend) Acquire()         { b.mu.Lock(); b.inFlight++; b.mu.Unlock() }
func (b *proxyBackend) Release()         { b.mu.Lock(); b.inFlight--; b.mu.Unlock() }
func (b *proxyBackend) NoteHardFailure() {}

// proxyNode is model m on one backend, as both route.LocalModels and the
// resolver's LocalCapacity.
type proxyNode struct{ b *proxyBackend }

func (n proxyNode) Backends(model string) ([]route.LocalBackend, bool, bool) {
	if model != "m" {
		return nil, false, false
	}
	return []route.LocalBackend{n.b}, true, true
}

func (n proxyNode) Capacity() []meshapi.ModelCapacity {
	healthy := 0
	if n.b.State() == supervisor.StateHealthy {
		healthy = 1
	}
	return []meshapi.ModelCapacity{{Name: "m", Engine: "llamacpp", Slots: healthy, Busy: n.b.InFlight(), Backends: 1, HealthyBackends: healthy}}
}

type mutableReports struct {
	mu   sync.Mutex
	list []capacity.Report
}

func (r *mutableReports) Reports() []capacity.Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capacity.Report(nil), r.list...)
}

func (r *mutableReports) set(list ...capacity.Report) {
	r.mu.Lock()
	r.list = list
	r.mu.Unlock()
}

type emptyMembers struct{}

func (emptyMembers) Members() []mesh.Member { return nil }

// TestProxyAliases wires a real Resolver into the P4 handler: alias stable
// points at local m, with fallback n served by peer P.
func TestProxyAliases(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"from m"}}]}`)
	}))
	t.Cleanup(engine.Close)
	var peerBodies []string
	var peerMu sync.Mutex
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		peerMu.Lock()
		peerBodies = append(peerBodies, string(body))
		peerMu.Unlock()
		w.Header().Set(meshapi.HeaderModel, "n")
		w.Header().Set(meshapi.HeaderNode, "P")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"from n"}}]}`)
	}))
	t.Cleanup(peer.Close)

	backend := &proxyBackend{addr: engine.Listener.Addr().String(), state: supervisor.StateHealthy}
	node := proxyNode{b: backend}
	reports := &mutableReports{}
	pReport := func() capacity.Report {
		return capacity.Report{Node: "P", APIAddr: peer.Listener.Addr().String(), Received: time.Now(),
			Models: []meshapi.ModelCapacity{{Name: "n", Engine: "llamacpp", Slots: 1, Backends: 1, HealthyBackends: 1}}}
	}
	reports.set(pReport())

	store := openTest(t, t.TempDir(), newClock())
	if _, err := store.Set("stable", "m", []string{"n"}); err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(store, NewServedView("self", node, reports, time.Minute, nil), nil, nil)

	router := route.New(route.Config{Self: "self", Local: node, Remote: reports, StaleAfter: time.Minute, QueueMax: 4, QueueTimeout: 200 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go router.Run(ctx)
	auth, err := proxy.NewForwardAuth("self", nil, nil, emptyMembers{})
	if err != nil {
		t.Fatal(err)
	}
	h := proxy.NewHandler(proxy.Deps{
		Self: "self", Version: "test", Router: router, Reports: reports, Local: node, Auth: auth,
		Counters: proxy.NewCounters(), ForwardRetry: 1, Resolve: resolver.Resolve, ExtraModels: resolver.ModelEntries,
	})
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"stable","messages":[]}`)))
		return rec
	}

	if rec := post(); rec.Code != 200 || rec.Header().Get(meshapi.HeaderAlias) != "stable" || rec.Header().Get(meshapi.HeaderModel) != "m" {
		t.Errorf("local: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	backend.mu.Lock()
	backend.state = supervisor.StateUnhealthy
	backend.mu.Unlock()
	rec := post()
	if rec.Code != 200 || rec.Header().Get(meshapi.HeaderAlias) != "stable" || rec.Header().Get(meshapi.HeaderModel) != "n" {
		t.Errorf("fallback: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	peerMu.Lock()
	if len(peerBodies) != 1 || !strings.Contains(peerBodies[0], `"model":"n"`) {
		t.Errorf("fallback: P received %q, want the real model n", peerBodies)
	}
	peerMu.Unlock()

	reports.set()
	if rec := post(); rec.Code != 503 || rec.Header().Get("Retry-After") != "5" {
		t.Errorf("nothing served: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var models meshapi.ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &models); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range models.Data {
		if e.ID == "stable" {
			found = e.OwnedBy == meshapi.OwnedByAlias && e.Target == "m"
		}
	}
	if !found {
		t.Errorf("/v1/models = %+v", models.Data)
	}
}
