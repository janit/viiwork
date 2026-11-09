package route

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

var t0 = time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

type fakeBackend struct {
	id       string
	mu       sync.Mutex
	state    supervisor.State
	slots    int
	inFlight int
	releases atomic.Int64
	hard     atomic.Int64
	// slotField makes it slot-addressed, as a supervisor.Backend whose
	// engine names slots: claims hand out free indices.
	slotField string
	slotBusy  map[int]uint64 // index -> session of the claim
	claims    []uint64       // sessions claimed, in order
}

func newFakeBackend(id string, slots int) *fakeBackend {
	return &fakeBackend{id: id, state: supervisor.StateHealthy, slots: slots}
}

func (b *fakeBackend) ID() string   { return b.id }
func (b *fakeBackend) Addr() string { return "127.0.0.1:1" }
func (b *fakeBackend) State() supervisor.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
func (b *fakeBackend) Slots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slots
}
func (b *fakeBackend) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}
func (b *fakeBackend) Acquire() {
	b.mu.Lock()
	b.inFlight++
	b.mu.Unlock()
}
func (b *fakeBackend) Release() {
	b.releases.Add(1)
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}
func (b *fakeBackend) NoteHardFailure() { b.hard.Add(1) }

func (b *fakeBackend) ClaimSlot(session uint64) (supervisor.SlotClaim, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.slotField == "" {
		return supervisor.SlotClaim{}, false
	}
	if b.slotBusy == nil {
		b.slotBusy = map[int]uint64{}
	}
	for i := 0; i < b.slots; i++ {
		if _, busy := b.slotBusy[i]; !busy {
			b.slotBusy[i] = session
			b.claims = append(b.claims, session)
			return supervisor.SlotClaim{Field: b.slotField, Index: i}, true
		}
	}
	return supervisor.SlotClaim{}, false
}

func (b *fakeBackend) FreeSlot(c supervisor.SlotClaim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.slotBusy, c.Index)
}

func (b *fakeBackend) set(state supervisor.State) {
	b.mu.Lock()
	b.state = state
	b.mu.Unlock()
}

type fakeLocal struct {
	mu        sync.Mutex
	models    map[string][]*fakeBackend
	admitting map[string]bool
}

func newFakeLocal() *fakeLocal {
	return &fakeLocal{models: map[string][]*fakeBackend{}, admitting: map[string]bool{}}
}

func (l *fakeLocal) add(model string, bs ...*fakeBackend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.models[model] = append(l.models[model], bs...)
	l.admitting[model] = true
}

func (l *fakeLocal) Backends(model string) ([]LocalBackend, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bs, ok := l.models[model]
	if !ok {
		return nil, false, false
	}
	out := make([]LocalBackend, len(bs))
	for i, b := range bs {
		out[i] = b
	}
	return out, l.admitting[model], true
}

type fakeReports struct {
	mu      sync.Mutex
	reports []capacity.Report
}

func (f *fakeReports) Reports() []capacity.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capacity.Report(nil), f.reports...)
}

func (f *fakeReports) set(rs ...capacity.Report) {
	f.mu.Lock()
	f.reports = rs
	f.mu.Unlock()
}

func report(node string, received time.Time, rtt time.Duration, models ...meshapi.ModelCapacity) capacity.Report {
	return capacity.Report{Node: node, APIAddr: node + ":8086", Received: received, RTT: rtt, Models: models}
}

func capM(slots, busy, healthy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{Name: "m", Engine: "llamacpp", Slots: slots, Busy: busy, Backends: 1, HealthyBackends: healthy}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

type fixture struct {
	local   *fakeLocal
	reports *fakeReports
	clock   *clock
	router  *Router
}

func newFixture() *fixture {
	f := &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: t0}}
	f.router = New(Config{Self: "self", Local: f.local, Remote: f.reports, StaleAfter: 3 * time.Second, QueueMax: 4, QueueTimeout: time.Second, Now: f.clock.Now})
	return f
}

func (f *fixture) pick(t *testing.T, req Request) *Lease {
	t.Helper()
	if req.Model == "" {
		req.Model = "m"
	}
	l, err := f.router.Pick(req)
	if err != nil {
		t.Fatalf("Pick(%+v): %v", req, err)
	}
	return l
}

