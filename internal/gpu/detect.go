package gpu

import (
	"context"
	"strings"
)

// Detect resolves gpu.vendor "auto" from the tools present on the machine:
// nvidia when `nvidia-smi -L` lists a GPU, else amd when `rocm-smi --version`
// answers, else none. nvidia is tried first because a CUDA host can carry a
// stray rocm-smi from a toolkit install, while an AMD host has no nvidia-smi
// that lists cards. The output is checked, not just the exit status: an
// nvidia-smi with no driver loaded prints "No devices were found".
func Detect(ctx context.Context, run Runner) Vendor {
	if out, err := run(ctx, "nvidia-smi", "-L"); err == nil && strings.Contains(string(out), "GPU ") {
		return VendorNVIDIA
	}
	if out, err := run(ctx, "rocm-smi", "--version"); err == nil && strings.Contains(string(out), "ROCM-SMI") {
		return VendorAMD
	}
	return VendorNone
}
