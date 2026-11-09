package sessionshare

import (
	"context"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
	"github.com/janit/viiwork/v2/internal/supervisor"
)

// newLocalNode is a node whose model keeps local copies; group "" keeps
// them only.
func newLocalNode(t *testing.T, name, group, stateDir string) *slotNode {
	n := newNode(t, name)
	e := &fakeSlotEngine{dir: t.TempDir(), slots: map[int]kvsharetest.Session{}}
	cfg := engine.SessionShareConfig{Group: group, Transport: "http", SaveDir: e.dir, MaxBytes: 1 << 30, MinTokens: 1000, MaxFileBytes: 1 << 30, Model: 0xabc, Config: 7, Local: true}
	if stateDir != "" {
		cfg.StoreDir = stateDir
	}
	n.m.SetModels([]Model{{Name: testModel, Slots: e, Config: cfg}})
	return &slotNode{node: n, slots: e}
}

func TestLocalCopyRestoresALostSlot(t *testing.T) {
	a := newLocalNode(t, "a", "", "")
	// Another member serves the model with no kv_share group: a local-only
	// model must not take it for a replica host.
	b := newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "", 1, time.Now())
	ctx := context.Background()
	md := a.m.model(testModel)

	turn1 := kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.slots.slots[1] = turn1
	ba := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}, holds: true}
	a.m.AfterLocal(testModel, "sess", 42, ba, 2000)
	waitFor(t, "the local copy", func() bool { return md.locals.Load() == 1 })
	if rep, ok, _ := md.store.Info(storeKey(42)); !ok || rep.Tokens != 2000 || rep.Model != 0xabc {
		t.Fatalf("local copy %+v %v", rep, ok)
	}
	if b.m.Stats(testModel).Puts != 0 || md.pushes.Load() != 0 {
		t.Fatal("a local-only model pushed to a member")
	}

	// The next turn finds its own slot holding what was saved: nothing to
	// restore.
	ba.claim.Held = true
	a.m.BeforeLocal(ctx, testModel, "sess", 42, ba)
	if _, n := a.slots.counts(); n != 0 {
		t.Fatalf("restored into the slot that holds the session: %v", a.slots.restores)
	}

	// Another conversation took slot 1; the session is given slot 0.
	b0 := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}}
	a.m.BeforeLocal(ctx, testModel, "sess", 42, b0)
	if _, n := a.slots.counts(); n != 1 || a.slots.restores[0].slot != 0 || a.slots.restores[0].bytes != len(turn1.Sealed()) {
		t.Fatalf("restores %v, want the local copy into slot 0", a.slots.restores)
	}
}

func TestLocalCopySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	a := newLocalNode(t, "a", "", dir)
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 3000, StateSeed: 3}
	ba := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}
	a.m.AfterLocal(testModel, "sess", 7, ba, 3000)
	waitFor(t, "the local copy", func() bool { return a.m.model(testModel).locals.Load() == 1 })
	a.m.Close()

	// A new process on the same store: its slots hold nothing.
	a2 := newLocalNode(t, "a", "", dir)
	a2.m.BeforeLocal(context.Background(), testModel, "sess", 7, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}})
	if _, n := a2.slots.counts(); n != 1 {
		t.Fatal("the local copy was not restored after a restart")
	}
}

func TestLocalCopyAndReplicaBothKept(t *testing.T) {
	a, b := newLocalNode(t, "a", "g", ""), newSlotNode(t, "b", 0xabc)
	a.sees(b.node, "g", 1, time.Now())
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	waitFor(t, "the local copy", func() bool { return a.m.model(testModel).locals.Load() == 1 })
	if saves, _ := a.slots.counts(); saves != 1 {
		t.Fatalf("saved %d times, want once for both copies", saves)
	}
}

func TestLocalRequiresNoGroup(t *testing.T) {
	o := engine.KVShareOptions{Local: true}
	if err := o.Validate("x"); err != nil {
		t.Fatal(err)
	}
	o.Local = false
	if err := o.Validate("x"); err == nil {
		t.Fatal("no group and no local passed")
	}
}

func TestDisplacedSessionIsSavedBeforeItsSlotIsReused(t *testing.T) {
	a := newLocalNode(t, "a", "", "")
	a.m.d.SaveDelay = time.Hour // the after-turn save would come too late
	md := a.m.model(testModel)
	a.slots.slots[1] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.m.AfterLocal(testModel, "s42", 42, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}, holds: true}, 2000)

	// A request without a session claims slot 1, which still holds 42's
	// conversation: SlotHolds is false now (the slot is busy), yet 42 is
	// saved before the request runs.
	next := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1, Displaced: 42}}
	a.m.BeforeLocal(context.Background(), testModel, "", 0, next)
	if saves, _ := a.slots.counts(); saves != 1 || a.slots.saves[0] != 1 {
		t.Fatalf("saves %v, want slot 1 before the reuse", a.slots.saves)
	}
	waitFor(t, "the local copy", func() bool { return md.locals.Load() == 1 })
	// ... and only once: its waiting job is gone.
	a.m.BeforeLocal(context.Background(), testModel, "", 0, next)
	if saves, _ := a.slots.counts(); saves != 1 {
		t.Fatalf("saved %d times", saves)
	}
}

