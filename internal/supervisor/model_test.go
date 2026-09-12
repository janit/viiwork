package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestModelUnknownEngine(t *testing.T) {
	m := fakeModel("tgi-model", []int{0})
	m.Engine = "tgi"
	if _, err := newModel(m, testDeps(&syncBuffer{})); err == nil || !strings.Contains(err.Error(), `engine "tgi" is not registered`) {
		t.Errorf("err = %v", err)
	}
}

// healthyModel starts a one-backend model and returns it with its loop
// cancel func, for tests that drain it themselves.
func healthyModel(t *testing.T, name string) (*Model, context.CancelFunc) {
	t.Helper()
	h := newHarness(t)
	model, err := newModel(fakeModel(name, []int{0}), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := model.Backends()[0]
	model.startLoop(ctx, b, h.gate.enqueue(), h.gate, b.PIDs)
	t.Cleanup(func() {
		cancel()
		model.drain(0)
	})
	waitHealthy(t, b, 3*time.Second)
	return model, cancel
}

func TestModelDrainWaitsForInFlight(t *testing.T) {
	model, cancel := healthyModel(t, "drain-wait")
	b := model.Backends()[0]
	b.Acquire()
	cancel()

	done := make(chan struct{})
	go func() {
		model.drain(2 * time.Second)
		close(done)
	}()
	within(t, 100*time.Millisecond, "admitting off", func() bool { return !model.Admitting() })
	time.Sleep(200 * time.Millisecond)
	if b.PIDs() == nil {
		t.Fatal("drain stopped the process while a request was in flight")
	}
	b.Release()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("drain did not return within 500 ms of the release")
	}
	if b.PIDs() != nil {
		t.Error("process still running after drain")
	}
}

func TestModelDrainGrace(t *testing.T) {
	model, cancel := healthyModel(t, "drain-grace")
	b := model.Backends()[0]
	b.Acquire()
	defer b.Release()
	cancel()
	leader := b.PIDs()[0]
	start := time.Now()
	model.drain(300 * time.Millisecond)
	took := time.Since(start)
	if took < 300*time.Millisecond || took >= 1500*time.Millisecond {
		t.Errorf("drain took %v, want 300 ms to 1.5 s", took)
	}
	waitFor(t, "leader gone", func() bool { return processGone(leader) })
}
