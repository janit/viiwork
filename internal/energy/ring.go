package energy

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

const (
	ringMagic     = "VIIWENG1"
	ringHeaderLen = 32
)

// ring is a fixed-size, file-backed circular buffer.
//
// The slot for a bucket is derived from the bucket's own timestamp
// (bucketIndex % slots) rather than from a head pointer. That one choice
// removes most of what usually makes an on-disk ring fiddly: writes are
// idempotent, a restart needs no recovery scan, retention needs no purge (a new
// record overwrites the one exactly N periods older), and a re-run of the same
// roll-up simply rewrites the same slot. Reads filter by timestamp, so a slot
// still holding last year's value is ignored rather than mistaken for current.
type ring struct {
	mu      sync.Mutex
	f       *os.File
	slots   int
	lanes   int // records per slot: 1 for node series, one per GPU otherwise
	recSize int
}

// openRing opens or creates a ring file. A file whose geometry no longer
// matches the configuration is recreated rather than misread; that discards
// history, so the caller is expected to say so out loud.
func openRing(path string, slots, lanes, recSize int) (*ring, bool, error) {
	if slots < 1 || lanes < 1 {
		return nil, false, fmt.Errorf("ring %s: slots and lanes must be >= 1", path)
	}
	size := int64(ringHeaderLen) + int64(slots)*int64(lanes)*int64(recSize)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("opening %s: %w", path, err)
	}

	r := &ring{f: f, slots: slots, lanes: lanes, recSize: recSize}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false, err
	}

	if info.Size() == size && r.headerMatches() {
		return r, false, nil
	}

	if err := r.reinit(size); err != nil {
		f.Close()
		return nil, false, err
	}
	// reset is true when an existing file was discarded, not on first creation.
	return r, info.Size() != 0, nil
}

func (r *ring) headerMatches() bool {
	head := make([]byte, ringHeaderLen)
	if _, err := r.f.ReadAt(head, 0); err != nil {
		return false
	}
	return string(head[0:8]) == ringMagic &&
		int(binary.LittleEndian.Uint16(head[8:])) == r.recSize &&
		int(binary.LittleEndian.Uint32(head[10:])) == r.slots &&
		int(binary.LittleEndian.Uint32(head[14:])) == r.lanes
}

func (r *ring) reinit(size int64) error {
	if err := r.f.Truncate(0); err != nil {
		return err
	}
	if err := r.f.Truncate(size); err != nil {
		return err
	}
	head := make([]byte, ringHeaderLen)
	copy(head[0:8], ringMagic)
	binary.LittleEndian.PutUint16(head[8:], uint16(r.recSize))
	binary.LittleEndian.PutUint32(head[10:], uint32(r.slots))
	binary.LittleEndian.PutUint32(head[14:], uint32(r.lanes))
	_, err := r.f.WriteAt(head, 0)
	return err
}

func (r *ring) offset(slot, lane int) int64 {
	return int64(ringHeaderLen) + (int64(slot)*int64(r.lanes)+int64(lane))*int64(r.recSize)
}

// put writes one record. bucket is the absolute bucket index; the slot is
// bucket modulo the ring length.
func (r *ring) put(bucket int64, lane int, enc func([]byte)) error {
	if lane < 0 || lane >= r.lanes {
		return fmt.Errorf("lane %d out of range", lane)
	}
	buf := make([]byte, r.recSize)
	enc(buf)

	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.f.WriteAt(buf, r.offset(int(mod(bucket, int64(r.slots))), lane))
	return err
}

// all reads every slot. Callers filter by timestamp; a slot never written, or
// holding a record older than the window asked for, is simply skipped.
func (r *ring) all() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := make([]byte, r.slots*r.lanes*r.recSize)
	if _, err := r.f.ReadAt(buf, ringHeaderLen); err != nil {
		return nil, err
	}
	return buf, nil
}

func (r *ring) sync() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Sync()
}

func (r *ring) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// mod is a modulo that stays non-negative for timestamps before the epoch,
// which a clock that has not yet been set can briefly produce.
func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}
