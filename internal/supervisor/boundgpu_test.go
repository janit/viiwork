package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// boundEngine is the fake engine plus engine.GPUBindingReader, registered
// under its own name so that every other test's engine stays one that reports
// no binding — which is the majority case in a real fleet too.
type boundEngine struct{ fakeEngine }

var bound struct {
	sync.Mutex
	uuid     string
	reported bool
	err      error
}

func setBoundGPU(uuid string, reported bool, err error) {
	bound.Lock()
	defer bound.Unlock()
	bound.uuid, bound.reported, bound.err = uuid, reported, err
}

func (*boundEngine) BoundGPU(context.Context, string) (string, bool, error) {
	bound.Lock()
	defer bound.Unlock()
	return bound.uuid, bound.reported, bound.err
}

func init() {
	engine.Register(&boundEngine{fakeEngine{name: "fake-bound", client: &http.Client{}}})
}

// nvidiaHost answers the three nvidia-smi calls the checks make: the card list
// for the binding check, and the index/uuid and compute-app tables for the
// on-GPU PID check. The backend's own PID is placed on its assigned card, so
// the PID check passes and only the engine's own report is in question.
func nvidiaHost(pid func() int, cards map[int]string, backendCard int) gpu.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "nvidia-smi" {
			return nil, fmt.Errorf("unexpected command %s", name)
		}
		indexes := make([]int, 0, len(cards))
		for i := range cards {
			indexes = append(indexes, i)
		}
		slices.Sort(indexes)
		switch {
		case slices.Equal(args, []string{"-L"}):
			var out []byte
			for _, i := range indexes {
				out = append(out, fmt.Sprintf("GPU %d: NVIDIA Test (UUID: %s)\n", i, cards[i])...)
			}
			return out, nil
		case slices.Equal(args, []string{"--query-gpu=index,uuid", "--format=csv,noheader"}):
			var out []byte
			for _, i := range indexes {
				out = append(out, fmt.Sprintf("%d, %s\n", i, cards[i])...)
			}
			return out, nil
		case slices.Equal(args, []string{"--query-compute-apps=pid,gpu_uuid", "--format=csv,noheader"}):
			return []byte(fmt.Sprintf("%d, %s\n", pid(), cards[backendCard])), nil
		}
		return nil, fmt.Errorf("unexpected args %v", args)
	}
}

func boundModel(name string, gpus []int) config.Model {
	m := fakeModel(name, gpus)
	m.Engine = "fake-bound"
	return m
}

// A backend whose engine reports a different card than the node pinned it to
// is reported and left alone: still healthy, never respawned. The wrong label
// is the problem; removing a working backend from the mesh would be worse.
func TestBoundGPUMismatchIsReportedNotActedOn(t *testing.T) {
	setBoundGPU("GPU-bbb", true, nil)
	h := newHarness(t)
	h.deps.Vendor = gpu.VendorNVIDIA
	var b *Backend
	h.deps.Run = nvidiaHost(func() int {
		if p := b.PIDs(); len(p) > 0 {
			return p[0]
		}
		return 0
	}, map[int]string{0: "GPU-aaa", 1: "GPU-bbb"}, 0)

	m, err := newModel(boundModel("bnd", []int{0}), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	b = m.Backends()[0]
	startModel(t, h, m)
	waitHealthy(t, b, 3*time.Second)

	within(t, 2*time.Second, "wrong-GPU event", func() bool {
		return h.events.has("is on the wrong GPU")
	})
	if !h.events.has("expected GPU-aaa") || !h.events.has("engine bound GPU-bbb") {
		t.Error("the event must name both cards")
	}
	time.Sleep(300 * time.Millisecond)
	if b.State() != StateHealthy {
		t.Errorf("state = %v, want it left healthy", b.State())
	}
	if b.Respawns() != 0 {
		t.Errorf("respawns = %d, want 0: a mismatch is reported, not acted on", b.Respawns())
	}
}

// Everything that is merely unknown stays silent. A check that guessed would
// fire on every node running an engine that predates the field.
func TestBoundGPUUnknownCasesAreSilent(t *testing.T) {
	cards := map[int]string{0: "GPU-aaa", 1: "GPU-bbb"}
	for _, tc := range []struct {
		name     string
		uuid     string
		reported bool
		gpus     []int
	}{
		{name: "engine reports the right card", uuid: "GPU-aaa", reported: true, gpus: []int{0}},
		{name: "engine reports nothing", uuid: "", reported: false, gpus: []int{0}},
		{name: "engine reports an empty uuid", uuid: "", reported: true, gpus: []int{0}},
		{name: "multi-card backend has no single expected card", uuid: "GPU-bbb", reported: true, gpus: []int{0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBoundGPU(tc.uuid, tc.reported, nil)
			h := newHarness(t)
			h.deps.Vendor = gpu.VendorNVIDIA
			var b *Backend
			h.deps.Run = nvidiaHost(func() int {
				if p := b.PIDs(); len(p) > 0 {
					return p[0]
				}
				return 0
			}, cards, tc.gpus[0])

			m, err := newModel(boundModel("bnd-"+tc.name, tc.gpus), h.deps)
			if err != nil {
				t.Fatal(err)
			}
			b = m.Backends()[0]
			startModel(t, h, m)
			waitHealthy(t, b, 3*time.Second)
			time.Sleep(300 * time.Millisecond)
			if h.events.has("is on the wrong GPU") {
				t.Error("an unknown binding must not be reported as a mismatch")
			}
		})
	}
}
