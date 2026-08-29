package power

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Source values accepted by NewSampler.
const (
	// SourceAuto probes every known source and keeps the first that returns a
	// plausible wattage. This is the default.
	SourceAuto = "auto"
	// SourceDCMI reads the DCMI whole-node power reading.
	SourceDCMI = "dcmi"
	// SourceSDR sums Watts-valued rows of the Power Supply sensor class. This
	// is what viiwork did unconditionally before source selection existed.
	SourceSDR = "sdr"
	// SourceNone disables power monitoring without probing.
	SourceNone = "none"
	// SourceSensorPrefix pins one named sensor, e.g. "sensor:SYS_POWER".
	SourceSensorPrefix = "sensor:"
)

// Timeouts are per-stage because the stages differ by more than an order of
// magnitude. Measured on a Gigabyte G431-MM0-OT (gb1): "dcmi power reading"
// 73-113ms, `sdr type "Power Supply"` 65-111ms, but "sensor get <NAME>"
// 2314-2485ms and "sensor list" 3161-4488ms -- the sensor paths resolve a name
// against the whole SDR repository. A single 2s budget silently killed every
// steady-state read on a pinned sensor, freezing the value at its probe
// reading.
const (
	// sampleTimeout bounds a steady-state read. Sized for the slowest adopted
	// source (~2.5s), with margin, while staying under a 5s health tick.
	sampleTimeout = 4 * time.Second
	// probeTimeout bounds one probe attempt, not the whole probe.
	probeTimeout = 10 * time.Second
	// discoverTimeout covers "sensor list", the slowest call viiwork makes.
	discoverTimeout = 15 * time.Second
	// startupBudget backstops the whole probe so a wedged BMC cannot hang
	// startup indefinitely.
	startupBudget = 30 * time.Second
)

type cmdFunc func(ctx context.Context) ([]byte, error)

// runner executes ipmitool. Injected so probing can be tested without a BMC.
type runner func(ctx context.Context, args ...string) ([]byte, error)

// source is one way of asking the BMC for node wattage.
type source struct {
	name  string
	args  []string
	parse func(string) (float64, bool)
}

// sourceFn yields a source, possibly after asking the BMC what it has. Sources
// are resolved lazily so that auto mode only pays for sensor discovery when the
// cheaper, more standard readings have already failed.
type sourceFn func(ctx context.Context) (source, bool)

type Sampler struct {
	mu         sync.RWMutex
	lastWatts  float64
	available  atomic.Bool
	logger     *log.Logger
	run        runner
	cmdFactory cmdFunc
	parseFn    func(string) (float64, bool)
	sourceName string
}

func dcmiSource() source {
	return source{name: "dcmi", args: []string{"dcmi", "power", "reading"}, parse: ParseDCMIWatts}
}

func sdrSource() source {
	return source{
		name: `sdr type "Power Supply"`,
		args: []string{"sdr", "type", "Power Supply"},
		// Unlike ParseWatts on its own, a zero sum is treated as "this class
		// carries no wattage here" rather than as a reading. Gigabyte BMCs
		// answer this query with presence flags and no numbers at all, which is
		// how power monitoring came to be silently dead on the gfx906 fleet.
		parse: func(out string) (float64, bool) {
			w := ParseWatts(out)
			return w, w > 0
		},
	}
}

func sensorSource(name string) source {
	return source{
		name:  SourceSensorPrefix + name,
		args:  []string{"sensor", "get", name},
		parse: ParseSensorWatts,
	}
}

func staticSource(s source) sourceFn {
	return func(context.Context) (source, bool) { return s, true }
}

func execIpmitool(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ipmitool", args...).Output()
}

// NewSampler probes for a usable node power reading. sourceSpec is one of
// SourceAuto (or ""), SourceDCMI, SourceSDR, SourceNone, or "sensor:<NAME>".
// A sampler that finds nothing reports Available() == false forever rather
// than reporting zero watts, so that cost tracking stays off instead of
// accumulating a confidently wrong bill.
func NewSampler(sourceSpec string) *Sampler {
	s := &Sampler{
		logger: log.New(os.Stdout, "[power] ", log.LstdFlags),
		run:    execIpmitool,
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupBudget)
	defer cancel()
	s.probe(ctx, sourceSpec)

	return s
}

func (s *Sampler) logf(format string, v ...any) {
	if s.logger != nil {
		s.logger.Printf(format, v...)
	}
}

