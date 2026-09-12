package route

import (
	"errors"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
)

var (
	ErrModelNotFound  = errors.New("route: model not found")
	ErrHostNotServing = errors.New("route: host does not serve the model")
	ErrNoFreeSlot     = errors.New("route: no free slot")
)

type Config struct {
	Self         string
	Local        LocalModels
	Remote       Reports
	StaleAfter   time.Duration
	QueueMax     int
	QueueTimeout time.Duration
	Now          func() time.Time
}

// Target is where a lease runs its request.
type Target struct {
	Local     bool
	Node      string // this node's name for a local target
	BackendID string // local targets only
	Addr      string // backend loopback address, or the member's API address
}

// Key identifies a route for retry exclusions.
func (t Target) Key() string {
	if t.Local {
		return "local:" + t.BackendID
	}
	return "peer:" + t.Node
}

type Request struct {
	Model       string
	Host        string          // ?host= pin, "" = none
	Exclude     map[string]bool // Target.Key() values already tried
	Forwarded   bool
	QueueBudget time.Duration // how long Acquire may queue; 0 = QueueTimeout, < 0 = do not queue (Decision 17)
}

type resKey struct{ node, model string }

type reservation struct{ start time.Time }

// Lease is one slot taken for one request. Release it exactly once when the
// request ends; further calls do nothing.
type Lease struct {
	r       *Router
	target  Target
	backend LocalBackend
	key     resKey
	res     *reservation
	once    sync.Once
}

func (l *Lease) Target() Target { return l.target }

// Backend is the local backend, nil for a peer lease.
func (l *Lease) Backend() LocalBackend { return l.backend }

// Refused records that the lease's peer refused the request, or could not be
// reached, before responding. Until a report received after this moment
// arrives, the router gives that peer no free slot for the model: the refusal
// is fresher evidence than any report taken before it, and without it the
// final acquisition and the queue would send the request straight back to the
// peer that just refused it (Decision 18). Call it before Release, whose wake
// could otherwise hand the same slot out again. A local lease ignores it: local
// occupancy is exact.
func (l *Lease) Refused() {
	if l.backend != nil {
		return
	}
	l.r.mu.Lock()
	l.r.refused[l.key] = l.r.c.Now()
	l.r.mu.Unlock()
}

func (l *Lease) Release() {
	l.once.Do(func() {
		if l.backend != nil {
			l.backend.Release()
		} else {
			l.r.mu.Lock()
			l.r.dropReservationLocked(l.key, l.res)
			l.r.mu.Unlock()
		}
		l.r.released()
	})
}

// Router picks slots. One mutex covers computing candidates and taking the
// lease, so two concurrent picks never count the same free slot.
type Router struct {
	c Config

	mu           sync.Mutex
	localRR      map[string]int
	peerRR       map[string]int
	reservations map[resKey][]*reservation
	refused      map[resKey]time.Time // a peer's last refusal of a model, until a newer report
	queues       map[string][]*waiter
}

func New(c Config) *Router {
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Router{
		c:            c,
		localRR:      map[string]int{},
		peerRR:       map[string]int{},
		reservations: map[resKey][]*reservation{},
		refused:      map[resKey]time.Time{},
		queues:       map[string][]*waiter{},
	}
}

// Pick takes a free slot for req now, or reports why there is none.
func (r *Router) Pick(req Request) (*Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pickLocked(req)
}

