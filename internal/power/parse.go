package power

import (
	"strconv"
	"strings"
)

// ParseWatts parses ipmitool sdr output and returns total wattage.
// Expects 5 pipe-delimited fields per line: name | sensor_id | status | entity | value.
// Only sums lines where the value field contains "Watts".
func ParseWatts(output string) float64 {
	var total float64
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 5 {
			continue
		}
		value := strings.TrimSpace(fields[4])
		if !strings.Contains(value, "Watts") {
			continue
		}
		numStr := strings.TrimSpace(strings.TrimSuffix(value, "Watts"))
		w, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			continue
		}
		total += w
	}
	return total
}
