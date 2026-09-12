package supervisor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/gpu"
)

type recordedEvent struct {
	At  time.Time
	Msg string
}

// eventRecorder is an Events that keeps every message with its time.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *eventRecorder) Emit(_ string, _ int, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{At: time.Now(), Msg: fmt.Sprintf(format, args...)})
}

func (r *eventRecorder) find(sub string) (recordedEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if strings.Contains(e.Msg, sub) {
			return e, true
		}
	}
	return recordedEvent{}, false
}

func (r *eventRecorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if strings.Contains(e.Msg, sub) {
			n++
		}
	}
	return n
}

func (r *eventRecorder) has(sub string) bool {
	_, ok := r.find(sub)
	return ok
}

type loopHarness struct {
	t      *testing.T
	deps   Deps
	events *eventRecorder
	log    *syncBuffer
	gate   *loadGate
}

func newHarness(t *testing.T) *loopHarness {
	log := &syncBuffer{}
	events := &eventRecorder{}
	deps := testDeps(log)
	deps.Events = events
	return &loopHarness{t: t, deps: deps, events: events, log: log, gate: newLoadGate()}
}

// start builds the model, enqueues one ticket per backend in index order,
// then starts the loops. Cleanup cancels the loops and drains the model.
func (h *loopHarness) start(m config.Model) (*Model, context.CancelFunc) {
	h.t.Helper()
	model, err := newModel(m, h.deps)
	if err != nil {
		h.t.Fatal(err)
	}
	tickets := make([]*loadTicket, len(model.Backends()))
	for i := range tickets {
		tickets[i] = h.gate.enqueue()
	}
	ctx, cancel := context.WithCancel(context.Background())
	nodePIDs := func() []int {
		var all []int
		for _, b := range model.Backends() {
			all = append(all, b.PIDs()...)
		}
		return all
	}
	for i, b := range model.Backends() {
		model.startLoop(ctx, b, tickets[i], h.gate, nodePIDs)
	}
	h.t.Cleanup(func() {
		cancel()
		model.drain(0)
	})
	return model, cancel
}

func (h *loopHarness) freshTicketGranted() bool {
	h.t.Helper()
	tk := h.gate.enqueue()
	defer tk.release()
	return granted(h.t, tk, time.Second)
}

