package kvshare

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInfoCountsTokens(t *testing.T) {
	st := NewStore(t.TempDir())
	if _, ok, err := st.Info("c"); ok || err != nil {
		t.Fatalf("empty store: ok %v err %v", ok, err)
	}
	b := fakeSession{model: 1, config: 2, cells: 3000, stateSeed: 1}.bytes()
	push(t, st, "c", b)
	r, ok, err := st.Info("c")
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if r.Tokens != 3000 || r.Size != int64(len(b)) || len(r.Digest) != 64 {
		t.Fatalf("replica %+v", r)
	}
}

func TestEvictOldestFirst(t *testing.T) {
	st := NewStore(t.TempDir())
	for i, k := range []string{"a", "b", "c"} {
		push(t, st, k, fakeSession{model: 1, config: 2, cells: 2000, stateSeed: uint64(i)}.bytes())
		time.Sleep(5 * time.Millisecond) // distinct Updated
	}
	u, err := st.Evict(1 << 40)
	if err != nil || u.Replicas != 3 {
		t.Fatalf("no eviction under budget: %+v %v", u, err)
	}
	one := u.Bytes / 3
	u, err = st.Evict(u.Bytes - one/2)
	if err != nil || u.Replicas != 2 {
		t.Fatalf("over budget by half a replica: %+v %v", u, err)
	}
	if _, ok, _ := st.Info("a"); ok {
		t.Fatal("the oldest replica must go first")
	}
	if _, ok, _ := st.Info("c"); !ok {
		t.Fatal("the newest replica must stay")
	}
}

