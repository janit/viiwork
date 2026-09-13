package supervisor

import (
	"context"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
)

// newTestSupervisor is a Supervisor on Task 9's timing with an event recorder,
// shut down at cleanup.
func newTestSupervisor(t *testing.T) (*Supervisor, *eventRecorder) {
	t.Helper()
	events := &eventRecorder{}
	deps := testDeps(&syncBuffer{})
	deps.Events = events
	s := New(deps)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, events
}

func mustApply(t *testing.T, s *Supervisor, models ...config.Model) {
	t.Helper()
	if err := s.Apply(models); err != nil {
		t.Fatal(err)
	}
}

func modelHealthy(t *testing.T, s *Supervisor, name string, within time.Duration) *Model {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if m, ok := s.Model(name); ok {
			all := true
			for _, b := range m.Backends() {
				all = all && b.State() == StateHealthy
			}
			if all {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("model %s not healthy within %v", name, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func leaderOf(m *Model) int {
	if p := m.Backends()[0].PIDs(); len(p) > 0 {
		return p[0]
	}
	return 0
}

// callsInOrder is the fake engine's call log for several models, in the order
// the calls happened.
func callsInOrder(names ...string) []fakeCall {
	fakeLog.mu.Lock()
	defer fakeLog.mu.Unlock()
	var out []fakeCall
	for _, c := range fakeLog.calls {
		if slices.Contains(names, c.Spec.Name) {
			out = append(out, c)
		}
	}
	return out
}

func singleGPUBackends(m config.Model) config.Model {
	m.GPUsPerBackend = 1
	return m
}

func TestSupervisorT1LoadOrder(t *testing.T) {
	s, events := newTestSupervisor(t)
	a := singleGPUBackends(fakeModel("t1-a", []int{0, 1}, "ready_after=150ms"))
	b := singleGPUBackends(fakeModel("t1-b", []int{2, 3}, "ready_after=150ms"))
	mustApply(t, s, a, b)
	modelHealthy(t, s, "t1-a", 10*time.Second)
	modelHealthy(t, s, "t1-b", 10*time.Second)

	calls := callsInOrder("t1-a", "t1-b")
	want := []string{"t1-a/0", "t1-b/0", "t1-a/1", "t1-b/1"}
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	for i, c := range calls {
		id := c.Spec.Name + "/" + map[int]string{0: "0", 1: "1", 2: "0", 3: "1"}[c.Spec.GPUs[0]]
		if id != want[i] {
			t.Errorf("launch %d = %s, want %s", i, id, want[i])
		}
		if i > 0 {
			prev, ok := events.find(want[i-1] + ": ready")
			if !ok || !c.At.After(prev.At) {
				t.Errorf("%s launched at %v, before %s was ready (%v)", want[i], c.At, want[i-1], prev.At)
			}
		}
	}
}

func TestSupervisorT2RespawnDoesNotWaitForTheGate(t *testing.T) {
	s, events := newTestSupervisor(t)
	a := fakeModel("t2-a", []int{0})
	mustApply(t, s, a)
	ma := modelHealthy(t, s, "t2-a", 5*time.Second)

	mustApply(t, s, a, fakeModel("t2-b", []int{1}, "ready_after=800ms"))
	time.Sleep(100 * time.Millisecond) // B is loading and holds the gate
	leader := leaderOf(ma)
	_ = syscall.Kill(leader, syscall.SIGKILL)

	within(t, 2*time.Second, "A relaunched", func() bool { return len(fakeCallsFor("t2-a")) == 2 })
	relaunch := fakeCallsFor("t2-a")[1].At
	if ready, ok := events.find("t2-b/0: ready"); ok && ready.At.Before(relaunch) {
		t.Errorf("A relaunched at %v, after B's ready event at %v: a respawn must not wait for the gate", relaunch, ready.At)
	}
	modelHealthy(t, s, "t2-b", 5*time.Second)
	within(t, 2*time.Second, "status respawns 1", func() bool {
		for _, st := range s.Status() {
			if st.Name == "t2-a" {
				return st.Backends[0].Respawns == 1
			}
		}
		return false
	})
}

func TestSupervisorT3SameListTwice(t *testing.T) {
	s, _ := newTestSupervisor(t)
	a := fakeModel("t3", []int{0})
	mustApply(t, s, a)
	m := modelHealthy(t, s, "t3", 5*time.Second)
	pids := m.Backends()[0].PIDs()
	mustApply(t, s, a)
	time.Sleep(200 * time.Millisecond)
	m2, _ := s.Model("t3")
	if m2 != m || !slices.Equal(m2.Backends()[0].PIDs(), pids) || len(fakeCallsFor("t3")) != 1 {
		t.Errorf("re-applying the same list restarted the model")
	}
}

func TestSupervisorT4ChangedModelRestartsAlone(t *testing.T) {
	s, _ := newTestSupervisor(t)
	a, b := fakeModel("t4-a", []int{0}), fakeModel("t4-b", []int{1})
	mustApply(t, s, a, b)
	ma := modelHealthy(t, s, "t4-a", 5*time.Second)
	mb := modelHealthy(t, s, "t4-b", 5*time.Second)
	oldA, pidsB := leaderOf(ma), mb.Backends()[0].PIDs()

	a.Args = append(a.Args, "tag=2")
	mustApply(t, s, a, b)
	ma2 := modelHealthy(t, s, "t4-a", 5*time.Second)
	within(t, 2*time.Second, "new A leader", func() bool { l := leaderOf(ma2); return l != 0 && l != oldA })
	if !slices.Equal(mb.Backends()[0].PIDs(), pidsB) {
		t.Error("changing A restarted B")
	}
	waitFor(t, "old A leader gone", func() bool { return processGone(oldA) })
}

func TestSupervisorT5RemovedModelDrains(t *testing.T) {
	s, _ := newTestSupervisor(t)
	mustApply(t, s, fakeModel("t5-a", []int{0}))
	ma := modelHealthy(t, s, "t5-a", 5*time.Second)
	b := ma.Backends()[0]
	leader := leaderOf(ma)
	b.Acquire()

	mustApply(t, s, fakeModel("t5-b", []int{1}))
	if _, ok := s.Model("t5-a"); ok {
		t.Error("a removed model must leave Models() at once")
	}
	time.Sleep(200 * time.Millisecond)
	if !slices.Contains(s.PIDs(), leader) {
		t.Error("a draining model's processes must stay in PIDs()")
	}
	b.Release()
	within(t, time.Second, "A stopped", func() bool { return processGone(leader) && !slices.Contains(s.PIDs(), leader) })
}

func TestSupervisorT6NoLoadOntoAHeldGPU(t *testing.T) {
	s, _ := newTestSupervisor(t)
	mustApply(t, s, fakeModel("t6-a", []int{0}))
	ma := modelHealthy(t, s, "t6-a", 5*time.Second)
	b := ma.Backends()[0]
	b.Acquire()

	mustApply(t, s, fakeModel("t6-c", []int{0}))
	time.Sleep(250 * time.Millisecond)
	if n := len(fakeCallsFor("t6-c")); n != 0 {
		t.Fatalf("C launched %d times while A still held GPU 0", n)
	}
	time.Sleep(50 * time.Millisecond)
	releasedAt := time.Now()
	b.Release()
	modelHealthy(t, s, "t6-c", 5*time.Second)
	if c := fakeCallsFor("t6-c"); len(c) == 0 || !c[0].At.After(releasedAt) {
		t.Errorf("C must launch only after A drained (released %v, calls %+v)", releasedAt, c)
	}
}

func TestSupervisorT7UnregisteredEngineIsAtomic(t *testing.T) {
	s, _ := newTestSupervisor(t)
	x := fakeModel("t7-x", []int{1})
	x.Engine = "tgi"
	err := s.Apply([]config.Model{fakeModel("t7-a", []int{0}), x})
	if err == nil || !strings.Contains(err.Error(), "t7-x") {
		t.Fatalf("err = %v, want it to name t7-x", err)
	}
	time.Sleep(100 * time.Millisecond)
	if len(fakeCallsFor("t7-a")) != 0 || len(s.Models()) != 0 {
		t.Error("a failed Apply must change nothing")
	}
	mustApply(t, s, fakeModel("t7-a", []int{0}))
	modelHealthy(t, s, "t7-a", 5*time.Second)
}

func TestSupervisorT8DuplicateNames(t *testing.T) {
	s, _ := newTestSupervisor(t)
	err := s.Apply([]config.Model{fakeModel("t8", []int{0}), fakeModel("t8", []int{1})})
	if err == nil || !strings.Contains(err.Error(), "t8") {
		t.Errorf("err = %v, want it to name t8", err)
	}
}

func TestSupervisorT9Shutdown(t *testing.T) {
	s, _ := newTestSupervisor(t)
	mustApply(t, s, singleGPUBackends(fakeModel("t9", []int{0, 1})))
	m := modelHealthy(t, s, "t9", 5*time.Second)
	var pids []int
	for _, b := range m.Backends() {
		pids = append(pids, b.PIDs()...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Shutdown(ctx)
	for _, pid := range pids {
		waitFor(t, "backend gone", func() bool { return processGone(pid) })
	}
	if err := s.Apply(nil); err == nil || !strings.Contains(err.Error(), "supervisor is shut down") {
		t.Errorf("Apply after Shutdown = %v", err)
	}
}

func TestSupervisorT10ShutdownDeadline(t *testing.T) {
	s, _ := newTestSupervisor(t)
	mustApply(t, s, fakeModel("t10", []int{0}, "term=ignore"))
	m := modelHealthy(t, s, "t10", 5*time.Second)
	m.Backends()[0].Acquire()
	leader := leaderOf(m)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Shutdown(ctx)
	if took := time.Since(start); took >= 1500*time.Millisecond {
		t.Errorf("Shutdown took %v, want under 1.5 s", took)
	}
	waitFor(t, "leader gone", func() bool { return processGone(leader) })
}

func TestSupervisorT11Capacity(t *testing.T) {
	s, _ := newTestSupervisor(t)
	mustApply(t, s, fakeModel("t11", []int{0}, "slots=4"))
	m := modelHealthy(t, s, "t11", 5*time.Second)
	b := m.Backends()[0]
	if err := postFake(b.Addr(), "/busy?n=3"); err != nil {
		t.Fatal(err)
	}
	b.Acquire()
	defer b.Release()
	within(t, time.Second, "capacity 4/3", func() bool {
		c := s.Capacity()
		return len(c) == 1 && c[0].Slots == 4 && c[0].Busy == 3 && c[0].Backends == 1 && c[0].HealthyBackends == 1
	})
}
