// Package kvshare moves engine conversation snapshots between hosts: the
// session-file layouts, block manifests, deltas against a base the receiver
// already holds, and the transports that carry them.
//
// It reads session files (Strata's STRSESS v1, docs/DETAILS.md "Session files"
// in Strata, here; other formats from their engine's package through
// RegisterLayout) but never writes one itself: a file is rebuilt from a base
// and a delta byte for byte, and its whole-file hash is checked before it is
// published. The engine validates it again on restore.
package kvshare

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	sessMagic   = "STRSESS\x01"
	sessVersion = 1
	sessHeader  = 64
	sessTrailer = 16 // payload hash + end marker
)

// Section is one named byte range of a session file whose length the file
// declares: a state array, a token list or one part of a K/V layer. Bytes
// between sections (the header, counts, K/V layer headers, the trailer) are
// framing and always travel in a delta as data.
type Section struct {
	Name string
	Off  int64
	Len  int64
}

// Layout is a parsed session file.
type Layout struct {
	Size int64
	// Model and Config are the file's own fingerprints (Strata's), zero for
	// a format without any.
	Model    uint64
	Config   uint64
	Payload  int64
	Sections []Section
}

// LayoutParser walks one session file format without reading the bulk data.
type LayoutParser func(r io.ReaderAt, size int64) (*Layout, error)

type layoutFormat struct {
	magic string
	parse LayoutParser
}

var layouts struct {
	sync.RWMutex
	formats []layoutFormat
}

func init() { RegisterLayout(sessMagic, parseStrata) }

// RegisterLayout adds a session file format, recognised by the bytes it
// starts with. An engine's package registers its own format at init, so a
// format is one package's business. Registering a magic twice, or one that
// is a prefix of another's, panics.
func RegisterLayout(magic string, parse LayoutParser) {
	if magic == "" || parse == nil {
		panic("kvshare: RegisterLayout needs a magic and a parser")
	}
	layouts.Lock()
	defer layouts.Unlock()
	for _, f := range layouts.formats {
		if len(f.magic) <= len(magic) && magic[:len(f.magic)] == f.magic || len(magic) < len(f.magic) && f.magic[:len(magic)] == magic {
			panic(fmt.Sprintf("kvshare: session file magic %q clashes with %q", magic, f.magic))
		}
	}
	layouts.formats = append(layouts.formats, layoutFormat{magic, parse})
}

// ParseLayout walks a session file of any registered format.
func ParseLayout(r io.ReaderAt, size int64) (*Layout, error) {
	layouts.RLock()
	formats := layouts.formats
	layouts.RUnlock()
	for _, f := range formats {
		if size < int64(len(f.magic)) {
			continue
		}
		b := make([]byte, len(f.magic))
		if _, err := r.ReadAt(b, 0); err != nil {
			return nil, fmt.Errorf("kvshare: reading the magic: %w", err)
		}
		if string(b) == f.magic {
			return f.parse(r, size)
		}
	}
	return nil, errors.New("kvshare: not a session file of any known format")
}

// parseStrata walks a Strata session file (STRSESS v1).
func parseStrata(r io.ReaderAt, size int64) (*Layout, error) {
	if size < sessHeader+sessTrailer {
		return nil, errors.New("kvshare: too short for a session file")
	}
	var h [sessHeader]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return nil, fmt.Errorf("kvshare: reading the header: %w", err)
	}
	if string(h[:8]) != sessMagic {
		return nil, errors.New("kvshare: not a Strata session file")
	}
	if v := binary.LittleEndian.Uint32(h[8:]); v != sessVersion {
		return nil, fmt.Errorf("kvshare: session file version %d, want %d", v, sessVersion)
	}
	if hs := binary.LittleEndian.Uint32(h[12:]); hs != sessHeader {
		return nil, fmt.Errorf("kvshare: header size %d, want %d", hs, sessHeader)
	}
	l := &Layout{
		Size:    size,
		Model:   binary.LittleEndian.Uint64(h[16:]),
		Config:  binary.LittleEndian.Uint64(h[24:]),
		Payload: int64(binary.LittleEndian.Uint64(h[32:])),
	}
	if l.Payload < 0 || sessHeader+l.Payload+sessTrailer != size {
		return nil, fmt.Errorf("kvshare: payload %d does not fit a %d-byte file", l.Payload, size)
	}
	w := walker{r: r, p: sessHeader, end: sessHeader + l.Payload}
	w.skip(18*8 + 3*8) // geometry, layer range, cvec flag
	w.checkpoint("live")
	n := w.u64()
	for i := uint64(0); i < n && w.err == nil; i++ {
		w.checkpoint(fmt.Sprintf("ckpt%d", i))
	}
	nkv := w.u64()
	for i := uint64(0); i < nkv && w.err == nil; i++ {
		w.skip(7 * 8) // format, cells, heads, head_dim, page_size, pooled_rows, idx_dim
		for _, part := range []string{"k", "v", "k_scale", "v_scale", "pooled"} {
			w.vec(fmt.Sprintf("kv%d.%s", i, part), 1)
		}
	}
	if w.err != nil {
		return nil, w.err
	}
	if w.p != w.end {
		return nil, fmt.Errorf("kvshare: payload walk ended at %d, payload ends at %d", w.p, w.end)
	}
	l.Sections = w.out
	return l, nil
}

type walker struct {
	r      io.ReaderAt
	p, end int64
	out    []Section
	err    error
}

func (w *walker) skip(n int64) {
	if w.err != nil {
		return
	}
	if n < 0 || n > w.end-w.p {
		w.err = fmt.Errorf("kvshare: a field of %d bytes at %d runs past the payload", n, w.p)
		return
	}
	w.p += n
}

func (w *walker) u64() uint64 {
	if w.err != nil {
		return 0
	}
	var b [8]byte
	if 8 > w.end-w.p {
		w.err = fmt.Errorf("kvshare: a count at %d runs past the payload", w.p)
		return 0
	}
	if _, err := w.r.ReadAt(b[:], w.p); err != nil {
		w.err = fmt.Errorf("kvshare: reading at %d: %w", w.p, err)
		return 0
	}
	w.p += 8
	return binary.LittleEndian.Uint64(b[:])
}

// vec is a count followed by count*elem bytes, recorded as a section.
func (w *walker) vec(name string, elem uint64) {
	n := w.u64()
	if w.err != nil {
		return
	}
	if elem != 0 && n > uint64(w.end-w.p)/elem {
		w.err = fmt.Errorf("kvshare: %s: %d elements run past the payload", name, n)
		return
	}
	size := int64(n * elem)
	w.out = append(w.out, Section{Name: name, Off: w.p, Len: size})
	w.p += size
}

func (w *walker) checkpoint(tag string) {
	w.vec(tag+".ids", 4)
	w.vec(tag+".imgs", 16) // (start i64, hash u64) pairs
	for _, part := range []string{"gdn", "ple", "tails", "dead", "block_pos"} {
		w.vec(tag+"."+part, 1)
	}
	w.skip(8) // used
}
