package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeMembers struct {
	mu      sync.Mutex
	members []mesh.Member
}

func (f *fakeMembers) Members() []mesh.Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mesh.Member(nil), f.members...)
}

func (f *fakeMembers) set(ms ...mesh.Member) {
	f.mu.Lock()
	f.members = ms
	f.mu.Unlock()
}

// server is a fake member API that counts requests and tracks concurrency.
type server struct {
	*httptest.Server
	hits    atomic.Int64
	active  atomic.Int64
	maxSeen atomic.Int64
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		n := s.active.Add(1)
		defer s.active.Add(-1)
		for {
			m := s.maxSeen.Load()
			if n <= m || s.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func answer(node string, models ...meshapi.ModelCapacity) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(meshapi.CapacityResponse{Node: node, Ver: "2.0.0", Models: models})
	}
}

func member(t *testing.T, name string, s *server, state, role string, local bool) mesh.Member {
	t.Helper()
	_, portText, _ := net.SplitHostPort(s.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	return mesh.Member{
		Name: name, Addr: netip.MustParseAddr("127.0.0.1"), Port: 7946,
		Meta: mesh.NodeMeta{V: mesh.MetaVersion, API: port, Ver: "2.0.0", Role: role}, State: state, Local: local,
	}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func run(t *testing.T, p *Poller) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var modelM = meshapi.ModelCapacity{Name: "m", Engine: "llamacpp", Slots: 2, Busy: 1, Ctx: 4096, Backends: 1, HealthyBackends: 1}

func TestPollingSet(t *testing.T) {
	a, b := newServer(t, answer("A", modelM)), newServer(t, answer("B", modelM))
	local, gw, dead := newServer(t, answer("L")), newServer(t, answer("G")), newServer(t, answer("D"))
	members := &fakeMembers{}
	members.set(
		member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false),
		member(t, "B", b, meshapi.MemberAlive, meshapi.RoleNode, false),
		member(t, "L", local, meshapi.MemberAlive, meshapi.RoleNode, true),
		member(t, "G", gw, meshapi.MemberAlive, meshapi.RoleGateway, false),
		member(t, "D", dead, meshapi.MemberDead, meshapi.RoleNode, false),
	)
	p := NewPoller(Config{Self: "L", Members: members, Interval: 50 * time.Millisecond, Logf: (&logLines{}).logf})
	run(t, p)
	time.Sleep(400 * time.Millisecond)
	for name, s := range map[string]*server{"A": a, "B": b} {
		if n := s.hits.Load(); n < 5 || n > 9 {
			t.Errorf("%s polled %d times in 400 ms at 50 ms, want 5-9", name, n)
		}
	}
	for name, s := range map[string]*server{"local": local, "gateway": gw, "dead": dead} {
		if n := s.hits.Load(); n != 0 {
			t.Errorf("%s polled %d times, want 0", name, n)
		}
	}
}

func TestAcceptedReport(t *testing.T) {
	a := newServer(t, answer("A", modelM))
	members := &fakeMembers{}
	members.set(member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false))
	var reported atomic.Value
	p := NewPoller(Config{Self: "L", Members: members, Interval: 50 * time.Millisecond, OnReport: func(n string) { reported.Store(n) }})
	run(t, p)
	eventually(t, time.Second, "report", func() bool { _, ok := p.Report("A"); return ok })
	r, _ := p.Report("A")
	if r.Node != "A" || r.APIAddr != a.Listener.Addr().String() || r.Received.IsZero() || r.RTT <= 0 {
		t.Errorf("report = %+v", r)
	}
	if m, ok := r.Model("m"); !ok || m.Slots != 2 || m.Busy != 1 {
		t.Errorf("Model(m) = %+v, %v", m, ok)
	}
	if _, ok := r.Model("x"); ok {
		t.Error("Model(x) must be false")
	}
	if reported.Load() != "A" {
		t.Error("OnReport must be called with A")
	}
}

func TestNameMismatch(t *testing.T) {
	a := newServer(t, answer("B", modelM))
	members := &fakeMembers{}
	members.set(member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false))
	log := &logLines{}
	p := NewPoller(Config{Self: "L", Members: members, Interval: 50 * time.Millisecond, Logf: log.logf})
	run(t, p)
	eventually(t, time.Second, "three polls", func() bool { return a.hits.Load() >= 3 })
	if _, ok := p.Report("A"); ok {
		t.Error("a report naming another node must not be stored")
	}
	if n := log.count("expected A"); n != 1 {
		t.Errorf("mismatch logged %d times, want 1", n)
	}
}

