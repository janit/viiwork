package energy

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func testStore(t *testing.T, gpus ...int) *Store {
	t.Helper()
	if len(gpus) == 0 {
		gpus = []int{0, 1}
	}
	s, err := Open(Config{Dir: t.TempDir(), GPUIDs: gpus, Location: time.UTC}, quiet())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestRecordRoundTrip(t *testing.T) {
	node := NodeRecord{TS: 1756000000, Watts: 372.5, CoveredS: 60}
	buf := make([]byte, NodeRecordSize)
	node.encode(buf)
	if got := decodeNode(buf); got != node {
		t.Errorf("node round trip: got %+v want %+v", got, node)
	}

	gpu := GPURecord{TS: 1756000000, GPUID: 7, ModelIdx: 3, AttrW: 163.25, RawW: 214.5, CoveredS: 60}
	buf = make([]byte, GPURecordSize)
	gpu.encode(buf)
	if got := decodeGPU(buf); got != gpu {
		t.Errorf("gpu round trip: got %+v want %+v", got, gpu)
	}
}

func TestRecordKWh(t *testing.T) {
	// 3600W for a full hour is exactly 3.6 kWh.
	r := NodeRecord{Watts: 3600, CoveredS: 3600}
	if got := r.KWh(); got < 3.5999 || got > 3.6001 {
		t.Errorf("expected 3.6 kWh, got %f", got)
	}
	// Half-covered bucket carries half the energy, not a full extrapolation.
	half := NodeRecord{Watts: 3600, CoveredS: 1800}
	if got := half.KWh(); got < 1.7999 || got > 1.8001 {
		t.Errorf("expected 1.8 kWh for a half-covered bucket, got %f", got)
	}
}

// The measured case: two cards of a tensor-split pair under load, eight idle.
func TestAttributeSeparatesLoadedFromIdle(t *testing.T) {
	idle := map[int]float64{}
	readings := []GPUReading{{GPUID: 0, Watts: 163, Model: "qwen"}, {GPUID: 1, Watts: 214, Model: "qwen"}}
	for i := 2; i < 10; i++ {
		readings = append(readings, GPUReading{GPUID: i, Watts: 21})
	}
	for i := range 10 {
		idle[i] = 21
	}

	got := Attribute(372, readings, idle, 195)

	for i := 2; i < 10; i++ {
		if got.Watts[i] != 0 {
			t.Errorf("idle gpu%d should carry no attributed load, got %f", i, got.Watts[i])
		}
	}
	if got.Watts[0] <= 0 || got.Watts[1] <= 0 {
		t.Fatalf("loaded GPUs should carry load, got %f and %f", got.Watts[0], got.Watts[1])
	}
	// gpu1 was drawing more above idle, so it must carry the larger share.
	if got.Watts[1] <= got.Watts[0] {
		t.Errorf("expected gpu1 > gpu0, got %f vs %f", got.Watts[1], got.Watts[0])
	}
}

// Nothing may be invented: baseline plus every share must equal what was measured.
func TestAttributeReconciles(t *testing.T) {
	cases := []struct {
		name     string
		nodeW    float64
		baseline float64
		readings []GPUReading
	}{
		{"under load", 372, 195, []GPUReading{{GPUID: 0, Watts: 163}, {GPUID: 1, Watts: 21}}},
		{"fully idle", 195, 195, []GPUReading{{GPUID: 0, Watts: 21}, {GPUID: 1, Watts: 21}}},
		{"below baseline", 180, 195, []GPUReading{{GPUID: 0, Watts: 21}}},
		{"no gpus", 195, 100, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idle := map[int]float64{0: 21, 1: 21}
			got := Attribute(tc.nodeW, tc.readings, idle, tc.baseline)
			total := got.BaselineW
			for _, w := range got.Watts {
				total += w
			}
			if diff := total - tc.nodeW; diff > 0.001 || diff < -0.001 {
				t.Errorf("attribution must sum to node power: got %f want %f", total, tc.nodeW)
			}
		})
	}
}

func TestAttributeIdleNodeChargesNothing(t *testing.T) {
	got := Attribute(195, []GPUReading{{GPUID: 0, Watts: 21}, {GPUID: 1, Watts: 21}}, map[int]float64{0: 21, 1: 21}, 195)
	if got.BaselineW != 195 {
		t.Errorf("an idle node is all baseline, got %f", got.BaselineW)
	}
}