func (f *fixture) pickErr(req Request) error {
	if req.Model == "" {
		req.Model = "m"
	}
	l, err := f.router.Pick(req)
	if err == nil {
		l.Release()
	}
	return err
}

func TestRouterPick(t *testing.T) {
	t.Run("R1 local before a freer peer", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("P", t0, time.Millisecond, capM(5, 0, 1)))
		if l := f.pick(t, Request{}); !l.Target().Local || l.Target().BackendID != "m/0" {
			t.Errorf("target = %+v", l.Target())
		}
	})

	t.Run("R2 most free local", func(t *testing.T) {
		f := newFixture()
		b0, b1 := newFakeBackend("m/0", 2), newFakeBackend("m/1", 2)
		b0.Acquire()
		f.local.add("m", b0, b1)
		if l := f.pick(t, Request{}); l.Target().BackendID != "m/1" {
			t.Errorf("target = %+v", l.Target())
		}
	})

	t.Run("R3 ties rotate", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 2), newFakeBackend("m/1", 2))
		var got []string
		for i := 0; i < 4; i++ {
			l := f.pick(t, Request{})
			got = append(got, l.Target().BackendID)
			l.Release()
		}
		if got[0] != "m/0" || got[1] != "m/1" || got[2] != "m/0" || got[3] != "m/1" {
			t.Errorf("order = %v", got)
		}
	})

	t.Run("R4 unhealthy locals give way to a peer", func(t *testing.T) {
		f := newFixture()
		b0, b1 := newFakeBackend("m/0", 2), newFakeBackend("m/1", 2)
		b0.set(supervisor.StateStarting)
		b1.set(supervisor.StateUnhealthy)
		f.local.add("m", b0, b1)
		f.reports.set(report("P", t0, time.Millisecond, capM(1, 0, 1)))
		if l := f.pick(t, Request{}); l.Target().Local || l.Target().Node != "P" {
			t.Errorf("target = %+v", l.Target())
		}
	})

	t.Run("R5 a draining model gives way to a peer", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 2))
		f.local.admitting["m"] = false
		f.reports.set(report("P", t0, time.Millisecond, capM(1, 0, 1)))
		if l := f.pick(t, Request{}); l.Target().Node != "P" {
			t.Errorf("target = %+v", l.Target())
		}
	})

	t.Run("R6 equal free, lower RTT", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0, 20*time.Millisecond, capM(2, 0, 1)), report("Q", t0, 5*time.Millisecond, capM(2, 0, 1)))
		if l := f.pick(t, Request{}); l.Target().Node != "Q" {
			t.Errorf("target = %+v", l.Target())
		}
	})

	t.Run("R7 equal peers rotate", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 1)), report("Q", t0, time.Millisecond, capM(2, 0, 1)))
		var got []string
		for i := 0; i < 4; i++ {
			l := f.pick(t, Request{})
			got = append(got, l.Target().Node)
			l.Release()
		}
		if got[0] != "P" || got[1] != "Q" || got[2] != "P" || got[3] != "Q" {
			t.Errorf("order = %v", got)
		}
	})

	t.Run("R8 stale report", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0.Add(-3*time.Second), time.Millisecond, capM(2, 0, 1)))
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R9 no healthy backends", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 0)))
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R10-R12 reservations", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 1)))
		f.clock.set(t0.Add(100 * time.Millisecond))
		var mu sync.Mutex
		var leases []*Lease
		var noSlot atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := f.router.Pick(Request{Model: "m"})
				if errors.Is(err, ErrNoFreeSlot) {
					noSlot.Add(1)
					return
				}
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()
			}()
		}
		wg.Wait()
		if len(leases) != 2 || noSlot.Load() != 8 {
			t.Fatalf("R10: %d leases, %d no-slot; want 2 and 8", len(leases), noSlot.Load())
		}

		f.clock.set(t0.Add(time.Second))
		f.reports.set(report("P", t0.Add(time.Second), time.Millisecond, capM(2, 2, 1)))
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("R11: err = %v, want no free slot (not double counted, not freed)", err)
		}

		for _, l := range leases {
			l.Release()
		}
		f.reports.set(report("P", t0.Add(time.Second), time.Millisecond, capM(2, 0, 1)))
		if err := f.pickErr(Request{}); err != nil {
			t.Errorf("R12: err = %v", err)
		}
	})

	t.Run("R13 exclusions", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		if err := f.pickErr(Request{Exclude: map[string]bool{"local:m/0": true}}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R14-R17 host pin", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		f.reports.set(report("P", t0, time.Millisecond, capM(5, 0, 1)))
		if l := f.pick(t, Request{Host: "self"}); !l.Target().Local {
			t.Errorf("R14 pin self: %+v", l.Target())
		}
		if l := f.pick(t, Request{Host: "P"}); l.Target().Node != "P" {
			t.Errorf("R15 pin P: %+v", l.Target())
		}
		if err := f.pickErr(Request{Host: "nosuch"}); !errors.Is(err, ErrHostNotServing) {
			t.Errorf("R16 pin nosuch: %v", err)
		}
		f.reports.set(report("P", t0, time.Millisecond, capM(5, 5, 1)))
		if err := f.pickErr(Request{Host: "P"}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("R17 pin a full P: %v", err)
		}
	})

	t.Run("R18 unknown model", func(t *testing.T) {
		f := newFixture()
		if err := f.pickErr(Request{}); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R19 model only in a stale report", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0.Add(-time.Hour), time.Millisecond, capM(2, 0, 1)))
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R20 forwarded never spills", func(t *testing.T) {
		f := newFixture()
		b := newFakeBackend("m/0", 1)
		b.Acquire()
		f.local.add("m", b)
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 1)))
		if err := f.pickErr(Request{Forwarded: true}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R21 forwarded for a model not served here", func(t *testing.T) {
		f := newFixture()
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 1)))
		if err := f.pickErr(Request{Forwarded: true}); !errors.Is(err, ErrModelNotFound) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("R22 double release", func(t *testing.T) {
		f := newFixture()
		b := newFakeBackend("m/0", 1)
		f.local.add("m", b)
		l := f.pick(t, Request{})
		l.Release()
		l.Release()
		if n := b.releases.Load(); n != 1 {
			t.Errorf("backend Release called %d times, want 1", n)
		}
	})
}

