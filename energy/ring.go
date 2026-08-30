package energy

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// The ring file's fixed prefix. Both values are compatibility surface, not
// tunables: see docs/energy-store-format.md.
//
// ringMagic is bumped only when the meaning of the bytes after the header
// changes -- a new record size, a new lane layout, a repurposed field. That
// bump is what turns such a change into a refusal at Open (see openRing)
// rather than a silent reinitialisation of somebody else's history. Adding a
// sibling file, or changing a slot count, is not that kind of change and does
// not bump it.
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

// ringHeader is the parsed 32-byte prefix of a ring file. See
// docs/energy-store-format.md for the byte layout.
type ringHeader struct {
	magic   string
	recSize int
	slots   int
	lanes   int
}

// openRing opens or creates a ring file, and draws a hard line between the two
// kinds of mismatch it can meet.
//
// A *geometry* change — a different slot count, or a host that gained or lost
// a GPU — is legitimately per-deployment configuration. The file is recreated,
// which discards history, and the caller is expected to say so out loud.
//
// A *format* change — a foreign or newer magic, or a different record size —
// is refused instead. That distinction is the whole point: with two
// independent implementations writing this format, a build whose records
// disagree about their own size would otherwise silently reinitialise the
// other's history on first open, and the operator's first clue would be a
// year of missing energy. Refusing is recoverable; a silent reinit is not.
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

	// A file this process just created has no header to disagree with.
	if info.Size() == 0 {
		if err := r.reinit(size); err != nil {
			f.Close()
			return nil, false, err
		}
		return r, false, nil
	}

	head, err := r.readHeader()
	if err != nil {
		f.Close()
		return nil, false, fmt.Errorf("%s: cannot read the %d-byte header of an existing %d-byte file: %w (move the store directory aside to start a new history)", path, ringHeaderLen, info.Size(), err)
	}
	if head.magic != ringMagic {
		f.Close()
		return nil, false, fmt.Errorf("%s: on-disk format is %q, this build writes %q; refusing to overwrite it (move the store directory aside to start a new history)", path, head.magic, ringMagic)
	}
	if head.recSize != recSize {
		f.Close()
		return nil, false, fmt.Errorf("%s: records on disk are %d bytes, this build writes %d; refusing to overwrite it (move the store directory aside to start a new history)", path, head.recSize, recSize)
	}

	if head.slots == slots && head.lanes == lanes && info.Size() == size {
		return r, false, nil
	}

	if err := r.reinit(size); err != nil {
		f.Close()
		return nil, false, err
	}
	// reset is true when an existing file was discarded, not on first creation.
	return r, true, nil
}

func (r *ring) readHeader() (ringHeader, error) {
	head := make([]byte, ringHeaderLen)
	if _, err := r.f.ReadAt(head, 0); err != nil {
		return ringHeader{}, err
	}
	return ringHeader{
		magic:   string(head[0:8]),
		recSize: int(binary.LittleEndian.Uint16(head[8:])),
		slots:   int(binary.LittleEndian.Uint32(head[10:])),
		lanes:   int(binary.LittleEndian.Uint32(head[14:])),
	}, nil
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