func TestStoreWriteAndRead(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	idx, err := s.ModelIndex("qwen")
	if err != nil {
		t.Fatalf("model index: %v", err)
	}
	node := NodeRecord{TS: base.Unix(), Watts: 372, CoveredS: 60}
	gpus := []GPURecord{
		{TS: base.Unix(), GPUID: 0, ModelIdx: idx, AttrW: 100, RawW: 163, CoveredS: 60},
		{TS: base.Unix(), GPUID: 1, ModelIdx: idx, AttrW: 77, RawW: 214, CoveredS: 60},
	}
	if err := s.WriteMinute(node, gpus); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := s.ReadNode(TierMinute, base.Add(-time.Minute), base.Add(time.Minute))
	if len(got) != 1 || got[0].Watts != 372 {
		t.Fatalf("expected the node record back, got %+v", got)
	}
	if g := s.ReadGPU(TierMinute, base.Add(-time.Minute), base.Add(time.Minute)); len(g) != 2 {
		t.Fatalf("expected 2 GPU records, got %d", len(g))
	}

	// The write must have rolled up into the hour and day tiers.
	if h := s.ReadNode(TierHour, base.Add(-time.Hour), base.Add(time.Hour)); len(h) != 1 {
		t.Errorf("expected an hourly roll-up, got %+v", h)
	}
	if d := s.ReadNode(TierDay, base.Add(-24*time.Hour), base.Add(24*time.Hour)); len(d) != 1 {
		t.Errorf("expected a daily roll-up, got %+v", d)
	}
}

