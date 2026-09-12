package gpu

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// ErrPowerLimitUnsupported is returned for a vendor on which the node does not
// set power limits itself.
var ErrPowerLimitUnsupported = errors.New("gpu power limit is not applied by viiwork on this vendor")

// SetPowerLimit caps one GPU at watts. watts <= 0 means leave the card alone.
//
// Only AMD is supported, with v1's exact command (`rocm-smi
// --setpoweroverdrive <watts> -d <gpu>`), run at every backend start because a
// card's limit resets when the driver does. On NVIDIA the limit belongs to
// host provisioning: `nvidia-smi -pl` needs root and already lives in the
// unit that starts the node, so this returns ErrPowerLimitUnsupported and runs
// nothing.
func SetPowerLimit(ctx context.Context, vendor Vendor, run Runner, gpu, watts int) error {
	if watts <= 0 {
		return nil
	}
	if vendor != VendorAMD {
		return fmt.Errorf("%w (vendor %s)", ErrPowerLimitUnsupported, vendor)
	}
	if _, err := run(ctx, "rocm-smi", "--setpoweroverdrive", strconv.Itoa(watts), "-d", strconv.Itoa(gpu)); err != nil {
		return fmt.Errorf("setting a %d W power limit on GPU %d: %w", watts, gpu, err)
	}
	return nil
}
