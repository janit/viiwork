package accept

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// fakeNode answers the endpoints the checks read, from values the test sets.
type fakeNode struct {
	name string
	srv  *httptest.Server

	mu      sync.Mutex
	cluster *meshapi.ClusterResponse
	status  *meshapi.NodeStatus
	chat    func(r *http.Request, body map[string]any) (int, http.Header, any)
	seen    []string // "METHOD path Authorization" of every request
}

func newFakeNode(t *testing.T, name string) *fakeNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return startFakeNode(t, name, ln)
}

// startFakeNode serves on ln, for a test that needs the node to appear on an
// address that refused connections until now.
func startFakeNode(t *testing.T, name string, ln net.Listener) *fakeNode {
	t.Helper()
	f := &fakeNode{name: name}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	f.srv = srv
	t.Cleanup(srv.Close)
	return f
}

// URL is the node's host:port.
func (f *fakeNode) URL() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func (f *fakeNode) SetCluster(c meshapi.ClusterResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cluster = &c
}

func (f *fakeNode) SetStatus(s meshapi.NodeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = &s
}

func (f *fakeNode) OnChat(h func(r *http.Request, body map[string]any) (status int, headers http.Header, resp any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chat = h
}

// Seen lists every request received as "METHOD path Authorization".
func (f *fakeNode) Seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	cluster, status, chat := f.cluster, f.status, f.chat
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == meshapi.PathCluster && cluster != nil:
		writeTestJSON(w, http.StatusOK, nil, cluster)
	case r.Method == http.MethodGet && r.URL.Path == meshapi.PathStatus && status != nil:
		writeTestJSON(w, http.StatusOK, nil, status)
	case r.Method == http.MethodPost && r.URL.Path == meshapi.PathChatCompletions && chat != nil:
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		code, headers, resp := chat(r, body)
		writeTestJSON(w, code, headers, resp)
	default:
		http.NotFound(w, r)
	}
}

func writeTestJSON(w http.ResponseWriter, code int, headers http.Header, v any) {
	for k, vs := range headers {
		w.Header()[k] = vs
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeClock is an Env clock whose Sleep advances Now at once. onSleep, when
// set, runs after each advance with the time elapsed since the start, so a
// test can change what a node answers from a given poll on.
type fakeClock struct {
	mu      sync.Mutex
	start   time.Time
	now     time.Time
	onSleep func(elapsed time.Duration)
}

func newFakeClock() *fakeClock {
	t0 := time.Date(2026, 9, 11, 23, 0, 0, 0, time.UTC)
	return &fakeClock{start: t0, now: t0}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	elapsed := c.now.Sub(c.start)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(elapsed)
	}
	return nil
}

// env is DefaultEnv on this clock.
func (c *fakeClock) env() Env {
	e := DefaultEnv()
	e.Now = c.Now
	e.Sleep = c.Sleep
	return e
}

// backendModel is a model whose backends have the given statuses; a backend
// that is not healthy is in phase "loading".
func backendModel(name string, statuses ...string) meshapi.ModelStatus {
	m := meshapi.ModelStatus{Name: name, Engine: "llamacpp"}
	for i, s := range statuses {
		b := meshapi.BackendStatus{ID: name + "/" + string(rune('0'+i)), Status: s, Slots: 1}
		if s != meshapi.StatusHealthy {
			b.Phase = "loading"
		}
		m.Backends = append(m.Backends, b)
		m.Slots++
	}
	return m
}

func nodeStatus(name string, models ...meshapi.ModelStatus) meshapi.NodeStatus {
	return meshapi.NodeStatus{Node: name, APIPort: 8086, Models: models}
}

// clusterOf is a cluster view from observer listing the given members.
func clusterOf(observer string, members ...meshapi.Member) meshapi.ClusterResponse {
	return meshapi.ClusterResponse{View: observer, Mesh: meshapi.MeshOpen, Members: members}
}

func member(name, state string, status *meshapi.NodeStatus) meshapi.Member {
	return meshapi.Member{Node: name, Addr: "192.0.2.1", Role: meshapi.RoleNode, State: state, Status: status}
}

// closedAddr is a loopback address nothing listens on, and the listener to
// open there later.
func closedAddr(t *testing.T) (string, func() net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr, func() net.Listener {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("reopening %s: %v", addr, err)
		}
		return ln
	}
}
