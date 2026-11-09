package kvshare

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// PushResult times one push.
type PushResult struct {
	Plan    *Plan
	Put     PutResult
	DiffMs  float64 // parsing, hashing and planning on the sender
	WireMs  float64 // streaming the delta until the receiver answered
	TotalMs float64
}

// Push sends the session file at path to the receiver under key, as a delta
// against m (the receiver's manifest; nil sends the whole file).
func Push(ctx context.Context, t Transport, addr, key, path string, m *Manifest) (PushResult, error) {
	return PushSave(ctx, t, addr, key, path, m, "")
}

// PushSave is Push for a file the engine saved under saveID, which the
// receiver records so that the engine's next save can be a delta against it.
func PushSave(ctx context.Context, t Transport, addr, key, path string, m *Manifest, saveID string) (PushResult, error) {
	return PushSaveAs(ctx, t, addr, key, path, m, saveID, Identity{})
}

// Identity fingerprints the state a file can be restored into. A zero one
// leaves the file's own (Strata's header); a format without any takes the
// node's (engine.SessionShareConfig.Model, Config).
type Identity struct{ Model, Config uint64 }

// PushSaveAs is PushSave with id stamped on the replica, for a file whose
// format carries no fingerprints. A base of another identity is not diffed
// against, so a replica never mixes the bytes of two models.
func PushSaveAs(ctx context.Context, t Transport, addr, key, path string, m *Manifest, saveID string, id Identity) (PushResult, error) {
	start := time.Now()
	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PushResult{}, err
	}
	l, err := ParseLayout(f, st.Size())
	if err != nil {
		return PushResult{}, err
	}
	if id != (Identity{}) {
		l.Model, l.Config = id.Model, id.Config
	}
	p, err := Diff(f, l, m)
	if err != nil {
		return PushResult{}, err
	}
	res := PushResult{Plan: p, DiffMs: ms(time.Since(start))}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(WriteDelta(pw, f, p)) }()
	wire := time.Now()
	if mp, ok := t.(MetaPutter); ok {
		res.Put, err = mp.PutFile(ctx, addr, key, FileMeta{Size: p.Size, Digest: p.Digest, SaveID: saveID}, pr)
	} else {
		res.Put, err = t.Put(ctx, addr, key, pr)
	}
	pr.CloseWithError(io.ErrClosedPipe)
	res.WireMs = ms(time.Since(wire))
	res.TotalMs = ms(time.Since(start))
	return res, err
}

// PushEngineDelta sends an engine delta file (STRSDLT) as it is: the
// receiver applies it to the replica of its base save. The transport must
// carry the file's meta (MetaPutter). Plan reports the delta's bytes as Data
// and the rebuilt file as Size.
func PushEngineDelta(ctx context.Context, t Transport, addr, key, path string) (PushResult, error) {
	start := time.Now()
	mp, ok := t.(MetaPutter)
	if !ok {
		return PushResult{}, fmt.Errorf("kvshare: transport %s cannot carry an engine delta", t.Name())
	}
	f, err := os.Open(path)
	if err != nil {
		return PushResult{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return PushResult{}, err
	}
	h, err := ReadEngineDeltaHeader(f)
	if err != nil {
		return PushResult{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return PushResult{}, err
	}
	res := PushResult{Plan: &Plan{Size: int64(h.FileSize), Data: st.Size(), Model: h.Model, Config: h.Config}}
	wire := time.Now()
	res.Put, err = mp.PutFile(ctx, addr, key, FileMeta{Size: int64(h.FileSize), SaveID: SaveIDString(h.New), Format: FormatEngineDelta}, f)
	res.WireMs = ms(time.Since(wire))
	res.TotalMs = ms(time.Since(start))
	return res, err
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// SaveLocal keeps the file at path in s under key, as PushSaveAs would send
// it to a member: a delta against what s holds, stamped with id. It is the
// node's own copy of a session it ran (kv_share.local).
func SaveLocal(ctx context.Context, s *Store, key, path string, id Identity) (PushResult, error) {
	m, err := s.Manifest(ctx, key)
	if err != nil {
		return PushResult{}, err
	}
	return PushSaveAs(ctx, storeTransport{s}, "", key, path, m, "", id)
}

// StoreTransport is a Transport into a store in this process: the address
// is ignored, a put of an engine delta (FormatEngineDelta) is applied to the
// replica of its base.
func StoreTransport(s *Store) Transport { return storeTransport{s} }

// storeTransport puts into a store in this process.
type storeTransport struct{ s *Store }

func (t storeTransport) Name() string { return "store" }

func (t storeTransport) Listen(string, Handler) (Server, error) {
	return nil, fmt.Errorf("kvshare: the store transport does not listen")
}

func (t storeTransport) Manifest(ctx context.Context, _, key string) (*Manifest, error) {
	return t.s.Manifest(ctx, key)
}

func (t storeTransport) Put(ctx context.Context, _, key string, delta io.Reader) (PutResult, error) {
	return t.s.Put(ctx, key, delta)
}

func (t storeTransport) PutFile(ctx context.Context, _, key string, meta FileMeta, delta io.Reader) (PutResult, error) {
	if meta.Format == FormatEngineDelta {
		return t.s.PutEngineDelta(ctx, key, delta, meta)
	}
	return t.s.PutExpect(ctx, key, delta, meta)
}
