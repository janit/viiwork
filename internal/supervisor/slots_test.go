package supervisor

import (
	"testing"
	"time"
)

func claimOK(t *testing.T, tab *slotTable, session uint64, now time.Time) SlotClaim {
	t.Helper()
	c, ok := tab.claim(session, now)
	if !ok {
		t.Fatalf("no slot for session %d", session)
	}
	if c.Field != "id_slot" {
		t.Fatalf("field %q", c.Field)
	}
	return c
}

func TestSlotTableSessionKeepsItsSlot(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 3, 1)
	a := claimOK(t, tab, 11, t0)
	b := claimOK(t, tab, 22, t0)
	if a.Index == b.Index {
		t.Fatal("two busy claims share a slot")
	}
	tab.free(a, t0.Add(time.Second))
	tab.free(b, t0.Add(time.Second))
	// Session 22 comes back: its own slot, although an empty one is free.
	if c := claimOK(t, tab, 22, t0.Add(2*time.Second)); c.Index != b.Index {
		t.Fatalf("session 22 got slot %d, held %d", c.Index, b.Index)
	}
}

func TestSlotTableOrder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 3, 1)
	s0 := claimOK(t, tab, 1, t0)
	s1 := claimOK(t, tab, 2, t0)
	tab.free(s0, t0.Add(1*time.Second))
	tab.free(s1, t0.Add(2*time.Second))

	// A new session takes the empty slot before either session's.
	empty := claimOK(t, tab, 3, t0.Add(3*time.Second))
	if empty.Index == s0.Index || empty.Index == s1.Index {
		t.Fatalf("new session took slot %d, which a session holds", empty.Index)
	}
	tab.free(empty, t0.Add(4*time.Second))

	// No empty slot left, all sessions recent: the least recently used.
	if c := claimOK(t, tab, 4, t0.Add(5*time.Second)); c.Index != s0.Index {
		t.Fatalf("took slot %d, want the least recently used %d", c.Index, s0.Index)
	} else {
		tab.free(c, t0.Add(6*time.Second))
	}

	// A slot whose session has been idle past slotSessionIdle comes before
	// the least recently used slot of a session still active.
	tab2 := newSlotTable("id_slot", 2, 1)
	recent := claimOK(t, tab2, 7, t0)
	idle := claimOK(t, tab2, 8, t0)
	tab2.free(idle, t0)                  // session 8 last ran at t0
	tab2.free(recent, t0.Add(time.Hour)) // session 7 ran until an hour later
	now := t0.Add(time.Hour + time.Minute)
	if c := claimOK(t, tab2, 9, now); c.Index != idle.Index {
		t.Fatalf("took slot %d, want the idle session's %d", c.Index, idle.Index)
	}
}

func TestSlotTableSessionlessSlotGoesFirst(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 2, 1)
	a := claimOK(t, tab, 1, t0) // session
	b := claimOK(t, tab, 0, t0) // no session
	tab.free(a, t0.Add(2*time.Second))
	tab.free(b, t0.Add(1*time.Second))
	// A slot holding no session's conversation is reused before a recent
	// session's, whichever of the two was used last.
	if c := claimOK(t, tab, 5, t0.Add(3*time.Second)); c.Index != b.Index {
		t.Fatalf("took %d, want the sessionless slot %d", c.Index, b.Index)
	} else {
		tab.free(c, t0.Add(4*time.Second))
	}
	tab3 := newSlotTable("id_slot", 2, 1)
	x := claimOK(t, tab3, 1, t0)
	y := claimOK(t, tab3, 0, t0)
	tab3.free(x, t0.Add(1*time.Second)) // session slot is the LRU one
	tab3.free(y, t0.Add(2*time.Second))
	if c := claimOK(t, tab3, 5, t0.Add(3*time.Second)); c.Index != y.Index {
		t.Fatalf("took %d, want the sessionless slot %d over a recent session's", c.Index, y.Index)
	}
}