// probe walks the candidate sources in order and adopts the first that yields a
// plausible reading, leaving the sampler unavailable if none do.
func (s *Sampler) probe(ctx context.Context, sourceSpec string) {
	candidates, err := s.candidates(sourceSpec)
	if err != nil {
		s.available.Store(false)
		s.logf("%v (power monitoring disabled)", err)
		return
	}

	var tried []string
	for _, candidate := range candidates {
		src, ok := candidate(ctx)
		if !ok {
			continue
		}
		watts, err := s.try(ctx, src)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", src.name, err))
			continue
		}
		s.adopt(src, watts)
		return
	}

	s.available.Store(false)
	if len(tried) == 0 {
		s.logf("IPMI unavailable, no usable power source (power monitoring disabled)")
		return
	}
	s.logf("no usable power source, tried %s (power monitoring disabled)", strings.Join(tried, "; "))
}

// try runs one source once and returns its reading. A source that runs but
// reports nothing is rejected here, which is what keeps a presence-only sensor
// class from being adopted and then reporting 0W indefinitely.
func (s *Sampler) try(ctx context.Context, src source) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := s.run(ctx, src.args...)
	if err != nil {
		return 0, err
	}
	watts, ok := src.parse(string(out))
	if !ok {
		return 0, errors.New("no wattage in output")
	}
	if watts <= 0 {
		return 0, fmt.Errorf("implausible reading %.0fW", watts)
	}
	return watts, nil
}

func (s *Sampler) adopt(src source, watts float64) {
	s.cmdFactory = func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, sampleTimeout)
		defer cancel()
		return s.run(ctx, src.args...)
	}
	s.parseFn = src.parse
	s.sourceName = src.name

	s.mu.Lock()
	s.lastWatts = watts
	s.mu.Unlock()

	s.available.Store(true)
	s.logf("power monitoring enabled via %s (%.0fW)", src.name, watts)
}

func (s *Sampler) candidates(sourceSpec string) ([]sourceFn, error) {
	spec := strings.TrimSpace(sourceSpec)
	if spec == "" {
		spec = SourceAuto
	}

	switch {
	case spec == SourceNone:
		return nil, errors.New(`power.source is "none"`)
	case spec == SourceDCMI:
		return []sourceFn{staticSource(dcmiSource())}, nil
	case spec == SourceSDR:
		return []sourceFn{staticSource(sdrSource())}, nil
	case strings.HasPrefix(spec, SourceSensorPrefix):
		name := strings.TrimSpace(strings.TrimPrefix(spec, SourceSensorPrefix))
		if name == "" {
			return nil, fmt.Errorf(`power.source %q needs a sensor name`, spec)
		}
		return []sourceFn{staticSource(sensorSource(name))}, nil
	case spec == SourceAuto:
		// DCMI first: it is the standardised whole-node reading and needs no
		// per-board knowledge. The SDR class second, so a host that already
		// worked keeps the reading it had. Named sensors last, because
		// choosing one is a heuristic.
		return []sourceFn{
			staticSource(dcmiSource()),
			staticSource(sdrSource()),
			s.discoverSensor,
		}, nil
	}

	return nil, fmt.Errorf("unknown power.source %q", spec)
}

// discoverSensor asks the BMC for its sensor list and picks the most plausible
// whole-node wattage reading from it.
func (s *Sampler) discoverSensor(ctx context.Context) (source, bool) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	out, err := s.run(ctx, "sensor", "list")
	if err != nil {
		return source{}, false
	}
	sensors := ParseWattsSensors(string(out))
	picked, ok := PickWattsSensor(sensors)
	if !ok {
		return source{}, false
	}
	if len(sensors) > 1 {
		names := make([]string, 0, len(sensors))
		for _, sensor := range sensors {
			names = append(names, sensor.Name)
		}
		s.logf("%d Watts sensors found (%s), picking %s; pin one with power.source: %q",
			len(sensors), strings.Join(names, ", "), picked.Name, SourceSensorPrefix+"<NAME>")
	}
	return sensorSource(picked.Name), true
}

func (s *Sampler) Sample(ctx context.Context) {
	if !s.available.Load() {
		return
	}
	out, err := s.cmdFactory(ctx)
	if err != nil {
		s.logf("%s read failed: %v (keeping last value: %.0fW)", s.SourceName(), err, s.Watts())
		return
	}

	// A hand-built Sampler (as in the package's own tests) has no probed
	// source; fall back to the historical SDR parser.
	parse := s.parseFn
	if parse == nil {
		parse = func(out string) (float64, bool) { return ParseWatts(out), true }
	}

	watts, ok := parse(string(out))
	if !ok {
		s.logf("%s returned no wattage (keeping last value: %.0fW)", s.SourceName(), s.Watts())
		return
	}

	s.mu.Lock()
	s.lastWatts = watts
	s.mu.Unlock()
}

func (s *Sampler) Watts() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastWatts
}

func (s *Sampler) Available() bool {
	return s.available.Load()
}

// SourceName reports which source won the probe, for status and logging.
func (s *Sampler) SourceName() string {
	if s.sourceName == "" {
		return "ipmitool"
	}
	return s.sourceName
}