// A peer's refusal outranks the report it was picked by (Decision 18).
func TestRouterRefusal(t *testing.T) {
	modelN := meshapi.ModelCapacity{Name: "n", Engine: "llamacpp", Slots: 1, Busy: 0, Backends: 1, HealthyBackends: 1}

	t.Run("R23 refused peer is full until a newer report", func(t *testing.T) {
		f := newFixture()
		f.clock.set(t0.Add(100 * time.Millisecond))
		f.reports.set(report("P", t0, time.Millisecond, capM(1, 0, 1), modelN))
		l := f.pick(t, Request{})
		l.Refused()
		l.Release()
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("refused peer picked again by the same report: err = %v", err)
		}
		if l := f.pick(t, Request{Model: "n"}); l.Target().Node != "P" {
			t.Errorf("a refusal of m must not rule P out for n: %+v", l.Target())
		}

		f.clock.set(t0.Add(300 * time.Millisecond))
		f.reports.set(report("P", t0.Add(200*time.Millisecond), time.Millisecond, capM(1, 0, 1), modelN))
		if l := f.pick(t, Request{}); l.Target().Node != "P" {
			t.Errorf("R24: a report received after the refusal must make P a candidate again: %+v", l.Target())
		}
	})

	// A poll sent before the refusal can arrive after it, carrying the
	// peer's state from before: it must not clear the mark.
	t.Run("R24b a report requested before the refusal does not clear it", func(t *testing.T) {
		f := newFixture()
		f.clock.set(t0.Add(100 * time.Millisecond))
		f.reports.set(report("P", t0, time.Millisecond, capM(1, 0, 1)))
		l := f.pick(t, Request{})
		f.clock.set(t0.Add(500 * time.Millisecond))
		l.Refused() // refused at t0+500ms
		l.Release()
		late := report("P", t0.Add(600*time.Millisecond), 300*time.Millisecond, capM(1, 0, 1))
		late.Sent = t0.Add(300 * time.Millisecond) // requested before the refusal
		f.clock.set(t0.Add(700 * time.Millisecond))
		f.reports.set(late)
		if err := f.pickErr(Request{}); !errors.Is(err, ErrNoFreeSlot) {
			t.Errorf("a report requested before the refusal cleared it: err = %v", err)
		}
	})

	t.Run("R25 other peers stay candidates", func(t *testing.T) {
		f := newFixture()
		f.clock.set(t0.Add(100 * time.Millisecond))
		f.reports.set(report("P", t0, time.Millisecond, capM(2, 0, 1)), report("Q", t0, 5*time.Millisecond, capM(1, 0, 1)))
		l := f.pick(t, Request{})
		if l.Target().Node != "P" {
			t.Fatalf("first pick = %+v, want P", l.Target())
		}
		l.Refused()
		l.Release()
		if l := f.pick(t, Request{}); l.Target().Node != "Q" {
			t.Errorf("pick after P refused = %+v, want Q", l.Target())
		}
	})

	t.Run("R26 local lease ignores Refused", func(t *testing.T) {
		f := newFixture()
		f.local.add("m", newFakeBackend("m/0", 1))
		l := f.pick(t, Request{})
		l.Refused()
		l.Release()
		if l := f.pick(t, Request{}); l.Target().BackendID != "m/0" {
			t.Errorf("pick = %+v", l.Target())
		}
	})
}

