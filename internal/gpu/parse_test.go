package gpu

import "testing"

const sampleROCmJSON = `{
  "card0": {
    "GPU use (%)": "85",
    "VRAM Total Memory (B)": "17163091968",
    "VRAM Total Used Memory (B)": "14889222144"
  },
  "card1": {
    "GPU use (%)": "42",
    "VRAM Total Memory (B)": "17163091968",
    "VRAM Total Used Memory (B)": "8589934592"
  }
}`

func TestParseROCmSMI(t *testing.T) {
	samples := ParseROCmSMI([]byte(sampleROCmJSON))
	if len(samples) != 2 { t.Fatalf("expected 2 samples, got %d", len(samples)) }
	s0 := findByGPU(samples, 0)
	if s0 == nil { t.Fatal("missing gpu 0") }
	if s0.Utilization != 85.0 { t.Errorf("gpu0 util: expected 85.0, got %f", s0.Utilization) }
	if int(s0.VRAMTotalMB) != 16368 { t.Errorf("gpu0 vram total: expected 16368, got %d", int(s0.VRAMTotalMB)) }
	if int(s0.VRAMUsedMB) != 14199 { t.Errorf("gpu0 vram used: expected ~14199, got %d", int(s0.VRAMUsedMB)) }
	s1 := findByGPU(samples, 1)
	if s1 == nil { t.Fatal("missing gpu 1") }
	if s1.Utilization != 42.0 { t.Errorf("gpu1 util: expected 42.0, got %f", s1.Utilization) }
}

func TestParseROCmSMIEmpty(t *testing.T) {
	samples := ParseROCmSMI([]byte(`{}`))
	if len(samples) != 0 { t.Errorf("expected 0, got %d", len(samples)) }
}

func TestParseROCmSMIInvalid(t *testing.T) {
	samples := ParseROCmSMI([]byte(`not json`))
	if len(samples) != 0 { t.Errorf("expected 0, got %d", len(samples)) }
}

func findByGPU(samples []GPUSample, id int) *GPUSample {
	for i := range samples { if samples[i].GPUID == id { return &samples[i] } }
	return nil
}
