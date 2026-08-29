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

// ParseDCMIWatts parses "ipmitool dcmi power reading" and returns the
// instantaneous whole-node reading.
//
// It reports ok rather than falling back to 0 because a BMC that cannot serve
// the request still exits 0, printing "DCMI request failed because: ..." and no
// reading at all. Zero watts is indistinguishable from a real measurement of
// nothing, and silently recording it would poison both the cost rate and any
// energy integral built on top of this.
func ParseDCMIWatts(output string) (float64, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Instantaneous power reading") {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		return leadingWatts(value)
	}
	return 0, false
}

// ParseSensorWatts parses "ipmitool sensor get <NAME>", whose reading line looks
// like " Sensor Reading        : 175 (+/- 0) Watts".
//
// The unit is checked so that pointing power.source at a temperature or voltage
// sensor fails the probe instead of reporting degrees as watts.
func ParseSensorWatts(output string) (float64, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "Sensor Reading") {
			continue
		}
		_, value, found := strings.Cut(line, ":")
		if !found || !strings.Contains(value, "Watts") {
			continue
		}
		return leadingWatts(value)
	}
	return 0, false
}

// WattsSensor is one Watts-valued row of "ipmitool sensor list".
type WattsSensor struct {
	Name  string
	Watts float64
}

// ParseWattsSensors returns every sensor reporting Watts. Fields are
// name | reading | unit | status | thresholds...
func ParseWattsSensors(output string) []WattsSensor {
	var found []WattsSensor
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 3 || strings.TrimSpace(fields[2]) != "Watts" {
			continue
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		if err != nil {
			continue
		}
		found = append(found, WattsSensor{Name: strings.TrimSpace(fields[0]), Watts: w})
	}
	return found
}

// nodeSensorHints name whole-node readings, in descending order of confidence.
// A board that exposes both a system total and per-PSU inputs would otherwise
// have its first row picked, which on some chassis is a single PSU.
var nodeSensorHints = []string{"sys", "total", "consumption", "pwr"}

// PickWattsSensor chooses the most likely whole-node sensor. This is a
// heuristic and the last resort in auto mode: the caller is expected to log
// what it picked and what else was on offer, so an operator can pin the right
// one explicitly with power.source: "sensor:<NAME>".
func PickWattsSensor(sensors []WattsSensor) (WattsSensor, bool) {
	if len(sensors) == 0 {
		return WattsSensor{}, false
	}
	for _, hint := range nodeSensorHints {
		for _, s := range sensors {
			if strings.Contains(strings.ToLower(s.Name), hint) {
				return s, true
			}
		}
	}
	return sensors[0], true
}

// leadingWatts pulls the number off the front of a value like "195 Watts" or
// "175 (+/- 0) Watts".
func leadingWatts(s string) (float64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	w, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return w, true
}
