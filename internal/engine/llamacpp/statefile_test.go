package llamacpp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/kvshare"
)

// A synthetic slot save file, written field by field as llama.cpp writes one
// (llama_context::state_seq_save_file, llama_kv_cache::state_write,
// llama_memory_recurrent::state_write, server_context save_slot_checkpoints
// and save_slot_draft). Every section's bytes are filled with its own value,
// so a test can check that a parsed section points at the right bytes. The
// value comes from the section's name: a checkpoint holds the same bytes in
// two saves although its place in the appendix moved.

type kvSpec struct {
	streams, active int // n_stream, and the one holding the sequence
	cells           int
	firstPos        int
	ext             bool // M-RoPE / PLE cells carry 12 more bytes
	kRows           []int
	vRows           []int // nil with noV
	noV             bool
	vTrans          bool
}

type recSpec struct {
	cells int
	rRows []int
	pRows []int // 0 where the layer has no p block
	sRows []int
}

type ckptSpec struct {
	nTokens        int64
	tgt, dft, spec int
}

type stateSpec struct {
	tokens int
	kv     []kvSpec
	rec    *recSpec
	ckpts  []ckptSpec
	draft  *[2]int
	fill   byte // added to every fill value: two files of one spec differ in it
}

type stateWriter struct {
	b    bytes.Buffer
	fill map[string]byte
	base byte
}

func (w *stateWriter) u32(v uint32) { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *stateWriter) i32(v int32)  { binary.Write(&w.b, binary.LittleEndian, v) }
func (w *stateWriter) u64(v uint64) { binary.Write(&w.b, binary.LittleEndian, v) }

func (w *stateWriter) section(name string, n int) {
	h := fnv.New32a()
	h.Write([]byte(name))
	v := byte(h.Sum32()) + w.base
	w.fill[name] = v
	w.b.Write(bytes.Repeat([]byte{v}, n))
}

func (s stateSpec) bytes() ([]byte, map[string]byte) {
	w := &stateWriter{fill: map[string]byte{}, base: s.fill}
	w.b.WriteString(stateMagic)
	w.u32(stateVersion)
	w.u32(uint32(s.tokens))
	w.section("tokens", 4*s.tokens)
	for k, kv := range s.kv {
		w.u32(uint32(kv.streams))
		for st := 0; st < kv.streams; st++ {
			if st != kv.active {
				w.u32(0)
				continue
			}
			w.u32(uint32(kv.cells))
			meta := &stateWriter{fill: map[string]byte{}}
			for c := 0; c < kv.cells; c++ {
				meta.i32(int32(kv.firstPos + c))
				meta.u32(1)
				if kv.ext {
					meta.b.Write(make([]byte, 12))
				}
				meta.i32(int32(kv.active))
			}
			w.b.Write(meta.b.Bytes())
			vt := uint32(0)
			if kv.vTrans {
				vt = 1
			}
			w.u32(vt)
			w.u32(uint32(len(kv.kRows)))
			w.u32(128) // n_rot_k
			w.u32(128) // n_rot_v
			for i, row := range kv.kRows {
				w.i32(1) // f16
				w.u64(uint64(row))
				w.section(fmt.Sprintf("kv%d.k.%d", k, i), kv.cells*row)
			}
			if kv.noV {
				continue
			}
			for i, row := range kv.vRows {
				w.i32(1)
				if kv.vTrans {
					w.u32(2)               // element size
					w.u32(uint32(row / 2)) // n_embd_v_gqa
				} else {
					w.u64(uint64(row))
				}
				w.section(fmt.Sprintf("kv%d.v.%d", k, i), kv.cells*row)
			}
		}
	}
	if r := s.rec; r != nil {
		w.u32(uint32(r.cells))
		for c := 0; c < r.cells; c++ {
			w.i32(int32(s.tokens - 1))
			w.u32(0) // n_seq_id: 0 in a sequence save
		}
		w.u32(0)                                   // s_trans
		w.u32(uint32(len(r.rRows) + len(r.sRows))) // n_layer (attention layers included)
		for i, row := range r.rRows {
			w.i32(0) // f32
			w.u64(uint64(row))
			w.section(fmt.Sprintf("rs.r.%d", i), r.cells*row)
			if r.pRows != nil && r.pRows[i] > 0 {
				w.u64(uint64(r.pRows[i]))
				w.section(fmt.Sprintf("rs.p.%d", i), r.cells*r.pRows[i])
			}
		}
		for i, row := range r.sRows {
			w.i32(0)
			w.u64(uint64(row))
			w.section(fmt.Sprintf("rs.s.%d", i), r.cells*row)
		}
	}
	if len(s.ckpts) > 0 {
		w.u32(ckptMagic)
		w.u32(ckptVersion)
		w.u32(uint32(len(s.ckpts)))
		for _, c := range s.ckpts {
			binary.Write(&w.b, binary.LittleEndian, c.nTokens)
			w.i32(int32(c.nTokens - 1))
			w.i32(int32(c.nTokens - 1))
			for _, part := range []struct {
				name string
				n    int
			}{{"tgt", c.tgt}, {"dft", c.dft}, {"spec", c.spec}} {
				w.u64(uint64(part.n))
				w.section(fmt.Sprintf("ckpt-%d.%s", c.nTokens, part.name), part.n)
			}
		}
	}
	if d := s.draft; d != nil {
		w.u32(draftMagic)
		w.u32(draftVersion)
		w.u64(uint64(d[0]))
		w.section("draft.dft", d[0])
		w.u64(uint64(d[1]))
		w.section("draft.spec", d[1])
	}
	return w.b.Bytes(), w.fill
}

