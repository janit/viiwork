package gpu

import (
	"slices"
	"strconv"
	"strings"
)

// deviceVars are the device-selection variables a backend must never inherit.
// HIP_VISIBLE_DEVICES is ROCm's older name for ROCR_VISIBLE_DEVICES and is
// still honoured by its runtime, so an inherited value would silently narrow
// or override the selection this node makes.
var deviceVars = []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"}

// PinningEnv returns environ with every inherited device-selection variable
// removed (matched by exact name, so CUDA_VISIBLE_DEVICES_EXTRA stays) and the
// vendor's pinning for gpus appended: CUDA_DEVICE_ORDER=PCI_BUS_ID plus
// CUDA_VISIBLE_DEVICES on nvidia, ROCR_VISIBLE_DEVICES on amd. The GPU list
// keeps its given order, because llama.cpp's --tensor-split and --main-gpu
// index the visible devices in that order.
//
// Nothing is appended for VendorNone or an empty GPU list, and environ is never
// modified. Stripping first matters on any host where the node itself runs
// with a device variable set (a systemd unit, a container): without it every
// backend would inherit the node's selection instead of its own.
func PinningEnv(environ []string, vendor Vendor, gpus []int) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(deviceVars, name) {
			continue
		}
		out = append(out, kv)
	}
	if len(gpus) == 0 {
		return out
	}
	list := joinInts(gpus)
	switch vendor {
	case VendorNVIDIA:
		// PCI_BUS_ID makes CUDA number cards the way nvidia-smi does; CUDA's
		// default "fastest first" order would not match the configured indices.
		out = append(out, "CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES="+list)
	case VendorAMD:
		out = append(out, "ROCR_VISIBLE_DEVICES="+list)
	}
	return out
}

func joinInts(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}
