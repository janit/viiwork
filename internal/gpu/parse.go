package gpu

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

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
			GPUID: id, Utilization: util,
			VRAMUsedMB: vramUsed / 1024 / 1024, VRAMTotalMB: vramTotal / 1024 / 1024,
		})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].GPUID < samples[j].GPUID })
	return samples
}