func TestSupervisorModels(t *testing.T) {
	var _ LocalBackend = (*supervisor.Backend)(nil)
	s := supervisor.New(supervisor.Deps{})
	if _, _, ok := SupervisorModels(s).Backends("m"); ok {
		t.Error("an empty supervisor serves no model")
	}
}

// The same within a node: four pairs on one host, and the most-free pick
// moved 7% of a session's turns to another backend's cold cache. A session
// keeps its backend while that backend has a free slot.
func TestSessionKeepsItsLocalBackend(t *testing.T) {
	local := newFakeLocal()
	bs := []*fakeBackend{newFakeBackend("m/0", 2), newFakeBackend("m/1", 2), newFakeBackend("m/2", 2), newFakeBackend("m/3", 2)}
	local.add("m", bs...)
	r := New(Config{Self: "self", Local: local, Remote: &fakeReports{}, StaleAfter: time.Hour, QueueMax: 8,
		QueueTimeout: time.Second, Now: func() time.Time { return t0 }})
	req := Request{Model: "m", SessionKey: 0xfeedface}
	l, err := r.Pick(req)
	if err != nil {
		t.Fatal(err)
	}
	home := l.Target().BackendID
	l.Release()

	// Load every other backend less than home, so most-free would move it.
	for _, b := range bs {
		if b.id == home {
			b.Acquire()
		}
	}
	for i := 0; i < 5; i++ {
		l, err := r.Pick(req)
		if err != nil {
			t.Fatal(err)
		}
		if l.Target().BackendID != home {
			t.Fatalf("pick %d went to %s; the session's backend %s still has a free slot", i, l.Target().BackendID, home)
		}
		l.Release()
	}

	// Full: the session takes another backend, and returns once home frees.
	for _, b := range bs {
		if b.id == home {
			b.Acquire()
		}
	}
	l, err = r.Pick(req)
	if err != nil || l.Target().BackendID == home {
		t.Fatalf("got %+v %v; a full backend is never picked", l, err)
	}
	l.Release()
	for _, b := range bs {
		if b.id == home {
			b.Release()
		}
	}
	l, _ = r.Pick(req)
	if l.Target().BackendID != home {
		t.Fatalf("picked %s; once %s has a free slot the session returns", l.Target().BackendID, home)
	}
}

// Sessions spread over a node's backends rather than piling onto one.
func TestSessionsSpreadOverLocalBackends(t *testing.T) {
	local := newFakeLocal()
	local.add("m", newFakeBackend("m/0", 64), newFakeBackend("m/1", 64), newFakeBackend("m/2", 64), newFakeBackend("m/3", 64))
	r := New(Config{Self: "self", Local: local, Remote: &fakeReports{}, StaleAfter: time.Hour, QueueMax: 8,
		QueueTimeout: time.Second, Now: func() time.Time { return t0 }})
	seen := map[string]int{}
	for k := uint64(1); k <= 64; k++ {
		l, err := r.Pick(Request{Model: "m", SessionKey: k * 0x9e3779b97f4a7c15})
		if err != nil {
			t.Fatal(err)
		}
		seen[l.Target().BackendID]++
		l.Release()
	}
	if len(seen) != 4 {
		t.Fatalf("spread %v; 64 sessions must reach all four backends", seen)
	}
}
