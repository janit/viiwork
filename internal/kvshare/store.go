package kvshare

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is a receiver's replicas, one directory per key:
//
//	<dir>/<key>/state.json   identity, digest, the file's items in order, complete flag
//	<dir>/<key>/frames.bin   the framing bytes, concatenated
//	<dir>/<key>/s-<section>  one file per section
//
// A delta truncates each section to what it keeps and appends the rest, so a
// turn costs what travelled, not the size of the conversation. The session
// file Strata restores is assembled only when it is needed (Materialize), and
// its digest is checked then. A Put that fails removes the replica, so the
// next push sends everything.
type Store struct {
	Dir string

	mu    sync.Mutex
	locks map[string]*keyLock
	man   map[string]*Manifest
}

type replicaItem struct {
	Section string `json:"section,omitempty"`
	Len     int64  `json:"len"`
	Frame   int64  `json:"frame"` // offset in frames.bin (framing only)
}

type replicaState struct {
	Model    uint64        `json:"model"`
	Config   uint64        `json:"config"`
	Size     int64         `json:"size"`
	Digest   string        `json:"digest"`
	Complete bool          `json:"complete"`
	Items    []replicaItem `json:"items"`
	Updated  time.Time     `json:"updated"`
	// Tokens is the conversation's length: its token list section (tokenSection)
	// holds one int32 per token.
	Tokens int64 `json:"tokens"`
	// SaveID is the engine's id of the save these bytes are, "" when the
	// sender did not say: an engine delta applies only against it.
	SaveID string `json:"save_id,omitempty"`
	// Chunks are each K/V section's ChunkSize hashes (SessionHash64): what an
	// engine delta's kept chunks are checked against, and every chunk again
	// on assembly.
	Chunks map[string][]uint64 `json:"chunks,omitempty"`
	// Trailer: the file's STRSESS trailer is computed on assembly (a replica
	// rebuilt from an engine delta, which carries no whole-file digest).
	Trailer bool `json:"trailer,omitempty"`
}

func NewStore(dir string) *Store {
	return &Store{Dir: dir, locks: map[string]*keyLock{}, man: map[string]*Manifest{}}
}

func (s *Store) keyDir(key string) string { return filepath.Join(s.Dir, key) }

// keyLock is one key's mutex, counted so that a key nobody holds or waits
// for takes no memory: members may ask about any number of keys.
type keyLock struct {
	sync.Mutex
	refs int
}

func (s *Store) lock(key string) func() {
	s.mu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &keyLock{}
		s.locks[key] = l
	}
	l.refs++
	s.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		s.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
}

