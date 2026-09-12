package mesh

import (
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)}
}

func mustMeta(t *testing.T, role string) []byte {
	t.Helper()
	b, err := EncodeMeta(NodeMeta{V: MetaVersion, API: 8086, Ver: "2.0.0", Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func node(t *testing.T, name, ip string) *memberlist.Node {
	return &memberlist.Node{Name: name, Addr: net.ParseIP(ip), Port: 7946, Meta: mustMeta(t, meshapi.RoleNode)}
}

func selfMember() Member {
	return Member{Name: "gb1", Addr: netip.MustParseAddr("100.64.0.1"), Port: 7946, State: meshapi.MemberAlive, Local: true}
}

func TestMemberTable(t *testing.T) {
	clock := newClock()
	tbl := newMemberTable(selfMember(), clock.Now)

	tbl.join(node(t, "node-a", "100.64.0.3"))
	tbl.join(node(t, "b", "100.64.0.2"))
	snap := tbl.snapshot()
	if len(snap) != 3 || snap[0].Name != "b" || snap[1].Name != "gb1" || !snap[1].Local || snap[2].Name != "node-a" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].APIAddr() != "100.64.0.2:8086" {
		t.Errorf("APIAddr = %q", snap[0].APIAddr())
	}

	gone := tbl.depart("node-a")
	if gone.State != meshapi.MemberDead {
		t.Errorf("departure without a notice = %q, want dead", gone.State)
	}
	addrs := tbl.aliveAddrs()
	if !addrs["100.64.0.1:7946"] || !addrs["100.64.0.2:7946"] || addrs["100.64.0.3:7946"] {
		t.Errorf("aliveAddrs = %v", addrs)
	}

	clock.Advance(23 * time.Hour)
	if len(tbl.snapshot()) != 3 {
		t.Error("a 23 h old departure must be kept")
	}
	clock.Advance(2 * time.Hour)
	if len(tbl.snapshot()) != 2 {
		t.Error("a departure older than 24 h must be pruned")
	}

	tbl.join(node(t, "c", "100.64.0.4"))
	tbl.depart("c")
	clock.Advance(time.Minute)
	tbl.join(node(t, "c", "100.64.0.4"))
	for _, m := range tbl.snapshot() {
		if m.Name == "c" && (m.State != meshapi.MemberAlive || !m.Since.Equal(clock.Now())) {
			t.Errorf("rejoined c = %+v", m)
		}
	}
}

func TestMemberTableConcurrent(t *testing.T) {
	tbl := newMemberTable(selfMember(), time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				n := node(t, "n", "100.64.0.9")
				tbl.join(n)
				tbl.depart("n")
				_ = tbl.snapshot()
				_ = tbl.aliveAddrs()
			}
		}()
	}
	wg.Wait()
}
