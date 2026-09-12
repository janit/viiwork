//go:build integration

package mesh

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type eventLog struct {
	mu     sync.Mutex
	events []MemberEvent
}

func (e *eventLog) record(ev MemberEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *eventLog) indexOf(kind EventKind, name string, after int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := after + 1; i < len(e.events); i++ {
		if e.events[i].Kind == kind && e.events[i].Member.Name == name {
			return i
		}
	}
	return -1
}

func (e *eventLog) sawName(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Member.Name == name {
			return true
		}
	}
	return false
}

type testNode struct {
	*Mesh
	log    *lockedBuffer
	events *eventLog
}

// startNode starts a member on the in-process network with fast timings. The
// caller's change runs last and may set seeds, keys, payloads and checks.
func startNode(t *testing.T, net *meshtest.Network, name string, change func(*Options)) *testNode {
	t.Helper()
	tr := net.Transport(name)
	logBuf := &lockedBuffer{}
	events := &eventLog{}
	o := Options{
		Name: name, Network: NetworkTailnet, BindPort: 7946, Advertise: net.Addr(name).Addr(),
		APIPort: 8086, Role: meshapi.RoleNode, Version: "test", Enforce: EnforceFull,
		RejoinInterval: 300 * time.Millisecond, Log: logBuf, Debug: true, OnChange: events.record,
		Tune: func(c *memberlist.Config) {
			c.Transport = tr
			c.ProbeInterval = 200 * time.Millisecond
			c.ProbeTimeout = 100 * time.Millisecond
			c.SuspicionMult = 2
			c.GossipInterval = 50 * time.Millisecond
			c.PushPullInterval = time.Second
			c.TCPTimeout = 500 * time.Millisecond
			c.DeadNodeReclaimTime = time.Second
		},
	}
	if change != nil {
		change(&o)
	}
	m, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return &testNode{Mesh: m, log: logBuf, events: events}
}

func seeds(net *meshtest.Network, names ...string) func(*Options) {
	return func(o *Options) {
		for _, n := range names {
			o.Seeds = append(o.Seeds, net.Addr(n).String())
		}
	}
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func stateOf(m *Mesh, name string) string {
	if mem, ok := m.Member(name); ok {
		return mem.State
	}
	return ""
}

func chain(t *testing.T, net *meshtest.Network) (a, b, c *testNode) {
	t.Helper()
	a = startNode(t, net, "A", nil)
	b = startNode(t, net, "B", seeds(net, "A"))
	c = startNode(t, net, "C", seeds(net, "B"))
	within(t, 10*time.Second, "chain join", func() bool {
		return a.NumAlive() == 3 && b.NumAlive() == 3 && c.NumAlive() == 3
	})
	return a, b, c
}

func TestIntegrationI1ChainJoin(t *testing.T) {
	chain(t, meshtest.NewNetwork())
}

func TestIntegrationI2KilledWithoutLeave(t *testing.T) {
	net := meshtest.NewNetwork()
	a, _, c := chain(t, net)
	net.Unplug("C")
	_ = c.Shutdown()
	within(t, 10*time.Second, "C dead on A", func() bool { return stateOf(a.Mesh, "C") == meshapi.MemberDead })
	if a.events.indexOf(EventFail, "C", -1) < 0 {
		t.Error("A's OnChange must see EventFail for C")
	}
}

func TestIntegrationI3GracefulLeave(t *testing.T) {
	net := meshtest.NewNetwork()
	a, _, c := chain(t, net)
	if err := c.Leave(time.Second); err != nil {
		t.Fatal(err)
	}
	_ = c.Shutdown()
	within(t, 2*time.Second, "C left on A", func() bool { return stateOf(a.Mesh, "C") == meshapi.MemberLeft })
	if a.events.indexOf(EventLeave, "C", -1) < 0 {
		t.Error("A's OnChange must see EventLeave for C")
	}
}

func killC(t *testing.T, net *meshtest.Network) (*testNode, *testNode) {
	t.Helper()
	a, _, c := chain(t, net)
	net.Unplug("C")
	_ = c.Shutdown()
	within(t, 10*time.Second, "C dead on A", func() bool { return stateOf(a.Mesh, "C") == meshapi.MemberDead })
	return a, c
}

func TestIntegrationI4PowerOn(t *testing.T) {
	net := meshtest.NewNetwork()
	a, _ := killC(t, net)
	failAt := a.events.indexOf(EventFail, "C", -1)
	net.Plug("C")
	startNode(t, net, "C", seeds(net, "A"))
	within(t, 10*time.Second, "C alive on A", func() bool { return stateOf(a.Mesh, "C") == meshapi.MemberAlive })
	if a.events.indexOf(EventJoin, "C", failAt) < 0 {
		t.Error("A must see EventJoin for C after its EventFail")
	}
}

func TestIntegrationI5NewAddressReclaim(t *testing.T) {
	net := meshtest.NewNetwork()
	a, _ := killC(t, net)
	old := net.Addr("C")
	net.Plug("C")
	net.Readdress("C")
	time.Sleep(1500 * time.Millisecond)
	startNode(t, net, "C", seeds(net, "A"))
	within(t, 10*time.Second, "C alive at its new address", func() bool {
		mem, ok := a.Member("C")
		return ok && mem.State == meshapi.MemberAlive && mem.Addr != old.Addr()
	})
}

var joinErrorRE = regexp.MustCompile(`\[DEBUG\] join (\S+): (.*)`)

func TestIntegrationI6WrongSecret(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "A", func(o *Options) { o.SecretKey = key1 })
	b := startNode(t, net, "B", func(o *Options) { o.SecretKey = key2; seeds(net, "A")(o) })
	time.Sleep(3 * 300 * time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	if a.NumAlive() != 1 || b.NumAlive() != 1 {
		t.Errorf("A alive=%d B alive=%d, want strangers", a.NumAlive(), b.NumAlive())
	}
	if n := strings.Count(b.log.String(), "mesh mode mismatch with "+net.Addr("A").String()); n != 1 {
		t.Errorf("B mismatch lines = %d, want 1\n%s", n, b.log.String())
	}
	if !strings.Contains(a.log.String(), "no installed keys could decrypt") {
		t.Errorf("A's log lacks the decryption error:\n%s", a.log.String())
	}
	// I16: pin the joiner-side error text.
	for _, m := range joinErrorRE.FindAllStringSubmatch(b.log.String(), -1) {
		t.Logf("rejected join error: %s", m[2])
		if got := classifyJoinError(fmt.Errorf("%s", m[2])); got != joinMismatch {
			t.Errorf("join error %q classifies as %v, want joinMismatch", m[2], got)
		}
	}
}

func TestIntegrationI7OpenMeetsSecured(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "A", nil)
	b := startNode(t, net, "B", func(o *Options) { o.SecretKey = key1; seeds(net, "A")(o) })
	time.Sleep(1200 * time.Millisecond)
	if a.NumAlive() != 1 || b.NumAlive() != 1 {
		t.Errorf("A alive=%d B alive=%d, want strangers", a.NumAlive(), b.NumAlive())
	}
	if !strings.Contains(b.log.String(), "mesh mode mismatch with") {
		t.Errorf("B's log lacks the mismatch line:\n%s", b.log.String())
	}
	if !strings.Contains(a.log.String(), "remote state is encrypted and encryption is not configured") {
		t.Errorf("A's log lacks the encryption error:\n%s", a.log.String())
	}
}