func TestNoOverlap(t *testing.T) {
	interval := 50 * time.Millisecond
	slow := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * interval):
		case <-r.Context().Done():
		}
		answer("A")(w, r)
	})
	b := newServer(t, answer("B"))
	members := &fakeMembers{}
	members.set(
		member(t, "A", slow, meshapi.MemberAlive, meshapi.RoleNode, false),
		member(t, "B", b, meshapi.MemberAlive, meshapi.RoleNode, false),
	)
	p := NewPoller(Config{Self: "L", Members: members, Interval: interval, Logf: (&logLines{}).logf})
	run(t, p)
	time.Sleep(500 * time.Millisecond)
	if m := slow.maxSeen.Load(); m > 1 {
		t.Errorf("a slow member saw %d concurrent polls, want at most 1", m)
	}
	if n := b.hits.Load(); n < 6 {
		t.Errorf("B polled %d times in 500 ms, want it on schedule", n)
	}
}

func TestFailureKeepsReportUntilStale(t *testing.T) {
	var fail atomic.Bool
	a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		answer("A", modelM)(w, r)
	})
	members := &fakeMembers{}
	members.set(member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false))
	p := NewPoller(Config{Self: "L", Members: members, Interval: 20 * time.Millisecond, Logf: (&logLines{}).logf})
	run(t, p)
	eventually(t, time.Second, "report", func() bool { _, ok := p.Report("A"); return ok })
	fail.Store(true)
	time.Sleep(50 * time.Millisecond)
	first, ok := p.Report("A")
	if !ok {
		t.Fatal("a failed poll must keep the previous report")
	}
	time.Sleep(60 * time.Millisecond)
	again, _ := p.Report("A")
	if !again.Received.Equal(first.Received) {
		t.Error("a failed poll must not refresh Received")
	}
	eventually(t, time.Second, "stale", func() bool { return !Fresh(again, time.Now(), 100*time.Millisecond) })
}

func TestDepartureAndFail(t *testing.T) {
	a, b := newServer(t, answer("A", modelM)), newServer(t, answer("B", modelM))
	members := &fakeMembers{}
	ma, mb := member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false), member(t, "B", b, meshapi.MemberAlive, meshapi.RoleNode, false)
	members.set(ma, mb)
	p := NewPoller(Config{Self: "L", Members: members, Interval: 30 * time.Millisecond, Logf: (&logLines{}).logf})
	run(t, p)
	eventually(t, time.Second, "both reports", func() bool { return len(p.Reports()) == 2 })
	members.set(mb)
	eventually(t, time.Second, "A gone", func() bool { _, ok := p.Report("A"); return !ok })
	p.HandleMemberEvent(mesh.MemberEvent{Kind: mesh.EventFail, Member: mb})
	if _, ok := p.Report("B"); ok {
		t.Error("EventFail must forget B at once")
	}
}

func TestJoinPollsAtOnce(t *testing.T) {
	a := newServer(t, answer("A", modelM))
	members := &fakeMembers{}
	p := NewPoller(Config{Self: "L", Members: members, Interval: time.Hour, Logf: (&logLines{}).logf})
	run(t, p)
	ma := member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false)
	members.set(ma)
	p.HandleMemberEvent(mesh.MemberEvent{Kind: mesh.EventJoin, Member: ma})
	eventually(t, time.Second, "poll on join", func() bool { return a.hits.Load() >= 1 })
}

func TestOversize(t *testing.T) {
	a := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"node":"A","ver":"2.0.0","models":[],"pad":"` + strings.Repeat("x", 1536*1024) + `"}`))
	})
	members := &fakeMembers{}
	members.set(member(t, "A", a, meshapi.MemberAlive, meshapi.RoleNode, false))
	p := NewPoller(Config{Self: "L", Members: members, Interval: 30 * time.Millisecond, Logf: (&logLines{}).logf})
	run(t, p)
	eventually(t, 2*time.Second, "polls", func() bool { return a.hits.Load() >= 2 })
	if _, ok := p.Report("A"); ok {
		t.Error("an oversized report must be rejected")
	}
}

func TestFreshBoundary(t *testing.T) {
	now := time.Now()
	if !Fresh(Report{Received: now.Add(-2999 * time.Millisecond)}, now, 3*time.Second) {
		t.Error("2.999 s old must be fresh")
	}
	if Fresh(Report{Received: now.Add(-3 * time.Second)}, now, 3*time.Second) {
		t.Error("exactly 3 s old must not be fresh")
	}
}