func waitHealthy(t *testing.T, b *Backend, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for b.State() != StateHealthy {
		if time.Now().After(deadline) {
			t.Fatalf("%s not healthy within %v (state %v)", b.ID(), within, b.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func within(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLoopL1Ready(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l1", []int{0}, "ready_after=200ms"))
	waitHealthy(t, m.Backends()[0], 3*time.Second)
	within(t, time.Second, "ready event", func() bool { return h.events.has("l1/0: ready (loaded in") })
	if !h.freshTicketGranted() {
		t.Error("a ready backend must release its load ticket")
	}
}

func TestLoopL2LoadingPhase(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l2", []int{0}, "ready_after=500ms"))
	b := m.Backends()[0]
	within(t, 2*time.Second, "loading phase", func() bool { return b.Phase() == "loading" })
	waitHealthy(t, b, 3*time.Second)
	if n := h.events.count("l2/0: loading"); n != 1 {
		t.Errorf("loading events = %d, want exactly 1", n)
	}
}

func TestLoopL3QueuedBehindTheGate(t *testing.T) {
	h := newHarness(t)
	outside := h.gate.enqueue()
	if !granted(t, outside, time.Second) {
		t.Fatal("outside ticket not granted")
	}
	model := fakeModel("l3", []int{0, 1}, "ready_after=200ms")
	model.GPUsPerBackend = 1 // two single-GPU backends
	m, _ := h.start(model)
	bs := m.Backends()
	time.Sleep(150 * time.Millisecond)
	for _, b := range bs {
		if b.Phase() != "queued" || b.State() != StateStarting || b.PIDs() != nil {
			t.Errorf("%s before release: phase=%q state=%v pids=%v", b.ID(), b.Phase(), b.State(), b.PIDs())
		}
	}
	outside.release()
	for _, b := range bs {
		waitHealthy(t, b, 5*time.Second)
	}
	calls := fakeCallsFor("l3")
	if len(calls) != 2 || calls[0].Spec.GPUs[0] != 0 {
		t.Fatalf("calls = %+v, want backend 0 launched first", calls)
	}
	ready0, ok := h.events.find("l3/0: ready")
	if !ok || !calls[1].At.After(ready0.At) {
		t.Errorf("backend 1 launched at %v, backend 0 ready at %v: launches must not overlap", calls[1].At, ready0.At)
	}
}

func TestLoopL4KilledLeaderRespawns(t *testing.T) {
	h := newHarness(t)
	h.deps.Health.Interval = config.Duration{Duration: 5 * time.Second}
	m, _ := h.start(fakeModel("l4", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	leader := b.PIDs()[0]
	_ = syscall.Kill(leader, syscall.SIGKILL)
	within(t, time.Second, "new leader", func() bool {
		p := b.PIDs()
		return len(p) > 0 && p[0] != leader
	})
	if b.Respawns() != 1 || !h.events.has("process exited") {
		t.Errorf("respawns=%d exited event=%v", b.Respawns(), h.events.has("process exited"))
	}
}

func TestLoopL5ProbesFail(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l5", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	if err := postFake(b.Addr(), "/fail"); err != nil {
		t.Fatal(err)
	}
	within(t, 2*time.Second, "respawn", func() bool { return b.Respawns() == 1 })
	if !h.events.has("health probes failed") {
		t.Error("respawn event must name the probe cause")
	}
}

func TestLoopL6HardFailureRespawnsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.deps.Health.MaxFailures = 10
	h.deps.Health.Interval = config.Duration{Duration: 200 * time.Millisecond}
	m, _ := h.start(fakeModel("l6", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	if err := postFake(b.Addr(), "/fail"); err != nil {
		t.Fatal(err)
	}
	b.NoteHardFailure()
	within(t, 600*time.Millisecond, "respawn", func() bool { return b.Respawns() == 1 })
	if !h.events.has(CauseHardFailure) {
		t.Error("respawn event must name the hard failure")
	}
}

func TestLoopL7GraceWhileInFlight(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l7", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	b.Acquire()
	defer b.Release()
	if err := postFake(b.Addr(), "/fail"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if b.Respawns() != 0 {
		t.Fatalf("respawned %d times 350 ms into a 400 ms grace with a request in flight", b.Respawns())
	}
	within(t, 1500*time.Millisecond, "respawn after the grace", func() bool { return b.Respawns() == 1 })
}

func TestLoopL8RespawnOnRelease(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l8", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	b.Acquire()
	if err := postFake(b.Addr(), "/fail"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	b.Release()
	within(t, 400*time.Millisecond, "respawn after release", func() bool { return b.Respawns() == 1 })
}

func TestLoopL9RespawnsThenDead(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l9", []int{0}, "ready_after=50ms", "exit_after=300ms"))
	b := m.Backends()[0]
	within(t, 5*time.Second, "dead", func() bool { return b.State() == StateDead })
	time.Sleep(time.Second)
	if b.State() != StateDead || b.Respawns() != 2 {
		t.Errorf("state=%v respawns=%d, want dead after 2", b.State(), b.Respawns())
	}
	if !h.events.has("marked dead after 2 respawns") {
		t.Error("missing the marked-dead event")
	}
	if !h.freshTicketGranted() {
		t.Error("a dead backend must not hold the gate")
	}
}

func TestLoopL10StartupTimeout(t *testing.T) {
	h := newHarness(t)
	model := fakeModel("l10", []int{0}, "ready_after=1h")
	model.StartupTimeout = config.Duration{Duration: 400 * time.Millisecond}
	m, _ := h.start(model)
	within(t, 3*time.Second, "respawn", func() bool { return m.Backends()[0].Respawns() >= 1 })
	if !h.events.has(CauseStartupTimeout) {
		t.Error("respawn event must name the startup timeout")
	}
}

func TestLoopL11QuickExitRetry(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l11", []int{0}, "first_mode=exit1"))
	b := m.Backends()[0]
	waitHealthy(t, b, 5*time.Second)
	calls := fakeCallsFor("l11")
	if b.Respawns() != 0 || !h.events.has("retrying once on a new port") || len(calls) != 2 || calls[0].Port == calls[1].Port {
		t.Errorf("respawns=%d retry event=%v calls=%d", b.Respawns(), h.events.has("retrying once on a new port"), len(calls))
	}
}

func TestLoopL12AlwaysExits(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l12", []int{0}, "mode=exit1"))
	within(t, 5*time.Second, "dead", func() bool { return m.Backends()[0].State() == StateDead })
}

func TestLoopL13CommandError(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l13", []int{0}, "command_error"))
	b := m.Backends()[0]
	within(t, time.Second, "dead", func() bool { return b.State() == StateDead })
	if !h.events.has("cannot build command") || b.PIDs() != nil {
		t.Errorf("event=%v pids=%v", h.events.has("cannot build command"), b.PIDs())
	}
	if !h.freshTicketGranted() {
		t.Error("a backend that cannot build a command must release the gate")
	}
}

func TestLoopL14Load(t *testing.T) {
	h := newHarness(t)
	m, _ := h.start(fakeModel("l14", []int{0}, "slots=4"))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	if err := postFake(b.Addr(), "/busy?n=3"); err != nil {
		t.Fatal(err)
	}
	within(t, 500*time.Millisecond, "load 4/3", func() bool {
		l, decoded, _, _ := b.lastLoad()
		return l.Slots == 4 && l.Busy == 3 && decoded == 0
	})
}

func TestLoopL15LoadProgress(t *testing.T) {
	h := newHarness(t)
	model := fakeModel("l15", []int{0})
	model.Engine = "fake-progress"
	m, _ := h.start(model)
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	within(t, 500*time.Millisecond, "progress", func() bool {
		_, decoded, remain, _ := b.lastLoad()
		return decoded == 120 && remain == 380
	})
}

// rocmPIDs answers rocm-smi --showpidgpus with the leader of b holding gpuIndex.
func rocmPIDs(pid func() int, gpuIndex string) gpu.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "rocm-smi" || !slices.Equal(args, []string{"--showpidgpus"}) {
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		}
		return []byte(fmt.Sprintf("PID %d is using 1 DRM device(s):\n%s \n", pid(), gpuIndex)), nil
	}
}

func TestLoopL16OnAssignedGPU(t *testing.T) {
	h := newHarness(t)
	h.deps.Vendor = gpu.VendorAMD
	var b *Backend
	h.deps.Run = rocmPIDs(func() int { return b.PIDs()[0] }, "0")
	m, err := newModel(fakeModel("l16", []int{0}), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	b = m.Backends()[0]
	startModel(t, h, m)
	waitHealthy(t, b, 3*time.Second)
	time.Sleep(1500 * time.Millisecond)
	if b.Respawns() != 0 || strings.Count(h.log.String(), "on assigned GPUs [0]") != 1 {
		t.Errorf("respawns=%d log=%q", b.Respawns(), h.log.String())
	}
}

func TestLoopL17WrongGPU(t *testing.T) {
	h := newHarness(t)
	h.deps.Vendor = gpu.VendorAMD
	var b *Backend
	h.deps.Run = rocmPIDs(func() int {
		if p := b.PIDs(); len(p) > 0 {
			return p[0]
		}
		return 0
	}, "7")
	m, err := newModel(fakeModel("l17", []int{0}), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	b = m.Backends()[0]
	startModel(t, h, m)
	waitHealthy(t, b, 3*time.Second)
	within(t, time.Second, "respawn", func() bool { return b.Respawns() >= 1 })
	if !h.events.has("not on assigned GPU") || !h.events.has("want [0]") {
		t.Error("missing the not-on-assigned-GPU event")
	}
}

func TestLoopL18UnknownNamespace(t *testing.T) {
	h := newHarness(t)
	h.deps.Vendor = gpu.VendorAMD
	h.deps.Run = rocmPIDs(func() int { return 999999 }, "0")
	m, _ := h.start(fakeModel("l18", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	time.Sleep(1500 * time.Millisecond)
	if b.Respawns() != 0 || strings.Count(h.log.String(), "on-GPU check unknown") != 1 {
		t.Errorf("respawns=%d log=%q", b.Respawns(), h.log.String())
	}
}

func TestLoopL19ToolFailsThroughTheWindow(t *testing.T) {
	h := newHarness(t)
	h.deps.Vendor = gpu.VendorAMD
	h.deps.Timing.GPUCheckWindow = 300 * time.Millisecond
	h.deps.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, fmt.Errorf("rocm-smi: driver wedged")
	}
	m, _ := h.start(fakeModel("l19", []int{0}))
	b := m.Backends()[0]
	waitHealthy(t, b, 3*time.Second)
	healthyAt := time.Now()
	within(t, 2*time.Second, "unknown line", func() bool { return strings.Contains(h.log.String(), "on-GPU check unknown") })
	if time.Since(healthyAt) < 200*time.Millisecond {
		t.Error("the unknown verdict must wait for the check window")
	}
	time.Sleep(300 * time.Millisecond)
	if b.Respawns() != 0 || strings.Count(h.log.String(), "driver wedged") != 1 {
		t.Errorf("respawns=%d log=%q", b.Respawns(), h.log.String())
	}
}

func TestLoopL20NoCheckWithoutGPUs(t *testing.T) {
	failIfCalled := func(context.Context, string, ...string) ([]byte, error) {
		t.Error("the on-GPU check must not run")
		return nil, fmt.Errorf("unexpected")
	}
	h := newHarness(t)
	h.deps.Run = failIfCalled
	m, _ := h.start(fakeModel("l20-none", []int{0}))
	waitHealthy(t, m.Backends()[0], 3*time.Second)

	h2 := newHarness(t)
	h2.deps.Vendor = gpu.VendorAMD
	h2.deps.Run = failIfCalled
	m2, _ := h2.start(fakeModel("l20-cpu", nil))
	waitHealthy(t, m2.Backends()[0], 3*time.Second)
	time.Sleep(300 * time.Millisecond)
}

func TestLoopL21CancelLeavesProcessRunning(t *testing.T) {
	h := newHarness(t)
	model, err := newModel(fakeModel("l21", []int{0}), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	b := model.Backends()[0]
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		b.run(ctx, h.gate.enqueue(), h.gate, b.PIDs)
		close(returned)
	}()
	t.Cleanup(func() { b.stop(time.Second) })
	waitHealthy(t, b, 3*time.Second)
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1 s of cancel")
	}
	if b.PIDs() == nil {
		t.Error("cancelling the loop must leave the process for its owner to stop")
	}
}

// startModel is harness.start for a model built by the test.
func startModel(t *testing.T, h *loopHarness, model *Model) {
	t.Helper()
	tickets := make([]*loadTicket, len(model.Backends()))
	for i := range tickets {
		tickets[i] = h.gate.enqueue()
	}
	ctx, cancel := context.WithCancel(context.Background())
	nodePIDs := func() []int {
		var all []int
		for _, b := range model.Backends() {
			all = append(all, b.PIDs()...)
		}
		return all
	}
	for i, b := range model.Backends() {
		model.startLoop(ctx, b, tickets[i], h.gate, nodePIDs)
	}
	t.Cleanup(func() {
		cancel()
		model.drain(0)
	})
}
