package node

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

type fakeMemberList struct {
	mu      sync.Mutex
	members []mesh.Member
}

func (f *fakeMemberList) Members() []mesh.Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mesh.Member(nil), f.members...)
}

func (f *fakeMemberList) set(ms ...mesh.Member) {
	f.mu.Lock()
	f.members = ms
	f.mu.Unlock()
}

// statusServer is a fake member API counting requests and concurrency.
type statusServer struct {
	*httptest.Server
	hits    atomic.Int64
	active  atomic.Int64
	maxSeen atomic.Int64
}

func newStatusServer(t *testing.T, h http.HandlerFunc) *statusServer {
	t.Helper()
	s := &statusServer{}
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

func statusAnswer(node string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != meshapi.PathStatus {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(meshapi.NodeStatus{Node: node, Ver: "2.0.0", Models: []meshapi.ModelStatus{{Name: "m"}}})
	}
}

func memberAt(t *testing.T, name string, s *httptest.Server, state, role string, local bool) mesh.Member {
	t.Helper()
	port := 8086
	if s != nil {
		_, portText, _ := net.SplitHostPort(s.Listener.Addr().String())
		port, _ = strconv.Atoi(portText)
	}
	return mesh.Member{Name: name, Addr: netip.MustParseAddr("127.0.0.1"), Port: 7946,
		Meta: mesh.NodeMeta{V: mesh.MetaVersion, API: port, Ver: "2.0.0", Role: role}, State: state, Local: local}
}

type lines struct {
	mu  sync.Mutex
	all []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, fmt.Sprintf(format, args...))
}

func (l *lines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.all {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func runPoller(t *testing.T, p *StatusPoller) {
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

func TestStatusPollerSet(t *testing.T) {
	a, local, gw, dead := newStatusServer(t, statusAnswer("A")), newStatusServer(t, statusAnswer("L")), newStatusServer(t, statusAnswer("GW")), newStatusServer(t, statusAnswer("D"))
	members := &fakeMemberList{}
	members.set(
		memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false),
		memberAt(t, "L", local.Server, meshapi.MemberAlive, meshapi.RoleNode, true),
		memberAt(t, "GW", gw.Server, meshapi.MemberAlive, meshapi.RoleGateway, false),
		memberAt(t, "D", dead.Server, meshapi.MemberDead, meshapi.RoleNode, false),
	)
	p := NewStatusPoller("L", members, 30*time.Millisecond, nil, (&lines{}).logf)
	runPoller(t, p)
	eventually(t, time.Second, "A polled", func() bool { _, _, ok := p.Status("A"); return ok })
	time.Sleep(100 * time.Millisecond)
	if local.hits.Load()+gw.hits.Load()+dead.hits.Load() != 0 {
		t.Errorf("polled local %d, gateway %d, dead %d", local.hits.Load(), gw.hits.Load(), dead.hits.Load())
	}
	st, received, _ := p.Status("A")
	if st.Node != "A" || len(st.Models) != 1 || received.IsZero() {
		t.Errorf("accepted status = %+v at %v", st, received)
	}
}

func TestStatusPollerNameMismatch(t *testing.T) {
	a := newStatusServer(t, statusAnswer("someone-else"))
	members := &fakeMemberList{}
	members.set(memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false))
	logs := &lines{}
	p := NewStatusPoller("L", members, 20*time.Millisecond, nil, logs.logf)
	runPoller(t, p)
	eventually(t, time.Second, "several polls", func() bool { return a.hits.Load() >= 3 })
	if _, _, ok := p.Status("A"); ok || logs.count("someone-else") != 1 {
		t.Errorf("status accepted=%v, log lines %d", ok, logs.count("someone-else"))
	}
}

func TestStatusPollerNoOverlap(t *testing.T) {
	a := newStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		statusAnswer("A")(w, r)
	})
	members := &fakeMemberList{}
	members.set(memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false))
	p := NewStatusPoller("L", members, 20*time.Millisecond, nil, (&lines{}).logf)
	runPoller(t, p)
	eventually(t, 2*time.Second, "three polls", func() bool { return a.hits.Load() >= 3 })
	if a.maxSeen.Load() != 1 {
		t.Errorf("%d concurrent polls of one member", a.maxSeen.Load())
	}
}

func TestStatusPollerFailureKeepsStatus(t *testing.T) {
	var fail atomic.Bool
	a := newStatusServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", 500)
			return
		}
		statusAnswer("A")(w, r)
	})
	members := &fakeMemberList{}
	members.set(memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false))
	p := NewStatusPoller("L", members, 20*time.Millisecond, nil, (&lines{}).logf)
	runPoller(t, p)
	eventually(t, time.Second, "first status", func() bool { _, _, ok := p.Status("A"); return ok })
	_, first, _ := p.Status("A")
	fail.Store(true)
	hits := a.hits.Load()
	eventually(t, time.Second, "failing polls", func() bool { return a.hits.Load() >= hits+3 })
	if _, received, ok := p.Status("A"); !ok || !received.Equal(first) {
		t.Errorf("after failures: ok=%v received=%v, want the first status at %v", ok, received, first)
	}
}

func TestStatusPollerForget(t *testing.T) {
	a, b := newStatusServer(t, statusAnswer("A")), newStatusServer(t, statusAnswer("B"))
	members := &fakeMemberList{}
	ma, mb := memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false), memberAt(t, "B", b.Server, meshapi.MemberAlive, meshapi.RoleNode, false)
	members.set(ma, mb)
	p := NewStatusPoller("L", members, 30*time.Millisecond, nil, (&lines{}).logf)
	runPoller(t, p)
	eventually(t, time.Second, "both", func() bool {
		_, _, okA := p.Status("A")
		_, _, okB := p.Status("B")
		return okA && okB
	})
	members.set(mb)
	eventually(t, time.Second, "A forgotten on departure", func() bool { _, _, ok := p.Status("A"); return !ok })
	p.HandleMemberEvent(mesh.MemberEvent{Kind: mesh.EventFail, Member: mb})
	if _, _, ok := p.Status("B"); ok {
		t.Error("EventFail must forget B at once")
	}
}

func TestStatusPollerJoinPollsAtOnce(t *testing.T) {
	a := newStatusServer(t, statusAnswer("A"))
	members := &fakeMemberList{}
	p := NewStatusPoller("L", members, time.Hour, nil, (&lines{}).logf)
	runPoller(t, p)
	ma := memberAt(t, "A", a.Server, meshapi.MemberAlive, meshapi.RoleNode, false)
	members.set(ma)
	p.HandleMemberEvent(mesh.MemberEvent{Kind: mesh.EventJoin, Member: ma})
	eventually(t, time.Second, "poll on join", func() bool { _, _, ok := p.Status("A"); return ok })
}
