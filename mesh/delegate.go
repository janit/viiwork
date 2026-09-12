package mesh

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

// eventBuffer bounds queued member events; memberlist must never block on us.
const eventBuffer = 1024

// defaultConflictWindow is how young a process must be for a duplicate name
// to be its fault (Decision 4).
const defaultConflictWindow = 60 * time.Second

// delegate is everything memberlist calls back into.
type delegate struct {
	o       Options
	meta    []byte
	table   *memberTable
	queue   *memberlist.TransmitLimitedQueue
	logf    func(string, ...any)
	started time.Time
	now     func() time.Time

	eventCh chan MemberEvent
	fatalCh chan error

	mu            sync.Mutex
	loggedKinds   map[byte]bool
	loggedRejects map[string]bool
	droppedLogged bool
	fatalSent     bool
}

var _ delegates = (*delegate)(nil)

func newDelegate(o Options, meta []byte, table *memberTable, queue *memberlist.TransmitLimitedQueue, logf func(string, ...any), started time.Time, now func() time.Time) *delegate {
	return &delegate{
		o: o, meta: meta, table: table, queue: queue, logf: logf, started: started, now: now,
		eventCh:       make(chan MemberEvent, eventBuffer),
		fatalCh:       make(chan error, 1),
		loggedKinds:   map[byte]bool{},
		loggedRejects: map[string]bool{},
	}
}

func (d *delegate) events() <-chan MemberEvent { return d.eventCh }
func (d *delegate) fatal() <-chan error        { return d.fatalCh }

// NodeMeta returns the pre-encoded C3 metadata; EncodeMeta already made sure
// it fits memberlist's limit.
func (d *delegate) NodeMeta(int) []byte { return d.meta }

// NotifyMsg handles one user message. It copies first: memberlist reuses its
// buffers.
func (d *delegate) NotifyMsg(msg []byte) {
	msg = append([]byte(nil), msg...)
	kind, body, ok := parseFrame(msg)
	if !ok {
		var k byte
		if len(msg) > 0 {
			k = msg[0]
		}
		d.mu.Lock()
		first := !d.loggedKinds[k]
		d.loggedKinds[k] = true
		d.mu.Unlock()
		if first {
			d.logf("ignoring a gossip message of unknown kind 0x%02x", k)
		}
		return
	}
	switch kind {
	case frameLeave:
		d.table.leaveNotice(string(body))
	case framePayload:
		if d.o.Payload != nil {
			d.o.Payload.NotifyMsg(body)
		}
	}
}

func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	return d.queue.GetBroadcasts(overhead, limit)
}

func (d *delegate) LocalState(join bool) []byte {
	if d.o.Payload == nil {
		return nil
	}
	return d.o.Payload.LocalState(join)
}

func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	if d.o.Payload != nil {
		d.o.Payload.MergeRemoteState(buf, join)
	}
}

// broadcast queues one payload message under key; a newer message with the
// same key replaces one still queued.
func (d *delegate) broadcast(key string, msg []byte) {
	d.queue.QueueBroadcast(namedBroadcast{name: key, msg: frame(framePayload, msg)})
}

type namedBroadcast struct {
	name string
	msg  []byte
}

func (b namedBroadcast) Name() string    { return b.name }
func (b namedBroadcast) Message() []byte { return b.msg }
func (b namedBroadcast) Finished()       {}

func (b namedBroadcast) Invalidates(other memberlist.Broadcast) bool {
	nb, ok := other.(memberlist.NamedBroadcast)
	return ok && nb.Name() == b.name
}

// NotifyAlive is the alive filter (Decision 10). The node itself is always
// accepted, because memberlist runs this for the local node while it
// bootstraps. Other members need viiwork v2 metadata, and in an open mesh an
// address inside the mesh network's range. A secured mesh does no address
// check: its membership is authenticated by the secret.
func (d *delegate) NotifyAlive(peer *memberlist.Node) error {
	if peer.Name == d.o.Name {
		return nil
	}
	if _, err := DecodeMeta(peer.Meta); err != nil {
		return d.reject(peer, fmt.Errorf("not viiwork v2 metadata: %w", err))
	}
	if d.o.SecretKey == nil {
		ip, ok := netip.AddrFromSlice(peer.Addr)
		if !ok {
			return d.reject(peer, errors.New("no address"))
		}
		check := d.o.AddrCheck
		if check == nil {
			check = func(a netip.Addr) error { return CheckMemberAddr(d.o.Network, a) }
		}
		if err := check(ip.Unmap()); err != nil {
			return d.reject(peer, err)
		}
	}
	return nil
}

func (d *delegate) reject(peer *memberlist.Node, reason error) error {
	key := peer.Name + "|" + peer.Address() + "|" + reason.Error()
	d.mu.Lock()
	first := !d.loggedRejects[key]
	d.loggedRejects[key] = true
	d.mu.Unlock()
	if first {
		d.logf("ignoring member %s (%s): %v", peer.Name, peer.Address(), reason)
	}
	return reason
}

// NotifyConflict handles one name at two addresses. memberlist tells both
// claimants alike, with their own entry as existing, so the start age is the
// only signal separating the newer process: a node that started inside the
// conflict window reports a fatal error (once) and its process exits; an older
// node keeps the name. Two claimants that both started inside the window both
// fail, which is loud and safe (Decision 4).
func (d *delegate) NotifyConflict(existing, other *memberlist.Node) {
	if existing.Name != d.o.Name {
		d.logf("node name %s is claimed by both %s and %s", existing.Name, existing.Address(), other.Address())
		return
	}
	window := d.o.ConflictWindow
	if window <= 0 {
		window = defaultConflictWindow
	}
	if d.now().Sub(d.started) < window {
		d.mu.Lock()
		first := !d.fatalSent
		d.fatalSent = true
		d.mu.Unlock()
		if first {
			err := fmt.Errorf("duplicate node name %q: %s already holds it", existing.Name, other.Address())
			d.logf("%v", err)
			select {
			case d.fatalCh <- err:
			default:
			}
		}
		return
	}
	d.logf("duplicate node name %q is also claimed by %s; keeping it, the newer process must stop", existing.Name, other.Address())
}

func (d *delegate) NotifyJoin(n *memberlist.Node) {
	m := d.table.join(n)
	d.logf("member %s (%s) joined", n.Name, n.Address())
	d.send(MemberEvent{Kind: EventJoin, Member: m})
}

func (d *delegate) NotifyUpdate(n *memberlist.Node) {
	m := d.table.update(n)
	d.logf("member %s (%s) updated", n.Name, n.Address())
	d.send(MemberEvent{Kind: EventUpdate, Member: m})
}

func (d *delegate) NotifyLeave(n *memberlist.Node) {
	m := d.table.depart(n.Name)
	if m.State == meshapi.MemberLeft {
		d.logf("member %s (%s) left", n.Name, n.Address())
		d.send(MemberEvent{Kind: EventLeave, Member: m})
		return
	}
	d.logf("member %s (%s) failed", n.Name, n.Address())
	d.send(MemberEvent{Kind: EventFail, Member: m})
}

// send enqueues an event without ever blocking memberlist; a full queue drops
// the event and says so once.
func (d *delegate) send(ev MemberEvent) {
	select {
	case d.eventCh <- ev:
	default:
		d.mu.Lock()
		first := !d.droppedLogged
		d.droppedLogged = true
		d.mu.Unlock()
		if first {
			d.logf("member events dropped: nobody is reading them")
		}
	}
}
