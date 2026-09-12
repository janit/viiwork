package gpu

import (
	"slices"
	"testing"
)

func TestPinningEnv(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin",
		"CUDA_VISIBLE_DEVICES=0,1",
		"ROCR_VISIBLE_DEVICES=5",
		"HIP_VISIBLE_DEVICES=5",
		"HOME=/home/janit",
	}
	cases := []struct {
		name   string
		vendor Vendor
		gpus   []int
		want   []string
	}{
		{"nvidia pair", VendorNVIDIA, []int{0, 1}, []string{"PATH=/usr/bin", "HOME=/home/janit", "CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0,1"}},
		{"amd single", VendorAMD, []int{2}, []string{"PATH=/usr/bin", "HOME=/home/janit", "ROCR_VISIBLE_DEVICES=2"}},
		{"amd order kept", VendorAMD, []int{5, 4}, []string{"PATH=/usr/bin", "HOME=/home/janit", "ROCR_VISIBLE_DEVICES=5,4"}},
		{"nvidia cpu backend", VendorNVIDIA, nil, []string{"PATH=/usr/bin", "HOME=/home/janit"}},
		{"no vendor", VendorNone, []int{0}, []string{"PATH=/usr/bin", "HOME=/home/janit"}},
	}
	for _, tc := range cases {
		if got := PinningEnv(inherited, tc.vendor, tc.gpus); !slices.Equal(got, tc.want) {
			t.Errorf("%s: PinningEnv = %v, want %v", tc.name, got, tc.want)
		}
	}

	if len(inherited) != 5 || inherited[1] != "CUDA_VISIBLE_DEVICES=0,1" {
		t.Errorf("PinningEnv modified its input: %v", inherited)
	}

	extra := PinningEnv([]string{"CUDA_VISIBLE_DEVICES_EXTRA=1", "CUDA_VISIBLE_DEVICES=3"}, VendorNone, nil)
	if !slices.Equal(extra, []string{"CUDA_VISIBLE_DEVICES_EXTRA=1"}) {
		t.Errorf("a variable that only shares a prefix must be kept, got %v", extra)
	}
}
