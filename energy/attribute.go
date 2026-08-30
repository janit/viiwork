package energy

import "time"

// GPUReading is one instantaneous per-GPU observation, as the recorder sees it.
type GPUReading struct {
	GPUID int
	Watts float64
	// Model is whichever model was resident on this GPU. Empty means none.
	Model string
}

// Attribution splits measured node power between the models causing it and the
// baseline that would be drawn anyway.
type Attribution struct {
	// BaselineW is the residual: fans, CPU, idle cards, PSU losses. It is
	// deliberately reported rather than smeared across models, because a node
	// that is merely switched on is not a cost any model caused.
	BaselineW float64
	// Watts maps GPU id to its share of node power.
	Watts map[int]float64
}

// Attribute charges each GPU a share of node power proportional to how far it
// is above its own idle floor.
//
// The measured data this is built on: under load two cards of a tensor-split
// pair went to 163W and 214W while the other eight stayed at 20-25W, moving by
// at most 1W. So marginal draw cleanly separates the cards doing work, and a
// proportional split of the *marginal* node power charges a model for what it
// caused rather than for the building it sits in.
//
// The result always reconciles: BaselineW plus the sum of Watts equals nodeW,
// so no total is ever invented. When nothing is above idle, everything is
// baseline.
func Attribute(nodeW float64, readings []GPUReading, gpuIdle map[int]float64, nodeBaseline float64) Attribution {
	out := Attribution{Watts: make(map[int]float64, len(readings))}

	marginal := make(map[int]float64, len(readings))
	var totalMarginal float64
	for _, r := range readings {
		m := r.Watts - gpuIdle[r.GPUID]
		if m < 0 {
			m = 0
		}
		marginal[r.GPUID] = m
		totalMarginal += m
		out.Watts[r.GPUID] = 0
	}

	// The baseline is also inferred from this sample, not just from history:
	// node power minus the work the GPUs are visibly doing is, by definition,
	// the part nothing on a GPU explains. Taking the lower of the two matters on
	// a host that is never idle -- a busy inference node may go weeks without an
	// idle minute, and a purely historical minimum would then sit at the lowest
	// *loaded* draw, quietly attributing nothing at all.
	//
	// On gb1 under real multi-tenant load this inference gave 730W - 532W =
	// 198W, within 2% of the 195W idle draw measured directly.
	if inferred := nodeW - totalMarginal; inferred < nodeBaseline {
		nodeBaseline = inferred
	}
	if nodeBaseline < 0 {
		nodeBaseline = 0
	}

	work := nodeW - nodeBaseline
	if work < 0 {
		work = 0
	}

	if totalMarginal > 0 && work > 0 {
		for id, m := range marginal {
			out.Watts[id] = work * m / totalMarginal
		}
	}

	// Whatever was not charged to a GPU is baseline by definition, which is
	// what keeps the split exhaustive even when the floors are stale.
	var charged float64
	for _, w := range out.Watts {
		charged += w
	}
	out.BaselineW = nodeW - charged
	if out.BaselineW < 0 {
		out.BaselineW = 0
	}
	return out
}

// Floors returns the idle floor for each GPU and for the node, taken as the
// minimum observed over the last 24h of minute records.
//
// A minimum converges from the safe side: with little history it sits close to
// the current reading, so almost nothing is attributed and the baseline absorbs
// it. As real idle periods are observed the floor can only fall, and attributed
// energy rises toward the truth. Guessing a floor would fail in the other
// direction, inventing model cost on day one.
func (s *Store) Floors(now time.Time, fallbackNode float64, fallbackGPU map[int]float64) (float64, map[int]float64) {
	from := now.Add(-24 * time.Hour)

	node := fallbackNode
	for _, rec := range s.ReadNode(TierMinute, from, now) {
		if rec.CoveredS == 0 {
			continue
		}
		if w := float64(rec.Watts); w > 0 && w < node {
			node = w
		}
	}

	// The lowest reading across the host's cards is a serviceable idle proxy:
	// these are identical GPUs, and on a 10-card box at least one is normally
	// doing nothing. It gives a usable floor from the very first sample instead
	// of waiting for a card's own idle period to be observed.
	crossMin := 0.0
	for _, w := range fallbackGPU {
		if w > 0 && (crossMin == 0 || w < crossMin) {
			crossMin = w
		}
	}

	gpu := make(map[int]float64, len(fallbackGPU))
	for id, w := range fallbackGPU {
		gpu[id] = w
		if crossMin > 0 && crossMin < w {
			gpu[id] = crossMin
		}
	}
	for _, rec := range s.ReadGPU(TierMinute, from, now) {
		if rec.CoveredS == 0 {
			continue
		}
		id := int(rec.GPUID)
		w := float64(rec.RawW)
		if w <= 0 {
			continue
		}
		if cur, ok := gpu[id]; !ok || w < cur {
			gpu[id] = w
		}
	}
	return node, gpu
}
