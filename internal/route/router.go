package route

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

var (
	ErrModelNotFound  = errors.New("route: model not found")
	ErrHostNotServing = errors.New("route: host does not serve the model")
	ErrNoFreeSlot     = errors.New("route: no free slot")
	// ErrNoImageInput: the request carries an image and every host that
	// serves the model, by fresh evidence, says it does not read images.
	ErrNoImageInput = errors.New("route: no host reads images with the model")
)

type Config struct {
	Self         string
	Local        LocalModels
	Remote       Reports
	StaleAfter   time.Duration
	QueueMax     int
	QueueTimeout time.Duration
	Now          func() time.Time
	// Performance enables the scored choice (routing.performance).
	Performance bool
	// LocalScore is this node's own score for a model; nil = none.
	LocalScore func(model string) (overheadMs, msPer1k int, ok bool)
	// LocalImages is whether this node reads images with a model; known is
	// false when it cannot say. nil = it never can.
	LocalImages func(model string) (reads, known bool)
	// LocalKVShare is this node's kv_share group for a model and its measured
	// import cost per 1000 tokens (0 = not measured); ok is false when the
	// model shares nothing here. nil = never.
	LocalKVShare func(model string) (group string, importMsPer1k int, ok bool)
	// Rand returns a float in [0,1); nil = math/rand/v2. Tests inject it.
	Rand func() float64
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
	Model string
	Host  string // ?host= pin, "" = none
	// Prefer is the ?prefer= list: node names in order of preference. The
	// first one with a free slot takes the request, ahead of scores and
	// sessions; with none free the request routes as if the list were
	// absent. Compared against known names only, like the pin, and ignored
	// with a pin or on a forward.
	Prefer      []string
	Exclude     map[string]bool // Target.Key() values already tried
	Forwarded   bool
	QueueBudget time.Duration // how long Acquire may queue; 0 = QueueTimeout, < 0 = do not queue (Decision 17)
	// EstK is the prompt's estimated size in thousands of tokens, for the
	// scored choice's prediction. The entry cannot see any cache, so it
	// assumes every token is uncached.
	EstK float64
	// SessionKey identifies the client session (a hash of its affinity
	// header), 0 = none. Within the scored band it sends every turn of a
	// session to the same host, so the turns reuse that host's KV cache.
	SessionKey uint64
	// Images says the request carries an image. It then goes only to hosts
	// that say they read images; when no host says so, to hosts that cannot
	// say; and when every host says no, nowhere (ErrNoImageInput). A forward
	// ignores it: the origin chose this node.
	Images bool
	// imgWant is the class of host this pick may use, set by the pick itself
	// from Images and the evidence at hand. imgAny is no filter.
	imgWant imageClass
}

type resKey struct{ node, model string }

type reservation struct{ start time.Time }

// Lease is one slot taken for one request. Release it exactly once when the
// request ends; further calls do nothing.
type Lease struct {
	r       *Router
	target  Target
	backend LocalBackend
	slot    supervisor.SlotClaim
	hasSlot bool
	key     resKey
	res     *reservation
	once    sync.Once
}

func (l *Lease) Target() Target { return l.target }

// Backend is the local backend, nil for a peer lease.
func (l *Lease) Backend() LocalBackend { return l.backend }

// Slot is the slot a local lease holds on a slot-addressed backend, which
// the request must name in its body; ok is false for any other lease.
func (l *Lease) Slot() (supervisor.SlotClaim, bool) { return l.slot, l.hasSlot }

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
			if l.hasSlot {
				l.backend.(SlotClaimer).FreeSlot(l.slot)
			}
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
	lastHosts    lastHosts // where each session's last turn went (warm.go)
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
	var reports []capacity.Report
	if !req.Forwarded {
		reports = r.c.Remote.Reports()
	}
	return r.pickWithReportsLocked(req, reports)
}

