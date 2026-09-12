package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// memberStream is a fake member serving /v1/activity/stream from a channel.
type memberStream struct {
	srv    *httptest.Server
	events chan string
}

func newMemberStream(t *testing.T) *memberStream {
	t.Helper()
	m := &memberStream{events: make(chan string, 16)}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathActivityStream {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-m.events:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

type sseEvent struct{ name, data string }

// openStream reads named SSE events from the node's mesh stream.
func openStream(t *testing.T, h http.Handler) (<-chan sseEvent, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+meshapi.PathMeshStream, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan sseEvent, 256)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				out <- sseEvent{name, line[len("data: "):]}
			}
		}
	}()
	stop := func() { cancel(); resp.Body.Close(); srv.Close() }
	t.Cleanup(stop)
	return out, stop
}

// collect gathers events for d.
func collect(ch <-chan sseEvent, d time.Duration) []sseEvent {
	var got []sseEvent
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
}

func waitEvent(t *testing.T, ch <-chan sseEvent, within time.Duration, match func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream ended")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("no matching event")
			return sseEvent{}
		}
	}
}

type streamFx struct {
	mu      sync.Mutex
	members []mesh.Member
	cluster meshapi.ClusterResponse
	aliases meshapi.AliasesResponse
}

func (f *streamFx) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

func newStreamDeps(t *testing.T) (ServerDeps, *serverFakes, *streamFx) {
	d, sf := fakeServer(t)
	fx := &streamFx{cluster: meshapi.ClusterResponse{View: "gb1", Mesh: "open"}, aliases: meshapi.AliasesResponse{Aliases: []meshapi.AliasInfo{}}}
	d.Members = func() []mesh.Member {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return append([]mesh.Member(nil), fx.members...)
	}
	d.Cluster = func() meshapi.ClusterResponse {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		// A deep copy, as BuildCluster returns fresh statuses on every call.
		raw, _ := json.Marshal(fx.cluster)
		var c meshapi.ClusterResponse
		_ = json.Unmarshal(raw, &c)
		return c
	}
	d.AliasInfo = func() meshapi.AliasesResponse {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.aliases
	}
	d.Status = func() meshapi.NodeStatus { return meshapi.NodeStatus{Node: "gb1", NodeID: "viiwork-gb1"} }
	return d, sf, fx
}

func TestMeshStreamActivity(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	gb2 := newMemberStream(t)
	m := memberAt(t, "gb2", gb2.srv, meshapi.MemberAlive, meshapi.RoleNode, false)
	fx.members = []mesh.Member{memberAt(t, "gb1", nil, meshapi.MemberAlive, meshapi.RoleNode, true), m}
	fx.cluster.Members = []meshapi.Member{{Node: "gb2", State: meshapi.MemberAlive, Role: meshapi.RoleNode, Status: &meshapi.NodeStatus{Node: "gb2", NodeID: "viiwork-gb2"}}}
	d.Activity.EmitRequest(7, -1, "m → m/0")

	events, _ := openStream(t, NewServer(d))
	first := waitEvent(t, events, 2*time.Second, func(e sseEvent) bool { return e.name == meshapi.SSEActivity })
	var local meshapi.MeshEvent
	if json.Unmarshal([]byte(first.data), &local) != nil || local.RequestID != 7 || local.Addr != "" || local.Hostname != "gb1" || local.NodeID != "viiwork-gb1" {
		t.Errorf("MS1: first activity event %s", first.data)
	}

	time.Sleep(300 * time.Millisecond) // let the follower connect
	gb2.events <- `{"t":1,"type":"request","message":"m → m/1","rid":9}`
	ev := waitEvent(t, events, 3*time.Second, func(e sseEvent) bool { return e.name == meshapi.SSEActivity && strings.Contains(e.data, `"rid":9`) })
	var remote meshapi.MeshEvent
	if json.Unmarshal([]byte(ev.data), &remote) != nil || remote.Hostname != "gb2" || remote.Addr != m.APIAddr() || remote.NodeID != "viiwork-gb2" {
		t.Errorf("MS2: %s", ev.data)
	}
}

func TestMeshStreamSnapshots(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	fx.cluster.Members = []meshapi.Member{{Node: "gb1", State: meshapi.MemberAlive, Role: meshapi.RoleNode,
		Status: &meshapi.NodeStatus{Node: "gb1", HostMemTotalMB: 64000, HostMemUsedMB: 20000}}}
	events, _ := openStream(t, NewServer(d))

	got := collect(events, 3200*time.Millisecond)
	if n := countNamed(got, meshapi.SSECluster); n != 1 {
		t.Errorf("MS3: %d cluster events for an unchanged cluster", n)
	}
	if n := countNamed(got, "aliases"); n != 1 {
		t.Errorf("MS5: %d aliases events for unchanged aliases", n)
	}

	fx.set(func() { fx.cluster.Members[0].Status.HostMemUsedMB = 20400 }) // under one step (1000 MB)
	if n := countNamed(collect(events, 2200*time.Millisecond), meshapi.SSECluster); n != 0 {
		t.Errorf("MS6: %d cluster events for a sub-step memory change", n)
	}

	fx.set(func() { fx.cluster.Mesh = "secured" })
	waitEvent(t, events, 2*time.Second, func(e sseEvent) bool { return e.name == meshapi.SSECluster && strings.Contains(e.data, "secured") })

	resolved := "m"
	fx.set(func() {
		fx.aliases = meshapi.AliasesResponse{Aliases: []meshapi.AliasInfo{{Name: "stable", Target: "m", Fallbacks: []string{}, Resolved: &resolved, State: "ok"}}}
	})
	waitEvent(t, events, 2*time.Second, func(e sseEvent) bool { return e.name == "aliases" && strings.Contains(e.data, "stable") })
}

