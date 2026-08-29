package gpu

import (
	"errors"
	"context"
	"testing"
)

func TestCollectorWithMockCommand(t *testing.T) {
	hist := NewHistory(720)
	bcast := NewBroadcaster()
	ch := bcast.Subscribe()

	c := &StatCollector{history: hist, broadcaster: bcast}
	c.available.Store(true)
	c.cmdFactory = func(ctx context.Context) ([]byte, error) {
		return []byte(sampleROCmJSON), nil
	}
	c.Sample(context.Background())

	samples := hist.Samples(0)
	if len(samples) != 1 { t.Fatalf("expected 1 sample, got %d", len(samples)) }
	if samples[0].Utilization != 85 { t.Errorf("expected 85, got %f", samples[0].Utilization) }

	select {
	case msg := <-ch:
		if len(msg) == 0 { t.Error("expected non-empty broadcast") }
	default:
		t.Error("expected broadcast message")
	}
}

func TestCollectorUnavailable(t *testing.T) {
	hist := NewHistory(720)
	bcast := NewBroadcaster()
	c := &StatCollector{history: hist, broadcaster: bcast}
	c.Sample(context.Background())
	samples := hist.Samples(0)
	if len(samples) != 0 { t.Errorf("expected 0 samples, got %d", len(samples)) }
}

func TestCollectorAvailable(t *testing.T) {
	hist := NewHistory(720)
	bcast := NewBroadcaster()
	c := &StatCollector{history: hist, broadcaster: bcast}
	c.available.Store(true)
	if !c.Available() { t.Error("expected available") }
}

func TestCollectorRecordsPower(t *testing.T) {
	hist := NewHistory(720)
	c := &StatCollector{history: hist, broadcaster: NewBroadcaster()}
	c.available.Store(true)
	c.cmdFactory = func(ctx context.Context) ([]byte, error) { return []byte(sampleROCmJSONWithPower), nil }
	c.Sample(context.Background())

	samples := hist.Samples(0)
	if len(samples) != 1 { t.Fatalf("expected 1 sample, got %d", len(samples)) }
	if samples[0].PowerW != 163.0 { t.Errorf("expected 163.0 W in history, got %f", samples[0].PowerW) }
}

// fakeROCm answers only the arg sets that do not contain the given flag.
func fakeROCm(reject string, out string, seen *[][]string) func([]string) cmdFunc {
	return func(args []string) cmdFunc {
		return func(ctx context.Context) ([]byte, error) {
			*seen = append(*seen, args)
			for _, a := range args {
				if a == reject { return nil, errors.New("unrecognized argument " + reject) }
			}
			return []byte(out), nil
		}
	}
}

// Losing utilisation and VRAM to gain a bonus field would be a bad trade, so a
// rocm-smi that rejects --showpower must fall back rather than disable metrics.
func TestCollectorFallsBackWhenShowpowerRejected(t *testing.T) {
	var seen [][]string
	hist := NewHistory(720)
	c := newStatCollector(hist, NewBroadcaster(), fakeROCm("--showpower", sampleROCmJSON, &seen))

	if !c.Available() { t.Fatal("expected GPU metrics to survive a rejected --showpower") }
	if c.PowerAvailable() { t.Error("expected PowerAvailable() false when the flag was rejected") }
	if len(seen) < 2 { t.Fatalf("expected --showpower tried first then dropped, got %v", seen) }
	if len(hist.Samples(0)) == 0 { t.Error("expected utilisation and VRAM to still be recorded") }
}

func TestCollectorAdoptsPowerWhenAvailable(t *testing.T) {
	var seen [][]string
	c := newStatCollector(NewHistory(720), NewBroadcaster(), fakeROCm("--nonexistent", sampleROCmJSONWithPower, &seen))

	if !c.Available() || !c.PowerAvailable() { t.Fatalf("expected power to be adopted, available=%v power=%v", c.Available(), c.PowerAvailable()) }
	if len(seen) != 2 { t.Errorf("expected one probe plus the initial sample, got %v", seen) }
}

// A rocm-smi that accepts --showpower but reports no wattage must be reported
// as powerless rather than leaving a silently empty column.
func TestCollectorPowerAcceptedButEmpty(t *testing.T) {
	var seen [][]string
	c := newStatCollector(NewHistory(720), NewBroadcaster(), fakeROCm("--nonexistent", sampleROCmJSON, &seen))

	if !c.Available() { t.Fatal("expected GPU metrics available") }
	if c.PowerAvailable() { t.Error("expected PowerAvailable() false when no wattage was reported") }
}
