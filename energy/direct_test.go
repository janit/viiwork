package energy

import (
	"math"
	"testing"
	"time"
)

// directRecorder wires a Direct recorder over a fixed set of per-card readings,
// samples one full minute, and returns the finalised records.
func directRecorder(t *testing.T, attribute AttributeFunc, base time.Time, watts ...float64) (NodeRecord, []GPURecord, *Store) {
	t.Helper()

	gpus := make([]int, len(watts))
	readings := make([]GPUReading, len(watts))
	var nodeW float64
	for i, w := range watts {
		gpus[i] = i
		readings[i] = GPUReading{GPUID: i, Watts: w, Model: "qwen"}
		// The producer this exists for derives node power by summing the very
		// readings it reports, which is the whole reason there is nothing to split.
		nodeW += w
	}

	s := testStore(t, gpus...)
	rec := NewRecorderWithAttribution(s, 30*time.Second,
		func() (float64, bool) { return nodeW, true },
		func() []GPUReading { return readings },
		attribute, quiet())

	rec.Sample(base)
	rec.Sample(base.Add(30 * time.Second))
	rec.Flush(base.Add(30 * time.Second))

	nodes := s.ReadNode(TierMinute, base, base.Add(time.Minute))
	if len(nodes) != 1 {
		t.Fatalf("expected 1 finalised minute, got %d", len(nodes))
	}
	return nodes[0], s.ReadGPU(TierMinute, base, base.Add(time.Minute)), s
}

// The headline guarantee: a card is charged what it drew, and the shares add up
// to the node figure, so no energy is invented and none goes missing.
func TestDirectChargesEachCardItsOwnDraw(t *testing.T) {
	base := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	node, gpus, _ := directRecorder(t, Direct, base, 40, 160)

	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPU records, got %d", len(gpus))
	}
	var sum float64
	for _, g := range gpus {
		if g.AttrW != g.RawW {
			t.Errorf("gpu %d: AttrW %v != RawW %v", g.GPUID, g.AttrW, g.RawW)
		}
		sum += float64(g.AttrW)
	}
	if math.Abs(sum-float64(node.Watts)) > 1e-4 {
		t.Errorf("shares sum to %v, node measured %v — the two series must reconcile", sum, node.Watts)
	}
}

// The case that motivated the change. Store.Floors falls back to the lowest
// current reading when it has no history, so on a fresh store an evenly loaded
// host has no marginal power at all and the chassis model attributes nothing.
// A busy inference node may go weeks without the idle minute that would fix it.
func TestDirectSurvivesAFreshStoreWhereTheDefaultDoesNot(t *testing.T) {
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	// Default path, evenly loaded: every card attributed zero.
	_, chassis, _ := directRecorder(t, nil, base, 100, 100)
	for _, g := range chassis {
		if g.AttrW != 0 {
			t.Fatalf("precondition changed: expected the chassis model to attribute 0 on a fresh evenly-loaded store, gpu %d got %v", g.GPUID, g.AttrW)
		}
	}

	// Direct, same input: each card charged its own draw.
	node, direct, _ := directRecorder(t, Direct, base, 100, 100)
	for _, g := range direct {
		if g.AttrW != 100 {
			t.Errorf("gpu %d: expected 100 W attributed, got %v", g.GPUID, g.AttrW)
		}
	}
	if node.Watts != 200 {
		t.Errorf("node watts = %v, want 200", node.Watts)
	}
}

// Per-model energy is the point of the split, so check it through the query
// callers actually use rather than only through the raw records.
func TestByModelOverDirectStoreGivesFullCardDraw(t *testing.T) {
	base := time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)
	_, _, s := directRecorder(t, Direct, base, 40, 160)

	got := s.ByModel(TierMinute, base, base.Add(time.Minute))
	// 200 W over the 60 s the two samples cover.
	if want := 200.0 * 60 / 3600 / 1000; math.Abs(got["qwen"]-want) > 1e-9 {
		t.Errorf("ByModel[qwen] = %v, want %v (from %v)", got["qwen"], want, got)
	}
}

// Direct is the identity on watts, and must not lose or merge a card.
func TestDirectIsIdentityOnReadings(t *testing.T) {
	readings := []GPUReading{
		{GPUID: 3, Watts: 214.5, Model: "a"},
		{GPUID: 7, Watts: 20.25, Model: "b"},
	}
	got := Direct(0, readings)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %v", got)
	}
	for _, r := range readings {
		if got[r.GPUID] != r.Watts {
			t.Errorf("gpu %d: got %v want %v", r.GPUID, got[r.GPUID], r.Watts)
		}
	}
}

// A nil AttributeFunc must be exactly NewRecorder, so the promotion of this
// seam cannot change what viiwork records.
func TestNilAttributeMatchesNewRecorder(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	readings := []GPUReading{{GPUID: 0, Watts: 25, Model: "qwen"}, {GPUID: 1, Watts: 190, Model: "qwen"}}

	run := func(rec *Recorder, s *Store) []GPURecord {
		rec.Sample(base)
		rec.Sample(base.Add(30 * time.Second))
		rec.Flush(base.Add(30 * time.Second))
		return s.ReadGPU(TierMinute, base, base.Add(time.Minute))
	}

	sA := testStore(t, 0, 1)
	a := run(NewRecorder(sA, 30*time.Second,
		func() (float64, bool) { return 400, true },
		func() []GPUReading { return readings }, quiet()), sA)

	sB := testStore(t, 0, 1)
	b := run(NewRecorderWithAttribution(sB, 30*time.Second,
		func() (float64, bool) { return 400, true },
		func() []GPUReading { return readings }, nil, quiet()), sB)

	if len(a) != len(b) {
		t.Fatalf("record counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("record %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}