func TestCleanDropsIncomplete(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if _, err := st.Clean(); err != nil { // claims the new, empty directory
		t.Fatal(err)
	}
	push(t, st, "good", fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes())
	if err := os.MkdirAll(filepath.Join(dir, "half"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "half", "state.json"), []byte(`{"complete":false}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "stray.tmp"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("an operator's file"), 0o600)
	nested := NewStore(filepath.Join(dir, "other"))
	if _, err := nested.Clean(); err != nil {
		t.Fatal(err)
	}
	u, err := NewStore(dir).Clean()
	if err != nil || u.Replicas != 1 {
		t.Fatalf("%+v %v", u, err)
	}
	for _, gone := range []string{"half", "stray.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived Clean", gone)
		}
	}
	for _, kept := range []string{"good", "notes.txt", "other"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Fatalf("Clean removed %s", kept)
		}
	}
}

// A kv_share.dir pointed at a directory that already holds anything is never
// emptied: the store refuses it.
func TestCleanRefusesAForeignDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "models", "weights.gguf"), []byte("x"), 0o600)
	if _, err := NewStore(dir).Clean(); !errors.Is(err, ErrNotAStore) {
		t.Fatalf("a foreign directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "models", "weights.gguf")); err != nil {
		t.Fatal("Clean touched a foreign directory")
	}
}

func TestEvictExceptKeepsTheKeyBeingPut(t *testing.T) {
	st := NewStore(t.TempDir())
	push(t, st, "old", fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes())
	time.Sleep(5 * time.Millisecond)
	push(t, st, "new", fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 2}.bytes())
	if _, err := st.EvictExcept(1, "old"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Info("old"); !ok {
		t.Fatal("the key being put was evicted")
	}
	if _, ok, _ := st.Info("new"); ok {
		t.Fatal("the other replica should have gone")
	}
}

// Updating a replica replaces it: room for the new file is made among the
// other replicas without counting the old one.
func TestEvictExceptDoesNotCountTheReplicaItReplaces(t *testing.T) {
	st := NewStore(t.TempDir())
	var sizes []int64
	for i, k := range []string{"a", "b", "c"} {
		push(t, st, k, fakeSession{model: 1, config: 2, cells: 2000, stateSeed: uint64(i + 1)}.bytes())
		r, _, _ := st.Info(k)
		sizes = append(sizes, r.Size)
		time.Sleep(5 * time.Millisecond)
	}
	// Room for a and b (plus their state files, which the store counts) but
	// not for all three: c's old copy, still on disk, must not count.
	slack := sizes[2] / 2
	if _, err := st.EvictExcept(sizes[0]+sizes[1]+slack, "c"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok, _ := st.Info(k); !ok {
			t.Fatalf("%s was evicted although the others fit", k)
		}
	}
	if _, err := st.EvictExcept(sizes[1]+slack, "c"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Info("a"); ok {
		t.Fatal("the least recently put replica should have gone")
	}
	if _, ok, _ := st.Info("b"); !ok {
		t.Fatal("b fits and was evicted")
	}
}

func TestKeyLocksAreReleased(t *testing.T) {
	st := NewStore(t.TempDir())
	for _, k := range []string{"a", "b", "c"} {
		_, _ = st.Manifest(context.Background(), k)
	}
	push(t, st, "d", fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes())
	_, _, _ = st.Info("d") // waits for the put's background finish
	st.mu.Lock()
	n := len(st.locks)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d key locks left with nothing held", n)
	}
}

func TestPutExpectRefusesAnotherFile(t *testing.T) {
	st := NewStore(t.TempDir())
	b := fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes()
	p, err := Diff(bytes.NewReader(b), layoutOf(t, b), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	_ = WriteDelta(&wire, bytes.NewReader(b), p)
	want := FileMeta{Size: p.Size, Digest: p.Digest}
	want.Digest[0] ^= 1
	if _, err := st.PutExpect(context.Background(), "c", bytes.NewReader(wire.Bytes()), want); err == nil {
		t.Fatal("a delta for another digest must be refused")
	}
	want.Digest[0] ^= 1
	if _, err := st.PutExpect(context.Background(), "c", bytes.NewReader(wire.Bytes()), want); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPCarriesQuerySignatureAndMeta(t *testing.T) {
	st := NewStore(t.TempDir())
	var gotQuery url.Values
	var signed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		if r.Header.Get("X-Test-Sig") != "ok" {
			http.Error(w, `{"error":"unsigned"}`, http.StatusForbidden)
			return
		}
		HTTPHandler(st).ServeHTTP(w, r)
	}))
	defer srv.Close()
	tr := NewHTTPTransport(HTTPOptions{
		Query: url.Values{"model": {"m1"}},
		Sign: func(r *http.Request) error {
			signed++
			r.Header.Set("X-Test-Sig", "ok")
			return nil
		},
	})
	b := fakeSession{model: 1, config: 2, cells: 2000, stateSeed: 1}.bytes()
	src := filepath.Join(t.TempDir(), "s.bin")
	_ = os.WriteFile(src, b, 0o600)
	ctx := context.Background()
	m, err := tr.Manifest(ctx, srv.URL, "k1")
	if err != nil || m != nil {
		t.Fatalf("manifest of nothing: %v %v", m, err)
	}
	if _, err := Push(ctx, tr, srv.URL, "k1", src, m); err != nil {
		t.Fatal(err)
	}
	meta, err := FileMetaFromQuery(gotQuery)
	if err != nil || meta.Size != int64(len(b)) || gotQuery.Get("model") != "m1" || gotQuery.Get("key") != "k1" {
		t.Fatalf("query %v: %+v %v", gotQuery, meta, err)
	}
	if signed != 2 {
		t.Fatalf("signed %d requests, want 2", signed)
	}
	unsigned := NewHTTPTransport(HTTPOptions{})
	_, err = unsigned.Manifest(ctx, srv.URL, "k1")
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("an unsigned request: %v", err)
	}
}

func TestHTTPHandlerStatusError(t *testing.T) {
	h := HTTPHandler(refusing{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/kvshare/put?key=k", bytes.NewReader(nil)))
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status %d", rec.Code)
	}
}

type refusing struct{}

func (refusing) Manifest(context.Context, string) (*Manifest, error) { return nil, nil }
func (refusing) Put(context.Context, string, io.Reader) (PutResult, error) {
	return PutResult{}, Statusf(http.StatusInsufficientStorage, "full")
}
