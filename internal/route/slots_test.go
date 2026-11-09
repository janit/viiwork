package route

import "testing"

func TestLocalLeaseClaimsAndFreesASlot(t *testing.T) {
	f := newFixture()
	b := newFakeBackend("m/0", 2)
	b.slotField = "id_slot"
	f.local.add("m", b)

	l1 := f.pick(t, Request{SessionKey: 7})
	c1, ok := l1.Slot()
	if !ok || c1.Field != "id_slot" {
		t.Fatalf("slot %+v %v", c1, ok)
	}
	l2 := f.pick(t, Request{})
	c2, _ := l2.Slot()
	if c1.Index == c2.Index {
		t.Fatal("two leases share a slot")
	}
	if got := b.claims; len(got) != 2 || got[0] != 7 || got[1] != 0 {
		t.Fatalf("claims carried sessions %v, want [7 0]", got)
	}
	l1.Release()
	l1.Release() // once only
	b.mu.Lock()
	_, busy := b.slotBusy[c1.Index]
	n := len(b.slotBusy)
	b.mu.Unlock()
	if busy || n != 1 {
		t.Fatalf("after one release: %d slots busy, released one busy %v", n, busy)
	}
	l2.Release()
}

func TestLeaseWithoutSlotAddressing(t *testing.T) {
	f := newFixture()
	f.local.add("m", newFakeBackend("m/0", 1))
	l := f.pick(t, Request{SessionKey: 7})
	if _, ok := l.Slot(); ok {
		t.Fatal("a backend that does not address slots gave a slot")
	}
	l.Release()
	f.reports.set(report("peer", t0, 0, capM(4, 0, 1)))
	f.local.models["m"][0].set(0) // starting: not eligible
	p := f.pick(t, Request{})
	if _, ok := p.Slot(); ok || p.Target().Local {
		t.Fatal("a peer lease has no slot")
	}
	p.Release()
}
