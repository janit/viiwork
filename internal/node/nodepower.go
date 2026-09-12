// Package node composes viiwork 2's node: one process per machine that runs
// its models, joins the mesh, routes, serves aliases, dashboards and status,
// reloads models on SIGHUP and shuts down in order.
package node

import (
	"github.com/janit/viiwork/v2/internal/gpu"
)

// NodePower is the machine's power reading (P6 Decision 3).
type NodePower interface {
	Watts() float64
	Available() bool
	Source() string // "" when unavailable
	Chassis() bool  // true when the reading is whole-machine IPMI
}

type ipmiReader interface {
	Watts() float64
	Available() bool
	SourceName() string
}

type gpuLatest interface {
	Latest() []gpu.GPUSample
}

type nodePower struct {
	ipmi              ipmiReader
	gpus              gpuLatest
	vendor            gpu.Vendor
	gpuPowerAvailable bool
}

// NewNodePower reads IPMI when the sampler has a source, and otherwise the sum
// of GPU board power. The choice is made on every call, so a BMC that starts
// answering later wins.
func NewNodePower(ipmi ipmiReader, gpus gpuLatest, vendor gpu.Vendor, gpuPowerAvailable bool) NodePower {
	return &nodePower{ipmi: ipmi, gpus: gpus, vendor: vendor, gpuPowerAvailable: gpuPowerAvailable}
}

func (p *nodePower) ipmiUp() bool { return p.ipmi != nil && p.ipmi.Available() }

// gpuSum is the summed board power, and whether that counts as a reading.
func (p *nodePower) gpuSum() (float64, bool) {
	if !p.gpuPowerAvailable || p.gpus == nil {
		return 0, false
	}
	sum := 0.0
	for _, s := range p.gpus.Latest() {
		sum += s.PowerW
	}
	return sum, sum > 0
}

func (p *nodePower) Watts() float64 {
	if p.ipmiUp() {
		return p.ipmi.Watts()
	}
	w, _ := p.gpuSum()
	return w
}

func (p *nodePower) Available() bool {
	if p.ipmiUp() {
		return true
	}
	_, ok := p.gpuSum()
	return ok
}

func (p *nodePower) Source() string {
	if p.ipmiUp() {
		return p.ipmi.SourceName()
	}
	if _, ok := p.gpuSum(); !ok {
		return ""
	}
	switch p.vendor {
	case gpu.VendorNVIDIA:
		return "nvidia-smi"
	case gpu.VendorAMD:
		return "rocm-smi"
	}
	return ""
}

func (p *nodePower) Chassis() bool { return p.ipmiUp() }
