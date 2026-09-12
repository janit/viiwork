package gpu

import "testing"

func TestParseVendor(t *testing.T) {
	for _, s := range []string{"nvidia", "amd", "none"} {
		v, err := ParseVendor(s)
		if err != nil || string(v) != s {
			t.Errorf("ParseVendor(%q) = %q, %v", s, v, err)
		}
	}
	for _, s := range []string{"", "auto", "NVIDIA", "intel"} {
		if _, err := ParseVendor(s); err == nil {
			t.Errorf("ParseVendor(%q) must fail", s)
		}
	}
}