func rows(n, row int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = row
	}
	return out
}

// qwenLike is a hybrid model's save: attention every fourth layer, DeltaNet
// state for the rest, checkpoints appended, saved from slot 1 of 2.
func qwenLike(tokens int) stateSpec {
	return stateSpec{
		tokens: tokens,
		kv:     []kvSpec{{streams: 2, active: 1, cells: tokens, kRows: rows(4, 2048), vRows: rows(4, 2048)}},
		rec:    &recSpec{cells: 1, rRows: rows(12, 24576), sRows: rows(12, 262144)},
		ckpts:  []ckptSpec{{nTokens: int64(tokens - 300), tgt: 3 << 20}, {nTokens: int64(tokens - 40), tgt: 3 << 20}},
	}
}

// checkLayout parses b and checks every section of want is found, by name,
// at bytes holding its fill value, and that the layout covers the file.
func checkLayout(t *testing.T, b []byte, fill map[string]byte) *kvshare.Layout {
	t.Helper()
	l, structured, err := StateFileLayout(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !structured {
		t.Fatalf("fell back to the coarse layout")
	}
	got := map[string]kvshare.Section{}
	end := int64(0)
	for _, s := range l.Sections {
		if s.Off < end {
			t.Fatalf("section %s at %d overlaps the previous one ending at %d", s.Name, s.Off, end)
		}
		end = s.Off + s.Len
		if !kvshare.ValidKey(s.Name) {
			t.Fatalf("section name %q is not one a store can keep", s.Name)
		}
		if _, dup := got[s.Name]; dup {
			t.Fatalf("section name %s twice", s.Name)
		}
		got[s.Name] = s
	}
	if end > int64(len(b)) {
		t.Fatalf("sections end at %d past the file's %d bytes", end, len(b))
	}
	for name, v := range fill {
		s, ok := got[name]
		if !ok {
			t.Errorf("section %s missing (have %v)", name, names(l))
			continue
		}
		if s.Len > 0 && (b[s.Off] != v || b[s.Off+s.Len-1] != v) {
			t.Errorf("section %s at %d+%d does not hold its bytes", name, s.Off, s.Len)
		}
	}
	for name := range got {
		if _, ok := fill[name]; !ok && !strings.HasSuffix(name, ".meta") {
			t.Errorf("unexpected section %s", name)
		}
	}
	return l
}

func names(l *kvshare.Layout) []string {
	var out []string
	for _, s := range l.Sections {
		out = append(out, s.Name)
	}
	return out
}

func TestStateFileLayoutShapes(t *testing.T) {
	cases := map[string]stateSpec{
		"hybrid":           qwenLike(500),
		"hybrid with ext":  func() stateSpec { s := qwenLike(500); s.kv[0].ext = true; return s }(),
		"attention only":   {tokens: 300, kv: []kvSpec{{streams: 1, cells: 300, kRows: rows(6, 1024), vRows: rows(6, 1024)}}},
		"transposed V":     {tokens: 300, kv: []kvSpec{{streams: 1, cells: 300, kRows: rows(3, 1024), vRows: rows(3, 1024), vTrans: true}}},
		"no V (MLA)":       {tokens: 200, kv: []kvSpec{{streams: 3, active: 2, cells: 200, kRows: rows(5, 576), noV: true}}},
		"iSWA, two caches": {tokens: 400, kv: []kvSpec{{streams: 1, cells: 400, kRows: rows(2, 1024), vRows: rows(2, 1024)}, {streams: 1, cells: 128, firstPos: 272, kRows: rows(6, 1024), vRows: rows(6, 1024)}}},
		"recurrent only":   {tokens: 100, rec: &recSpec{cells: 1, rRows: rows(8, 4096), sRows: rows(8, 65536)}},
		"recurrent with p": {tokens: 100, rec: &recSpec{cells: 1, rRows: rows(4, 4096), pRows: []int{512, 0, 512, 0}, sRows: rows(4, 65536)}},
		"draft appendix":   func() stateSpec { s := qwenLike(200); s.draft = &[2]int{1 << 20, 64}; return s }(),
		"no checkpoints":   func() stateSpec { s := qwenLike(200); s.ckpts = nil; return s }(),
		"empty checkpoint": func() stateSpec { s := qwenLike(200); s.ckpts = []ckptSpec{{nTokens: 150, tgt: 4096}}; return s }(),
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			b, fill := spec.bytes()
			checkLayout(t, b, fill)
		})
	}
}

