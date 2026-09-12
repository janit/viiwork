package gpu

import (
	"context"
	"errors"
	"testing"
)

func TestSetPowerLimit(t *testing.T) {
	ctx := context.Background()

	amd := fakeRunner(map[string]string{"rocm-smi --setpoweroverdrive 190 -d 3": "ok\n"})
	if err := SetPowerLimit(ctx, VendorAMD, amd, 3, 190); err != nil {
		t.Errorf("amd GPU 3 at 190 W: %v", err)
	}

	if err := SetPowerLimit(ctx, VendorAMD, fakeRunner(nil), 3, 190); err == nil {
		t.Error("amd with rocm-smi failing must return an error")
	}

	if err := SetPowerLimit(ctx, VendorNVIDIA, failingRunner(t), 0, 190); !errors.Is(err, ErrPowerLimitUnsupported) {
		t.Errorf("nvidia: err = %v, want ErrPowerLimitUnsupported", err)
	}

	if err := SetPowerLimit(ctx, VendorAMD, failingRunner(t), 3, 0); err != nil {
		t.Errorf("0 W must do nothing, got %v", err)
	}
}
