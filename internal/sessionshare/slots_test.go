package sessionshare

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
	"github.com/janit/viiwork/v2/internal/supervisor"
)

// fakeSlotEngine is a SlotSharer: each slot holds a conversation, a save
// writes it, a restore records which file went into which slot.
type fakeSlotEngine struct {
	dir string

	mu       sync.Mutex
	slots    map[int]kvsharetest.Session
	saves    []int // slot of each save
	restores []restore
	delay    time.Duration // each save takes this long
	onSave   func()        // runs during each save
}

type restore struct{ slot, bytes int }

func (e *fakeSlotEngine) SessionShare(engine.Spec) (engine.SessionShareConfig, bool) {
	return engine.SessionShareConfig{}, false
}

func (e *fakeSlotEngine) SaveSlot(_ context.Context, _ string, slot int, name string) error {
	time.Sleep(e.delay)
	if e.onSave != nil {
		e.onSave()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.slots[slot]
	if !ok {
		return engine.ErrSessionGone
	}
	e.saves = append(e.saves, slot)
	return os.WriteFile(filepath.Join(e.dir, name), s.Sealed(), 0o600)
}

func (e *fakeSlotEngine) RestoreSlot(_ context.Context, _ string, slot int, name string) error {
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restores = append(e.restores, restore{slot, len(b)})
	return nil
}

func (e *fakeSlotEngine) counts() (saves, restores int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.saves), len(e.restores)
}

// slotBackend is a backend with the slot its request was given.
type slotBackend struct {
	fakeBackend
	claim supervisor.SlotClaim
	holds bool // what SlotHolds answers
	busy  atomic.Int32
}

func (b *slotBackend) InFlight() int { return int(b.busy.Load()) }

func (b *slotBackend) Slot() (supervisor.SlotClaim, bool)          { return b.claim, true }
func (b *slotBackend) SlotHolds(supervisor.SlotClaim, uint64) bool { return b.holds }

type slotNode struct {
	*node
	slots *fakeSlotEngine
}

func newSlotNode(t *testing.T, name string, model uint64) *slotNode {
	n := newNode(t, name)
	e := &fakeSlotEngine{dir: t.TempDir(), slots: map[int]kvsharetest.Session{}}
	cfg := engine.SessionShareConfig{Group: "g", Transport: "http", SaveDir: e.dir, MaxBytes: 1 << 30, MinTokens: 1000, MaxFileBytes: 1 << 30, Model: model, Config: 7}
	n.m.SetModels([]Model{{Name: testModel, Slots: e, Config: cfg}})
	return &slotNode{node: n, slots: e}
}

func TestSlotTurnIsSavedAndRestoredIntoTheGivenSlot(t *testing.T) {
	a, b := newSlotNode(t, "a", 0xabc), newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "g", 1, time.Now())
	b.sees(a.node, "g", 1, time.Now())
	ctx := context.Background()

	// A turn of session 42 ran in slot 1 of a's backend.
	turn1 := kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.slots.slots[1] = turn1
	ba := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}, holds: true}
	a.m.AfterLocal(testModel, "sess", 42, ba, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	if saves, _ := a.slots.counts(); saves != 1 || a.slots.saves[0] != 1 {
		t.Fatalf("saves %v, want slot 1 once", a.slots.saves)
	}
	md := b.m.model(testModel)
	rep, ok, _ := md.store.Info(storeKey(42))
	if !ok || rep.Model != 0xabc || rep.Config != 7 || rep.Tokens != 2000 {
		t.Fatalf("replica %+v", rep)
	}

	// The session spills to b, whose lease gave it slot 0, holding another
	// conversation: the replica goes there.
	bb := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}}
	b.m.BeforeLocal(ctx, testModel, "sess", 42, bb)
	if _, n := b.slots.counts(); n != 1 || b.slots.restores[0].slot != 0 || b.slots.restores[0].bytes != len(turn1.Sealed()) {
		t.Fatalf("restores %v", b.slots.restores)
	}
	// Its next turn on b finds its own slot: nothing to restore.
	bb.claim.Held = true
	b.m.BeforeLocal(ctx, testModel, "sess", 42, bb)
	if _, n := b.slots.counts(); n != 1 {
		t.Fatalf("restored into a slot that holds the session: %v", b.slots.restores)
	}
	// A turn on b, then a newer replica from a (the session went home and
	// came back): the replica is newer than the slot, so it is restored.
	b.m.AfterLocal(testModel, "sess", 42, bb, 2100)
	time.Sleep(5 * time.Millisecond)
	a.sees(b.node, "g", 1, time.Now()) // a report older than 3 s routes nothing
	a.slots.mu.Lock()
	a.slots.slots[1] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2600, StateSeed: 2}
	a.slots.mu.Unlock()
	a.m.AfterLocal(testModel, "sess", 42, ba, 2600)
	waitFor(t, "the second push", func() bool { return b.m.Stats(testModel).Puts >= 2 })
	b.m.BeforeLocal(ctx, testModel, "sess", 42, bb)
	if _, n := b.slots.counts(); n != 2 {
		t.Fatalf("a replica newer than the slot was not restored: %v", b.slots.restores)
	}
	// ... and only once.
	b.m.BeforeLocal(ctx, testModel, "sess", 42, bb)
	if _, n := b.slots.counts(); n != 2 {
		t.Fatalf("restored the same replica twice: %v", b.slots.restores)
	}
}