// The reason the layout exists: a turn adds cells, which moves every byte of
// the file after the cell metadata. Per section, the delta still finds the
// whole kept prefix of every K/V layer.
func TestStateFileLayoutDeltaSurvivesTheShift(t *testing.T) {
	old := qwenLike(2000)
	next := qwenLike(2035)
	next.ckpts = append(old.ckpts[1:], ckptSpec{nTokens: 2000, tgt: 3 << 20})
	ob, _ := old.bytes()
	nb, _ := next.bytes()
	ol, _, err := StateFileLayout(bytes.NewReader(ob), int64(len(ob)))
	if err != nil {
		t.Fatal(err)
	}
	nl, _, err := StateFileLayout(bytes.NewReader(nb), int64(len(nb)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := kvshare.FileManifest(bytes.NewReader(ob), ol)
	if err != nil {
		t.Fatal(err)
	}
	p, err := kvshare.Diff(bytes.NewReader(nb), nl, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		if strings.HasPrefix(it.Section, "kv0.k.") && it.Keep < (2000*2048)/kvshare.BlockSize*kvshare.BlockSize {
			t.Errorf("%s keeps %d of %d bytes", it.Section, it.Keep, it.Len)
		}
	}
	// What a turn sends: the new cells, the tails of each K/V layer's last
	// block and the new checkpoint. Far less than the file.
	if p.Data > int64(len(nb))/2 {
		t.Errorf("delta %d bytes of a %d-byte file", p.Data, len(nb))
	}
	// The checkpoint both files hold matches by name although its index moved.
	for _, it := range p.Items {
		if it.Section == fmt.Sprintf("ckpt-%d.tgt", old.ckpts[1].nTokens) && it.Keep == 0 {
			t.Errorf("the checkpoint both files hold was not kept")
		}
	}
}

func TestStateFileLayoutFallsBackToCoarse(t *testing.T) {
	b, _ := qwenLike(300).bytes()
	// A V row size of zero is no file llama.cpp writes.
	bad := bytes.Clone(b)
	off := bytes.Index(bad, []byte{1, 0, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0})
	if off < 0 {
		t.Fatal("test file has no K header to break")
	}
	binary.LittleEndian.PutUint64(bad[off+4:], 0)
	l, structured, err := StateFileLayout(bytes.NewReader(bad), int64(len(bad)))
	if err != nil {
		t.Fatal(err)
	}
	if structured {
		t.Fatal("a broken file parsed as structured")
	}
	if len(l.Sections) != 2 || l.Sections[0].Name != "tokens" || l.Sections[1].Name != "state" {
		t.Fatalf("coarse layout %v", names(l))
	}
	if s := l.Sections[1]; s.Off+s.Len != int64(len(bad)) {
		t.Fatalf("coarse state ends at %d, file %d", s.Off+s.Len, len(bad))
	}
	if _, _, err := StateFileLayout(bytes.NewReader(b[:10]), 10); err == nil {
		t.Fatal("a file shorter than its token list must not parse")
	}
	wrong := bytes.Clone(b)
	binary.LittleEndian.PutUint32(wrong[4:], 3)
	if _, _, err := StateFileLayout(bytes.NewReader(wrong), int64(len(wrong))); err == nil {
		t.Fatal("version 3 must not parse")
	}
}

func TestStateFileRegistered(t *testing.T) {
	b, _ := qwenLike(100).bytes()
	l, err := kvshare.ParseLayout(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Sections) < 10 {
		t.Fatalf("kvshare did not use the llama.cpp layout: %v", names(l))
	}
}

// LLAMACPP_STATE_FILES (a glob) checks real slot save files: each must parse
// structured, and two consecutive saves of one conversation, given in order,
// report what a delta between them would send.
func TestStateFileLayoutRealFiles(t *testing.T) {
	glob := os.Getenv("LLAMACPP_STATE_FILES")
	if glob == "" {
		t.Skip("LLAMACPP_STATE_FILES not set")
	}
	files, _ := filepath.Glob(glob)
	if len(files) == 0 {
		t.Fatalf("no files match %s", glob)
	}
	var prev *kvshare.Manifest
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := fh.Stat()
		l, structured, err := StateFileLayout(fh, st.Size())
		if err != nil || !structured {
			t.Fatalf("%s: structured %v, %v", f, structured, err)
		}
		var covered int64
		for _, s := range l.Sections {
			covered += s.Len
		}
		t.Logf("%s: %d bytes, %d sections covering %.2f%%", filepath.Base(f), st.Size(), len(l.Sections), 100*float64(covered)/float64(st.Size()))
		if prev != nil {
			p, err := kvshare.Diff(fh, l, prev)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("  delta against the previous file: %d of %d bytes (%.1f%%)", p.Data, p.Size, 100*float64(p.Data)/float64(p.Size))
			// by kind of section: K/V layers, recurrent state, checkpoints
			group := regexp.MustCompile(`\.\d+$|-\d+`)
			sent, count := map[string]int64{}, map[string]int{}
			for _, it := range p.Items {
				g := group.ReplaceAllString(it.Section, "*")
				if g == "" {
					g = "(framing)"
				}
				sent[g] += it.Len - it.Keep
				count[g]++
			}
			for g, n := range sent {
				if n > p.Data/100 {
					t.Logf("    %-14s x%-3d sent %7.1f MiB", g, count[g], float64(n)/(1<<20))
				}
			}
		}
		if prev, err = kvshare.FileManifest(fh, l); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
}

// storeTransport puts straight into a store, as the receiving node would.
type storeTransport struct{ st *kvshare.Store }

func (storeTransport) Name() string { return "store" }
func (storeTransport) Listen(string, kvshare.Handler) (kvshare.Server, error) {
	return nil, fmt.Errorf("not a server")
}
func (s storeTransport) Manifest(ctx context.Context, _, key string) (*kvshare.Manifest, error) {
	return s.st.Manifest(ctx, key)
}
func (s storeTransport) Put(ctx context.Context, _, key string, r io.Reader) (kvshare.PutResult, error) {
	return s.st.Put(ctx, key, r)
}

// A replica of a llama.cpp file knows its length in tokens and carries the
// sender's identity, which the file itself has no room for; a base of
// another identity is sent whole.
func TestStateFilePushCarriesTokensAndIdentity(t *testing.T) {
	dir := t.TempDir()
	st := kvshare.NewStore(filepath.Join(dir, "store"))
	if _, err := st.Clean(); err != nil {
		t.Fatal(err)
	}
	tr := storeTransport{st}
	write := func(spec stateSpec) string {
		b, _ := spec.bytes()
		p := filepath.Join(dir, fmt.Sprintf("s%d.bin", spec.tokens))
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctx := context.Background()
	id := kvshare.Identity{Model: 0xaa, Config: 0xbb}
	if _, err := kvshare.PushSaveAs(ctx, tr, "", "k1", write(qwenLike(2000)), nil, "", id); err != nil {
		t.Fatal(err)
	}
	rep, ok, err := st.Info("k1")
	if err != nil || !ok {
		t.Fatalf("no replica: %v", err)
	}
	if rep.Tokens != 2000 || rep.Model != 0xaa || rep.Config != 0xbb {
		t.Fatalf("replica %+v", rep)
	}
	m, _ := st.Manifest(ctx, "k1")
	res, err := kvshare.PushSaveAs(ctx, tr, "", "k1", write(qwenLike(2035)), m, "", id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Data > res.Plan.Size/2 {
		t.Fatalf("same identity: sent %d of %d", res.Plan.Data, res.Plan.Size)
	}
	m, _ = st.Manifest(ctx, "k1")
	res, err = kvshare.PushSaveAs(ctx, tr, "", "k1", write(qwenLike(2036)), m, "", kvshare.Identity{Model: 0xcc, Config: 0xbb})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Data != res.Plan.Size {
		t.Fatalf("another model's base was diffed against: sent %d of %d", res.Plan.Data, res.Plan.Size)
	}
	if rep, _, _ := st.Info("k1"); rep.Model != 0xcc || rep.Tokens != 2036 {
		t.Fatalf("replica after the other model's push: %+v", rep)
	}
}
