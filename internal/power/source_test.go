package power

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Verbatim captures from a Gigabyte G431-MM0-OT BMC (gb1, 2026-08-28). The
// fleet-wide bug was that the Power Supply class carries no wattage here, so
// these fixtures are the regression: if a change makes auto adopt sdrPresence,
// power reporting silently returns to zero.
const (
	dcmiReading = `
    Instantaneous power reading:                   195 Watts
    Minimum during sampling period:                188 Watts
    Maximum during sampling period:               1940 Watts
    Average power reading over sample period:      194 Watts
    IPMI timestamp:                           08/28/26 07:12:06 UTC    Sampling period:                          00000005 Seconds.
    Power reading state is:                   activated
`
	dcmiUnsupported = `
    DCMI request failed because: Invalid data field in request (cc)
`
	sdrPresence = `PS1_Status       | E6h | ok  | 10.1 | Presence detected
PS2_Status       | E7h | ok  | 10.2 | Presence detected
PS3_Status       | ECh | ok  | 10.3 | 
`
	sensorList = `P_V12S           | 11.895     | Volts      | ok    | na        | 10.270    | 10.790    | 13.130    | 13.650    | na        
P_VCORE          | 0.651      | Volts      | ok    | na        | 0.399     | 0.448     | 1.400     | 1.456     | na        
SYS_POWER        | 175.000    | Watts      | ok    | na        | na        | na        | na        | na        | na        
`
	sensorGet = ` Sensor ID              : SYS_POWER (0xE8)
 Entity ID             : 7.1
 Sensor Type (Analog)  : Current
 Sensor Reading        : 175 (+/- 0) Watts
 Status                : ok
`
	sensorGetTemp = ` Sensor ID              : CPU0_TEMP (0x1)
 Sensor Reading        : 45 (+/- 0) degrees C
 Status                : ok
`
)

// fakeRunner dispatches on the first two arguments, so that "sensor list" and
// "sensor get" are distinguishable the way they are for real ipmitool.
func fakeRunner(responses map[string]string, failures map[string]error) (runner, *[]string) {
	var calls []string
	return func(ctx context.Context, args ...string) ([]byte, error) {
		key := args[0]
		if len(args) > 1 {
			key = args[0] + " " + args[1]
		}
		calls = append(calls, strings.Join(args, " "))
		if err, ok := failures[key]; ok {
			return nil, err
		}
		out, ok := responses[key]
		if !ok {
			return nil, errors.New("command not supported")
		}
		return []byte(out), nil
	}, &calls
}

func probeWith(t *testing.T, spec string, responses map[string]string, failures map[string]error) (*Sampler, []string) {
	t.Helper()
	run, calls := fakeRunner(responses, failures)
	s := &Sampler{run: run}
	s.probe(context.Background(), spec)
	return s, *calls
}

func TestParseDCMIWatts(t *testing.T) {
	w, ok := ParseDCMIWatts(dcmiReading)
	if !ok || w != 195 {
		t.Errorf("expected 195 W ok, got %v ok=%v", w, ok)
	}
}

// The BMC exits 0 on an unsupported DCMI request, so "failed" must be
// distinguishable from a genuine zero.
func TestParseDCMIWattsUnsupported(t *testing.T) {
	if w, ok := ParseDCMIWatts(dcmiUnsupported); ok {
		t.Errorf("expected not-ok for a failed DCMI request, got %v", w)
	}
}

func TestParseSensorWatts(t *testing.T) {
	w, ok := ParseSensorWatts(sensorGet)
	if !ok || w != 175 {
		t.Errorf("expected 175 W ok, got %v ok=%v", w, ok)
	}
}

// Pointing power.source at a non-power sensor must fail the probe rather than
// report degrees as watts.
func TestParseSensorWattsRejectsNonWattUnit(t *testing.T) {
	if w, ok := ParseSensorWatts(sensorGetTemp); ok {
		t.Errorf("expected not-ok for a temperature sensor, got %v", w)
	}
}

func TestParseWattsSensorsFindsOnlyWatts(t *testing.T) {
	found := ParseWattsSensors(sensorList)
	if len(found) != 1 || found[0].Name != "SYS_POWER" || found[0].Watts != 175 {
		t.Fatalf("expected one SYS_POWER=175 sensor, got %+v", found)
	}
}

func TestPickWattsSensorPrefersSystemTotal(t *testing.T) {
	picked, ok := PickWattsSensor([]WattsSensor{
		{Name: "PSU1_PIN", Watts: 90},
		{Name: "SYS_POWER", Watts: 175},
	})
	if !ok || picked.Name != "SYS_POWER" {
		t.Errorf("expected SYS_POWER, got %+v ok=%v", picked, ok)
	}
}

// The headline case: on this hardware auto must reach DCMI and must not settle
// for the presence-only Power Supply class.
func TestProbeAutoPrefersDCMI(t *testing.T) {
	s, calls := probeWith(t, SourceAuto, map[string]string{
		"dcmi power": dcmiReading,
		"sdr type":   sdrPresence,
	}, nil)

	if !s.Available() {
		t.Fatal("expected sampler to be available")
	}
	if s.Watts() != 195 {
		t.Errorf("expected 195 W, got %v", s.Watts())
	}
	if s.SourceName() != "dcmi" {
		t.Errorf("expected dcmi, got %q", s.SourceName())
	}
	// Discovery is lazy: nothing beyond DCMI should have been run.
	if len(calls) != 1 {
		t.Errorf("expected exactly one probe call, got %v", calls)
	}
}

