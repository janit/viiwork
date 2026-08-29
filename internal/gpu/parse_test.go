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

// Verbatim from rocm-smi 6.2.4 on gfx906 (gb1) with --showpower.
const sampleROCmJSONWithPower = `{
  "card0": {
    "Current Socket Graphics Package Power (W)": "163.0",
    "GPU use (%)": "97",
    "VRAM Total Memory (B)": "17163091968",
    "VRAM Total Used Memory (B)": "12938014720"
  },
  "card1": {
    "Current Socket Graphics Package Power (W)": "21.0",
    "GPU use (%)": "0",
    "VRAM Total Memory (B)": "17163091968",
    "VRAM Total Used Memory (B)": "10944720"
  }
}`

func TestParseROCmSMIPower(t *testing.T) {
	samples := ParseROCmSMI([]byte(sampleROCmJSONWithPower))
	if len(samples) != 2 { t.Fatalf("expected 2 samples, got %d", len(samples)) }
	if got := findByGPU(samples, 0).PowerW; got != 163.0 { t.Errorf("gpu0 power: expected 163.0, got %f", got) }
	// An idle card still draws real power; that separation is what per-model
	// energy attribution divides by.
	if got := findByGPU(samples, 1).PowerW; got != 21.0 { t.Errorf("gpu1 power: expected 21.0, got %f", got) }
}

// Output from a rocm-smi without --showpower must still parse, leaving power at
// zero rather than failing the sample.
func TestParseROCmSMIWithoutPower(t *testing.T) {
	samples := ParseROCmSMI([]byte(sampleROCmJSON))
	if len(samples) != 2 { t.Fatalf("expected 2 samples, got %d", len(samples)) }
	if got := findByGPU(samples, 0).PowerW; got != 0 { t.Errorf("expected no power, got %f", got) }
	if got := findByGPU(samples, 0).Utilization; got != 85 { t.Errorf("utilisation must survive a missing power field, got %f", got) }
}

// The field name is ROCm-version-specific, so alternates are accepted.
func TestParseROCmSMIAlternatePowerKey(t *testing.T) {
	alt := `{"card0": {"Average Graphics Package Power (W)": "142.0", "GPU use (%)": "50"}}`
	samples := ParseROCmSMI([]byte(alt))
	if len(samples) != 1 || samples[0].PowerW != 142.0 { t.Errorf("expected 142.0 W from the alternate key, got %+v", samples) }
}