func (s *Store) readState(key string) (*replicaState, error) {
	b, err := os.ReadFile(filepath.Join(s.keyDir(key), "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st replicaState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) writeState(key string, st *replicaState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.keyDir(key), "state.json"), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sectionFile names a section's file; a section name is validated like a key.
func (s *Store) sectionFile(key, section string) (string, error) {
	if !ValidKey(section) {
		return "", fmt.Errorf("kvshare: invalid section name %q", section)
	}
	return filepath.Join(s.keyDir(key), "s-"+section), nil
}

func (s *Store) Manifest(_ context.Context, key string) (*Manifest, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("kvshare: invalid key %q", key)
	}
	defer s.lock(key)()
	return s.manifestLocked(key)
}

func (s *Store) manifestLocked(key string) (*Manifest, error) {
	s.mu.Lock()
	m := s.man[key]
	s.mu.Unlock()
	if m != nil {
		return m, nil
	}
	st, err := s.readState(key)
	if err != nil || st == nil || !st.Complete {
		return nil, err
	}
	m = &Manifest{Model: st.Model, Config: st.Config, Size: st.Size, SaveID: st.SaveID}
	for _, it := range st.Items {
		if it.Section == "" {
			continue
		}
		blocks, err := s.hashSection(key, it.Section, 0, it.Len, nil)
		if err != nil {
			return nil, err
		}
		m.Sections = append(m.Sections, SectionManifest{Name: it.Section, Len: it.Len, Blocks: blocks})
	}
	s.mu.Lock()
	s.man[key] = m
	s.mu.Unlock()
	return m, nil
}

// hashSection returns the block hashes of a section of n bytes, reusing prev's
// for the whole blocks below keep.
func (s *Store) hashSection(key, section string, keep, n int64, prev []byte) ([]byte, error) {
	path, err := s.sectionFile(key, section)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reuse := min(keep/BlockSize, int64(len(prev)/16))
	tail, err := hashBlocks(f, reuse*BlockSize, n-reuse*BlockSize)
	if err != nil {
		return nil, err
	}
	return append(bytes.Clone(prev[:reuse*16]), tail...), nil
}

func (s *Store) Put(ctx context.Context, key string, delta io.Reader) (PutResult, error) {
	return s.put(ctx, key, delta, nil)
}

// PutExpect is Put for a sender that declared the new file's size and digest
// up front (FileMeta): a delta whose header says otherwise is refused before
// anything is applied, and Materialize later checks the assembled file
// against the same digest.
func (s *Store) PutExpect(ctx context.Context, key string, delta io.Reader, want FileMeta) (PutResult, error) {
	return s.put(ctx, key, delta, &want)
}

func (s *Store) put(_ context.Context, key string, delta io.Reader, want *FileMeta) (res PutResult, err error) {
	if !ValidKey(key) {
		return PutResult{}, fmt.Errorf("kvshare: invalid key %q", key)
	}
	start := time.Now()
	// The header first, outside the lock: a refused put leaves the replica
	// it would have replaced as it was.
	d, err := NewDeltaReader(delta)
	if err != nil {
		return PutResult{}, err
	}
	if want != nil && (d.Header.Size != want.Size || d.Header.Digest != want.Digest) {
		return PutResult{}, errors.New("kvshare: the delta builds another file than the request declared")
	}
	if want != nil && want.Identity != (Identity{}) && (d.Header.Model != want.Identity.Model || d.Header.Config != want.Identity.Config) {
		return PutResult{}, Statusf(http.StatusConflict, "kvshare: the replica is of another model or configuration (%016x/%016x, this node %016x/%016x)",
			d.Header.Model, d.Header.Config, want.Identity.Model, want.Identity.Config)
	}
	unlock := s.lock(key)
	defer func() {
		if err != nil {
			_ = os.RemoveAll(s.keyDir(key))
			s.mu.Lock()
			delete(s.man, key)
			s.mu.Unlock()
			unlock()
		}
	}()
	old, err := s.readState(key)
	if err != nil {
		return PutResult{}, err
	}
	s.mu.Lock()
	prevMan := s.man[key]
	delete(s.man, key)
	s.mu.Unlock()
	if old != nil && (!old.Complete || old.Model != d.Header.Model || old.Config != d.Header.Config) {
		old, prevMan = nil, nil // nothing in it can be kept
		if err := os.RemoveAll(s.keyDir(key)); err != nil {
			return PutResult{}, err
		}
	}
	if err := os.MkdirAll(s.keyDir(key), 0o700); err != nil {
		return PutResult{}, err
	}
	have := map[string]int64{}
	if old != nil {
		for _, it := range old.Items {
			if it.Section != "" {
				have[it.Section] = it.Len
			}
		}
		old.Complete = false // from here on the sections are being rewritten
		if err := s.writeState(key, old); err != nil {
			return PutResult{}, err
		}
	}
	prevBlocks := map[string][]byte{}
	if prevMan != nil {
		for _, sm := range prevMan.Sections {
			prevBlocks[sm.Name] = sm.Blocks
		}
	}
	next := &replicaState{Model: d.Header.Model, Config: d.Header.Config, Size: d.Header.Size,
		Digest: hex.EncodeToString(d.Header.Digest[:])}
	if want != nil {
		next.SaveID = want.SaveID
	}
	var frames bytes.Buffer
	keeps := map[string]int64{}
	for {
		it, data, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return PutResult{}, err
		}
		if it.Section == "" {
			next.Items = append(next.Items, replicaItem{Len: it.Len, Frame: int64(frames.Len())})
			if _, err := io.Copy(&frames, data); err != nil {
				return PutResult{}, err
			}
			continue
		}
		if it.Keep > have[it.Section] {
			return PutResult{}, fmt.Errorf("kvshare: %s: keep %d of a %d-byte copy", it.Section, it.Keep, have[it.Section])
		}
		if err := s.patchSection(key, it, data); err != nil {
			return PutResult{}, err
		}
		keeps[it.Section] = it.Keep
		delete(have, it.Section)
		next.Items = append(next.Items, replicaItem{Section: it.Section, Len: it.Len})
		if tokenSection[it.Section] {
			next.Tokens = it.Len / 4
		}
	}
	for gone := range have { // sections the new file no longer has
		if p, err := s.sectionFile(key, gone); err == nil {
			_ = os.Remove(p)
		}
	}
	if err := writeAtomic(filepath.Join(s.keyDir(key), "frames.bin"), frames.Bytes()); err != nil {
		return PutResult{}, err
	}
	next.Complete, next.Updated = true, time.Now().UTC()
	if err := s.writeState(key, next); err != nil {
		return PutResult{}, err
	}
	res = PutResult{Size: d.Header.Size, ApplyMs: ms(time.Since(start))}
	go s.finish(key, next, keeps, prevMan, unlock)
	return res, nil
}

// finish builds the next manifest off the sender's clock, still under the
// key's lock, hashing only what this put wrote; after a full put it also
// hashes the K/V sections' chunks, so that an engine delta can be checked
// against them. It releases the lock.
func (s *Store) finish(key string, next *replicaState, keeps map[string]int64, prevMan *Manifest, unlock func()) {
	defer unlock()
	prevBlocks := map[string][]byte{}
	if prevMan != nil {
		for _, sm := range prevMan.Sections {
			prevBlocks[sm.Name] = sm.Blocks
		}
	}
	m := &Manifest{Model: next.Model, Config: next.Config, Size: next.Size, SaveID: next.SaveID}
	chunks := next.Chunks == nil
	if chunks {
		next.Chunks = map[string][]uint64{}
	}
	for _, it := range next.Items {
		if it.Section == "" {
			continue
		}
		blocks, err := s.hashSection(key, it.Section, keeps[it.Section], it.Len, prevBlocks[it.Section])
		if err != nil {
			return
		}
		m.Sections = append(m.Sections, SectionManifest{Name: it.Section, Len: it.Len, Blocks: blocks})
		if chunks && isKVSection(it.Section) {
			path, err := s.sectionFile(key, it.Section)
			if err != nil {
				return
			}
			f, err := os.Open(path)
			if err != nil {
				return
			}
			c, err := chunkHashes(f, 0, it.Len, nil)
			f.Close()
			if err != nil {
				return
			}
			next.Chunks[it.Section] = c
		}
	}
	if chunks && s.writeState(key, next) != nil {
		return
	}
	s.mu.Lock()
	s.man[key] = m
	s.mu.Unlock()
}

func (s *Store) patchSection(key string, it Item, data io.Reader) error {
	path, err := s.sectionFile(key, it.Section)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(it.Keep); err != nil {
		return err
	}
	if _, err := f.Seek(it.Keep, io.SeekStart); err != nil {
		return err
	}
	n, err := io.Copy(f, data)
	if err == nil && n != it.Len-it.Keep {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// MaterializeResult times assembling a session file.
type MaterializeResult struct {
	Size     int64   `json:"size"`
	Ms       float64 `json:"ms"`
	DigestMs float64 `json:"digest_ms"`
}

// Materialize writes the replica under key as a Strata session file at path
// (a temporary file beside it, the digest checked, then renamed over it).
func (s *Store) Materialize(key, path string) (MaterializeResult, error) {
	if !ValidKey(key) {
		return MaterializeResult{}, fmt.Errorf("kvshare: invalid key %q", key)
	}
	defer s.lock(key)()
	start := time.Now()
	st, err := s.readState(key)
	if err != nil {
		return MaterializeResult{}, err
	}
	if st == nil || !st.Complete {
		return MaterializeResult{}, fmt.Errorf("kvshare: no complete replica of %q", key)
	}
	frames, err := os.Open(filepath.Join(s.keyDir(key), "frames.bin"))
	if err != nil {
		return MaterializeResult{}, err
	}
	defer frames.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return MaterializeResult{}, err
	}
	fail := func(err error) (MaterializeResult, error) {
		tmp.Close()
		os.Remove(tmp.Name())
		return MaterializeResult{}, err
	}
	for _, it := range st.Items {
		src := frames
		if it.Section == "" {
			if _, err := frames.Seek(it.Frame, io.SeekStart); err != nil {
				return fail(err)
			}
		} else {
			p, err := s.sectionFile(key, it.Section)
			if err != nil {
				return fail(err)
			}
			if src, err = os.Open(p); err != nil {
				return fail(err)
			}
		}
		// a LimitedReader over a *File: copy_file_range
		n, err := io.Copy(tmp, io.LimitReader(src, it.Len))
		if src != frames {
			src.Close()
		}
		if err == nil && n != it.Len {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return fail(err)
		}
	}
	d := time.Now()
	if st.Trailer {
		if err := sealTrailer(tmp, st); err != nil {
			return fail(err)
		}
	} else {
		got, err := FileDigest(tmp, st.Size)
		if err != nil {
			return fail(err)
		}
		if hex.EncodeToString(got[:]) != st.Digest {
			return fail(errors.New("kvshare: assembled file does not match the sender's digest"))
		}
	}
	digestMs := ms(time.Since(d))
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return MaterializeResult{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return MaterializeResult{}, err
	}
	return MaterializeResult{Size: st.Size, Ms: ms(time.Since(start)), DigestMs: digestMs}, nil
}

// sealTrailer reads back the assembled payload of a replica rebuilt from
// engine deltas, checks every K/V chunk against its hash, and appends the
// STRSESS trailer (the payload's SessionHash64 and the end marker).
func sealTrailer(f *os.File, st *replicaState) error {
	h := NewSessionHash(0)
	var off int64 = sessHeader
	buf := make([]byte, 4<<20)
	read := func(n int64, each func([]byte)) error {
		for n > 0 {
			c := min(n, int64(len(buf)))
			if _, err := f.ReadAt(buf[:c], off); err != nil {
				return err
			}
			h.Write(buf[:c])
			if each != nil {
				each(buf[:c])
			}
			off += c
			n -= c
		}
		return nil
	}
	for i, it := range st.Items {
		n := it.Len
		if i == 0 {
			n -= sessHeader // the header frame comes first and is not payload
		}
		want, isKV := st.Chunks[it.Section]
		if it.Section == "" || !isKV {
			if err := read(n, nil); err != nil {
				return err
			}
			continue
		}
		if int64(len(want)) != (n+ChunkSize-1)/ChunkSize {
			return fmt.Errorf("kvshare: %s: %d chunk hashes for %d bytes", it.Section, len(want), n)
		}
		for c := 0; n > 0; c++ {
			k := min(n, ChunkSize)
			ch := NewSessionHash(0)
			if err := read(k, func(p []byte) { ch.Write(p) }); err != nil {
				return err
			}
			if ch.Sum64() != want[c] {
				return fmt.Errorf("kvshare: %s: chunk %d is damaged", it.Section, c)
			}
			n -= k
		}
	}
	var tr [sessTrailer]byte
	binary.LittleEndian.PutUint64(tr[:], h.Sum64())
	copy(tr[8:], sessEnd)
	if _, err := f.WriteAt(tr[:], off); err != nil {
		return err
	}
	if off+sessTrailer != st.Size {
		return fmt.Errorf("kvshare: assembled %d bytes, the replica is %d", off+sessTrailer, st.Size)
	}
	return nil
}

// tokenSection names the sections that are a file's token list, one int32
// per token, by format: Strata's live ids, llama.cpp's prompt tokens.
var tokenSection = map[string]bool{"live.ids": true, "tokens": true}

// Replica describes a complete replica.
type Replica struct {
	Size    int64
	Digest  string // FileDigest in hex; "" for a replica rebuilt from engine deltas
	SaveID  string
	Tokens  int64
	Updated time.Time
	// Model and Config are the identity the sender stamped (Identity).
	Model, Config uint64
}

// Info reports the complete replica under key; ok is false when there is none.
func (s *Store) Info(key string) (r Replica, ok bool, err error) {
	if !ValidKey(key) {
		return Replica{}, false, fmt.Errorf("kvshare: invalid key %q", key)
	}
	defer s.lock(key)()
	st, err := s.readState(key)
	if err != nil || st == nil || !st.Complete {
		return Replica{}, false, err
	}
	return Replica{Size: st.Size, Digest: st.Digest, SaveID: st.SaveID, Tokens: st.Tokens, Updated: st.Updated, Model: st.Model, Config: st.Config}, true, nil
}

// ResumeIDs names the conversation a replica resumes as Strata names one
// (SAVEC, HELD): the count of its deepest checkpoint's token ids and FNV-1a
// 64 over their bytes (one little-endian int32 per token). The checkpoint is
// where the next turn resumes; the live ids past it end in the reply as it
// was generated, which the chat template renders again on the next turn, so
// no later conversation starts with all of them. A replica without a
// checkpoint is named by its live ids.
func (s *Store) ResumeIDs(key string) (n int64, sum uint64, err error) {
	if !ValidKey(key) {
		return 0, 0, fmt.Errorf("kvshare: invalid key %q", key)
	}
	defer s.lock(key)()
	st, err := s.readState(key)
	if err != nil {
		return 0, 0, err
	}
	if st == nil || !st.Complete {
		return 0, 0, fmt.Errorf("kvshare: %s: no complete replica", key)
	}
	best, size := "live.ids", int64(-1)
	for _, it := range st.Items {
		if strings.HasPrefix(it.Section, "ckpt") && strings.HasSuffix(it.Section, ".ids") && it.Len > size {
			best, size = it.Section, it.Len
		}
	}
	p, err := s.sectionFile(key, best)
	if err != nil {
		return 0, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	h := fnv.New64a()
	m, err := io.Copy(h, f)
	if err != nil {
		return 0, 0, err
	}
	if m%4 != 0 || m == 0 {
		return 0, 0, fmt.Errorf("kvshare: %s: %s holds %d bytes", key, best, m)
	}
	return m / 4, h.Sum64(), nil
}

// Version names these bytes: the sender's digest, else the engine's save id.
// Two replicas with the same version hold the same file.
func (r Replica) Version() string {
	if r.Digest != "" {
		return r.Digest
	}
	return "save:" + r.SaveID
}

// Usage is what a store holds.
type Usage struct {
	Replicas int
	Bytes    int64
}

// storeMarker marks a directory as a store's own. Clean refuses a non-empty
// directory without it, so a kv_share.dir pointed at a directory that holds
// anything else is never emptied.
const storeMarker = ".kvshare-store"

// ErrNotAStore: the directory has content and no store marker.
var ErrNotAStore = errors.New("kvshare: the directory is not empty and is not a replica store")

// Clean claims the directory for the store (creating it, and the marker, when
// it is new) and removes what a stop can leave behind: replicas that are not
// complete, and the store's own temporary files. Anything else is left alone,
// a directory that is another store (a model whose store nests in this one)
// included. A non-empty directory with no marker is refused (ErrNotAStore).
// Run it before the store serves, on a directory no other process writes.
func (s *Store) Clean() (Usage, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Usage{}, err
	}
	if _, err := os.Stat(filepath.Join(s.Dir, storeMarker)); errors.Is(err, fs.ErrNotExist) {
		ents, err := os.ReadDir(s.Dir)
		if err != nil {
			return Usage{}, err
		}
		if len(ents) > 0 {
			return Usage{}, fmt.Errorf("%w: %s", ErrNotAStore, s.Dir)
		}
		if err := os.WriteFile(filepath.Join(s.Dir, storeMarker), nil, 0o600); err != nil {
			return Usage{}, err
		}
	} else if err != nil {
		return Usage{}, err
	}
	return s.evict(-1, "", true)
}

// Evict removes the least recently put replicas while the store holds more
// than maxBytes, and reports what is left.
func (s *Store) Evict(maxBytes int64) (Usage, error) {
	return s.evict(maxBytes, "", false)
}

// EvictExcept removes the least recently put replicas other than key's
// while they hold more than maxBytes: key's replica is the one a put is
// about to replace, so it is neither removed nor counted (counting it
// evicted a neighbour for every update of a large replica).
func (s *Store) EvictExcept(maxBytes int64, key string) (Usage, error) {
	return s.evict(maxBytes, key, false)
}

// isStore reports whether dir is a store's own directory.
func isStore(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, storeMarker))
	return err == nil
}

