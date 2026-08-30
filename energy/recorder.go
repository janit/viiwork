package energy

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

// NodeWattsFunc reports current whole-node draw, and whether it is measurable.
type NodeWattsFunc func() (float64, bool)

// GPUReadingsFunc reports the current per-GPU draw and which model holds each
// card. The recorder takes functions rather than the collectors themselves so
// this package stays independent of how power and GPU stats are sourced.
type GPUReadingsFunc func() []GPUReading

// Recorder samples power on an interval and writes one record per minute.
//
// It samples faster than it records on purpose. The BMC refreshes roughly once
// a minute and lags load by ~46s, so a single reading per minute would be a
// coin flip on where in that step it landed; and on a tensor-split pair the two
// cards alternate between ~20W and ~160W, so an instantaneous per-GPU sample
// misrepresents a backend's draw by roughly half. Averaging several samples
// into the bucket is what makes both honest.
type Recorder struct {
	store    *Store
	nodeFn   NodeWattsFunc
	gpuFn    GPUReadingsFunc
	interval time.Duration
	logger   *log.Logger

	mu  sync.Mutex
	acc *accumulator
}

type gpuAcc struct {
	sum       float64
	n         int
	modelTime map[string]int
}

type accumulator struct {
	bucket  int64
	nodeSum float64
	nodeN   int
	gpus    map[int]*gpuAcc
}

func newAccumulator(bucket int64) *accumulator {
	return &accumulator{bucket: bucket, gpus: make(map[int]*gpuAcc)}
}

func NewRecorder(store *Store, interval time.Duration, nodeFn NodeWattsFunc, gpuFn GPUReadingsFunc, logger *log.Logger) *Recorder {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[energy] ", log.LstdFlags)
	}
	return &Recorder{store: store, nodeFn: nodeFn, gpuFn: gpuFn, interval: interval, logger: logger}
}

// Run samples until ctx is cancelled, flushing the bucket in progress on the
// way out so a clean shutdown does not silently drop the last minute.
func (r *Recorder) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.Flush(time.Now())
			r.store.Sync()
			return
		case now := <-ticker.C:
			r.Sample(now)
		}
	}
}

// Sample folds one observation into the current minute, finalising the previous
// one when the minute rolls over.
func (r *Recorder) Sample(now time.Time) {
	watts, ok := r.nodeFn()
	if !ok {
		return
	}
	readings := r.gpuFn()
	bucket := minuteBucket(now)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.acc != nil && r.acc.bucket != bucket {
		r.finalise(r.acc)
		r.acc = nil
	}
	if r.acc == nil {
		r.acc = newAccumulator(bucket)
	}

	r.acc.nodeSum += watts
	r.acc.nodeN++
	for _, reading := range readings {
		g, ok := r.acc.gpus[reading.GPUID]
		if !ok {
			g = &gpuAcc{modelTime: make(map[string]int)}
			r.acc.gpus[reading.GPUID] = g
		}
		g.sum += reading.Watts
		g.n++
		g.modelTime[reading.Model]++
	}
}

// Flush finalises the bucket in progress. Used on shutdown.
func (r *Recorder) Flush(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.acc == nil {
		return
	}
	r.finalise(r.acc)
	r.acc = nil
}

func (r *Recorder) finalise(acc *accumulator) {
	if acc.nodeN == 0 {
		return
	}
	startTS := acc.bucket * 60
	nodeW := acc.nodeSum / float64(acc.nodeN)

	// Covered seconds come from how many samples actually landed, so a minute
	// that only saw part of its samples -- a restart, a slow BMC -- contributes
	// proportionally less energy instead of being extrapolated to a full minute.
	covered := clampCovered(min(int(float64(acc.nodeN)*r.interval.Seconds()+0.5), 60))

	raw := make(map[int]float64, len(acc.gpus))
	readings := make([]GPUReading, 0, len(acc.gpus))
	for id, g := range acc.gpus {
		if g.n == 0 {
			continue
		}
		mean := g.sum / float64(g.n)
		raw[id] = mean
		readings = append(readings, GPUReading{GPUID: id, Watts: mean, Model: dominantModel(g.modelTime)})
	}

	nodeFloor, gpuFloor := r.store.Floors(time.Unix(startTS, 0), nodeW, raw)
	attribution := Attribute(nodeW, readings, gpuFloor, nodeFloor)

	gpuRecords := make([]GPURecord, 0, len(readings))
	for _, reading := range readings {
		idx, err := r.store.ModelIndex(reading.Model)
		if err != nil {
			r.logger.Printf("model index for %q: %v", reading.Model, err)
			continue
		}
		gpuRecords = append(gpuRecords, GPURecord{
			TS:       startTS,
			GPUID:    uint16(reading.GPUID),
			ModelIdx: idx,
			AttrW:    float32(attribution.Watts[reading.GPUID]),
			RawW:     float32(reading.Watts),
			CoveredS: covered,
		})
	}

	node := NodeRecord{TS: startTS, Watts: float32(nodeW), CoveredS: covered}
	if err := r.store.WriteMinute(node, gpuRecords); err != nil {
		r.logger.Printf("writing minute %d: %v", startTS, err)
	}
}

// dominantModel picks the model that held the GPU for most of the bucket.
func dominantModel(modelTime map[string]int) string {
	best, bestName := -1, ""
	for name, n := range modelTime {
		if n > best || (n == best && name < bestName) {
			best, bestName = n, name
		}
	}
	return bestName
}
