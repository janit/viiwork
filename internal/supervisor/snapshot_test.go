package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

var qwen = config.Model{Name: "Qwen3.8-27B", Engine: config.EngineLlamaCpp, Context: 49152, Parallel: 2}

func healthyView(id string, slots, busy, inFlight int, ctx int64) backendView {
	return backendView{
		ID: id, GPUs: []int{0}, State: StateHealthy, PID: 100, Uptime: time.Minute,
		Load: engine.Load{Slots: slots, Busy: busy, CtxPerSlot: ctx}, LoadFresh: true, InFlight: inFlight,
	}
}

func TestSnapshotS1TwoHealthy(t *testing.T) {
	views := []backendView{healthyView("Qwen3.8-27B/0", 2, 1, 2, 49152), healthyView("Qwen3.8-27B/1", 2, 0, 0, 49152)}
	c := modelCapacity(qwen, views)
	if c.Slots != 4 || c.Busy != 2 || c.Backends != 2 || c.HealthyBackends != 2 || c.Ctx != 49152 || c.Name != "Qwen3.8-27B" || c.Engine != "llamacpp" {
		t.Errorf("capacity = %+v", c)
	}
}

func TestSnapshotS2UnhealthyExcluded(t *testing.T) {
	sick := healthyView("Qwen3.8-27B/2", 2, 2, 0, 49152)
	sick.State = StateUnhealthy
	views := []backendView{healthyView("Qwen3.8-27B/0", 2, 1, 2, 49152), healthyView("Qwen3.8-27B/1", 2, 0, 0, 49152), sick}
	c := modelCapacity(qwen, views)
	if c.Slots != 4 || c.Busy != 2 || c.Backends != 3 || c.HealthyBackends != 2 {
		t.Errorf("capacity = %+v", c)
	}
	s := modelStatus(qwen, views)
	if len(s.Backends) != 3 || s.Backends[2].Status != "unhealthy" {
		t.Errorf("status backends = %+v", s.Backends)
	}
}

func TestSnapshotS3StaleLoad(t *testing.T) {
	v := healthyView("Qwen3.8-27B/0", 8, 5, 1, 49152)
	v.LoadFresh = false
	v.Decoded = 50
	s := modelStatus(qwen, []backendView{v})
	b := s.Backends[0]
	if s.Slots != 2 || s.Busy != 1 || b.Slots != 2 || b.Busy != 1 || b.TokDecoded != 0 {
		t.Errorf("stale load: model %d/%d backend %+v", s.Slots, s.Busy, b)
	}
}

func TestSnapshotS4Starting(t *testing.T) {
	queued := backendView{ID: "Qwen3.8-27B/0", State: StateStarting, Phase: "queued"}
	loading := backendView{ID: "Qwen3.8-27B/1", State: StateStarting, Phase: "loading"}
	c := modelCapacity(qwen, []backendView{queued, loading})
	if c.Slots != 0 || c.Busy != 0 || c.Ctx != 49152 || c.HealthyBackends != 0 {
		t.Errorf("capacity = %+v", c)
	}
	if s := modelStatus(qwen, []backendView{queued, loading}); s.Backends[0].Phase != "queued" {
		t.Errorf("phase = %q, want queued", s.Backends[0].Phase)
	}
}

func TestSnapshotS5EngineContext(t *testing.T) {
	if c := modelCapacity(qwen, []backendView{healthyView("Qwen3.8-27B/0", 2, 0, 0, 32768)}); c.Ctx != 32768 {
		t.Errorf("Ctx = %d, want 32768 from the engine", c.Ctx)
	}
}

func TestSnapshotS6NotRunningAndCPU(t *testing.T) {
	stopped := backendView{ID: "Qwen3.8-27B/0", GPUs: []int{0}, State: StateDead}
	cpu := backendView{ID: "Qwen3.8-27B/1", State: StateHealthy, PID: 7, Uptime: 90 * time.Second}
	s := modelStatus(qwen, []backendView{stopped, cpu})
	if s.Backends[0].PID != 0 || s.Backends[0].UptimeS != 0 || s.Backends[1].UptimeS != 90 {
		t.Errorf("backends = %+v", s.Backends)
	}
	raw, err := json.Marshal(s.Backends[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"gpus"`) {
		t.Errorf("a CPU backend must omit gpus, got %s", raw)
	}
}