// pickLocked is Pick with the router mutex held; the queue's dispatch shares
// it, so nothing locks twice.
func (r *Router) pickLocked(req Request) (*Lease, error) {
	backends, admitting, localOK := r.c.Local.Backends(req.Model)
	var reports []capacity.Report
	if !req.Forwarded {
		reports = r.c.Remote.Reports()
	}

	// Existence: a model nobody configures or reports is a 404, kept distinct
	// from "that model, not on that host" (v1.8's rule).
	existsRemote, pinListed := false, false
	for _, rep := range reports {
		if rep.Node == r.c.Self {
			continue
		}
		if _, has := rep.Model(req.Model); has {
			existsRemote = true
			if rep.Node == req.Host {
				pinListed = true
			}
		}
	}
	if !localOK && !existsRemote {
		return nil, ErrModelNotFound
	}
	// The pin is compared against known names only; it is never dialled.
	if req.Host != "" {
		if req.Host == r.c.Self {
			if !localOK {
				return nil, ErrHostNotServing
			}
		} else if !pinListed {
			return nil, ErrHostNotServing
		}
	}

	if localOK && admitting && (req.Host == "" || req.Host == r.c.Self) {
		if l := r.pickLocalLocked(req, backends); l != nil {
			return l, nil
		}
	}
	if !req.Forwarded && req.Host != r.c.Self {
		if l := r.pickPeerLocked(req, reports); l != nil {
			return l, nil
		}
	}
	return nil, ErrNoFreeSlot
}

func (r *Router) pickLocalLocked(req Request, backends []LocalBackend) *Lease {
	var tied []LocalBackend
	best := 0
	for _, b := range backends {
		if b.State() != supervisor.StateHealthy || req.Exclude["local:"+b.ID()] {
			continue
		}
		free := b.Slots() - b.InFlight()
		switch {
		case free <= 0 || free < best:
		case free > best:
			best, tied = free, append(tied[:0], b)
		default:
			tied = append(tied, b)
		}
	}
	if len(tied) == 0 {
		return nil
	}
	n := r.localRR[req.Model]
	r.localRR[req.Model] = n + 1
	b := tied[n%len(tied)]
	b.Acquire()
	return &Lease{r: r, backend: b, target: Target{Local: true, Node: r.c.Self, BackendID: b.ID(), Addr: b.Addr()}}
}

func (r *Router) pickPeerLocked(req Request, reports []capacity.Report) *Lease {
	now := r.c.Now()
	var tied []capacity.Report
	best, bestRTT := 0, time.Duration(0)
	for _, rep := range reports {
		if rep.Node == r.c.Self || (req.Host != "" && rep.Node != req.Host) || req.Exclude["peer:"+rep.Node] {
			continue
		}
		if !capacity.Fresh(rep, now, r.c.StaleAfter) {
			continue
		}
		mc, has := rep.Model(req.Model)
		if !has || mc.HealthyBackends <= 0 {
			continue
		}
		key := resKey{rep.Node, req.Model}
		if at, ok := r.refused[key]; ok {
			if !rep.Received.After(at) {
				continue
			}
			delete(r.refused, key)
		}
		free := mc.Slots - mc.Busy - r.reservedLocked(key, rep.Received)
		switch {
		case free <= 0 || free < best:
		case free > best || rep.RTT < bestRTT:
			best, bestRTT, tied = free, rep.RTT, append(tied[:0], rep)
		case rep.RTT == bestRTT:
			tied = append(tied, rep)
		}
	}
	if len(tied) == 0 {
		return nil
	}
	n := r.peerRR[req.Model]
	r.peerRR[req.Model] = n + 1
	rep := tied[n%len(tied)]
	key := resKey{rep.Node, req.Model}
	res := &reservation{start: now}
	r.reservations[key] = append(r.reservations[key], res)
	return &Lease{r: r, key: key, res: res, target: Target{Node: rep.Node, Addr: rep.APIAddr}}
}

// reservedLocked counts this router's forwards to (node, model) that are
// still in flight and started after the report was received: an older one is
// already in the report's busy count.
func (r *Router) reservedLocked(key resKey, received time.Time) int {
	n := 0
	for _, res := range r.reservations[key] {
		if res.start.After(received) {
			n++
		}
	}
	return n
}

func (r *Router) dropReservationLocked(key resKey, res *reservation) {
	list := r.reservations[key]
	for i, x := range list {
		if x == res {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(r.reservations, key)
		return
	}
	r.reservations[key] = list
}
