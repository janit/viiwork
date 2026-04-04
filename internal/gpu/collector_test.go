package gpu

import (
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