func newLazyNode(t *testing.T) *slotNode {
	a := newLocalNode(t, "a", "", "")
	cfg := a.m.model(testModel).cfg
	cfg.LazySaves = true
	a.m.SetModels([]Model{{Name: testModel, Slots: a.slots, Config: cfg}})
	return a
}

func TestLazySaveWaitsForReuseOrFlush(t *testing.T) {
	a := newLazyNode(t)
	md := a.m.model(testModel)
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.slots.slots[1] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2500, StateSeed: 2}
	a.m.AfterLocal(testModel, "s1", 1, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}, holds: true}, 2000)
	a.m.AfterLocal(testModel, "s2", 2, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}, holds: true}, 2500)
	time.Sleep(100 * time.Millisecond) // ten save delays
	if saves, _ := a.slots.counts(); saves != 0 {
		t.Fatalf("a lazy save ran on the clock: %v", a.slots.saves)
	}
	// Session 3 takes slot 0 from session 1.
	a.m.BeforeLocal(context.Background(), testModel, "s3", 3, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0, Displaced: 1}})
	if saves, _ := a.slots.counts(); saves != 1 || a.slots.saves[0] != 0 {
		t.Fatalf("saves %v, want slot 0 at its reuse", a.slots.saves)
	}
	// The node stops: session 2's save runs.
	a.m.Flush(context.Background())
	if saves, _ := a.slots.counts(); saves != 2 || a.slots.saves[1] != 1 {
		t.Fatalf("saves %v, want slot 1 at the flush", a.slots.saves)
	}
	waitFor(t, "both copies", func() bool { return md.locals.Load() == 2 })
}

func TestSavesValidation(t *testing.T) {
	for _, c := range []struct {
		o  engine.KVShareOptions
		ok bool
	}{
		{engine.KVShareOptions{Local: true, Saves: "lazy"}, true},
		{engine.KVShareOptions{Group: "g", Saves: "turn"}, true},
		{engine.KVShareOptions{Group: "g", Local: true, Saves: "lazy"}, false},
		{engine.KVShareOptions{Group: "g", Saves: "lazy"}, false},
		{engine.KVShareOptions{Group: "g", Saves: "often"}, false},
	} {
		if err := c.o.Validate("x"); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.o, err)
		}
	}
}

func TestRestoreWaitsForACopyOnItsWay(t *testing.T) {
	a := newLocalNode(t, "a", "", "")
	md := a.m.model(testModel)
	a.slots.slots[1] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	release := md.markPending(storeKey(42))
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.m.BeforeLocal(context.Background(), testModel, "s42", 42, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}})
	}()
	select {
	case <-done:
		t.Fatal("BeforeLocal did not wait for the copy on its way")
	case <-time.After(50 * time.Millisecond):
	}
	// The copy lands, then the wait ends: it is restored.
	j := job{model: testModel, key: storeKey(42), hash: 42, raw: "s42", backend: &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 1}, holds: true}, slot: supervisor.SlotClaim{Field: "id_slot", Index: 1}, slotted: true}
	a.m.pushSlot(context.Background(), md, j, "", "")
	release()
	<-done
	if _, n := a.slots.counts(); n != 1 {
		t.Fatalf("restores %v, want the copy that was on its way", a.slots.restores)
	}
}

func TestFlushWaitsForASaveAlreadyRunning(t *testing.T) {
	a := newLocalNode(t, "a", "", "")
	md := a.m.model(testModel)
	a.slots.delay = 300 * time.Millisecond
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.m.AfterLocal(testModel, "s", 9, &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, holds: true}, 2000)
	time.Sleep(100 * time.Millisecond) // the worker has taken the job and is saving
	a.m.Flush(context.Background())
	if md.locals.Load() != 1 {
		t.Fatal("Flush returned before the running save was kept")
	}
}

// A node with no group saves a turn only once its backend serves nothing
// else: a save pauses every slot.
func TestLocalSaveWaitsForAnIdleBackend(t *testing.T) {
	a := newLocalNode(t, "a", "", "")
	md := a.m.model(testModel)
	a.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	b := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}, holds: true}
	b.busy.Store(1) // another slot is decoding
	a.m.AfterLocal(testModel, "s1", 1, b, 2000)
	time.Sleep(100 * time.Millisecond) // ten save delays
	if saves, _ := a.slots.counts(); saves != 0 {
		t.Fatalf("saved while the backend was busy: %v", a.slots.saves)
	}
	b.busy.Store(0)
	waitFor(t, "the save once idle", func() bool { return md.locals.Load() == 1 })

	// With a group the replica is due on time, busy or not.
	g := newLocalNode(t, "g", "g", "")
	g.slots.slots[0] = kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	gb := &slotBackend{fakeBackend: fakeBackend{id: "m/0"}, claim: supervisor.SlotClaim{Field: "id_slot", Index: 0}, holds: true}
	gb.busy.Store(1)
	g.m.AfterLocal(testModel, "s1", 1, gb, 2000)
	waitFor(t, "the save with a group", func() bool { return g.m.model(testModel).locals.Load() == 1 })
}
