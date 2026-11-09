package sessionshare

import (
	"context"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
)

// strataLocal turns n's (Strata) model into one that keeps local copies, in
// group ("" for none).
func strataLocal(n *node, group string) {
	cfg := shareConfig(n.eng)
	cfg.Group, cfg.Local = group, true
	n.m.SetModels([]Model{{Name: testModel, Engine: n.eng, Config: cfg}})
}

func (e *fakeEngine) all() (saves, deltas, imports int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saves, e.deltas, len(e.imports)
}

func TestStrataLocalCopyDeltasAndRestart(t *testing.T) {
	a := newNode(t, "a")
	strataLocal(a, "")
	md := a.m.model(testModel)
	ctx := context.Background()
	t0 := time.Unix(1000, 0)
	b0 := &fakeBackend{id: "m/0", launched: t0}

	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 5, b0, 2000)
	waitFor(t, "the first copy", func() bool { return md.locals.Load() == 1 })
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2400, StateSeed: 2})
	a.m.AfterLocal(testModel, "s", 5, b0, 2400)
	waitFor(t, "the second copy", func() bool { return md.locals.Load() == 2 })
	if saves, deltas, _ := a.eng.all(); saves != 2 || deltas != 1 {
		t.Fatalf("saves %d, deltas %d: want the second save as a delta against the copy", saves, deltas)
	}
	if rep, ok, _ := md.store.Info(storeKey(5)); !ok || rep.Tokens != 2400 {
		t.Fatalf("copy %+v %v", rep, ok)
	}

	// The backend that made the copy holds it: nothing to import.
	a.m.BeforeLocal(ctx, testModel, "s", 5, b0)
	if _, _, n := a.eng.all(); n != 0 {
		t.Fatal("imported into the backend that holds the session")
	}
	// It relaunched: the copy goes back in.
	a.m.BeforeLocal(ctx, testModel, "s", 5, &fakeBackend{id: "m/0", launched: t0.Add(time.Minute)})
	if _, _, n := a.eng.all(); n != 1 {
		t.Fatal("the copy was not imported into the relaunched backend")
	}
}

func TestStrataLocalCopyAndPeer(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	strataLocal(a, "g")
	a.sees(b, "g", 1, time.Now())
	b0 := &fakeBackend{id: "m/0"}

	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 5, b0, 2000)
	waitFor(t, "the first push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2400, StateSeed: 2})
	a.m.AfterLocal(testModel, "s", 5, b0, 2400)
	waitFor(t, "the second push", func() bool { return b.m.Stats(testModel).Puts == 2 })
	if _, deltas, _ := a.eng.all(); deltas != 1 {
		t.Fatalf("deltas %d, want 1", deltas)
	}
	bmd := b.m.model(testModel)
	if rep, ok, _ := bmd.store.Info(storeKey(5)); !ok || rep.Tokens != 2400 {
		t.Fatalf("b's replica %+v %v", rep, ok)
	}

	// b loses its replica: a's next delta is against a's copy, which b
	// does not hold, so b gets the whole file rebuilt from the copy.
	if _, err := bmd.store.Evict(0); err != nil {
		t.Fatal(err)
	}
	a.sees(b, "g", 1, time.Now()) // fresh again: the race detector makes this slow
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2800, StateSeed: 3})
	a.m.AfterLocal(testModel, "s", 5, b0, 2800)
	// The slowest step here: a save, the copy, and the whole file rebuilt
	// and diffed, several seconds under the race detector.
	deadline := time.Now().Add(60 * time.Second)
	for b.m.Stats(testModel).Puts != 3 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the third push")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rep, ok, _ := bmd.store.Info(storeKey(5)); !ok || rep.Tokens != 2800 {
		t.Fatalf("b's rebuilt replica %+v %v", rep, ok)
	}
	if s := a.m.Stats(testModel); s.PushFailures != 0 {
		t.Fatalf("push failures %d", s.PushFailures)
	}
}
