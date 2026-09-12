package node

import (
	"sort"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/cost"
)

// entsoeBaseURL is the ENTSO-E Transparency Platform API the spot fetcher uses.
const entsoeBaseURL = "https://web-api.tp.entsoe.eu/api"

// openEnergy opens the machine's energy store (v1's startEnergyRecorder, less
// the recorder). gpuIDs is every card the collector reports, or every GPU
// named in models[].gpus when it reports none: attribution divides by total
// marginal draw across the machine.
//
// The store is labelled with what node power measures only when a reading is
// available. When power is unavailable nothing will be recorded anyway, and
// writing a placeholder would claim a provenance no reading ever had.
func openEnergy(cfg config.EnergyConfig, timezone string, gpuIDs []int, pw NodePower, logf func(string, ...any)) (*energy.Store, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	source := ""
	if pw.Available() {
		source = pw.Source()
	}
	store, err := energy.Open(energy.Config{
		Dir:         cfg.Dir,
		GPUIDs:      gpuIDs,
		MinuteSlots: cfg.MinuteSlots,
		HourSlots:   cfg.HourSlots,
		DaySlots:    cfg.DaySlots,
		Location:    loc,
		Source:      source,
	}, nil)
	if err != nil {
		return nil, err
	}
	if logf != nil {
		logf("[energy] recording every %s for %d GPUs into %s", cfg.SampleInterval.Duration, len(gpuIDs), cfg.Dir)
	}
	return store, nil
}

// gpuOwners maps every GPU index in models[].gpus to its model.
func gpuOwners(models []config.Model) map[int]string {
	owners := map[int]string{}
	for _, m := range models {
		for _, id := range m.GPUs {
			owners[id] = m.Name
		}
	}
	return owners
}

// modelGPUIDs is every GPU named in models[].gpus, sorted: the energy store's
// card list when the collector reports none.
func modelGPUIDs(models []config.Model) []int {
	var ids []int
	for id := range gpuOwners(models) {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// gpuReadings is v1's readings closure: the latest sample of each card with
// power above 0, labelled with its owner. Owners are resolved on every sample,
// so a reload's new layout is used at once.
func gpuReadings(samples gpuLatest, gpuIDs []int, owners func() map[int]string) func() []energy.GPUReading {
	want := make(map[int]bool, len(gpuIDs))
	for _, id := range gpuIDs {
		want[id] = true
	}
	return func() []energy.GPUReading {
		owned := owners()
		latest := samples.Latest()
		sort.Slice(latest, func(i, j int) bool { return latest[i].GPUID < latest[j].GPUID })
		out := make([]energy.GPUReading, 0, len(latest))
		for _, s := range latest {
			if !want[s.GPUID] || s.PowerW <= 0 {
				continue
			}
			out = append(out, energy.GPUReading{GPUID: s.GPUID, Watts: s.PowerW, Model: owned[s.GPUID]})
		}
		return out
	}
}

// newRecorder attributes with the marginal whole-chassis split when node power
// is IPMI, and with energy.Direct when it is the GPUs' own sum, where a
// residual is not chassis overhead but idle draw on the cards (P6 Decision 3).
func newRecorder(store *energy.Store, interval time.Duration, pw NodePower, readings func() []energy.GPUReading) *energy.Recorder {
	nodeWatts := func() (float64, bool) { return pw.Watts(), pw.Available() }
	if pw.Chassis() {
		return energy.NewRecorder(store, interval, nodeWatts, readings, nil)
	}
	return energy.NewRecorderWithAttribution(store, interval, nodeWatts, readings, energy.Direct, nil)
}

// newCostTracker is v1's cost construction, reading power from pw. It returns
// nil when cost tracking is off: no bidding zone, or no API key.
func newCostTracker(cfg config.CostConfig, apiKey string, pw NodePower, logf func(string, ...any)) *cost.Tracker {
	if cfg.BiddingZone == "" {
		return nil
	}
	if apiKey == "" {
		logf("WARNING: cost section configured but ENTSOE_API_KEY not set, cost tracking disabled")
		return nil
	}
	fetcher := cost.NewSpotFetcher(apiKey, cfg.BiddingZone, entsoeBaseURL)
	tracker := cost.NewTracker(fetcher, cost.CostConfig{
		Transfer: cost.TransferConfig{
			Winter: cost.WinterTransferConfig{
				PeakCentsKWh:    cfg.Transfer.Winter.PeakCentsKWh,
				OffpeakCentsKWh: cfg.Transfer.Winter.OffpeakCentsKWh,
			},
			Summer: cost.SummerTransferConfig{
				FlatCentsKWh: cfg.Transfer.Summer.FlatCentsKWh,
			},
		},
		ElectricityTaxCentsKWh: cfg.ElectricityTaxCentsKWh,
		VATPercent:             cfg.VATPercent,
		Timezone:               cfg.Timezone,
	}, pw)
	logf("cost tracking enabled (zone: %s)", cfg.BiddingZone)
	return tracker
}