func TestIntegrationI8UpshiftWithoutDowntime(t *testing.T) {
	net := meshtest.NewNetwork()
	names := []string{"A", "B", "C"}
	nodes := map[string]*testNode{
		"A": startNode(t, net, "A", nil),
		"B": startNode(t, net, "B", seeds(net, "A")),
		"C": startNode(t, net, "C", seeds(net, "A")),
	}
	allAlive := func(what string) {
		t.Helper()
		within(t, 15*time.Second, what, func() bool {
			for _, n := range names {
				if nodes[n].NumAlive() != 3 {
					return false
				}
			}
			return true
		})
	}
	allAlive("initial join")
	for _, level := range []string{EnforceNone, EnforceOutgoing, EnforceFull} {
		for _, name := range names {
			old := nodes[name]
			_ = old.Leave(time.Second)
			_ = old.Shutdown()
			var others []string
			for _, n := range names {
				if n != name {
					others = append(others, n)
				}
			}
			nodes[name] = startNode(t, net, name, func(o *Options) {
				o.SecretKey = key1
				o.Enforce = level
				seeds(net, others...)(o)
			})
			allAlive(fmt.Sprintf("%s restarted with %s", name, level))
		}
	}
	for _, n := range names {
		if nodes[n].Mode() != meshapi.MeshSecured {
			t.Errorf("%s mode = %s", n, nodes[n].Mode())
		}
	}
}

