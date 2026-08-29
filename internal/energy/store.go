package energy

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Tier names one of the three retention resolutions.
type Tier int

const (
	TierMinute Tier = iota
	TierHour
	TierDay
)

func (t Tier) String() string {
	switch t {
	case TierMinute:
		return "minute"
	case TierHour:
		return "hour"
	default:
		return "day"
	}
}

// Defaults give 24h of minutes, 365d of hours and 365d of days. Sized so the
// whole store is a couple of megabytes and, more importantly, so it never
// grows past that: every ring is preallocated at these lengths.
const (
	DefaultMinuteSlots = 1440
	DefaultHourSlots   = 8760
	DefaultDaySlots    = 365
)

type Config struct {
	Dir         string
	GPUIDs      []int
	MinuteSlots int
	HourSlots   int
	DaySlots    int
	Location    *time.Location
}

// Store owns the six ring files and the model table.
type Store struct {
	cfg     Config
	logger  *log.Logger
	models  *modelTable
	gpuLane map[int]int

	nodeMinute, nodeHour, nodeDay *ring
	gpuMinute, gpuHour, gpuDay    *ring
}

func Open(cfg Config, logger *log.Logger) (*Store, error) {
	if cfg.MinuteSlots <= 0 {
		cfg.MinuteSlots = DefaultMinuteSlots
	}
	if cfg.HourSlots <= 0 {
		cfg.HourSlots = DefaultHourSlots
	}
	if cfg.DaySlots <= 0 {
		cfg.DaySlots = DefaultDaySlots
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if len(cfg.GPUIDs) == 0 {
		return nil, fmt.Errorf("energy: at least one GPU id is required")
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[energy] ", log.LstdFlags)
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("energy: creating %s: %w", cfg.Dir, err)
	}

	s := &Store{cfg: cfg, logger: logger, gpuLane: make(map[int]int, len(cfg.GPUIDs))}
	for lane, id := range cfg.GPUIDs {
		s.gpuLane[id] = lane
	}

	lanes := len(cfg.GPUIDs)
	specs := []struct {
		target  **ring
		name    string
		slots   int
		lanes   int
		recSize int
	}{
		{&s.nodeMinute, "node-minute.ring", cfg.MinuteSlots, 1, NodeRecordSize},
		{&s.nodeHour, "node-hour.ring", cfg.HourSlots, 1, NodeRecordSize},
		{&s.nodeDay, "node-day.ring", cfg.DaySlots, 1, NodeRecordSize},
		{&s.gpuMinute, "gpu-minute.ring", cfg.MinuteSlots, lanes, GPURecordSize},
		{&s.gpuHour, "gpu-hour.ring", cfg.HourSlots, lanes, GPURecordSize},
		{&s.gpuDay, "gpu-day.ring", cfg.DaySlots, lanes, GPURecordSize},
	}

	for _, spec := range specs {
		r, reset, err := openRing(filepath.Join(cfg.Dir, spec.name), spec.slots, spec.lanes, spec.recSize)
		if err != nil {
			s.Close()
			return nil, err
		}
		if reset {
			// Changing gpus.devices or a slot count changes the file geometry.
			// Saying so is the point: history is gone and a silent reset would
			// look exactly like a node that had simply been quiet.
			s.logger.Printf("%s geometry changed, history discarded", spec.name)
		}
		*spec.target = r
	}

	models, err := openModelTable(filepath.Join(cfg.Dir, "models.txt"))
	if err != nil {
		s.Close()
		return nil, err
	}
	s.models = models

	s.logger.Printf("store at %s: %d GPUs, %d minutes / %d hours / %d days, %s on disk",
		cfg.Dir, lanes, cfg.MinuteSlots, cfg.HourSlots, cfg.DaySlots, humanBytes(s.DiskBytes()))
	return s, nil
}

// DiskBytes is the fixed on-disk size. It cannot grow: every ring is
// preallocated, so this is both the current and the final size.
func (s *Store) DiskBytes() int64 {
	lanes := int64(len(s.cfg.GPUIDs))
	node := int64(s.cfg.MinuteSlots+s.cfg.HourSlots+s.cfg.DaySlots) * NodeRecordSize
	gpu := int64(s.cfg.MinuteSlots+s.cfg.HourSlots+s.cfg.DaySlots) * lanes * GPURecordSize
	return node + gpu + 6*ringHeaderLen
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func minuteBucket(ts time.Time) int64 { return ts.Unix() / 60 }
func hourBucket(ts time.Time) int64   { return ts.Unix() / 3600 }

// dayBucket keys on local midnight, so a "day" is the day the operator lives
// in and matches the midnight reset cost tracking already does.
func (s *Store) dayBucket(ts time.Time) (int64, int64) {
	local := ts.In(s.cfg.Location)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, s.cfg.Location)
	return midnight.Unix() / 86400, midnight.Unix()
}

// WriteMinute records one finalised minute and refreshes the hour and day it
// belongs to. Roll-ups are recomputed from the finer tier rather than
// accumulated, so they are idempotent: a restart mid-hour, or a repeated write,
// converges on the same value instead of double counting.
func (s *Store) WriteMinute(node NodeRecord, gpus []GPURecord) error {
	bucket := minuteBucket(time.Unix(node.TS, 0))
	if err := s.nodeMinute.put(bucket, 0, node.encode); err != nil {
		return err
	}
	for _, g := range gpus {
		lane, ok := s.gpuLane[int(g.GPUID)]
		if !ok {
			continue
		}
		if err := s.gpuMinute.put(bucket, lane, g.encode); err != nil {
			return err
		}
	}
	return s.rollUp(time.Unix(node.TS, 0))
}

func (s *Store) rollUp(ts time.Time) error {
	hourStart := ts.Truncate(time.Hour)
	if err := s.rollUpInto(s.nodeHour, s.gpuHour, hourBucket(ts), hourStart.Unix(),
		s.readNodeRange(s.nodeMinute, hourStart, hourStart.Add(time.Hour)),
		s.readGPURange(s.gpuMinute, hourStart, hourStart.Add(time.Hour))); err != nil {
		return err
	}

	dayBucket, dayStart := s.dayBucket(ts)
	from := time.Unix(dayStart, 0)
	return s.rollUpInto(s.nodeDay, s.gpuDay, dayBucket, dayStart,
		s.readNodeRange(s.nodeHour, from, from.Add(24*time.Hour)),
		s.readGPURange(s.gpuHour, from, from.Add(24*time.Hour)))
}

func (s *Store) rollUpInto(nodeRing, gpuRing *ring, bucket, startTS int64, nodes []NodeRecord, gpus []GPURecord) error {
	if err := nodeRing.put(bucket, 0, aggregateNode(nodes, startTS).encode); err != nil {
		return err
	}
	byGPU := make(map[uint16][]GPURecord)
	for _, g := range gpus {
		byGPU[g.GPUID] = append(byGPU[g.GPUID], g)
	}
	for id, recs := range byGPU {
		lane, ok := s.gpuLane[int(id)]
		if !ok {
			continue
		}
		if err := gpuRing.put(bucket, lane, aggregateGPU(recs, startTS).encode); err != nil {
			return err
		}
	}
	return nil
}

// aggregateNode combines finer buckets into a coarser one. The mean is weighted
// by covered seconds, so a partly-observed minute cannot pull an hour's average
// as hard as a fully observed one.
func aggregateNode(recs []NodeRecord, startTS int64) NodeRecord {
	out := NodeRecord{TS: startTS}
	var wattSeconds float64
	var covered int
	for _, r := range recs {
		wattSeconds += float64(r.Watts) * float64(r.CoveredS)
		covered += int(r.CoveredS)
	}
	if covered > 0 {
		out.Watts = float32(wattSeconds / float64(covered))
	}
	out.CoveredS = clampCovered(covered)
	return out
}

func aggregateGPU(recs []GPURecord, startTS int64) GPURecord {
	out := GPURecord{TS: startTS}
	if len(recs) == 0 {
		return out
	}
	out.GPUID = recs[0].GPUID

	var attrSeconds, rawSeconds float64
	var covered int
	// A model can change mid-period. The one that held the GPU for the most
	// covered time wins the label, which beats silently taking whichever record
	// happened to sort first.
	modelTime := make(map[uint16]int)
	for _, r := range recs {
		attrSeconds += float64(r.AttrW) * float64(r.CoveredS)
		rawSeconds += float64(r.RawW) * float64(r.CoveredS)
		covered += int(r.CoveredS)
		modelTime[r.ModelIdx] += int(r.CoveredS)
	}
	if covered > 0 {
		out.AttrW = float32(attrSeconds / float64(covered))
		out.RawW = float32(rawSeconds / float64(covered))
	}
	out.CoveredS = clampCovered(covered)

	best := -1
	for idx, secs := range modelTime {
		if secs > best || (secs == best && idx < out.ModelIdx) {
			out.ModelIdx, best = idx, secs
		}
	}
	return out
}

// clampCovered keeps the uint16 field from wrapping: a day of covered seconds
// (86400) does not fit, so a fully covered coarse bucket saturates instead.
func clampCovered(covered int) uint16 {
	if covered > 65535 {
		return 65535
	}
	if covered < 0 {
		return 0
	}
	return uint16(covered)
}

func (s *Store) ringFor(tier Tier, gpu bool) *ring {
	switch {
	case tier == TierMinute && !gpu:
		return s.nodeMinute
	case tier == TierHour && !gpu:
		return s.nodeHour
	case tier == TierDay && !gpu:
		return s.nodeDay
	case tier == TierMinute:
		return s.gpuMinute
	case tier == TierHour:
		return s.gpuHour
	default:
		return s.gpuDay
	}
}

// ReadNode returns node records in [from, to), oldest first.
func (s *Store) ReadNode(tier Tier, from, to time.Time) []NodeRecord {
	return s.readNodeRange(s.ringFor(tier, false), from, to)
}

// ReadGPU returns per-GPU records in [from, to), oldest first.
func (s *Store) ReadGPU(tier Tier, from, to time.Time) []GPURecord {
	return s.readGPURange(s.ringFor(tier, true), from, to)
}

func (s *Store) readNodeRange(r *ring, from, to time.Time) []NodeRecord {
	buf, err := r.all()
	if err != nil {
		s.logger.Printf("reading ring: %v", err)
		return nil
	}
	lo, hi := from.Unix(), to.Unix()
	var out []NodeRecord
	for off := 0; off+NodeRecordSize <= len(buf); off += NodeRecordSize {
		rec := decodeNode(buf[off : off+NodeRecordSize])
		if rec.TS >= lo && rec.TS < hi {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func (s *Store) readGPURange(r *ring, from, to time.Time) []GPURecord {
	buf, err := r.all()
	if err != nil {
		s.logger.Printf("reading ring: %v", err)
		return nil
	}
	lo, hi := from.Unix(), to.Unix()
	var out []GPURecord
	for off := 0; off+GPURecordSize <= len(buf); off += GPURecordSize {
		rec := decodeGPU(buf[off : off+GPURecordSize])
		if rec.TS >= lo && rec.TS < hi {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return out[i].GPUID < out[j].GPUID
	})
	return out
}

// ModelIndex maps a model name to its stored index, creating it if new.
func (s *Store) ModelIndex(name string) (uint16, error) { return s.models.Index(name) }

// ModelName resolves a stored index back to a model name.
func (s *Store) ModelName(idx uint16) string { return s.models.Name(idx) }

// ByModel totals attributed energy per model over a window.
func (s *Store) ByModel(tier Tier, from, to time.Time) map[string]float64 {
	out := make(map[string]float64)
	for _, rec := range s.ReadGPU(tier, from, to) {
		name := s.models.Name(rec.ModelIdx)
		if name == "" {
			name = "(idle)"
		}
		out[name] += rec.KWh()
	}
	return out
}

// NodeKWh totals measured node energy over a window.
// KWh24h is whole-node energy over the rolling last 24 hours.
//
// Read from the minute tier because that ring *is* 24 hours: 1440 one-minute
// slots, so the window and the retention are the same span and no record is
// counted twice or missed at the edge. Reads filter by timestamp, so a slot
// still holding yesterday's value at the same clock minute is ignored rather
// than mistaken for today's.
//
// The window slides continuously, but the value does not: buckets are
// minute-aligned, so one leaves the window and one is written about once a
// minute each. That matters because this rides on the pushed cluster snapshot,
// which is suppressed when unchanged — a figure recomputed from `now` that
// changed every second would defeat that, the way host memory once did.
func (s *Store) KWh24h() float64 {
	now := time.Now()
	return s.NodeKWh(TierMinute, now.Add(-24*time.Hour), now.Add(time.Minute))
}

// KWh30d is whole-node energy over the rolling last 30 days.
//
// Read from the day tier, not the hour tier: 30 days is 720 hourly buckets
// against 30 daily ones, and nothing here needs the resolution. The current,
// partly elapsed day is included — roll-ups rewrite a bucket's own slot rather
// than appending, so today's is already there and already current.
//
// Changes once a day plus once per roll-up of the current day, so like KWh24h
// it does not trouble the pushed snapshot's change detection.
func (s *Store) KWh30d() float64 {
	now := time.Now()
	return s.NodeKWh(TierDay, now.AddDate(0, 0, -30), now.Add(time.Minute))
}

func (s *Store) NodeKWh(tier Tier, from, to time.Time) float64 {
	var total float64
	for _, rec := range s.ReadNode(tier, from, to) {
		total += rec.KWh()
	}
	return total
}

func (s *Store) Sync() {
	for _, r := range []*ring{s.nodeMinute, s.nodeHour, s.nodeDay, s.gpuMinute, s.gpuHour, s.gpuDay} {
		if r != nil {
			if err := r.sync(); err != nil {
				s.logger.Printf("sync: %v", err)
			}
		}
	}
}

func (s *Store) Close() {
	for _, r := range []*ring{s.nodeMinute, s.nodeHour, s.nodeDay, s.gpuMinute, s.gpuHour, s.gpuDay} {
		if r != nil {
			r.Close()
		}
	}
}
