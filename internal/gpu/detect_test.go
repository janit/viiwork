package gpu

import (
	"context"
	"testing"
)

func TestDetect(t *testing.T) {
	const rocmVersion = "ROCM-SMI version: 3.0.0+55c0f58\nROCM-SMI-LIB version: 7.4.0\n"
	cases := []struct {
		name    string
		outputs map[string]string
		want    Vendor
	}{
		{
			name:    "nvidia",
			outputs: map[string]string{"nvidia-smi -L": "GPU 0: NVIDIA RTX A4000 (UUID: GPU-9ba7c87c-b150-9024-77d4-b1ab37cafc85)\n"},
			want:    VendorNVIDIA,
		},
		{
			name:    "amd",
			outputs: map[string]string{"rocm-smi --version": rocmVersion},
			want:    VendorAMD,
		},
		{
			name: "nvidia-smi without devices falls through to amd",
			outputs: map[string]string{
				"nvidia-smi -L":      "No devices were found\n",
				"rocm-smi --version": rocmVersion,
			},
			want: VendorAMD,
		},
		{name: "no tools", outputs: nil, want: VendorNone},
	}
	for _, tc := range cases {
		if got := Detect(context.Background(), fakeRunner(tc.outputs)); got != tc.want {
			t.Errorf("%s: Detect = %q, want %q", tc.name, got, tc.want)
		}
	}
}
