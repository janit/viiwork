package route

import (
	"container/list"
	"time"

	"github.com/janit/viiwork/v2/mesh/capacity"
)

// Warm pricing (kv_share, handover step 7). A session's state lives on two
// hosts at most: the one that ran its last turn, in that engine's cache, and
// that host's replica target, the first member of the model's kv_share group
// after it in SessionRank. Pricing either as cold makes the router send a
// spilled turn to whichever host is fastest at a full prefill, while the
// replica host would resume it in seconds (teddy, 50k tokens: 4.5 s warm,
// 42 s cold).
//
// The entry node knows the last host only for turns it routed itself: it
// remembers it per (model, session) after every pick. A first turn, a session
// that entered elsewhere, and a model without a group are priced as before.
// A memory that is wrong (the engine dropped the conversation, a push failed)
// costs a cold prefill on a host the router believed warm, which is what
// every spill cost before kv_share.

// sessionMemory bounds the remembered sessions; the least recently routed go
// first.
const sessionMemory = 4096

// defaultImportMsPer1k prices an import when no member has measured one:
// teddy's RTX A4000s, 2.5 s for a 50k-token replica, assembly included.
const defaultImportMsPer1k = 50

type sessionRef struct {
	model string
	key   uint64
}

type lastHosts struct {
	byRef map[sessionRef]*list.Element
	order *list.List // front = most recent; values are *lastHost
}

type lastHost struct {
	ref  sessionRef
	node string
}

func (l *lastHosts) get(ref sessionRef) (string, bool) {
	if l.byRef == nil {
		return "", false
	}
	e, ok := l.byRef[ref]
	if !ok {
		return "", false
	}
	return e.Value.(*lastHost).node, true
}

func (l *lastHosts) set(ref sessionRef, node string) {
	if l.byRef == nil {
		l.byRef, l.order = map[sessionRef]*list.Element{}, list.New()
	}
	if e, ok := l.byRef[ref]; ok {
		e.Value.(*lastHost).node = node
		l.order.MoveToFront(e)
		return
	}
	l.byRef[ref] = l.order.PushFront(&lastHost{ref, node})
	if l.order.Len() > sessionMemory {
		old := l.order.Back()
		l.order.Remove(old)
		delete(l.byRef, old.Value.(*lastHost).ref)
	}
}

// noteSessionLocked remembers where a session's turn was sent, for a model
// that some host shares sessions of: the memory is bounded, and the turns of
// models without kv_share must not push those of kv_share models out of it.
func (r *Router) noteSessionLocked(req Request, l *Lease, reports []capacity.Report) {
	if req.SessionKey == 0 || l == nil || req.Forwarded || !r.sharedLocked(req.Model, reports) {
		return
	}
	node := l.target.Node
	if l.target.Local {
		node = r.c.Self
	}
	r.lastHosts.set(sessionRef{req.Model, req.SessionKey}, node)
}

// sharedLocked reports whether any host, this one included, publishes a
// kv_share group for model.
func (r *Router) sharedLocked(model string, reports []capacity.Report) bool {
	if r.c.LocalKVShare != nil {
		if g, _, ok := r.c.LocalKVShare(model); ok && g != "" {
			return true
		}
	}
	for _, rep := range reports {
		if mc, has := rep.Model(model); has && mc.KVShareGroup != "" {
			return true
		}
	}
	return false
}

// warmPair is where a session's state is: last ran the previous turn, and
// replica received it. importMsPer1k prices restoring the replica. ok is
// false when nothing is known to be warm.
type warmPair struct {
	last, replica string
	importMsPer1k float64
	ok            bool
}

// warmLocked finds the session's warm pair among the members of the kv_share
// group its last host belongs to, as they stand now: fresh, serving the
// model with a healthy backend.
func (r *Router) warmLocked(req Request, localHealthy bool, reports []capacity.Report, now time.Time) warmPair {
	if req.SessionKey == 0 || req.Forwarded {
		return warmPair{}
	}
	last, ok := r.lastHosts.get(sessionRef{req.Model, req.SessionKey})
	if !ok {
		return warmPair{}
	}
	type member struct {
		group string
		cost  int
	}
	members := map[string]member{}
	if r.c.LocalKVShare != nil && localHealthy {
		if g, imp, ok := r.c.LocalKVShare(req.Model); ok && g != "" {
			members[r.c.Self] = member{g, imp}
		}
	}
	for _, rep := range reports {
		if rep.Node == r.c.Self || !capacity.Fresh(rep, now, r.c.StaleAfter) {
			continue
		}
		if mc, has := rep.Model(req.Model); has && mc.HealthyBackends > 0 && mc.KVShareGroup != "" && textModel(mc) {
			members[rep.Node] = member{mc.KVShareGroup, mc.KVShareImportMsPer1k}
		}
	}
	home, ok := members[last]
	if !ok {
		return warmPair{}
	}
	var names []string
	var costs []float64
	for name, m := range members {
		if m.group != home.group {
			continue
		}
		names = append(names, name)
		if m.cost > 0 {
			costs = append(costs, float64(m.cost))
		}
	}
	w := warmPair{last: last, ok: true}
	for _, n := range SessionRank(req.SessionKey, names) {
		if n != last {
			w.replica = n
			break
		}
	}
	// The replica host's own measurement, else the group's median, else the
	// default: an import is a disk read and a copy into the engine, alike
	// enough across a group that runs one model with one configuration.
	switch {
	case w.replica != "" && members[w.replica].cost > 0:
		w.importMsPer1k = float64(members[w.replica].cost)
	case len(costs) > 0:
		w.importMsPer1k = median(costs)
	default:
		w.importMsPer1k = defaultImportMsPer1k
	}
	return w
}

// warmPred is a candidate's prediction when it holds the session's state:
// the last host reads only the new turn, which the entry cannot size, so its
// overhead alone; the replica host imports first. cold is returned for any
// other host, and when warm would not be cheaper.
func (w warmPair) pred(name string, ov, estK, rttMs, cold float64) float64 {
	if !w.ok {
		return cold
	}
	switch name {
	case w.last:
		return min(cold, max(ov+rttMs, 1))
	case w.replica:
		return min(cold, max(ov+w.importMsPer1k*estK+rttMs, 1))
	}
	return cold
}