func (s *Store) evict(maxBytes int64, except string, clean bool) (Usage, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, err
	}
	type rep struct {
		key     string
		bytes   int64
		updated time.Time
	}
	var reps []rep
	var u Usage
	var kept int64 // except's bytes, which a put replaces
	for _, e := range ents {
		key := e.Name()
		if !e.IsDir() || !ValidKey(key) {
			if clean && !e.IsDir() && strings.HasSuffix(key, ".tmp") {
				_ = os.Remove(filepath.Join(s.Dir, key))
			}
			continue
		}
		if isStore(s.keyDir(key)) {
			continue
		}
		unlock := s.lock(key)
		st, err := s.readState(key)
		if clean && (err != nil || st == nil || !st.Complete) {
			_ = os.RemoveAll(s.keyDir(key))
			unlock()
			continue
		}
		var n int64
		files, _ := os.ReadDir(s.keyDir(key))
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				n += info.Size()
			}
		}
		unlock()
		var updated time.Time
		if st != nil {
			updated = st.Updated
		}
		u.Replicas++
		u.Bytes += n
		if key != except {
			reps = append(reps, rep{key, n, updated})
		} else {
			kept = n
		}
	}
	if maxBytes < 0 || u.Bytes-kept <= maxBytes {
		return u, nil
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].updated.Before(reps[j].updated) })
	for _, r := range reps {
		if u.Bytes-kept <= maxBytes {
			break
		}
		unlock := s.lock(r.key)
		err := os.RemoveAll(s.keyDir(r.key))
		s.mu.Lock()
		delete(s.man, r.key)
		s.mu.Unlock()
		unlock()
		if err != nil {
			return u, err
		}
		u.Replicas--
		u.Bytes -= r.bytes
	}
	return u, nil
}