// A member serving another model refuses the replica before storing it, and
// the sender stops saving for it for a while.
func TestSlotReplicaOfAnotherModelIsRefused(t *testing.T) {
	a, b := newSlotNode(t, "a", 0xabc), newSlotNode(t, "b", 0xdef)
	a.sees(b.node, "g", 1, time.Now())
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}, 2000)
	md := a.m.model(testModel)
	waitFor(t, "the refusal", func() bool { return md.pushFails.Load() == 1 })
	if b.m.Stats(testModel).Puts != 0 {
		t.Fatal("stored a replica of another model")
	}
	if _, ok, _ := b.m.model(testModel).store.Info(storeKey(9)); ok {
		t.Fatal("holds a replica of another model")
	}
	b.m.BeforeLocal(context.Background(), testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}})
	if _, n := b.slots.counts(); n != 0 {
		t.Fatal("restored a replica of another model")
	}
	// The next turn costs no save.
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}, 2100)
	time.Sleep(100 * time.Millisecond)
	if saves, _ := a.slots.counts(); saves != 1 {
		t.Fatalf("saved %d times for a peer that refuses the model", saves)
	}
}

func TestSlotThatMovedOnIsNotSaved(t *testing.T) {
	a, b := newSlotNode(t, "a", 0xabc), newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "g", 1, time.Now())
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	// SlotHolds is false: another request took the slot after the turn.
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}}, 2000)
	time.Sleep(100 * time.Millisecond)
	if saves, _ := a.slots.counts(); saves != 0 {
		t.Fatal("saved a slot that no longer holds the session")
	}
	// A backend without a slot (not a lease on a slot-addressed backend)
	// is neither saved nor restored.
	a.m.AfterLocal(testModel, "s", 9, &fakeBackend{id: "m/0"}, 2000)
	b.m.BeforeLocal(context.Background(), testModel, "s", 9, &fakeBackend{id: "m/0"})
	time.Sleep(50 * time.Millisecond)
	if saves, _ := a.slots.counts(); saves != 0 {
		t.Fatal("saved without a slot")
	}
}

// fpSlotEngine fingerprints per backend: every weights path is 0x77.
type fpSlotEngine struct{ *fakeSlotEngine }

func (fpSlotEngine) ModelFingerprint(path string) (uint64, error) {
	if path == "" {
		return 0, os.ErrNotExist
	}
	return 0x77, nil
}

type pathBackend struct {
	*slotBackend
	path string
}

func (b pathBackend) ModelPath() string { return b.path }

// A model with a source has no path when SessionShare runs (fingerprint 0):
// the backend's own weights stamp the replica and decide the restore.
func TestSlotFingerprintFromTheBackendsWeights(t *testing.T) {
	mk := func(name string) *slotNode {
		n := newNode(t, name)
		e := &fakeSlotEngine{dir: t.TempDir(), slots: map[int]kvsharetest.Session{}}
		cfg := engine.SessionShareConfig{Group: "g", Transport: "http", SaveDir: e.dir, MaxBytes: 1 << 30, MinTokens: 1000, MaxFileBytes: 1 << 30, Model: 0, Config: 7}
		n.m.SetModels([]Model{{Name: testModel, Slots: fpSlotEngine{e}, Config: cfg}})
		return &slotNode{node: n, slots: e}
	}
	a, b := mk("a"), mk("b")
	a.sees(b.node, "g", 1, time.Now())
	b.sees(a.node, "g", 1, time.Now())
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	ba := pathBackend{&slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot"}, holds: true}, "/models/q.gguf"}
	a.m.AfterLocal(testModel, "s", 9, ba, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	if rep, ok, _ := b.m.model(testModel).store.Info(storeKey(9)); !ok || rep.Model != 0x77 {
		t.Fatalf("replica %+v, want the backend's fingerprint", rep)
	}
	bb := pathBackend{&slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}}, "/models/q.gguf"}
	b.m.BeforeLocal(context.Background(), testModel, "s", 9, bb)
	if _, n := b.slots.counts(); n != 1 {
		t.Fatal("the replica was not restored")
	}
}

// A request that took the slot while it was being saved may have run first:
// the file is then not the session's, and is not sent.
func TestSlotTakenDuringTheSaveIsNotSent(t *testing.T) {
	a, b := newSlotNode(t, "a", 0xabc), newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "g", 1, time.Now())
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	ba := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}
	a.slots.onSave = func() { ba.holds = false }
	a.m.AfterLocal(testModel, "s", 9, ba, 2000)
	waitFor(t, "the save", func() bool { n, _ := a.slots.counts(); return n == 1 })
	time.Sleep(100 * time.Millisecond)
	if b.m.Stats(testModel).Puts != 0 {
		t.Fatal("sent a file saved while another request held the slot")
	}
	if left, _ := os.ReadDir(a.slots.dir); len(left) != 0 {
		t.Fatalf("left the file behind: %v", left)
	}
}

// A peer's push of an earlier turn that lands after this host ran the next
// turn is not restored over it.
func TestSlotLateOlderReplicaIsNotRestored(t *testing.T) {
	a, b := newSlotNode(t, "a", 0xabc), newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "g", 1, time.Now())
	ctx := context.Background()
	// b ran turn 3 (2600 tokens) in its slot 0 ...
	bb := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Held: true}}
	b.m.AfterLocal(testModel, "s", 9, bb, 2600)
	time.Sleep(5 * time.Millisecond)
	// ... before a's push of turn 2 (2000 tokens) arrives.
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	b.m.BeforeLocal(ctx, testModel, "s", 9, bb)
	if _, n := b.slots.counts(); n != 0 {
		t.Fatal("restored an earlier turn over the slot's newer one")
	}
}
