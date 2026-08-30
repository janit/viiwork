package energy

import (
	"bytes"
	"encoding/binary"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file guard the public contract rather than the behaviour:
// the on-disk layout a second implementation writes against, and the
// provenance label that says what NodeRecord.Watts measured. Everything here
// encodes bytes by hand instead of calling the package's own encoders, so a
// change to record.go cannot quietly redefine the format these assert.

const (
	legacyMinuteSlots = 1440
	legacyHourSlots   = 8760
	legacyDaySlots    = 365
)

// writeLegacyRing writes a v1.5.x ring file: the 32-byte VIIWENG1 header
// followed by a zeroed body of slots*lanes*recSize.
func writeLegacyRing(t *testing.T, path string, slots, lanes, recSize int) {
	t.Helper()
	buf := make([]byte, 32+slots*lanes*recSize)
	copy(buf[0:8], "VIIWENG1")
	binary.LittleEndian.PutUint16(buf[8:], uint16(recSize))
	binary.LittleEndian.PutUint32(buf[10:], uint32(slots))
	binary.LittleEndian.PutUint32(buf[14:], uint32(lanes))
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

// putLegacyNode encodes a NodeRecord by hand into the slot its own timestamp
// selects, which is the slot rule the format is built on.
func putLegacyNode(t *testing.T, path string, slots int, ts int64, watts float32, covered uint16) {
	t.Helper()
	rec := make([]byte, 16)
	binary.LittleEndian.PutUint64(rec[0:], uint64(ts))
	binary.LittleEndian.PutUint32(rec[8:], math.Float32bits(watts))
	binary.LittleEndian.PutUint16(rec[12:], covered)

	slot := (ts / 60) % int64(slots)
	writeAt(t, path, rec, 32+slot*16)
}

func putLegacyGPU(t *testing.T, path string, slots, lanes, lane int, ts int64, gpuID, modelIdx uint16, attrW, rawW float32, covered uint16) {
	t.Helper()
	rec := make([]byte, 24)
	binary.LittleEndian.PutUint64(rec[0:], uint64(ts))
	binary.LittleEndian.PutUint16(rec[8:], gpuID)
	binary.LittleEndian.PutUint16(rec[10:], modelIdx)
	binary.LittleEndian.PutUint32(rec[12:], math.Float32bits(attrW))
	binary.LittleEndian.PutUint32(rec[16:], math.Float32bits(rawW))
	binary.LittleEndian.PutUint16(rec[20:], covered)

	slot := (ts / 60) % int64(slots)
	writeAt(t, path, rec, 32+(slot*int64(lanes)+int64(lane))*24)
}

func writeAt(t *testing.T, path string, b []byte, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// legacyStoreDir builds a store directory in the exact shape v1.5.x left
// behind: six rings at the default geometry, a models table, and no source
// file. Returns the directory and the minute timestamps written.
func legacyStoreDir(t *testing.T, gpus int) (string, []int64) {
	t.Helper()
	dir := t.TempDir()

	writeLegacyRing(t, filepath.Join(dir, "node-minute.ring"), legacyMinuteSlots, 1, 16)
	writeLegacyRing(t, filepath.Join(dir, "node-hour.ring"), legacyHourSlots, 1, 16)
	writeLegacyRing(t, filepath.Join(dir, "node-day.ring"), legacyDaySlots, 1, 16)
	writeLegacyRing(t, filepath.Join(dir, "gpu-minute.ring"), legacyMinuteSlots, gpus, 24)
	writeLegacyRing(t, filepath.Join(dir, "gpu-hour.ring"), legacyHourSlots, gpus, 24)
	writeLegacyRing(t, filepath.Join(dir, "gpu-day.ring"), legacyDaySlots, gpus, 24)

	// Index 0 is reserved for "no model", so gemma-4 lands at index 1.
	if err := os.WriteFile(filepath.Join(dir, "models.txt"), []byte("\ngemma-4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 60 minutes ending at the current one, so the rolling 24h window covers
	// all of it wherever the clock happens to be.
	nodePath := filepath.Join(dir, "node-minute.ring")
	gpuPath := filepath.Join(dir, "gpu-minute.ring")
	nowBucket := time.Now().Unix() / 60
	var written []int64
	for i := range 60 {
		ts := (nowBucket - int64(59-i)) * 60
		putLegacyNode(t, nodePath, legacyMinuteSlots, ts, 300, 60)
		putLegacyGPU(t, gpuPath, legacyMinuteSlots, gpus, 0, ts, 0, 1, 150, 214.5, 60)
		written = append(written, ts)
	}
	return dir, written
}

// TestReadsStoreWrittenByPreviousVersion is the guarantee the second
// implementation depends on: the promotion moved the package without moving a
// byte on disk. The fixture is encoded by hand above, so this fails if the
// layout ever drifts, not merely if the encoder and decoder drift together.
func TestReadsStoreWrittenByPreviousVersion(t *testing.T) {
	dir, written := legacyStoreDir(t, 2)

	var logbuf bytes.Buffer
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC},
		log.New(&logbuf, "", 0))
	if err != nil {
		t.Fatalf("opening a v1.5.x store: %v", err)
	}
	defer s.Close()

	if strings.Contains(logbuf.String(), "discarded") {
		t.Errorf("v1.5.x geometry was rejected: %s", logbuf.String())
	}

	// 60 buckets x 300 W x 60 s = 0.3 kWh.
	if got := s.KWh24h(); math.Abs(got-0.3) > 1e-9 {
		t.Errorf("KWh24h = %v, want 0.3", got)
	}

	recs := s.ReadNode(TierMinute, time.Unix(written[0], 0), time.Unix(written[len(written)-1]+60, 0))
	if len(recs) != 60 {
		t.Fatalf("read %d node records, want 60", len(recs))
	}
	if recs[0].TS != written[0] || recs[0].Watts != 300 || recs[0].CoveredS != 60 {
		t.Errorf("first record decoded as %+v", recs[0])
	}

	// Per-GPU layout and the model table index together: 60 x 150 W x 60 s.
	byModel := s.ByModel(TierMinute, time.Unix(written[0], 0), time.Unix(written[len(written)-1]+60, 0))
	if got := byModel["gemma-4"]; math.Abs(got-0.15) > 1e-9 {
		t.Errorf("ByModel[gemma-4] = %v, want 0.15 (from %v)", got, byModel)
	}
}

// A store written before v1.6.0 carries no provenance label, and must read as
// unknown rather than as some default.
func TestLegacyStoreHasNoSource(t *testing.T) {
	dir, _ := legacyStoreDir(t, 2)
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := s.Source(); got != "" {
		t.Errorf("Source() = %q, want empty for a pre-v1.6.0 store", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(err) {
		t.Error("a caller that said nothing must not invent a source file")
	}
}

func TestSourceIsRecordedAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC, Source: "sensor:PSU1 Power"}

	s, err := Open(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Source(); got != "sensor:PSU1 Power" {
		t.Errorf("Source() = %q", got)
	}
	s.Close()

	// A build that cannot say keeps the label the history already earned.
	silent := cfg
	silent.Source = ""
	s2, err := Open(silent, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Source(); got != "sensor:PSU1 Power" {
		t.Errorf("after reopen with no source, Source() = %q, want the recorded one", got)
	}
}

func TestSourceChangeIsRecordedAndLogged(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC, Source: "dcmi"}, quiet()); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC, Source: "nvidia-smi"},
		log.New(&logbuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if got := s.Source(); got != "nvidia-smi" {
		t.Errorf("Source() = %q, want nvidia-smi", got)
	}
	if !strings.Contains(logbuf.String(), "changed") {
		t.Errorf("a change of what node power means must be logged, got: %s", logbuf.String())
	}

	onDisk, err := readSource(filepath.Join(dir, "source"))
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != "nvidia-smi" {
		t.Errorf("source file holds %q", onDisk)
	}
}

// A foreign or newer magic must stop the node, not reinitialise the history.
// With two implementations writing this format, a silent reinit here destroys
// the other's year of data and looks exactly like a node that was never on.
func TestForeignMagicIsRefusedAndPreservesData(t *testing.T) {
	dir, _ := legacyStoreDir(t, 2)
	path := filepath.Join(dir, "node-minute.ring")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	writeAt(t, path, []byte("VIIWENG2"), 0)

	if _, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC}, quiet()); err == nil {
		t.Fatal("expected Open to refuse a VIIWENG2 file")
	} else if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should say it refused rather than reinitialised: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || !bytes.Equal(after[32:], before[32:]) {
		t.Error("refusing to open must leave the records untouched")
	}
}

func TestMismatchedRecordSizeIsRefused(t *testing.T) {
	dir, _ := legacyStoreDir(t, 2)
	path := filepath.Join(dir, "gpu-minute.ring")

	size := make([]byte, 2)
	binary.LittleEndian.PutUint16(size, 32)
	writeAt(t, path, size, 8)

	_, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC}, quiet())
	if err == nil {
		t.Fatal("expected Open to refuse a file whose records are a different size")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("error should say it refused: %v", err)
	}
}

