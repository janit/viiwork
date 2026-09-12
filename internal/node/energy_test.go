package node

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/config"
)

type fakePower struct {
	mu        sync.Mutex
	watts     float64
	available bool
	source    string
	chassis   bool
}

func (p *fakePower) Watts() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watts
}
func (p *fakePower) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.available
}
func (p *fakePower) Source() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.source
}
func (p *fakePower) Chassis() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chassis
}

func TestGPUOwners(t *testing.T) {
	got := gpuOwners([]config.Model{{Name: "a", GPUs: []int{0, 1}}, {Name: "b", GPUs: []int{2}}})
	if want := map[int]string{0: "a", 1: "a", 2: "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("E1: %v", got)
	}
}

func TestGPUReadings(t *testing.T) {
	samples := fakeGPUs{{GPUID: 0, PowerW: 150}, {GPUID: 1, PowerW: 0}, {GPUID: 2, PowerW: 20}, {GPUID: 7, PowerW: 99}}
	owners := func() map[int]string { return map[int]string{0: "a", 1: "a", 2: "b"} }
	got := gpuReadings(samples, []int{0, 1, 2}, owners)()
	want := []energy.GPUReading{{GPUID: 0, Watts: 150, Model: "a"}, {GPUID: 2, Watts: 20, Model: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("E2: %+v", got)
	}
}

type fakeReadings struct {
	mu   sync.Mutex
	list []energy.GPUReading
}

func (f *fakeReadings) set(list ...energy.GPUReading) {
	f.mu.Lock()
	f.list = list
	f.mu.Unlock()
}

func (f *fakeReadings) get() []energy.GPUReading {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]energy.GPUReading(nil), f.list...)
}

// recordTwoMinutes records an idle minute (20 W cards) and then a loaded one
// (100 W cards), and returns the loaded minute's node watts and summed GPU
// attribution.
func recordTwoMinutes(t *testing.T, pw *fakePower, idleNode, loadedNode float64) (nodeW, attributed float64) {
	t.Helper()
	store, err := energy.Open(energy.Config{Dir: t.TempDir(), GPUIDs: []int{0, 1}, Location: time.UTC}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	readings := &fakeReadings{}
	rec := newRecorder(store, 30*time.Second, pw, readings.get)

	t0 := time.Now().Truncate(time.Minute).Add(-10 * time.Minute)
	pw.mu.Lock()
	pw.watts = idleNode
	pw.mu.Unlock()
	readings.set(energy.GPUReading{GPUID: 0, Watts: 20, Model: "a"}, energy.GPUReading{GPUID: 1, Watts: 20, Model: "a"})
	rec.Sample(t0.Add(time.Second))
	rec.Sample(t0.Add(31 * time.Second))

	pw.mu.Lock()
	pw.watts = loadedNode
	pw.mu.Unlock()
	readings.set(energy.GPUReading{GPUID: 0, Watts: 100, Model: "a"}, energy.GPUReading{GPUID: 1, Watts: 100, Model: "a"})
	rec.Sample(t0.Add(61 * time.Second))
	rec.Sample(t0.Add(91 * time.Second))
	rec.Flush(t0.Add(119 * time.Second))

	loaded := t0.Add(time.Minute).Unix()
	for _, n := range store.ReadNode(energy.TierMinute, t0.Add(-time.Minute), t0.Add(3*time.Minute)) {
		if n.TS == loaded {
			nodeW = float64(n.Watts)
		}
	}
	for _, g := range store.ReadGPU(energy.TierMinute, t0.Add(-time.Minute), t0.Add(3*time.Minute)) {
		if g.TS == loaded {
			attributed += float64(g.AttrW)
		}
	}
	if nodeW == 0 {
		t.Fatal("the loaded minute was not recorded")
	}
	return nodeW, attributed
}

func TestNewRecorderAttribution(t *testing.T) {
	nodeW, attributed := recordTwoMinutes(t, &fakePower{available: true, source: "dcmi", chassis: true}, 240, 400)
	if baseline := nodeW - attributed; nodeW != 400 || attributed <= 0 || baseline <= 0 {
		t.Errorf("E3 chassis: node %v W, attributed %v W, baseline %v W; want marginal attribution with a positive baseline", nodeW, attributed, baseline)
	}

	nodeW, attributed = recordTwoMinutes(t, &fakePower{available: true, source: "nvidia-smi"}, 40, 200)
	if nodeW != 200 || attributed < 199.9 || attributed > 200.1 {
		t.Errorf("E3 direct: node %v W, attributed %v W; want every watt attributed", nodeW, attributed)
	}
}

func TestOpenEnergySource(t *testing.T) {
	cfg := config.EnergyConfig{Enabled: true}

	cfg.Dir = t.TempDir()
	store, err := openEnergy(cfg, "Europe/Helsinki", []int{0}, &fakePower{source: "dcmi"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := os.Stat(filepath.Join(cfg.Dir, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("E4: power unavailable wrote a source file: %v", err)
	}

	cfg.Dir = t.TempDir()
	store, err = openEnergy(cfg, "not/a-zone", []int{0}, &fakePower{available: true, source: "dcmi", chassis: true}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if raw, err := os.ReadFile(filepath.Join(cfg.Dir, "source")); err != nil || strings.TrimSpace(string(raw)) != "dcmi" {
		t.Errorf("E4: source file = %q, %v", raw, err)
	}
}

func TestNewCostTracker(t *testing.T) {
	logs := &lines{}
	if tr := newCostTracker(config.CostConfig{}, "key", &fakePower{}, logs.logf); tr != nil {
		t.Error("E5: no bidding zone must mean no tracker")
	}
	if tr := newCostTracker(config.CostConfig{BiddingZone: "10YFI-1--------U"}, "", &fakePower{}, logs.logf); tr != nil || logs.count("ENTSOE_API_KEY") != 1 {
		t.Errorf("E5: zone without key: tracker %v, warnings %d", tr, logs.count("ENTSOE_API_KEY"))
	}
	if tr := newCostTracker(config.CostConfig{BiddingZone: "10YFI-1--------U", Timezone: "Europe/Helsinki"}, "key", &fakePower{}, logs.logf); tr == nil {
		t.Error("E5: zone and key must build a tracker")
	}
}
