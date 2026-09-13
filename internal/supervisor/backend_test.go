package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/engine/llamacpp"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// The llamacpp engine reports token progress. Only this test imports it:
// supervisor code stays engine-blind.
var _ engine.TokenProgressReader = llamacpp.New()

// fakeModel is a models[] entry on the fake engine; name must be unique per
// test because the fake engine's call log is keyed by it. Building it forgets
// earlier calls for that name, so -count=N runs start clean.
func fakeModel(name string, gpus []int, args ...string) config.Model {
	resetFakeCalls(name)
	per := 1
	if len(gpus) > 1 {
		per = len(gpus)
	}
	return config.Model{Name: name, Engine: "fake", Path: "/fake/" + name, GPUs: gpus, GPUsPerBackend: per, Context: 4096, Parallel: 2, Args: args}
}

func testDeps(log *syncBuffer) Deps {
	return Deps{
		Vendor: gpu.VendorNone,
		Health: config.HealthConfig{
			Interval:     config.Duration{Duration: 100 * time.Millisecond},
			Timeout:      config.Duration{Duration: 500 * time.Millisecond},
			MaxFailures:  2,
			RespawnGrace: config.Duration{Duration: 400 * time.Millisecond},
			MaxRespawns:  2,
		},
		Log: log,
		Timing: Timing{
			StartProbeInterval: 50 * time.Millisecond,
			LoadInterval:       50 * time.Millisecond,
			LoadStaleAfter:     150 * time.Millisecond,
			GPUCheckWindow:     time.Second,
			RespawnStopGrace:   200 * time.Millisecond,
			QuickExitWindow:    500 * time.Millisecond,
		},
	}
}

func mustEngine(t *testing.T, name string) engine.Engine {
	t.Helper()
	e, ok := engine.Lookup(name)
	if !ok {
		t.Fatalf("engine %q not registered", name)
	}
	return e
}

// launched starts one backend and stops it at cleanup.
func launched(t *testing.T, m config.Model, deps Deps) *Backend {
	t.Helper()
	b := newBackend(m, 0, mustEngine(t, m.Engine), deps)
	if err := b.launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.stop(time.Second) })
	return b
}

func TestBackendEnv(t *testing.T) {
	got := backendEnv(
		[]string{"PATH=/bin", "ROCR_VISIBLE_DEVICES=9", "SHARED=inherited"},
		gpu.VendorAMD, []int{0, 1},
		[]string{"ENGINE_VAR=1", "SHARED=engine"},
		map[string]string{"SHARED": "model", "B": "2", "A": "1"},
	)
	want := []string{"PATH=/bin", "SHARED=inherited", "ROCR_VISIBLE_DEVICES=0,1", "ENGINE_VAR=1", "SHARED=engine", "A=1", "B=2", "SHARED=model"}
	if !slices.Equal(got, want) {
		t.Errorf("backendEnv =\n %v\nwant\n %v", got, want)
	}
}

func TestBackendChildSeesLastEnvValue(t *testing.T) {
	log := &syncBuffer{}
	deps := testDeps(log)
	deps.Vendor = gpu.VendorAMD
	deps.Environ = func() []string { return []string{"ROCR_VISIBLE_DEVICES=9", "SHARED=inherited"} }
	m := fakeModel("env-last", []int{0, 1}, "print_env=SHARED,ROCR_VISIBLE_DEVICES")
	m.Env = map[string]string{"SHARED": "model"}
	launched(t, m, deps)
	waitFor(t, "printed env", func() bool { return strings.Contains(log.String(), "ROCR_VISIBLE_DEVICES=") })
	out := log.String()
	if !strings.Contains(out, "SHARED=model") || !strings.Contains(out, "ROCR_VISIBLE_DEVICES=0,1") || strings.Contains(out, "ROCR_VISIBLE_DEVICES=9") {
		t.Errorf("child output = %q, want SHARED=model and ROCR_VISIBLE_DEVICES=0,1 only", out)
	}
}

func TestFreeLoopbackPort(t *testing.T) {
	for i := 0; i < 2; i++ {
		port, err := freeLoopbackPort()
		if err != nil || port <= 0 {
			t.Fatalf("freeLoopbackPort = %d, %v", port, err)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatalf("port %d not listenable right after: %v", port, err)
		}
		ln.Close()
	}
}

func TestBackendLaunch(t *testing.T) {
	deps := testDeps(&syncBuffer{})
	deps.Vendor = gpu.VendorAMD
	b := launched(t, fakeModel("launch", []int{3, 4}), deps)

	calls := fakeCallsFor("launch")
	if len(calls) != 1 {
		t.Fatalf("Command called %d times, want 1", len(calls))
	}
	if b.Addr() != fmt.Sprintf("127.0.0.1:%d", calls[0].Port) {
		t.Errorf("Addr = %q, recorded port %d", b.Addr(), calls[0].Port)
	}
	if pids := b.PIDs(); len(pids) == 0 {
		t.Error("PIDs empty while running")
	}
	if !slices.Equal(calls[0].Spec.GPUs, []int{3, 4}) || calls[0].Spec.Vendor != gpu.VendorAMD {
		t.Errorf("spec = %+v", calls[0].Spec)
	}

	b.GPUs()[0] = 99 // a caller mutating the copy must not reach the backend
	b.stop(time.Second)
	if err := b.launch(); err != nil {
		t.Fatal(err)
	}
	if calls := fakeCallsFor("launch"); !slices.Equal(calls[1].Spec.GPUs, []int{3, 4}) {
		t.Errorf("second spec GPUs = %v, want [3 4]", calls[1].Spec.GPUs)
	}
}

