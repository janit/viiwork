package cost

import (
	"context"
	"testing"
	"time"
)

type mockPower struct {
	watts     float64
	available bool
}

func (m *mockPower) Watts() float64  { return m.watts }
func (m *mockPower) Available() bool { return m.available }

func TestTrackerBasic(t *testing.T) {
	cfg := CostConfig{
		Transfer: TransferConfig{
			Winter: WinterTransferConfig{PeakCentsKWh: 4.28, OffpeakCentsKWh: 2.49},
			Summer: SummerTransferConfig{FlatCentsKWh: 2.49},
		},
		ElectricityTaxCentsKWh: 2.253,
		VATPercent: 25.5,
		Timezone: "Europe/Helsinki",
	}
	pw := &mockPower{watts: 280.0, available: true}

	fetcher := &SpotFetcher{}
	now := time.Now().UTC().Truncate(time.Hour)
	fetcher.prices = []PricePoint{
		{Time: now, CentsKWh: 5.0},
		{Time: now.Add(time.Hour), CentsKWh: 4.5},
	}

	tracker := NewTracker(fetcher, cfg, pw)
	tracker.Update(context.Background())

	if !tracker.Available() { t.Error("expected available") }
	if tracker.EURPerHour() == 0 { t.Error("expected non-zero cost rate") }
}

func TestTrackerUnavailableWithoutPower(t *testing.T) {
	cfg := CostConfig{Timezone: "Europe/Helsinki"}
	pw := &mockPower{watts: 0, available: false}
	fetcher := &SpotFetcher{}
	tracker := NewTracker(fetcher, cfg, pw)
	tracker.Update(context.Background())
	if tracker.Available() { t.Error("expected unavailable without power") }
}

func TestTrackerUnavailableWithoutPrices(t *testing.T) {
	cfg := CostConfig{Timezone: "Europe/Helsinki"}
	pw := &mockPower{watts: 280.0, available: true}
	fetcher := &SpotFetcher{} // empty cache
	tracker := NewTracker(fetcher, cfg, pw)
	tracker.Update(context.Background())
	if tracker.Available() { t.Error("expected unavailable without prices") }
}

func TestTrackerAccumulation(t *testing.T) {
	cfg := CostConfig{
		Transfer: TransferConfig{
			Summer: SummerTransferConfig{FlatCentsKWh: 2.49},
		},
		ElectricityTaxCentsKWh: 2.253,
		VATPercent: 25.5,
		Timezone: "UTC",
	}
	pw := &mockPower{watts: 1000.0, available: true}

	fetcher := &SpotFetcher{}
	now := time.Now().UTC().Truncate(time.Hour)
	fetcher.prices = []PricePoint{
		{Time: now, CentsKWh: 5.0},
		{Time: now.Add(time.Hour), CentsKWh: 5.0},
	}

	tracker := NewTracker(fetcher, cfg, pw)
	tracker.Update(context.Background())
	first := tracker.TodayEUR()

	time.Sleep(10 * time.Millisecond)
	tracker.Update(context.Background())
	second := tracker.TodayEUR()

	if second <= first { t.Errorf("expected accumulation: first=%f, second=%f", first, second) }
}
