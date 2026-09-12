package node

import (
	"sync"
	"testing"

	"github.com/janit/viiwork/v2/internal/gpu"
)

type fakeIPMI struct {
	mu        sync.Mutex
	watts     float64
	available bool
	source    string
}

func (f *fakeIPMI) Watts() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watts
}

func (f *fakeIPMI) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

func (f *fakeIPMI) SourceName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.source
}

type fakeGPUs []gpu.GPUSample

func (f fakeGPUs) Latest() []gpu.GPUSample { return f }

func TestNodePower(t *testing.T) {
	t.Run("P1 IPMI", func(t *testing.T) {
		p := NewNodePower(&fakeIPMI{watts: 412, available: true, source: "dcmi"}, fakeGPUs{{GPUID: 0, PowerW: 100}}, gpu.VendorNVIDIA, true)
		if p.Watts() != 412 || !p.Available() || p.Source() != "dcmi" || !p.Chassis() {
			t.Errorf("watts=%v available=%v source=%q chassis=%v", p.Watts(), p.Available(), p.Source(), p.Chassis())
		}
	})

	t.Run("P2 NVIDIA sum", func(t *testing.T) {
		p := NewNodePower(&fakeIPMI{}, fakeGPUs{{GPUID: 0, PowerW: 71.3}, {GPUID: 1, PowerW: 18.95}}, gpu.VendorNVIDIA, true)
		if w := p.Watts(); w < 90.249 || w > 90.251 || !p.Available() || p.Source() != "nvidia-smi" || p.Chassis() {
			t.Errorf("watts=%v available=%v source=%q chassis=%v", w, p.Available(), p.Source(), p.Chassis())
		}
	})

	t.Run("P3 AMD sum", func(t *testing.T) {
		p := NewNodePower(&fakeIPMI{}, fakeGPUs{{GPUID: 0, PowerW: 150}, {GPUID: 1, PowerW: 20}}, gpu.VendorAMD, true)
		if p.Source() != "rocm-smi" || p.Watts() != 170 {
			t.Errorf("watts=%v source=%q", p.Watts(), p.Source())
		}
	})

	t.Run("P4 nothing", func(t *testing.T) {
		p := NewNodePower(&fakeIPMI{}, fakeGPUs{{GPUID: 0}}, gpu.VendorAMD, false)
		if p.Available() || p.Source() != "" || p.Watts() != 0 {
			t.Errorf("watts=%v available=%v source=%q", p.Watts(), p.Available(), p.Source())
		}
		if q := NewNodePower(&fakeIPMI{}, fakeGPUs{{GPUID: 0}}, gpu.VendorAMD, true); q.Available() || q.Source() != "" {
			t.Errorf("GPU power available but reading 0 W: available=%v source=%q", q.Available(), q.Source())
		}
	})

	t.Run("P5 BMC answers later", func(t *testing.T) {
		ipmi := &fakeIPMI{}
		p := NewNodePower(ipmi, fakeGPUs{{GPUID: 0, PowerW: 50}}, gpu.VendorNVIDIA, true)
		if p.Chassis() {
			t.Fatal("chassis before the BMC answered")
		}
		ipmi.mu.Lock()
		ipmi.watts, ipmi.available, ipmi.source = 400, true, "sdr"
		ipmi.mu.Unlock()
		if p.Watts() != 400 || !p.Chassis() || p.Source() != "sdr" {
			t.Errorf("watts=%v chassis=%v source=%q", p.Watts(), p.Chassis(), p.Source())
		}
	})

	t.Run("nil sources", func(t *testing.T) {
		p := NewNodePower(nil, nil, gpu.VendorNone, false)
		if p.Available() || p.Watts() != 0 || p.Source() != "" || p.Chassis() {
			t.Error("nil sources must read as unavailable")
		}
	})
}