func countNamed(events []sseEvent, name string) int {
	n := 0
	for _, e := range events {
		if e.name == name {
			n++
		}
	}
	return n
}

func TestMeshStreamLateMember(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	// Created before the stream so its cleanup runs after the stream's: the
	// member server's Close waits for the follower to hang up.
	gb3 := newMemberStream(t)
	events, _ := openStream(t, NewServer(d))
	collect(events, 200*time.Millisecond)

	fx.set(func() {
		fx.members = []mesh.Member{memberAt(t, "gb3", gb3.srv, meshapi.MemberAlive, meshapi.RoleNode, false)}
	})
	go func() {
		for i := 0; i < 20; i++ {
			select {
			case gb3.events <- fmt.Sprintf(`{"t":1,"type":"request","message":"late","rid":%d}`, 100+i):
			case <-time.After(time.Second):
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	waitEvent(t, events, 3*time.Second, func(e sseEvent) bool {
		return e.name == meshapi.SSEActivity && strings.Contains(e.data, `"hostname":"gb3"`)
	})
}

func TestMeshStreamEndsWithStreamCtx(t *testing.T) {
	d, sf, _ := newStreamDeps(t)
	events, _ := openStream(t, NewServer(d))
	collect(events, 200*time.Millisecond)
	sf.cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("MS9: the stream did not end within 1s")
		}
	}
}

// gatedRecorder is a ResponseWriter that can hold a write open, and that counts
// any write landing after the handler has returned.
//
// net/http's contract is that a ResponseWriter must not be touched once
// ServeHTTP returns. This handler runs a producer goroutine per member plus the
// snapshot loop, and the question is whether any of them can still be writing
// by then. The race detector only caught it when the scheduler interleaved the
// right way — roughly two runs in three — so this arranges the interleaving
// instead of hoping for it: the cluster write is held open, and the handler is
// given every chance to return underneath it.
type gatedRecorder struct {
	mu     sync.Mutex
	sealed bool
	after  int
	hdr    http.Header

	started  chan struct{} // signalled when a cluster write begins
	release  chan struct{} // closed to let held writes complete
	finished chan struct{} // signalled once a held write has actually landed
}

func newGatedRecorder() *gatedRecorder {
	return &gatedRecorder{
		hdr:      make(http.Header),
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan struct{}, 1),
	}
}

func (g *gatedRecorder) Header() http.Header { return g.hdr }
func (g *gatedRecorder) WriteHeader(int)     {}
func (g *gatedRecorder) Flush()              {}

func (g *gatedRecorder) Write(b []byte) (int, error) {
	// Only the cluster snapshot is gated. In the buggy shape that write is
	// performed by the snapshot goroutine; in the fixed one the handler
	// performs it, which is the entire difference this test is looking for.
	gated := bytes.Contains(b, []byte("event: cluster"))
	if gated {
		select {
		case g.started <- struct{}{}:
		default:
		}
		<-g.release
	}

	g.mu.Lock()
	if g.sealed {
		g.after++
	}
	g.mu.Unlock()

	// Announce the landing, so the test can observe it rather than racing the
	// scheduler to read the counter first.
	if gated {
		select {
		case g.finished <- struct{}{}:
		default:
		}
	}
	return len(b), nil
}

// seal is called by the serving goroutine the instant ServeHTTP returns, so
// anything counted afterwards is a write net/http would consider out of
// contract.
func (g *gatedRecorder) seal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealed = true
}

func (g *gatedRecorder) writesAfterSeal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.after
}

func TestMeshStreamWritesNothingAfterHandlerReturns(t *testing.T) {
	d, _, fx := newStreamDeps(t)
	d.Activity = activity.NewLog()
	d.Activity.EmitRequest(7, 0, "model-a → gpu-0")
	// Unreachable members on purpose: their followers then sit on reconnect
	// backoff, the producers most likely to outlive the request.
	for i, port := range []int{1, 2} {
		m := memberAt(t, fmt.Sprintf("dead%d", i), nil, meshapi.MemberAlive, meshapi.RoleNode, false)
		m.Meta.API = port
		fx.members = append(fx.members, m)
	}
	h := NewServer(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newGatedRecorder()
	req := httptest.NewRequest(http.MethodGet, meshapi.PathMeshStream, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		rec.seal()
		close(done)
	}()

	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no cluster snapshot was written; the test cannot observe what it is for")
	}

	// A cluster write is now held open. Cancel, and give the handler room to
	// return while that write is still outstanding.
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}

	close(rec.release)

	select {
	case <-rec.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the held cluster write never completed")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the held write was released")
	}

	if n := rec.writesAfterSeal(); n != 0 {
		t.Errorf("%d write(s) reached the ResponseWriter after ServeHTTP returned — net/http forbids this", n)
	}
}
