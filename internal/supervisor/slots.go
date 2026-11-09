package supervisor

import (
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// slotSessionIdle is how long a session's slot is kept for it: after that,
// a request without a slot of its own may take it before the least recently
// used slot is. A short-lived session must not pin a slot for good.
const slotSessionIdle = 10 * time.Minute

// SlotClaim is one request's slot on a slot-addressed backend
// (engine.SlotAddressed): the node names it in the request body, so it knows
// which slot holds which session.
type SlotClaim struct {
	// Field is the request body's field for the slot index.
	Field string
	Index int
	// Held: the slot held this session's conversation when it was claimed
	// (nothing else has run there since the session's last request).
	Held bool
	// Displaced is the session whose conversation the slot held when it
	// was claimed for another, 0 for none: kv_share saves it before the
	// request overwrites it, if its save is still waiting.
	Displaced uint64
	gen       uint64 // the launch it was claimed in
}

// slotTable is the node's record of a slot-addressed backend's slots. The
// node is the backend's only client, so the record is exact: a slot it does
// not name is not busy.
type slotTable struct {
	mu    sync.Mutex
	field string
	gen   uint64
	slots []slotState
}

type slotState struct {
	busy    bool
	used    bool      // has run a request since the launch
	session uint64    // the session whose conversation it holds, 0 for none
	last    time.Time // when its last request started or ended
}

func newSlotTable(field string, n int, gen uint64) *slotTable {
	return &slotTable{field: field, gen: gen, slots: make([]slotState, max(n, 1))}
}

// reset empties the table for a new launch; claims from before it free
// nothing.
func (t *slotTable) reset(field string, n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.field, t.gen, t.slots = field, t.gen+1, make([]slotState, max(n, 1))
}

// claim takes a free slot for a request of session (0 for none), in order:
// the slot holding the session's conversation; an empty slot; the least
// recently used slot holding no session's conversation, or an idle
// session's; the least recently used slot. ok is false when every slot is
// busy.
func (t *slotTable) claim(session uint64, now time.Time) (SlotClaim, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pick := -1
	if session != 0 {
		// The most recent, when concurrent requests of the session left its
		// conversation in two slots: the older may be a shorter branch.
		for i, s := range t.slots {
			if !s.busy && s.used && s.session == session && (pick < 0 || s.last.After(t.slots[pick].last)) {
				pick = i
			}
		}
	}
	if pick < 0 {
		for i, s := range t.slots {
			if !s.busy && !s.used {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		pick = t.lruLocked(func(s slotState) bool { return s.session == 0 || now.Sub(s.last) >= slotSessionIdle })
	}
	if pick < 0 {
		pick = t.lruLocked(func(slotState) bool { return true })
	}
	if pick < 0 {
		return SlotClaim{}, false
	}
	prev := t.slots[pick]
	held := session != 0 && prev.used && prev.session == session
	var displaced uint64
	if prev.used && prev.session != 0 && prev.session != session {
		displaced = prev.session
	}
	t.slots[pick] = slotState{busy: true, used: true, session: session, last: now}
	return SlotClaim{Field: t.field, Index: pick, Held: held, Displaced: displaced, gen: t.gen}, true
}

// holds reports whether c's slot, in c's launch, is idle and holds
// session's conversation.
func (t *slotTable) holds(c SlotClaim, session uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.gen != t.gen || c.Index < 0 || c.Index >= len(t.slots) {
		return false
	}
	s := t.slots[c.Index]
	return session != 0 && s.used && !s.busy && s.session == session
}

func (t *slotTable) lruLocked(ok func(slotState) bool) int {
	pick := -1
	for i, s := range t.slots {
		if !s.busy && ok(s) && (pick < 0 || s.last.Before(t.slots[pick].last)) {
			pick = i
		}
	}
	return pick
}

func (t *slotTable) free(c SlotClaim, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c.gen != t.gen || c.Index < 0 || c.Index >= len(t.slots) {
		return
	}
	t.slots[c.Index].busy = false
	t.slots[c.Index].last = now
}

// ClaimSlot takes a slot for one request of session (0 for none) on a
// slot-addressed backend; ok is false for any other backend, and when every
// slot is busy (which the router's in-flight count keeps from happening).
// Pair it with FreeSlot.
func (b *Backend) ClaimSlot(session uint64) (SlotClaim, bool) {
	t := b.slots.Load()
	if t == nil {
		return SlotClaim{}, false
	}
	return t.claim(session, b.deps.Now())
}

// FreeSlot returns a ClaimSlot claim. A claim from an earlier launch frees
// nothing.
func (b *Backend) FreeSlot(c SlotClaim) {
	if t := b.slots.Load(); t != nil {
		t.free(c, b.deps.Now())
	}
}

// SlotHolds reports whether the slot of claim c is idle and still holds
// session's conversation: nothing else has run there, and the backend has
// not relaunched. A save of a session's slot checks it first.
func (b *Backend) SlotHolds(c SlotClaim, session uint64) bool {
	t := b.slots.Load()
	return t != nil && t.holds(c, session)
}

// resetSlots starts the slot record of a new launch: a table for a backend
// its engine addresses by slot, none otherwise.
func (b *Backend) resetSlots(spec engine.Spec) {
	field := engine.SlotFieldOf(b.eng, spec)
	switch t := b.slots.Load(); {
	case field == "":
		b.slots.Store(nil)
	case t == nil:
		b.slots.Store(newSlotTable(field, spec.Parallel, 1))
	default:
		t.reset(field, spec.Parallel)
	}
}
