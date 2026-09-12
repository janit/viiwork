package gpu

import "context"

// Collector is GPU telemetry for one vendor: sampled on the node's health
// tick, recorded into History and broadcast live.
type Collector interface {
	Sample(ctx context.Context)
	Available() bool
	PowerAvailable() bool
}

var (
	_ Collector = (*StatCollector)(nil)
	_ Collector = (*NVIDIACollector)(nil)
)

// NewCollector returns the telemetry collector for vendor: nvidia-smi on
// NVIDIA, the existing rocm-smi collector on AMD, and nil when the node has
// no GPU stack.
func NewCollector(vendor Vendor, history *History, broadcaster *Broadcaster) Collector {
	switch vendor {
	case VendorNVIDIA:
		return NewNVIDIACollector(history, broadcaster, ExecRunner)
	case VendorAMD:
		return NewStatCollector(history, broadcaster)
	}
	return nil
}