func TestBackendCommandError(t *testing.T) {
	b := newBackend(fakeModel("cmd-error", []int{0}, "command_error"), 0, mustEngine(t, "fake"), testDeps(&syncBuffer{}))
	err := b.launch()
	if !errors.Is(err, errCommand) {
		t.Errorf("err = %v, want errCommand", err)
	}
	if b.PIDs() != nil {
		t.Error("PIDs must be nil after a failed launch")
	}
}

func TestBackendMissingBinary(t *testing.T) {
	b := newBackend(fakeModel("missing-bin", []int{0}, "path=/nonexistent/binary"), 0, mustEngine(t, "fake"), testDeps(&syncBuffer{}))
	err := b.launch()
	if err == nil || errors.Is(err, errCommand) {
		t.Errorf("err = %v, want a start failure that is not errCommand", err)
	}
}

type recordingRunner struct {
	mu   sync.Mutex
	keys []string
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, strings.Join(append([]string{name}, args...), " "))
	return []byte("ok\n"), nil
}

func TestBackendPowerLimit(t *testing.T) {
	rec := &recordingRunner{}
	deps := testDeps(&syncBuffer{})
	deps.Vendor = gpu.VendorAMD
	deps.Run = rec.run
	deps.PowerLimitWatts = 190
	launched(t, fakeModel("power-amd", []int{3, 4}), deps)
	want := []string{"rocm-smi --setpoweroverdrive 190 -d 3", "rocm-smi --setpoweroverdrive 190 -d 4"}
	if !slices.Equal(rec.keys, want) {
		t.Errorf("runner saw %v, want %v", rec.keys, want)
	}

	log := &syncBuffer{}
	nv := testDeps(log)
	nv.Vendor = gpu.VendorNVIDIA
	nvRec := &recordingRunner{}
	nv.Run = nvRec.run
	nv.PowerLimitWatts = 190
	b := launched(t, fakeModel("power-nvidia", []int{0}), nv)
	b.stop(time.Second)
	if err := b.launch(); err != nil {
		t.Fatal(err)
	}
	if len(nvRec.keys) != 0 {
		t.Errorf("nvidia ran %v, want nothing", nvRec.keys)
	}
	if n := strings.Count(log.String(), "host provisioning"); n != 1 {
		t.Errorf("host provisioning logged %d times across two launches, want 1", n)
	}
}

func TestBackendStop(t *testing.T) {
	b := launched(t, fakeModel("stop", []int{0}), testDeps(&syncBuffer{}))
	leader := b.PIDs()[0]
	b.stop(time.Second)
	if b.Addr() != "" || b.PIDs() != nil {
		t.Errorf("after stop: Addr=%q PIDs=%v", b.Addr(), b.PIDs())
	}
	waitFor(t, "old leader gone", func() bool { return processGone(leader) })
}

func TestBackendCounters(t *testing.T) {
	b := newBackend(fakeModel("counters", []int{0}), 0, mustEngine(t, "fake"), testDeps(&syncBuffer{}))
	b.Acquire()
	b.Acquire()
	b.Release()
	if b.InFlight() != 1 {
		t.Errorf("InFlight = %d, want 1", b.InFlight())
	}
	b.Release()
	defer func() {
		if recover() == nil {
			t.Error("Release below zero must panic")
		}
	}()
	b.Release()
}

func TestBackendSlots(t *testing.T) {
	now := time.Now()
	deps := testDeps(&syncBuffer{})
	deps.Now = func() time.Time { return now }
	b := newBackend(fakeModel("slots", []int{0}), 0, mustEngine(t, "fake"), deps)
	if b.Slots() != 2 {
		t.Errorf("no load: Slots = %d, want parallel 2", b.Slots())
	}
	b.mu.Lock()
	b.load, b.loadAt = engine.Load{Slots: 4}, now
	b.mu.Unlock()
	if b.Slots() != 4 {
		t.Errorf("fresh load: Slots = %d, want 4", b.Slots())
	}
	now = now.Add(deps.Timing.LoadStaleAfter)
	if b.Slots() != 2 {
		t.Errorf("stale load: Slots = %d, want parallel 2", b.Slots())
	}
}

func TestBackendID(t *testing.T) {
	b := newBackend(config.Model{Name: "Qwen3.8-27B", Engine: "fake", GPUs: []int{0, 1}, GPUsPerBackend: 1}, 1, mustEngine(t, "fake"), testDeps(&syncBuffer{}))
	if b.ID() != "Qwen3.8-27B/1" || b.Index() != 1 || !slices.Equal(b.GPUs(), []int{1}) {
		t.Errorf("ID=%q Index=%d GPUs=%v", b.ID(), b.Index(), b.GPUs())
	}
}

func TestBackendHardFailureBeforeLaunch(t *testing.T) {
	b := newBackend(fakeModel("hard-early", []int{0}), 0, mustEngine(t, "fake"), testDeps(&syncBuffer{}))
	done := make(chan struct{})
	go func() {
		b.NoteHardFailure()
		b.NoteHardFailure()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("NoteHardFailure blocked before launch")
	}
}
