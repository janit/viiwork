package mesh

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// leaveNoticeTTL is how recent a leave notice must be for a departure to
	// read as left rather than dead.
	leaveNoticeTTL = 30 * time.Second
	// departedTTL keeps a machine that went away visible (Decision 8).
	departedTTL = 24 * time.Hour
)

// Member is one mesh member as this node sees it.
type Member struct {
	Name  string
	Addr  netip.Addr
	Port  int      // gossip port
	Meta  NodeMeta // last metadata seen
	State string   // meshapi.MemberAlive | meshapi.MemberDead | meshapi.MemberLeft
	Local bool
	Since time.Time // last state change seen by this node
}

// APIAddr is the member's HTTP API address, from its gossip address and the
// API port in its metadata.
func (m Member) APIAddr() string {
	return netip.AddrPortFrom(m.Addr, uint16(m.Meta.API)).String()
}

// EventKind is what happened to a member.
type EventKind int

const (
	EventJoin   EventKind = iota
	EventUpdate           // metadata changed
	EventLeave            // left gracefully
	EventFail             // declared dead
)

// MemberEvent is one membership change, delivered to Options.OnChange.
type MemberEvent struct {
	Kind   EventKind
	Member Member
}

// memberTable is this node's view of the mesh. memberlist never writes
// Node.State (the real state lives in its unexported nodeState), so suspect
// is not observable and left is recognised from the leave notice a node sends
// before leaving (Decision 6).
type memberTable struct {
	mu      sync.Mutex
	now     func() time.Time
	members map[string]*Member
	notices map[string]time.Time // name -> when it announced its leave
}

func newMemberTable(self Member, now func() time.Time) *memberTable {
	self.State = meshapi.MemberAlive
	self.Local = true
	if self.Since.IsZero() {
		self.Since = now()
	}
	return &memberTable{
		now:     now,
		members: map[string]*Member{self.Name: &self},
		notices: map[string]time.Time{},
	}
}

func (t *memberTable) join(n *memberlist.Node) Member   { return t.setAlive(n) }
func (t *memberTable) update(n *memberlist.Node) Member { return t.setAlive(n) }

func (t *memberTable) setAlive(n *memberlist.Node) Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.members[n.Name]
	if !ok {
		m = &Member{Name: n.Name}
		t.members[n.Name] = m
	}
	if m.State != meshapi.MemberAlive {
		m.State = meshapi.MemberAlive
		m.Since = t.now()
	}
	if ip, ok := netip.AddrFromSlice(n.Addr); ok {
		m.Addr = ip.Unmap()
	}
	m.Port = int(n.Port)
	meta, err := DecodeMeta(n.Meta)
	if err != nil {
		meta = NodeMeta{} // the alive filter rejects such remote members
	}
	m.Meta = meta
	delete(t.notices, n.Name)
	return *m
}

// leaveNotice remembers that name announced it is leaving.
func (t *memberTable) leaveNotice(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notices[name] = t.now()
}

// depart marks name left (a leave notice at most 30 s old) or dead.
func (t *memberTable) depart(name string) Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	m, ok := t.members[name]
	if !ok {
		m = &Member{Name: name}
		t.members[name] = m
	}
	m.State = meshapi.MemberDead
	if at, ok := t.notices[name]; ok && now.Sub(at) <= leaveNoticeTTL {
		m.State = meshapi.MemberLeft
	}
	delete(t.notices, name)
	m.Since = now
	return *m
}

// snapshot returns copies sorted by name, pruning departures older than 24 h.
func (t *memberTable) snapshot() []Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	out := make([]Member, 0, len(t.members))
	for name, m := range t.members {
		if m.State != meshapi.MemberAlive && now.Sub(m.Since) > departedTTL {
			delete(t.members, name)
			continue
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// aliveAddrs is the gossip ip:port of every alive member, self included.
func (t *memberTable) aliveAddrs() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]bool{}
	for _, m := range t.members {
		if m.State == meshapi.MemberAlive && m.Addr.IsValid() {
			out[netip.AddrPortFrom(m.Addr, uint16(m.Port)).String()] = true
		}
	}
	return out
}
