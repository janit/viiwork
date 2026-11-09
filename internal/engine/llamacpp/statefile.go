package llamacpp

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/janit/viiwork/v2/internal/kvshare"
)

// The layout of a llama.cpp slot save file (POST /slots/N?action=save), for
// kv_share's per-section deltas.
//
// Two saves of one conversation hold the same K/V bytes at shifting offsets:
// each cell's metadata comes before the per-layer data, and every layer's
// rows run over all cells, so a turn that adds cells moves everything after
// it. kvshare diffs per named section from each section's start, which takes
// the shift out. Spec: docs/superpowers/specs/2026-10-29-llamacpp-kv-share-design.md.
//
// The format (LLAMA_STATE_SEQ_VERSION 4, unchanged from b11718 to f5222f2):
//
//	"qsgg" | version u32 | n_tokens u32 | tokens i32[n]
//	memory parts, in order: K/V caches (two for an iSWA model), then the
//	recurrent state (a hybrid or recurrent model)
//	appendices: "SCKP" context checkpoints, "SDFT" draft state
//
// Three things are not marked in the file: whether cells carry the 12-byte
// extension (M-RoPE, PLE), whether every attention layer has V, and whether a
// recurrent layer has a p block. The walk tries the readings in turn and
// keeps the one that consumes the file exactly with every field plausible.
// A file none fits gets the coarse layout (tokens + one "state" section):
// the replica still travels, whole, rather than not at all.

const (
	stateMagic   = "qsgg" // LLAMA_FILE_MAGIC_GGSQ, little-endian
	stateVersion = 4
	ckptMagic    = 0x504b4353 // "SCKP", server_context SLOT_CKPT_MAGIC
	ckptVersion  = 1
	draftMagic   = 0x54464453 // "SDFT", SLOT_DRAFT_MAGIC
	draftVersion = 1

	// ggml types are below GGML_TYPE_COUNT (43 in b11804); room for more.
	maxGGMLType = 63
	// Bounds that only reject what llama.cpp cannot have written.
	maxStreams = 256
	maxCells   = 1 << 26
	maxLayers  = 4096
	maxSeqIDs  = 64
	maxRowSize = 1 << 30
	cellExt    = 12 // llama_kv_cell_ext: x, y, tok
)

func init() { kvshare.RegisterLayout(stateMagic, ParseStateFile) }

// ParseStateFile is StateFileLayout for kvshare.RegisterLayout.
func ParseStateFile(r io.ReaderAt, size int64) (*kvshare.Layout, error) {
	l, _, err := StateFileLayout(r, size)
	return l, err
}

// StateFileLayout walks a slot save file. structured is false when it fell
// back to the coarse layout; an error means the file is not a version 4
// state file at all.
func StateFileLayout(r io.ReaderAt, size int64) (l *kvshare.Layout, structured bool, err error) {
	h := walk{r: r, end: size}
	if string(h.bytes(4)) != stateMagic {
		return nil, false, errors.New("llamacpp: not a slot save file")
	}
	if v := h.u32(); v != stateVersion {
		return nil, false, fmt.Errorf("llamacpp: slot save file version %d, want %d", v, stateVersion)
	}
	n := h.u32()
	h.section("tokens", 4*int64(n))
	if h.err != nil {
		return nil, false, fmt.Errorf("llamacpp: slot save file: %w", h.err)
	}
	head := h.p
	for _, rd := range readings {
		w := walk{r: r, p: head, end: size, out: append([]kvshare.Section(nil), h.out...), reading: rd}
		if w.state(); w.err == nil && w.p == size {
			return &kvshare.Layout{Size: size, Payload: size, Sections: w.out}, true, nil
		}
	}
	coarse := append(h.out, kvshare.Section{Name: "state", Off: head, Len: size - head})
	return &kvshare.Layout{Size: size, Payload: size, Sections: coarse}, false, nil
}

// reading is one interpretation of what the file does not mark.
type reading struct {
	ext bool // cells carry the 12-byte extension
	noV bool // the K/V cache has no V tensors
}

// The common case first: no extension, V present.
var readings = []reading{{}, {ext: true}, {noV: true}, {ext: true, noV: true}}

type walk struct {
	r       io.ReaderAt
	p, end  int64
	out     []kvshare.Section
	err     error
	reading reading
}

func (w *walk) fail(format string, args ...any) {
	if w.err == nil {
		w.err = fmt.Errorf("at %d: "+format, append([]any{w.p}, args...)...)
	}
}

// skip moves past n bytes without reading them.
func (w *walk) skip(n int64) {
	if w.err != nil {
		return
	}
	if n < 0 || n > w.end-w.p {
		w.fail("%d bytes run past the end", n)
		return
	}
	w.p += n
}