// Roll-ups recompute from the finer tier, so writing the same minute twice must
// not double count. This is what makes a restart mid-hour safe.
func TestRollUpIsIdempotent(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	node := NodeRecord{TS: base.Unix(), Watts: 300, CoveredS: 60}

	for range 3 {
		if err := s.WriteMinute(node, nil); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	hour := s.ReadNode(TierHour, base.Truncate(time.Hour), base.Truncate(time.Hour).Add(time.Hour))
	if len(hour) != 1 {
		t.Fatalf("expected 1 hourly record, got %d", len(hour))
	}
	if hour[0].CoveredS != 60 {
		t.Errorf("repeated writes must not accumulate coverage: got %d, want 60", hour[0].CoveredS)
	}
	if hour[0].Watts != 300 {
		t.Errorf("expected 300W, got %f", hour[0].Watts)
	}
}

// The mean must be weighted by covered time, so a barely-observed minute cannot
// pull an hour as hard as a fully observed one.
func TestRollUpWeightsByCoverage(t *testing.T) {
	s := testStore(t)
	hourStart := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)

	if err := s.WriteMinute(NodeRecord{TS: hourStart.Unix(), Watts: 100, CoveredS: 60}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMinute(NodeRecord{TS: hourStart.Add(time.Minute).Unix(), Watts: 400, CoveredS: 6}, nil); err != nil {
		t.Fatal(err)
	}

	hour := s.ReadNode(TierHour, hourStart, hourStart.Add(time.Hour))
	if len(hour) != 1 {
		t.Fatalf("expected 1 hourly record, got %d", len(hour))
	}
	// (100*60 + 400*6) / 66 = 127.27, not the unweighted 250.
	if got := hour[0].Watts; got < 127.2 || got > 127.3 {
		t.Errorf("expected a coverage-weighted 127.3W, got %f", got)
	}
}

// Retention is the wrap: a bucket exactly one ring-length later reuses the slot.
func TestRingWrapEvictsOldest(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	later := base.Add(time.Duration(DefaultMinuteSlots) * time.Minute)

	if err := s.WriteMinute(NodeRecord{TS: base.Unix(), Watts: 100, CoveredS: 60}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMinute(NodeRecord{TS: later.Unix(), Watts: 200, CoveredS: 60}, nil); err != nil {
		t.Fatal(err)
	}

	if old := s.ReadNode(TierMinute, base.Add(-time.Minute), base.Add(time.Minute)); len(old) != 0 {
		t.Errorf("expected the older record to be overwritten, got %+v", old)
	}
	if recent := s.ReadNode(TierMinute, later.Add(-time.Minute), later.Add(time.Minute)); len(recent) != 1 {
		t.Errorf("expected the newer record, got %+v", recent)
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	s, err := Open(Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := s.ModelIndex("qwen")
	if err := s.WriteMinute(NodeRecord{TS: base.Unix(), Watts: 372, CoveredS: 60},
		[]GPURecord{{TS: base.Unix(), GPUID: 0, ModelIdx: idx, AttrW: 150, RawW: 163, CoveredS: 60}}); err != nil {
		t.Fatal(err)
	}
	s.Sync()
	s.Close()

	reopened, err := Open(Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC}, quiet())
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer reopened.Close()

	got := reopened.ReadNode(TierMinute, base.Add(-time.Minute), base.Add(time.Minute))
	if len(got) != 1 || got[0].Watts != 372 {
		t.Fatalf("expected the record to survive a restart, got %+v", got)
	}
	// The model table must survive too, or old records would decode to the
	// wrong name.
	if name := reopened.ModelName(idx); name != "qwen" {
		t.Errorf("expected the model name to survive, got %q", name)
	}
}

func TestByModelTotals(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	qwen, _ := s.ModelIndex("qwen")
	gemma, _ := s.ModelIndex("gemma")

	err := s.WriteMinute(NodeRecord{TS: base.Unix(), Watts: 372, CoveredS: 60}, []GPURecord{
		{TS: base.Unix(), GPUID: 0, ModelIdx: qwen, AttrW: 3600, CoveredS: 3600},
		{TS: base.Unix(), GPUID: 1, ModelIdx: gemma, AttrW: 1800, CoveredS: 3600},
	})
	if err != nil {
		t.Fatal(err)
	}

	totals := s.ByModel(TierMinute, base.Add(-time.Minute), base.Add(time.Minute))
	if got := totals["qwen"]; got < 3.5999 || got > 3.6001 {
		t.Errorf("qwen: expected 3.6 kWh, got %f", got)
	}
	if got := totals["gemma"]; got < 1.7999 || got > 1.8001 {
		t.Errorf("gemma: expected 1.8 kWh, got %f", got)
	}
}

// A GPU with no model resident must not be silently credited to a real model.
func TestByModelSeparatesIdle(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	err := s.WriteMinute(NodeRecord{TS: base.Unix(), Watts: 195, CoveredS: 60}, []GPURecord{
		{TS: base.Unix(), GPUID: 0, ModelIdx: 0, AttrW: 3600, CoveredS: 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ByModel(TierMinute, base.Add(-time.Minute), base.Add(time.Minute))["(idle)"]; !ok {
		t.Error("expected unattributed energy to be labelled idle, not folded into a model")
	}
}

func TestDiskBytesIsBounded(t *testing.T) {
	s := testStore(t, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	// The design budget for a 10-GPU host: a couple of megabytes, forever.
	if got := s.DiskBytes(); got > 3<<20 {
		t.Errorf("expected under 3 MB for 10 GPUs, got %s", humanBytes(got))
	}
	before := s.DiskBytes()
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for i := range 200 {
		ts := base.Add(time.Duration(i) * time.Minute)
		if err := s.WriteMinute(NodeRecord{TS: ts.Unix(), Watts: 300, CoveredS: 60}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if s.DiskBytes() != before {
		t.Error("preallocated rings must not grow with writes")
	}
}

func TestRecorderBucketsSamplesIntoMinutes(t *testing.T) {
	s := testStore(t)
	watts := 195.0
	gpuW := 21.0
	rec := NewRecorder(s, 30*time.Second,
		func() (float64, bool) { return watts, true },
		func() []GPUReading {
			return []GPUReading{{GPUID: 0, Watts: gpuW, Model: "qwen"}, {GPUID: 1, Watts: 21, Model: "qwen"}}
		}, quiet())

	base := time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC)
	rec.Sample(base)
	watts, gpuW = 372.0, 163.0
	rec.Sample(base.Add(30 * time.Second))
	// Crossing into the next minute finalises the previous one.
	rec.Sample(base.Add(60 * time.Second))

	got := s.ReadNode(TierMinute, base, base.Add(time.Minute))
	if len(got) != 1 {
		t.Fatalf("expected 1 finalised minute, got %d", len(got))
	}
	if want := float32((195 + 372) / 2.0); got[0].Watts != want {
		t.Errorf("expected the mean of both samples (%f), got %f", want, got[0].Watts)
	}
	if got[0].CoveredS != 60 {
		t.Errorf("two 30s samples cover the minute: got %d", got[0].CoveredS)
	}
}

// A minute that saw only one of its two samples must carry half the coverage,
// not be extrapolated to a full minute.
func TestRecorderPartialMinuteCoverage(t *testing.T) {
	s := testStore(t)
	rec := NewRecorder(s, 30*time.Second,
		func() (float64, bool) { return 300, true },
		func() []GPUReading { return nil }, quiet())

	base := time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC)
	rec.Sample(base)
	rec.Flush(base)

	got := s.ReadNode(TierMinute, base, base.Add(time.Minute))
	if len(got) != 1 {
		t.Fatalf("expected the flushed minute, got %d", len(got))
	}
	if got[0].CoveredS != 30 {
		t.Errorf("expected 30s covered from a single 30s sample, got %d", got[0].CoveredS)
	}
}

// Unmeasurable power must record nothing rather than a confident zero -- the
// same failure the sampler probe exists to prevent.
func TestRecorderSkipsWhenPowerUnavailable(t *testing.T) {
	s := testStore(t)
	rec := NewRecorder(s, 30*time.Second,
		func() (float64, bool) { return 0, false },
		func() []GPUReading { return nil }, quiet())

	base := time.Date(2026, 8, 28, 16, 0, 0, 0, time.UTC)
	rec.Sample(base)
	rec.Flush(base.Add(time.Minute))

	if got := s.ReadNode(TierMinute, base, base.Add(2*time.Minute)); len(got) != 0 {
		t.Errorf("expected nothing recorded, got %+v", got)
	}
}

func TestRecorderRunStopsOnContextCancel(t *testing.T) {
	s := testStore(t)
	rec := NewRecorder(s, 10*time.Millisecond,
		func() (float64, bool) { return 300, true },
		func() []GPUReading { return []GPUReading{{GPUID: 0, Watts: 21, Model: "qwen"}} }, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestModelTableAssignsStableIndices(t *testing.T) {
	s := testStore(t)
	a, _ := s.ModelIndex("qwen")
	b, _ := s.ModelIndex("gemma")
	again, _ := s.ModelIndex("qwen")

	if a == b {
		t.Error("distinct models must get distinct indices")
	}
	if a != again {
		t.Errorf("index must be stable: %d then %d", a, again)
	}
	if a == 0 || b == 0 {
		t.Error("index 0 is reserved for 'no model resident'")
	}
	if s.ModelName(a) != "qwen" || s.ModelName(b) != "gemma" {
		t.Errorf("names must round trip, got %q and %q", s.ModelName(a), s.ModelName(b))
	}
}

// With no history, the lowest card on the host stands in as the idle floor, so
// a fresh store can attribute from its first sample instead of waiting for a
// card's own idle period to be observed.
func TestFloorsUseCrossGPUMinimum(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	node, gpu := s.Floors(now, 195, map[int]float64{0: 163, 1: 21})
	if node != 195 {
		t.Errorf("expected the fallback node floor, got %f", node)
	}
	if gpu[0] != 21 || gpu[1] != 21 {
		t.Errorf("expected the idle card to set the floor for both, got %+v", gpu)
	}
}

// The failure this guards: a busy inference host may go weeks without an idle
// minute. A purely historical baseline would sit at the lowest *loaded* draw and
// attribute nothing at all, silently. Inferring it from unexplained node power
// keeps attribution working on a host that never idles.
func TestAttributeWorksOnNeverIdleHost(t *testing.T) {
	// gb1 under real multi-tenant load: four models, ten cards, nothing idle.
	readings := []GPUReading{
		{GPUID: 0, Watts: 23}, {GPUID: 1, Watts: 77, Model: "qwen"},
		{GPUID: 2, Watts: 22}, {GPUID: 3, Watts: 150, Model: "translategemma"},
		{GPUID: 4, Watts: 22}, {GPUID: 5, Watts: 23},
		{GPUID: 6, Watts: 123, Model: "translategemma"}, {GPUID: 7, Watts: 142, Model: "granite"},
		{GPUID: 8, Watts: 148, Model: "granite"}, {GPUID: 9, Watts: 22},
	}
	idle := make(map[int]float64, len(readings))
	for _, r := range readings {
		idle[r.GPUID] = 22
	}

	// A historical baseline as high as the node's own draw: what a store that
	// has never seen an idle minute would hold.
	got := Attribute(730, readings, idle, 730)

	var charged float64
	for _, w := range got.Watts {
		charged += w
	}
	if charged <= 0 {
		t.Fatal("a never-idle host must still attribute load, got nothing")
	}
	// The inferred baseline should land near the 195W measured idle draw.
	if got.BaselineW < 150 || got.BaselineW > 250 {
		t.Errorf("expected an inferred baseline near the measured 195W, got %f", got.BaselineW)
	}
	// Busy cards carry load; genuinely idle ones do not.
	if got.Watts[8] <= 0 || got.Watts[3] <= 0 {
		t.Errorf("loaded cards should carry load, got gpu3=%f gpu8=%f", got.Watts[3], got.Watts[8])
	}
	for _, id := range []int{0, 2, 4, 5, 9} {
		if got.Watts[id] > 5 {
			t.Errorf("idle gpu%d should carry ~nothing, got %f", id, got.Watts[id])
		}
	}
}

func TestFloorsTakeObservedMinimum(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	for i, w := range []float32{300, 195, 372} {
		ts := base.Add(time.Duration(i) * time.Minute)
		err := s.WriteMinute(NodeRecord{TS: ts.Unix(), Watts: w, CoveredS: 60},
			[]GPURecord{{TS: ts.Unix(), GPUID: 0, RawW: w / 10, CoveredS: 60}})
		if err != nil {
			t.Fatal(err)
		}
	}

	node, gpu := s.Floors(base.Add(5*time.Minute), 999, map[int]float64{0: 999})
	if node != 195 {
		t.Errorf("expected the observed minimum 195, got %f", node)
	}
	if gpu[0] < 19.4 || gpu[0] > 19.6 {
		t.Errorf("expected the observed GPU minimum 19.5, got %f", gpu[0])
	}
}
