package gpu

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// powerKeys are the field names rocm-smi has used for GPU package power. The
// name is ROCm-version-specific -- 6.2.4 on gfx906 reports the first -- so the
// parser accepts any of them rather than tying per-GPU power to one ROCm build.
var powerKeys = []string{
	"Current Socket Graphics Package Power (W)",
	"Average Graphics Package Power (W)",
	"Current Graphics Package Power (W)",
	"Socket Graphics Package Power (W)",
}

// firstFloat returns the first key present and numeric, so a missing field is
// distinguishable from a field that is genuinely zero.
func firstFloat(fields map[string]string, keys []string) float64 {
	for _, key := range keys {
		if raw, ok := fields[key]; ok {
			if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
				return v
			}
		}
	}
	return 0
}

func ParseROCmSMI(data []byte) []GPUSample {
	var raw map[string]map[string]string
	if err := json.Unmarshal(data, &raw); err != nil { return nil }
	var samples []GPUSample
	for card, fields := range raw {
		if !strings.HasPrefix(card, "card") { continue }
		id, err := strconv.Atoi(strings.TrimPrefix(card, "card"))
		if err != nil { continue }
		util, _ := strconv.ParseFloat(fields["GPU use (%)"], 64)
		vramTotal, _ := strconv.ParseFloat(fields["VRAM Total Memory (B)"], 64)
		vramUsed, _ := strconv.ParseFloat(fields["VRAM Total Used Memory (B)"], 64)
		samples = append(samples, GPUSample{
			GPUID: id, Utilization: util, PowerW: firstFloat(fields, powerKeys),
			VRAMUsedMB: vramUsed / 1024 / 1024, VRAMTotalMB: vramTotal / 1024 / 1024,
		})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].GPUID < samples[j].GPUID })
	return samples
}