func TestIntegrationI9RogueMemberOutOfRange(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "A", nil)
	b := startNode(t, net, "B", seeds(net, "A"))
	within(t, 5*time.Second, "A and B", func() bool { return a.NumAlive() == 2 && b.NumAlive() == 2 })

	rogueAddr := netip.MustParseAddrPort("192.0.2.10:7946")
	tr := net.TransportAt("R", rogueAddr)
	permissive := func(netip.Addr) error { return nil }
	r, err := Start(context.Background(), Options{
		Name: "R", Network: NetworkTailnet, BindPort: 7946, Advertise: rogueAddr.Addr(), APIPort: 8086,
		Role: meshapi.RoleNode, Version: "test", Enforce: EnforceFull, RejoinInterval: 300 * time.Millisecond,
		Log: &lockedBuffer{}, AddrCheck: permissive, Seeds: []string{net.Addr("A").String()},
		Tune: func(c *memberlist.Config) {
			c.Transport = tr
			c.ProbeInterval = 200 * time.Millisecond
			c.ProbeTimeout = 100 * time.Millisecond
			c.GossipInterval = 50 * time.Millisecond
			c.PushPullInterval = time.Second
			c.TCPTimeout = 500 * time.Millisecond
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	time.Sleep(1200 * time.Millisecond)
	if _, ok := a.Member("R"); ok {
		t.Error("A lists the rogue member")
	}
	if _, ok := b.Member("R"); ok {
		t.Error("B lists the rogue member")
	}
	if a.events.sawName("R") {
		t.Error("A's OnChange saw the rogue member")
	}
	if n := strings.Count(a.log.String(), "ignoring member R"); n != 1 {
		t.Errorf("A logged %d rejection lines for R, want 1", n)
	}
}

func TestIntegrationI10DuplicateName(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "gb1", func(o *Options) { o.ConflictWindow = time.Millisecond })
	b := startNode(t, net, "B", seeds(net, "gb1"))
	within(t, 5*time.Second, "gb1 and B", func() bool { return a.NumAlive() == 2 && b.NumAlive() == 2 })
	aAddr := net.Addr("gb1")
	net.Readdress("gb1")
	a2 := startNode(t, net, "gb1", seeds(net, "B"))
	select {
	case err := <-a2.Fatal():
		if !strings.Contains(err.Error(), `duplicate node name "gb1"`) {
			t.Errorf("A2 fatal = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A2 did not report its duplicate name")
	}
	select {
	case err := <-a.Fatal():
		t.Errorf("the older gb1 must keep its name, got %v", err)
	default:
	}
	if mem, ok := b.Member("gb1"); !ok || mem.Addr != aAddr.Addr() {
		t.Errorf("B lists gb1 at %v, want the original %v", mem.Addr, aAddr.Addr())
	}
}

type memPayload struct {
	mu     sync.Mutex
	msgs   []string
	merged []string
	joins  []bool
	state  []byte
}

func (p *memPayload) NotifyMsg(msg []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, string(msg))
}

func (p *memPayload) LocalState(bool) []byte { return p.state }

func (p *memPayload) MergeRemoteState(buf []byte, join bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.merged = append(p.merged, string(buf))
	p.joins = append(p.joins, join)
}

func (p *memPayload) received(msg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func (p *memPayload) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.msgs) == 0 {
		return ""
	}
	return p.msgs[len(p.msgs)-1]
}

func TestIntegrationI11toI13Payload(t *testing.T) {
	net := meshtest.NewNetwork()
	pa, pb, pc := &memPayload{}, &memPayload{}, &memPayload{}
	with := func(p *memPayload, seed ...string) func(*Options) {
		return func(o *Options) {
			o.Payload = p
			seeds(net, seed...)(o)
		}
	}
	a := startNode(t, net, "A", with(pa))
	b := startNode(t, net, "B", with(pb, "A"))
	c := startNode(t, net, "C", with(pc, "A"))
	within(t, 10*time.Second, "joined", func() bool { return a.NumAlive() == 3 && b.NumAlive() == 3 && c.NumAlive() == 3 })

	a.Broadcast("stable-coder", []byte("m1"))
	within(t, 2*time.Second, "I11 m1 everywhere", func() bool { return pb.received("m1") && pc.received("m1") })

	a.Broadcast("stable-coder", []byte("m2"))
	a.Broadcast("stable-coder", []byte("m2"))
	within(t, 2*time.Second, "I12 m2 last", func() bool { return pb.last() == "m2" && pc.last() == "m2" })

	pd := &memPayload{state: []byte("d-state")}
	startNode(t, net, "D", with(pd, "A"))
	within(t, 5*time.Second, "I13 A merged D's state on join", func() bool {
		pa.mu.Lock()
		defer pa.mu.Unlock()
		for i, s := range pa.merged {
			if s == "d-state" && pa.joins[i] {
				return true
			}
		}
		return false
	})
}

func TestIntegrationI14I15PartitionAndHeal(t *testing.T) {
	net := meshtest.NewNetwork()
	names := []string{"A", "B", "C", "D"}
	nodes := map[string]*testNode{}
	for _, n := range names {
		var others []string
		for _, o := range names {
			if o != n {
				others = append(others, o)
			}
		}
		nodes[n] = startNode(t, net, n, seeds(net, others...))
	}
	all := func(want int) bool {
		for _, n := range names {
			if nodes[n].NumAlive() != want {
				return false
			}
		}
		return true
	}
	within(t, 10*time.Second, "4 alive", func() bool { return all(4) })

	net.Partition([]string{"A", "B"}, []string{"C", "D"})
	within(t, 15*time.Second, "I14 A sees C and D dead", func() bool {
		return stateOf(nodes["A"].Mesh, "C") == meshapi.MemberDead && stateOf(nodes["A"].Mesh, "D") == meshapi.MemberDead
	})

	net.Heal()
	within(t, 5*300*time.Millisecond+5*time.Second, "I15 healed", func() bool { return all(4) })
}
