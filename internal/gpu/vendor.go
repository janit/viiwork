package gpu

import "fmt"

// Vendor is the GPU stack a node drives. It decides the pinning environment
// variable and the telemetry tool (spec "GPU vendor"). "auto" is a config
// value that is resolved before a Vendor exists, so it is not one.
type Vendor string

const (
	VendorNVIDIA Vendor = "nvidia"
	VendorAMD    Vendor = "amd"
	VendorNone   Vendor = "none"
)

func ParseVendor(s string) (Vendor, error) {
	switch v := Vendor(s); v {
	case VendorNVIDIA, VendorAMD, VendorNone:
		return v, nil
	}
	return "", fmt.Errorf("gpu vendor %q must be nvidia, amd or none", s)
}