func TestSlotTableFullAndStaleFrees(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 1, 1)
	c := claimOK(t, tab, 1, t0)
	if _, ok := tab.claim(2, t0); ok {
		t.Fatal("claimed a slot of a full backend")
	}
	// A relaunch resets the table; a claim from before it frees nothing.
	tab.reset("id_slot", 1)
	d := claimOK(t, tab, 2, t0)
	tab.free(c, t0)
	if _, ok := tab.claim(3, t0); ok {
		t.Fatal("a claim from the previous launch freed the new launch's slot")
	}
	tab.free(d, t0)
	claimOK(t, tab, 3, t0)
}

func TestBackendClaimSlotOnlyWhenAddressed(t *testing.T) {
	b := &Backend{deps: Deps{}.withDefaults()}
	if _, ok := b.ClaimSlot(1); ok {
		t.Fatal("a backend whose engine does not address slots claimed one")
	}
	b.FreeSlot(SlotClaim{}) // must not panic
	b.slots.Store(newSlotTable("id_slot", 2, 1))
	c, ok := b.ClaimSlot(1)
	if !ok || c.Field != "id_slot" {
		t.Fatalf("claim %+v %v", c, ok)
	}
	b.FreeSlot(c)
}

func TestSlotTableHeldAndHolds(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 2, 1)
	first := claimOK(t, tab, 5, t0)
	if first.Held {
		t.Fatal("a first claim held the session")
	}
	if tab.holds(first, 5) {
		t.Fatal("a busy slot holds its session for a save")
	}
	tab.free(first, t0)
	if !tab.holds(first, 5) || tab.holds(first, 6) || tab.holds(first, 0) {
		t.Fatal("holds after the turn")
	}
	again := claimOK(t, tab, 5, t0)
	if !again.Held || again.Index != first.Index {
		t.Fatalf("the session's next claim %+v", again)
	}
	tab.free(again, t0)
	// Another session takes the slot: it no longer holds 5.
	tab.slots[1-first.Index].busy = true
	other := claimOK(t, tab, 6, t0.Add(time.Hour))
	if other.Index != first.Index || other.Held {
		t.Fatalf("claim %+v", other)
	}
	tab.free(other, t0.Add(time.Hour))
	if tab.holds(first, 5) {
		t.Fatal("the slot still holds a session another one replaced")
	}
	tab.reset("id_slot", 2)
	if tab.holds(other, 6) {
		t.Fatal("a slot holds its session across a relaunch")
	}
}

// Two concurrent requests of one session leave its conversation in two
// slots: the next turn takes the one that ran last.
func TestSlotTableSessionTakesItsLatestSlot(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 3, 1)
	a := claimOK(t, tab, 7, t0)
	b := claimOK(t, tab, 7, t0)
	tab.free(b, t0.Add(time.Second))
	tab.free(a, t0.Add(5*time.Second)) // slot a's request ended last
	if c := claimOK(t, tab, 7, t0.Add(6*time.Second)); c.Index != a.Index || !c.Held {
		t.Fatalf("got slot %d (held %v), want %d", c.Index, c.Held, a.Index)
	}
}

func TestSlotTableDisplaced(t *testing.T) {
	t0 := time.Unix(1000, 0)
	tab := newSlotTable("id_slot", 1, 1)
	a := claimOK(t, tab, 11, t0)
	if a.Displaced != 0 {
		t.Fatalf("an empty slot displaced %d", a.Displaced)
	}
	tab.free(a, t0.Add(time.Second))
	if c := claimOK(t, tab, 11, t0.Add(2*time.Second)); c.Displaced != 0 || !c.Held {
		t.Fatalf("the session's own slot: %+v", c)
	} else {
		tab.free(c, t0.Add(3*time.Second))
	}
	c := claimOK(t, tab, 22, t0.Add(4*time.Second))
	if c.Displaced != 11 || c.Held {
		t.Fatalf("another session's slot: %+v", c)
	}
	tab.free(c, t0.Add(5*time.Second))
	if c := claimOK(t, tab, 0, t0.Add(6*time.Second)); c.Displaced != 22 {
		t.Fatalf("a sessionless request: %+v", c)
	} else {
		tab.free(c, t0.Add(7*time.Second))
	}
	if c := claimOK(t, tab, 33, t0.Add(8*time.Second)); c.Displaced != 0 {
		t.Fatalf("a slot that held no session: %+v", c)
	}
}
