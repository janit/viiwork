package meshtest

import (
	"fmt"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

func testConfig(net *Network, name string) *memberlist.Config {
	c := memberlist.DefaultLocalConfig()
	c.Name = name
	c.Transport = net.Transport(name)
	c.ProbeInterval = 200 * time.Millisecond
	c.ProbeTimeout = 100 * time.Millisecond
	c.SuspicionMult = 2
	c.GossipInterval = 50 * time.Millisecond
	c.PushPullInterval = 2 * time.Second
	c.TCPTimeout = 500 * time.Millisecond
	c.LogOutput = io.Discard
	return c
}

func create(t *testing.T, net *Network, name string) *memberlist.Memberlist {
	t.Helper()
	m, err := memberlist.Create(testConfig(net, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return m
}

func join(t *testing.T, m *memberlist.Memberlist, net *Network, names ...string) {
	t.Helper()
	var addrs []string
	for _, n := range names {
		addrs = append(addrs, net.Addr(n).String())
	}
	if _, err := m.Join(addrs); err != nil {
		t.Fatalf("join %v: %v", names, err)
	}
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAddresses(t *testing.T) {
	n := NewNetwork()
	n.Transport("a")
	n.Transport("b")
	if n.Addr("a") != netip.MustParseAddrPort("100.64.0.1:7946") || n.Addr("b") != netip.MustParseAddrPort("100.64.0.2:7946") {
		t.Errorf("addresses a=%v b=%v", n.Addr("a"), n.Addr("b"))
	}
	n.Transport("a")
	if n.Addr("a") != netip.MustParseAddrPort("100.64.0.1:7946") {
		t.Errorf("a new transport for a keeps its address, got %v", n.Addr("a"))
	}
	n.Readdress("a")
	n.Transport("a")
	if n.Addr("a") != netip.MustParseAddrPort("100.64.0.3:7946") {
		t.Errorf("after Readdress a = %v, want 100.64.0.3:7946", n.Addr("a"))
	}
}

func TestJoin(t *testing.T) {
	n := NewNetwork()
	a, b := create(t, n, "a"), create(t, n, "b")
	join(t, b, n, "a")
	eventually(t, 5*time.Second, "2 members each", func() bool { return a.NumMembers() == 2 && b.NumMembers() == 2 })
}

func TestUnplugAndPlug(t *testing.T) {
	n := NewNetwork()
	a, b := create(t, n, "a"), create(t, n, "b")
	join(t, b, n, "a")
	eventually(t, 5*time.Second, "joined", func() bool { return a.NumMembers() == 2 })
	n.Unplug("b")
	eventually(t, 10*time.Second, "a sees b gone", func() bool { return a.NumMembers() == 1 })
	n.Plug("b")
	join(t, b, n, "a")
	eventually(t, 5*time.Second, "a sees b again", func() bool { return a.NumMembers() == 2 })
}

func TestPartitionAndHeal(t *testing.T) {
	n := NewNetwork()
	a, b, c := create(t, n, "a"), create(t, n, "b"), create(t, n, "c")
	join(t, b, n, "a")
	join(t, c, n, "a")
	eventually(t, 5*time.Second, "3 members", func() bool { return a.NumMembers() == 3 && b.NumMembers() == 3 && c.NumMembers() == 3 })
	n.Partition([]string{"a"}, []string{"b", "c"})
	eventually(t, 10*time.Second, "a alone", func() bool { return a.NumMembers() == 1 })
	n.Heal()
	join(t, a, n, "b")
	eventually(t, 5*time.Second, "healed", func() bool { return a.NumMembers() == 3 && b.NumMembers() == 3 && c.NumMembers() == 3 })
}

func TestSendsNeverBlock(t *testing.T) {
	n := NewNetwork()
	a := n.Transport("a")
	n.Transport("b")
	n.Unplug("b")
	start := time.Now()
	for i := 0; i < 10000; i++ {
		if _, err := a.WriteTo([]byte("x"), n.Addr("b").String()); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("10,000 sends to an unplugged node took %v", took)
	}
}

func TestDialsFailFast(t *testing.T) {
	n := NewNetwork()
	a := n.Transport("a")
	n.Transport("b")
	n.Unplug("b")
	start := time.Now()
	if _, err := a.DialTimeout(n.Addr("b").String(), 10*time.Second); err == nil {
		t.Fatal("dial to an unplugged node succeeded")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("dial took %v, want under 500 ms", took)
	}
}

func TestConcurrentCreation(t *testing.T) {
	n := NewNetwork()
	names := []string{"n1", "n2", "n3", "n4", "n5"}
	nodes := make([]*memberlist.Memberlist, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := memberlist.Create(testConfig(n, name))
			if err != nil {
				t.Error(err)
				return
			}
			nodes[i] = m
		}()
	}
	wg.Wait()
	for i, m := range nodes {
		if m == nil {
			t.FailNow()
		}
		t.Cleanup(func() { _ = m.Shutdown() })
		if i > 0 {
			if _, err := m.Join([]string{n.Addr("n1").String()}); err != nil {
				t.Error(fmt.Errorf("%s join: %w", names[i], err))
			}
		}
	}
	eventually(t, 10*time.Second, "5 members everywhere", func() bool {
		for _, m := range nodes {
			if m == nil || m.NumMembers() != 5 {
				return false
			}
		}
		return true
	})
}
