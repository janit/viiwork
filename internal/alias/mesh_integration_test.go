//go:build integration

package alias

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// allServed is a ServedView in which the fixture's three models exist and are
// served everywhere.
type allServed map[string]bool

func (s allServed) Exists(m string) bool { return s[m] }
func (s allServed) Served(m string) bool { return s[m] }

var meshModels = allServed{"Qwen3.8-27B": true, "gemma-4-31B-it": true, "granite": true}

type aliasNode struct {
	name   string
	dir    string
	m      atomic.Pointer[mesh.Mesh]
	svc    *Service
	events *recorder
}

func (n *aliasNode) mesh() *mesh.Mesh { return n.m.Load() }

func (n *aliasNode) stop() {
	if m := n.mesh(); m != nil {
		_ = m.Shutdown()
	}
}

// startAliasNode opens the node's store in dir, builds its service, runs
// beforeStart (to look at what came from disk), and joins the mesh with the
// service as payload.
func startAliasNode(t *testing.T, net *meshtest.Network, name, dir string, beforeStart func(*Service), seedNames ...string) *aliasNode {
	t.Helper()
	store, err := Open(dir, name, nil)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	n := &aliasNode{name: name, dir: dir, events: &recorder{}}
	n.svc = NewService(store, NewResolver(store, meshModels, nil, nil), func(key string, msg []byte) {
		if m := n.mesh(); m != nil {
			m.Broadcast(key, msg)
		}
	}, n.events, nil)
	if beforeStart != nil {
		beforeStart(n.svc)
	}
	tr := net.Transport(name)
	o := mesh.Options{
		Name: name, Network: mesh.NetworkTailnet, BindPort: 7946, Advertise: net.Addr(name).Addr(),
		APIPort: 8086, Role: meshapi.RoleNode, Version: "test", Enforce: mesh.EnforceFull,
		RejoinInterval: 300 * time.Millisecond, Log: io.Discard, Payload: n.svc,
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
	for _, s := range seedNames {
		o.Seeds = append(o.Seeds, net.Addr(s).String())
	}
	m, err := mesh.Start(context.Background(), o)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	n.m.Store(m)
	t.Cleanup(n.stop)
	return n
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cluster starts the named nodes, each seeding all the others, and waits
// until every one sees all of them alive.
func cluster(t *testing.T, net *meshtest.Network, names ...string) map[string]*aliasNode {
	t.Helper()
	nodes := map[string]*aliasNode{}
	for _, name := range names {
		var others []string
		for _, o := range names {
			if o != name {
				others = append(others, o)
			}
		}
		nodes[name] = startAliasNode(t, net, name, t.TempDir(), nil, others...)
	}
	eventually(t, 10*time.Second, "all joined", func() bool {
		for _, n := range nodes {
			if n.mesh().NumAlive() != len(names) {
				return false
			}
		}
		return true
	})
	return nodes
}

func resolvesTo(n *aliasNode, alias, model string) bool {
	m, _, err := n.svc.Resolver().Resolve(alias)
	return err == nil && m == model
}

func set(t *testing.T, n *aliasNode, name, target string) meshapi.AliasBroadcast {
	t.Helper()
	b, err := n.svc.Set(name, meshapi.AliasWriteRequest{Target: target, Fallbacks: []string{}})
	if err != nil {
		t.Fatalf("%s.Set(%s): %v", n.name, name, err)
	}
	return b
}

func TestAliasMeshN1Propagation(t *testing.T) {
	nodes := cluster(t, meshtest.NewNetwork(), "A", "B", "C")
	set(t, nodes["A"], "stable-coder", "Qwen3.8-27B")
	eventually(t, 3*time.Second, "B and C resolve stable-coder", func() bool {
		return resolvesTo(nodes["B"], "stable-coder", "Qwen3.8-27B") && resolvesTo(nodes["C"], "stable-coder", "Qwen3.8-27B")
	})
	time.Sleep(1500 * time.Millisecond) // a push/pull round must not add a second event
	for _, name := range []string{"B", "C"} {
		if n := nodes[name].events.eventsContaining("alias stable-coder: (none) -> Qwen3.8-27B by A"); n != 1 {
			t.Errorf("%s recorded %d change events by A, want 1: %v", name, n, nodes[name].events.events)
		}
	}
}

func TestAliasMeshN2RestartCatchesUp(t *testing.T) {
	net := meshtest.NewNetwork()
	nodes := cluster(t, net, "A", "B", "C")
	set(t, nodes["A"], "stable-coder", "Qwen3.8-27B")
	eventually(t, 3*time.Second, "C has the alias", func() bool { return resolvesTo(nodes["C"], "stable-coder", "Qwen3.8-27B") })

	c := nodes["C"]
	net.Unplug("C")
	c.stop()
	set(t, nodes["A"], "stable-coder", "gemma-4-31B-it")
	net.Plug("C")

	var fromDisk bool
	c2 := startAliasNode(t, net, "C", c.dir, func(s *Service) {
		fromDisk = resolvesTo(&aliasNode{svc: s}, "stable-coder", "Qwen3.8-27B")
	}, "A", "B")
	if !fromDisk {
		t.Error("C did not start from its on-disk table")
	}
	eventually(t, 5*time.Second, "C resolves the newer target", func() bool { return resolvesTo(c2, "stable-coder", "gemma-4-31B-it") })
}

func TestAliasMeshN3SplitBrain(t *testing.T) {
	net := meshtest.NewNetwork()
	nodes := cluster(t, net, "A", "B", "C", "D")
	net.Partition([]string{"A", "B"}, []string{"C", "D"})
	dead := func(n *aliasNode, other string) bool {
		m, ok := n.mesh().Member(other)
		return ok && m.State == meshapi.MemberDead
	}
	eventually(t, 10*time.Second, "each side sees the other as dead", func() bool {
		return dead(nodes["A"], "C") && dead(nodes["B"], "D") && dead(nodes["C"], "A") && dead(nodes["D"], "B")
	})
	a := set(t, nodes["A"], "x", "Qwen3.8-27B")
	time.Sleep(2 * time.Millisecond)
	c := set(t, nodes["C"], "x", "granite")
	if a.Entry.Ver != 1 || c.Entry.Ver != 1 {
		t.Fatalf("versions %d and %d, want both 1", a.Entry.Ver, c.Entry.Ver)
	}
	eventually(t, 3*time.Second, "each side converged within itself", func() bool {
		b, _ := nodes["B"].svc.Store().Get("x")
		d, _ := nodes["D"].svc.Store().Get("x")
		return b.By == "A" && d.By == "C"
	})
	winner := a.Entry
	if meshapi.CompareAliasEntries(c.Entry, a.Entry) > 0 {
		winner = c.Entry
	}
	net.Heal()

	current := func(n *aliasNode) meshapi.AliasEntry {
		e, _ := n.svc.Store().Get("x")
		e.History = nil
		e.Fallbacks = nil
		return e
	}
	winner.History, winner.Fallbacks = nil, nil
	eventually(t, 10*time.Second, "all four hold the winner", func() bool {
		for _, n := range nodes {
			if !reflect.DeepEqual(current(n), winner) {
				return false
			}
		}
		return true
	})
	time.Sleep(1500 * time.Millisecond)
	total := 0
	for name, n := range nodes {
		k := n.events.eventsContaining("alias conflict x:")
		if k > 1 {
			t.Errorf("%s recorded %d conflict events for x: %v", name, k, n.events.events)
		}
		total += k
	}
	if total == 0 {
		t.Error("no node recorded the conflict")
	}
}

func TestAliasMeshN4ColdRestart(t *testing.T) {
	net := meshtest.NewNetwork()
	nodes := cluster(t, net, "A", "B", "C")
	set(t, nodes["A"], "stable-coder", "Qwen3.8-27B")
	eventually(t, 3*time.Second, "converged", func() bool {
		return resolvesTo(nodes["B"], "stable-coder", "Qwen3.8-27B") && resolvesTo(nodes["C"], "stable-coder", "Qwen3.8-27B")
	})
	for _, n := range nodes {
		n.stop()
	}
	restarted := map[string]*aliasNode{}
	for _, name := range []string{"C", "A", "B"} {
		var others []string
		for _, o := range []string{"A", "B", "C"} {
			if o != name {
				others = append(others, o)
			}
		}
		restarted[name] = startAliasNode(t, net, name, nodes[name].dir, nil, others...)
	}
	eventually(t, 10*time.Second, "all rejoined", func() bool {
		for _, n := range restarted {
			if n.mesh().NumAlive() != 3 {
				return false
			}
		}
		return true
	})
	for name, n := range restarted {
		e, _ := n.svc.Store().Get("stable-coder")
		if !resolvesTo(n, "stable-coder", "Qwen3.8-27B") || e.Ver != 1 {
			t.Errorf("%s: entry %+v", name, e)
		}
	}
}

func TestAliasMeshN5ManyWrites(t *testing.T) {
	nodes := cluster(t, meshtest.NewNetwork(), "A", "B")
	for i := 0; i < 30; i++ {
		set(t, nodes["A"], fmt.Sprintf("alias-%02d", i), "Qwen3.8-27B")
	}
	eventually(t, 5*time.Second, "B has 30 aliases", func() bool { return nodes["B"].svc.Store().Live() == 30 })
}

func TestAliasMeshN6HistoryTravels(t *testing.T) {
	nodes := cluster(t, meshtest.NewNetwork(), "A", "B")
	targets := []string{"Qwen3.8-27B", "gemma-4-31B-it", "granite"}
	for i := 0; i <= meshapi.MaxAliasHistory; i++ {
		set(t, nodes["A"], "busy", targets[i%len(targets)])
		time.Sleep(3 * time.Millisecond) // versions are told apart by ts and by
	}
	a, _ := nodes["A"].svc.Store().Get("busy")
	if len(a.History) != meshapi.MaxAliasHistory {
		t.Fatalf("A's history has %d versions", len(a.History))
	}
	eventually(t, 5*time.Second, "B has A's history", func() bool {
		b, _ := nodes["B"].svc.Store().Get("busy")
		return reflect.DeepEqual(b.History, a.History) && b.Ver == a.Ver
	})
}