// A presence-only Power Supply class must be rejected, not adopted at 0 W.
func TestProbeAutoSkipsPresenceOnlySDR(t *testing.T) {
	s, calls := probeWith(t, SourceAuto, map[string]string{
		"sdr type":    sdrPresence,
		"sensor list": sensorList, "sensor get": sensorGet,
	}, map[string]error{"dcmi power": errors.New("no dcmi")})

	if !s.Available() {
		t.Fatal("expected fallback to the named sensor")
	}
	if s.SourceName() != "sensor:SYS_POWER" {
		t.Errorf("expected sensor:SYS_POWER, got %q", s.SourceName())
	}
	if s.Watts() != 175 {
		t.Errorf("expected 175 W, got %v", s.Watts())
	}
	// dcmi, the Power Supply class, sensor discovery, then the chosen sensor.
	if len(calls) != 4 {
		t.Errorf("expected dcmi, sdr, sensor list, sensor get; got %v", calls)
	}
}

// A host where the old path worked keeps working, and keeps its old reading.
func TestProbeAutoKeepsWorkingSDR(t *testing.T) {
	s, _ := probeWith(t, SourceAuto, map[string]string{
		"sdr type": "PS1 Input Power | 64h | ok | 10.1 | 280 Watts\nPS2 Input Power | 66h | ok | 10.2 | 275 Watts\n",
	}, map[string]error{"dcmi power": errors.New("no dcmi")})

	if !s.Available() || s.Watts() != 555 {
		t.Errorf("expected 555 W from the SDR class, got %v available=%v", s.Watts(), s.Available())
	}
}

func TestProbeNoSourceLeavesUnavailable(t *testing.T) {
	s, _ := probeWith(t, SourceAuto, map[string]string{
		"sdr type":    sdrPresence,
		"sensor list": "P_V12S | 11.895 | Volts | ok | na\n",
	}, map[string]error{"dcmi power": errors.New("no dcmi")})

	if s.Available() {
		t.Errorf("expected unavailable, got %v W via %s", s.Watts(), s.SourceName())
	}
	if s.Watts() != 0 {
		t.Errorf("expected 0 W when unavailable, got %v", s.Watts())
	}
}

func TestProbeExplicitSourceDoesNotFallBack(t *testing.T) {
	s, calls := probeWith(t, SourceDCMI, map[string]string{
		"sdr type": "PS1 Input Power | 64h | ok | 10.1 | 280 Watts\n",
	}, map[string]error{"dcmi power": errors.New("no dcmi")})

	if s.Available() {
		t.Error("explicit dcmi must not silently fall back to another source")
	}
	if len(calls) != 1 {
		t.Errorf("expected only the dcmi attempt, got %v", calls)
	}
}

func TestProbePinnedSensor(t *testing.T) {
	s, calls := probeWith(t, "sensor:SYS_POWER", map[string]string{"sensor get": sensorGet}, nil)
	if !s.Available() || s.Watts() != 175 {
		t.Errorf("expected 175 W, got %v available=%v", s.Watts(), s.Available())
	}
	if len(calls) != 1 || calls[0] != "sensor get SYS_POWER" {
		t.Errorf("expected a single pinned sensor read, got %v", calls)
	}
}

func TestProbeNoneSkipsProbing(t *testing.T) {
	s, calls := probeWith(t, SourceNone, map[string]string{"dcmi power": dcmiReading}, nil)
	if s.Available() {
		t.Error("expected source none to disable monitoring")
	}
	if len(calls) != 0 {
		t.Errorf("expected no BMC calls, got %v", calls)
	}
}

func TestProbeUnknownSource(t *testing.T) {
	s, calls := probeWith(t, "nonsense", map[string]string{"dcmi power": dcmiReading}, nil)
	if s.Available() || len(calls) != 0 {
		t.Errorf("expected an unknown source to disable monitoring, calls=%v", calls)
	}
}

// After adopting a source, steady-state sampling must keep using it and parse
// with that source's parser rather than the historical SDR one.
func TestSampleUsesAdoptedSource(t *testing.T) {
	responses := map[string]string{"dcmi power": dcmiReading}
	run, calls := fakeRunner(responses, nil)
	s := &Sampler{run: run}
	s.probe(context.Background(), SourceAuto)

	responses["dcmi power"] = strings.Replace(dcmiReading, "195 Watts", "384 Watts", 1)
	s.Sample(context.Background())

	if s.Watts() != 384 {
		t.Errorf("expected 384 W after resample, got %v", s.Watts())
	}
	if len(*calls) != 2 {
		t.Errorf("expected probe + sample, got %v", *calls)
	}
}

// A transient read failure must not zero a live reading.
func TestSampleKeepsLastValueOnUnparseableOutput(t *testing.T) {
	responses := map[string]string{"dcmi power": dcmiReading}
	run, _ := fakeRunner(responses, nil)
	s := &Sampler{run: run}
	s.probe(context.Background(), SourceAuto)

	responses["dcmi power"] = dcmiUnsupported
	s.Sample(context.Background())

	if s.Watts() != 195 {
		t.Errorf("expected the last good 195 W to survive, got %v", s.Watts())
	}
}