func (w *walk) bytes(n int64) []byte {
	if w.err != nil {
		return nil
	}
	if n > w.end-w.p {
		w.fail("%d bytes run past the end", n)
		return nil
	}
	b := make([]byte, n)
	if _, err := w.r.ReadAt(b, w.p); err != nil {
		w.fail("%v", err)
		return nil
	}
	w.p += n
	return b
}

func (w *walk) u32() uint32 {
	if b := w.bytes(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (w *walk) u64() uint64 {
	if b := w.bytes(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

// peek is the u32 at the cursor; ok is false at the end of the file.
func (w *walk) peek() (v uint32, ok bool) {
	if w.err != nil || w.end-w.p < 4 {
		return 0, false
	}
	var b [4]byte
	if _, err := w.r.ReadAt(b[:], w.p); err != nil {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[:]), true
}

func (w *walk) section(name string, n int64) {
	if w.err != nil {
		return
	}
	if n < 0 || n > w.end-w.p {
		w.fail("%s: %d bytes run past the end", name, n)
		return
	}
	w.out = append(w.out, kvshare.Section{Name: name, Off: w.p, Len: n})
	w.p += n
}

// atAppendix: the llama state has ended (end of file or an appendix).
func (w *walk) atAppendix() bool {
	v, ok := w.peek()
	return !ok || v == ckptMagic || v == draftMagic
}

func (w *walk) state() {
	kv := 0
	for w.err == nil && !w.atAppendix() {
		// A K/V part and a recurrent part are told apart by trying: each
		// fails fast on the other's bytes (a recurrent part's position read
		// as a K/V cell count leads to impossible cell metadata).
		save := *w
		w.kvPart(kv)
		if w.err == nil {
			kv++
			continue
		}
		*w = save
		w.recurrentPart()
		if w.err == nil && !w.atAppendix() {
			w.fail("data after the recurrent state")
		}
	}
	for w.err == nil && w.p < w.end {
		switch w.u32() {
		case ckptMagic:
			w.checkpoints()
		case draftMagic:
			if v := w.u32(); v != draftVersion {
				w.fail("draft appendix version %d", v)
			}
			w.buf("draft.dft")
			w.buf("draft.spec")
		default:
			w.fail("unknown appendix")
		}
	}
}

func (w *walk) cellCount() int64 {
	n := w.u32()
	if n > maxCells {
		w.fail("%d cells", n)
	}
	return int64(n)
}

// kvPart is one llama_kv_cache::state_write. Sections are named by the
// part's index and, within it, the order of the streams that hold cells:
// never the stream number, which is the slot, so a conversation restored
// into another slot keeps its section names.
func (w *walk) kvPart(k int) {
	streams := w.u32()
	if streams == 0 || streams > maxStreams {
		w.fail("%d streams", streams)
		return
	}
	held := 0
	for s := uint32(0); s < streams && w.err == nil; s++ {
		cells := w.cellCount()
		if cells == 0 || w.err != nil {
			continue
		}
		name := fmt.Sprintf("kv%d", k)
		if held > 0 {
			name = fmt.Sprintf("kv%d.%d", k, held)
		}
		held++
		w.kvMeta(name, cells)
		w.kvData(name, cells)
	}
}

func (w *walk) kvMeta(name string, cells int64) {
	start := w.p
	br := bufio.NewReaderSize(io.NewSectionReader(w.r, w.p, w.end-w.p), 1<<20)
	var b [8]byte
	var n int64
	for c := int64(0); c < cells; c++ {
		if _, err := io.ReadFull(br, b[:]); err != nil {
			w.fail("cell metadata: %v", err)
			return
		}
		nseq := binary.LittleEndian.Uint32(b[4:])
		// A sequence save writes the cells that hold the sequence, each
		// naming at least it.
		if nseq == 0 || nseq > maxSeqIDs {
			w.fail("cell %d names %d sequences", c, nseq)
			return
		}
		skip := int64(4 * nseq)
		if w.reading.ext {
			skip += cellExt
		}
		if _, err := br.Discard(int(skip)); err != nil {
			w.fail("cell metadata: %v", err)
			return
		}
		n += 8 + skip
	}
	w.p = start
	w.section(name+".meta", n)
}

func (w *walk) typeAndRow(what string) int64 {
	t := w.u32()
	if t > maxGGMLType {
		w.fail("%s: type %d", what, t)
	}
	row := w.u64()
	if row == 0 || row > maxRowSize {
		w.fail("%s: row size %d", what, row)
	}
	return int64(row)
}

func (w *walk) kvData(name string, cells int64) {
	vTrans := w.u32()
	layers := w.u32()
	w.u32() // n_rot_k
	w.u32() // n_rot_v
	if vTrans > 1 || layers == 0 || layers > maxLayers {
		w.fail("K/V header: v_trans %d, %d layers", vTrans, layers)
		return
	}
	for i := uint32(0); i < layers && w.err == nil; i++ {
		row := w.typeAndRow("K")
		w.section(fmt.Sprintf("%s.k.%d", name, i), cells*row)
	}
	if w.reading.noV {
		return
	}
	for i := uint32(0); i < layers && w.err == nil; i++ {
		if vTrans == 0 {
			row := w.typeAndRow("V")
			w.section(fmt.Sprintf("%s.v.%d", name, i), cells*row)
			continue
		}
		// Transposed: per row of the embedding, that element of every cell.
		// One section per layer; the delta finds little in it, since each
		// row's run moves on its own when cells are added.
		if t := w.u32(); t > maxGGMLType {
			w.fail("V: type %d", t)
		}
		el, embd := int64(w.u32()), int64(w.u32())
		if el == 0 || el > 16 || embd == 0 || embd > maxRowSize {
			w.fail("V: element %d, %d rows", el, embd)
			return
		}
		w.section(fmt.Sprintf("%s.v.%d", name, i), embd*cells*el)
	}
}

// recurrentPart is one llama_memory_recurrent::state_write: r (and p) for
// each recurrent layer, then s for each. The file does not say how many
// layers hold state, so the entries are read until the state ends and split
// in half; a p block has no type, and is told from the next entry by its
// first four bytes, which as a row size are far above any ggml type.
func (w *walk) recurrentPart() {
	cells := w.cellCount()
	for c := int64(0); c < cells && w.err == nil; c++ {
		w.u32() // pos
		nseq := w.u32()
		if nseq > maxSeqIDs {
			w.fail("recurrent cell names %d sequences", nseq)
			return
		}
		w.skip(4 * int64(nseq))
	}
	if t := w.u32(); t != 0 {
		w.fail("s_trans %d", t)
		return
	}
	if n := w.u32(); n == 0 || n > maxLayers {
		w.fail("%d recurrent layers", n)
		return
	}
	type entry struct {
		p         bool
		off, size int64
	}
	var entries []entry
	typed := 0
	for w.err == nil && !w.atAppendix() {
		v, _ := w.peek()
		if v <= maxGGMLType {
			row := w.typeAndRow("recurrent")
			entries = append(entries, entry{off: w.p, size: cells * row})
			typed++
		} else {
			if typed == 0 || entries[len(entries)-1].p {
				w.fail("a p block where none can be")
				return
			}
			row := int64(w.u64())
			if row > maxRowSize {
				w.fail("p row size %d", row)
				return
			}
			entries = append(entries, entry{p: true, off: w.p, size: cells * row})
		}
		w.skip(entries[len(entries)-1].size)
	}
	if w.err != nil {
		return
	}
	if typed == 0 || typed%2 != 0 {
		w.fail("%d recurrent entries, want r and s in pairs", typed)
		return
	}
	half, seen := typed/2, 0
	for _, e := range entries {
		var name string
		switch {
		case e.p:
			name = fmt.Sprintf("rs.p.%d", seen-1)
		case seen < half:
			name = fmt.Sprintf("rs.r.%d", seen)
			seen++
		default:
			name = fmt.Sprintf("rs.s.%d", seen-half)
			seen++
		}
		if e.p && seen > half {
			w.fail("a p block among the s entries")
			return
		}
		w.out = append(w.out, kvshare.Section{Name: name, Off: e.off, Len: e.size})
	}
}

// checkpoints is server_context's SCKP appendix. Each is named by its token
// count, which identifies it across saves while its index moves as older
// ones are evicted.
func (w *walk) checkpoints() {
	if v := w.u32(); v != ckptVersion {
		w.fail("checkpoint appendix version %d", v)
		return
	}
	n := w.u32()
	seen := map[int64]int{}
	for i := uint32(0); i < n && w.err == nil; i++ {
		tokens := int64(w.u64())
		w.u32() // pos_min
		w.u32() // pos_max
		name := fmt.Sprintf("ckpt-%d", tokens)
		if k := seen[tokens]; k > 0 {
			name = fmt.Sprintf("%s-%d", name, k)
		}
		seen[tokens]++
		w.buf(name + ".tgt")
		w.buf(name + ".dft")
		w.buf(name + ".spec")
	}
}

// buf is a u64 length and that many bytes, as a section.
func (w *walk) buf(name string) {
	n := w.u64()
	if n > uint64(w.end-w.p) {
		w.fail("%s: %d bytes run past the end", name, n)
		return
	}
	w.section(name, int64(n))
}