// The other half of the rule: a slot count or GPU count is per-deployment
// configuration, so changing it still recreates rather than refusing.
func TestGeometryChangeStillRecreates(t *testing.T) {
	dir := t.TempDir()
	base := Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, MinuteSlots: 120}

	s, err := Open(base, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Truncate(time.Minute)
	if err := s.WriteMinute(NodeRecord{TS: ts.Unix(), Watts: 300, CoveredS: 60}, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	grown := base
	grown.MinuteSlots = 240

	var logbuf bytes.Buffer
	s2, err := Open(grown, log.New(&logbuf, "", 0))
	if err != nil {
		t.Fatalf("a slot count change must still be allowed: %v", err)
	}
	defer s2.Close()

	if !strings.Contains(logbuf.String(), "discarded") {
		t.Errorf("discarding history must be said out loud, got: %s", logbuf.String())
	}
	if got := s2.NodeKWh(TierMinute, ts.Add(-time.Hour), ts.Add(time.Hour)); got != 0 {
		t.Errorf("history should have been discarded, got %v kWh", got)
	}
}

// A GPU added to or removed from the host changes the lane count, which is the
// same class of change and must behave the same way.
func TestLaneCountChangeRecreates(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC}, quiet()); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1, 2}, Location: time.UTC},
		log.New(&logbuf, "", 0))
	if err != nil {
		t.Fatalf("adding a GPU must not be fatal: %v", err)
	}
	defer s.Close()
	if !strings.Contains(logbuf.String(), "discarded") {
		t.Errorf("expected the reset to be logged, got: %s", logbuf.String())
	}
}

// The record sizes and the default geometry are compatibility surface: a
// change to any of them is a format change that has to bump the magic, so pin
// them where a casual edit will trip over it.
func TestCompatibilitySurfaceIsPinned(t *testing.T) {
	if NodeRecordSize != 16 || GPURecordSize != 24 {
		t.Errorf("record sizes are on-disk format: got node=%d gpu=%d, want 16/24", NodeRecordSize, GPURecordSize)
	}
	if ringHeaderLen != 32 || ringMagic != "VIIWENG1" {
		t.Errorf("ring header is on-disk format: got %q/%d", ringMagic, ringHeaderLen)
	}
	if DefaultMinuteSlots != 1440 || DefaultHourSlots != 8760 || DefaultDaySlots != 365 {
		t.Errorf("default geometry changed: %d/%d/%d", DefaultMinuteSlots, DefaultHourSlots, DefaultDaySlots)
	}
}
