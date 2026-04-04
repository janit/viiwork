package power

import (
	"math"
	"testing"
)

func TestParseWattsTypicalOutput(t *testing.T) {
	output := `PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts
PS1 Curr Out %     | 65h | ok  | 10.1 | 34 percent
PS2 Input Power    | 66h | ok  | 10.2 | 275 Watts
PS2 Status         | 67h | ok  | 10.2 | Presence detected`

	watts := ParseWatts(output)
	if watts != 555.0 {
		t.Errorf("expected 555.0, got %f", watts)
	}
}

func TestParseWattsSinglePSU(t *testing.T) {
	output := `Pwr Consumption    | 70h | ok  | 10.1 | 320 Watts`
	watts := ParseWatts(output)
	if watts != 320.0 {
		t.Errorf("expected 320.0, got %f", watts)
	}
}

func TestParseWattsEmptyOutput(t *testing.T) {
	watts := ParseWatts("")
	if watts != 0.0 {
		t.Errorf("expected 0.0, got %f", watts)
	}
}

func TestParseWattsNoWattsRows(t *testing.T) {
	output := `PS1 Curr Out %     | 65h | ok  | 10.1 | 34 percent
PS2 Status         | 67h | ok  | 10.2 | Presence detected`
	watts := ParseWatts(output)
	if watts != 0.0 {
		t.Errorf("expected 0.0, got %f", watts)
	}
}

func TestParseWattsMalformedLines(t *testing.T) {
	output := `some garbage without pipes
PS1 Input Power    | 64h | ok  | 10.1 | 280 Watts
incomplete | line
PS2 Input Power    | 66h | ok  | 10.2 | not a number Watts`
	watts := ParseWatts(output)
	// Only first valid line parses (280), second "Watts" line has non-numeric prefix
	if watts != 280.0 {
		t.Errorf("expected 280.0, got %f", watts)
	}
}

func TestParseWattsDecimalWatts(t *testing.T) {
	output := `System Level       | 71h | ok  | 10.1 | 283.50 Watts`
	watts := ParseWatts(output)
	if math.Abs(watts-283.5) > 0.01 {
		t.Errorf("expected 283.5, got %f", watts)
	}
}