// pickWithReportsLocked is pickLocked against reports already fetched (nil
// for a forwarded request), so the queue's dispatch fetches them once per
// call rather than once per waiter.
func (r *Router) pickWithReportsLocked(req Request, reports []capacity.Report) (l *Lease, err error) {
	defer func() { r.noteSessionLocked(req, l, reports) }()
	backends, admitting, localOK := r.c.Local.Backends(req.Model)

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

	localElig := localOK && admitting
	if req.Images && !req.Forwarded {
		want, err := r.imageClassLocked(req, localOK && anyHealthy(backends), reports)
		if err != nil {
			return nil, err
		}
		req.imgWant = want
		localElig = localElig && r.localImageClass(req.Model) == want
	}

	if len(req.Prefer) > 0 && !req.Forwarded && req.Host == "" {
		if l := r.pickPreferredLocked(req, backends, localElig, reports); l != nil {
			return l, nil
		}
	}
	if r.c.Performance && !req.Forwarded && req.Host == "" {
		if l, handled := r.pickScoredLocked(req, backends, localElig, reports); handled {
			if l != nil {
				return l, nil
			}
			return nil, ErrNoFreeSlot
		}
		// No score anywhere: a session still follows its host, the same
		// rendezvous as the scored pick, over every host with a free slot.
		if req.SessionKey != 0 {
			if l := r.pickSessionLocked(req, backends, localElig, reports); l != nil {
				return l, nil
			}
		}
	}
	if localElig && (req.Host == "" || req.Host == r.c.Self) {
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

// pickPreferredLocked takes a slot on the first host of req.Prefer that has
// one, this node included when it is named. A name that is unknown, stale,
// full, refused or excluded is passed over, which is the difference from the
// pin: a preference never fails a request. Nil when none of them has room.
func (r *Router) pickPreferredLocked(req Request, backends []LocalBackend, localEligible bool, reports []capacity.Report) *Lease {
	now := r.c.Now()
	for _, name := range req.Prefer {
		if name == r.c.Self {
			if localEligible {
				if l := r.pickLocalLocked(req, backends); l != nil {
					logPreferredPick(req, name)
					return l
				}
			}
			continue
		}
		for _, rep := range reports {
			if rep.Node != name {
				continue
			}
			if _, free, _ := r.peerFreeLocked(req, rep, now); free > 0 {
				logPreferredPick(req, name)
				return r.peerLeaseLocked(rep, req.Model, now)
			}
			break
		}
	}
	return nil
}

func logPreferredPick(req Request, name string) {
	if logging.DebugEnabled() {
		log.Printf("[debug] route %s: * %s [prefer %v]", req.Model, name, req.Prefer)
	}
}

func (r *Router) pickLocalLocked(req Request, backends []LocalBackend) *Lease {
	var tied []LocalBackend
	best := 0
	var home LocalBackend
	var top uint64
	for _, b := range backends {
		if b.State() != supervisor.StateHealthy || req.Exclude["local:"+b.ID()] {
			continue
		}
		free := b.Slots() - b.InFlight()
		// A session keeps its backend, and so its KV cache, while that
		// backend has a free slot: the same rendezvous as across hosts, over
		// backend IDs. Without one, the most-free pick below.
		if req.SessionKey != 0 && free > 0 {
			if h := rendezvous(req.SessionKey, b.ID()); home == nil || h > top {
				home, top = b, h
			}
		}
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
	b := home
	if b == nil {
		n := r.localRR[req.Model]
		r.localRR[req.Model] = n + 1
		b = tied[n%len(tied)]
	}
	b.Acquire()
	l := &Lease{r: r, backend: b, target: Target{Local: true, Node: r.c.Self, BackendID: b.ID(), Addr: b.Addr()}}
	if sc, ok := b.(SlotClaimer); ok {
		l.slot, l.hasSlot = sc.ClaimSlot(req.SessionKey)
	}
	return l
}

// pickSessionLocked sends a session to the host with the highest rendezvous
// score among those with a free slot, this node included, before any score
// exists. Until v2.7.0 such a request went local-first, and on a fresh start
// or after a flag change (which drops every score) a session's turns crossed
// hosts until the scores came back: 22% of turns in the first five minutes on
// gb2 + gb3. Nil when no host has room.
func (r *Router) pickSessionLocked(req Request, backends []LocalBackend, localEligible bool, reports []capacity.Report) *Lease {
	now := r.c.Now()
	// A session whose state is known to be on a host goes there first: the
	// host of its last turn, else that host's replica target (warm.go).
	if w := r.warmLocked(req, anyHealthy(backends), reports, now); w.ok {
		for _, name := range []string{w.last, w.replica} {
			if name == "" {
				continue
			}
			if name == r.c.Self {
				if localEligible && localHasFree(req, backends) {
					return r.pickLocalLocked(req, backends)
				}
				continue
			}
			for _, rep := range reports {
				if rep.Node == name {
					if _, free, serves := r.peerFreeLocked(req, rep, now); serves && free > 0 {
						return r.peerLeaseLocked(rep, req.Model, now)
					}
					break
				}
			}
		}
	}
	var top uint64
	var pick *capacity.Report
	local := false
	if localEligible && localHasFree(req, backends) {
		local, top = true, rendezvous(req.SessionKey, r.c.Self)
	}
	for i, rep := range reports {
		if _, free, serves := r.peerFreeLocked(req, rep, now); !serves || free <= 0 {
			continue
		}
		if h := rendezvous(req.SessionKey, rep.Node); (!local && pick == nil) || h > top {
			pick, top, local = &reports[i], h, false
		}
	}
	switch {
	case local:
		return r.pickLocalLocked(req, backends)
	case pick != nil:
		return r.peerLeaseLocked(*pick, req.Model, now)
	}
	return nil
}

func (r *Router) pickPeerLocked(req Request, reports []capacity.Report) *Lease {
	now := r.c.Now()
	var tied []capacity.Report
	best, bestRTT := 0, time.Duration(0)
	for _, rep := range reports {
		_, free, serves := r.peerFreeLocked(req, rep, now)
		switch {
		case !serves || free <= 0 || free < best:
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
	return r.peerLeaseLocked(rep, req.Model, now)
}

// peerFreeLocked is the one rule for a peer, shared by the most-free and the
// scored pick. serves reports that rep is another node's fresh report listing
// the model with a healthy backend: such a peer's score counts toward the
// fleet median even when it has no room for this request. free is its free
// slots for this request: 0 when the request excludes it or pins another
// host, and 0 while a refusal is not older than the report's AsOf (a report
// requested after the refusal clears the mark); otherwise slots - busy - this
// router's forwards still in flight that started after the report's AsOf.
func (r *Router) peerFreeLocked(req Request, rep capacity.Report, now time.Time) (mc meshapi.ModelCapacity, free int, serves bool) {
	if rep.Node == r.c.Self || !capacity.Fresh(rep, now, r.c.StaleAfter) {
		return mc, 0, false
	}
	mc, has := rep.Model(req.Model)
	if !has || mc.HealthyBackends <= 0 {
		return mc, 0, false
	}
	if (req.Host != "" && rep.Node != req.Host) || req.Exclude["peer:"+rep.Node] {
		return mc, 0, true
	}
	if req.imgWant != imgAny && classOf(mc.Images) != req.imgWant {
		return mc, 0, true
	}
	key := resKey{rep.Node, req.Model}
	if at, ok := r.refused[key]; ok {
		// Only a report requested after the refusal can show it lifted:
		// one received after it may have been built before it.
		if !rep.AsOf().After(at) {
			return mc, 0, true
		}
		delete(r.refused, key)
	}
	return mc, mc.Slots - mc.Busy - r.reservedLocked(key, rep.AsOf()), true
}

// peerLeaseLocked reserves one slot on rep for model.
func (r *Router) peerLeaseLocked(rep capacity.Report, model string, now time.Time) *Lease {
	key := resKey{rep.Node, model}
	res := &reservation{start: now}
	r.reservations[key] = append(r.reservations[key], res)
	return &Lease{r: r, key: key, res: res, target: Target{Node: rep.Node, Addr: rep.APIAddr}}
}

// reservedLocked counts this router's forwards to (node, model) that are
// still in flight and started after asOf, the moment the report certainly
// reflects: an older one is already in the report's busy count, and one that
// started while the poll was in flight may not be.
func (r *Router) reservedLocked(key resKey, asOf time.Time) int {
	n := 0
	for _, res := range r.reservations[key] {
		if res.start.After(asOf) {
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
