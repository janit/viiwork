package sessionshare

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// fakeEngine saves the session it was told about into its save dir and
// records imports.
type fakeEngine struct {
	dir string

	mu          sync.Mutex
	sessions    map[string]kvsharetest.Session // by raw session header
	saves       int
	deltas      int                            // saves written as engine deltas
	lineage     map[string]kvsharetest.Session // save id -> what it saved
	nextID      uint64
	imports     []int // size of each file imported
	importErr   error
	importDelay time.Duration // each import reads its file this late
	// held answers SessionHeld; nil is an engine that cannot say
	held func(raw string, n int64, hash uint64) (int64, error)
}

func newFakeEngine(t *testing.T) *fakeEngine {
	return &fakeEngine{dir: t.TempDir(), sessions: map[string]kvsharetest.Session{}}
}

func (e *fakeEngine) set(raw string, s kvsharetest.Session) {
	e.mu.Lock()
	e.sessions[raw] = s
	e.mu.Unlock()
}

func (e *fakeEngine) SessionShare(engine.Spec) (engine.SessionShareConfig, bool) {
	return engine.SessionShareConfig{}, false
}

// SaveSession writes a delta against a base it saved itself, else a whole
// file, and gives every save an id, as kv-export's incremental save does.
func (e *fakeEngine) SaveSession(_ context.Context, _, raw, name, base string) (engine.SavedSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[raw]
	e.saves++
	if !ok {
		return engine.SavedSession{}, engine.ErrSessionGone
	}
	if e.lineage == nil {
		e.lineage = map[string]kvsharetest.Session{}
	}
	e.nextID++
	id := fmt.Sprintf("%016x", e.nextID)
	e.lineage[id] = s
	b, delta := s.Sealed(), false
	if prev, known := e.lineage[base]; known && base != "" {
		var baseID uint64
		fmt.Sscanf(base, "%x", &baseID)
		b, delta = s.EngineDelta(prev, baseID, e.nextID), true
		e.deltas++
	}
	return engine.SavedSession{ID: id, Delta: delta}, os.WriteFile(filepath.Join(e.dir, name), b, 0o600)
}

func (e *fakeEngine) ImportSession(_ context.Context, _, name string) error {
	time.Sleep(e.importDelay)
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.importErr != nil {
		return e.importErr
	}
	e.imports = append(e.imports, len(b))
	return nil
}

func (e *fakeEngine) SessionHeld(_ context.Context, _, raw string, n int64, hash uint64) (int64, error) {
	e.mu.Lock()
	held := e.held
	e.mu.Unlock()
	if held == nil {
		return 0, errors.New("cannot say")
	}
	return held(raw, n, hash)
}

func (e *fakeEngine) counts() (saves, imports int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saves, len(e.imports)
}

type fakeBackend struct {
	id       string
	launched time.Time
}

func (b *fakeBackend) ID() string            { return b.id }
func (b *fakeBackend) Addr() string          { return "127.0.0.1:1" }
func (b *fakeBackend) LaunchedAt() time.Time { return b.launched }

type fakeReports struct {
	mu   sync.Mutex
	reps []capacity.Report
}

func (f *fakeReports) Reports() []capacity.Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capacity.Report(nil), f.reps...)
}

const testModel = "m"

func shareConfig(e *fakeEngine) engine.SessionShareConfig {
	return engine.SessionShareConfig{Group: "g", Transport: "http", SaveDir: e.dir, MaxBytes: 1 << 30, MinTokens: 1000, MaxFileBytes: 1 << 30}
}

// node is one manager behind a real HTTP server.
type node struct {
	name    string
	m       *Manager
	eng     *fakeEngine
	srv     *httptest.Server
	reports *fakeReports
}

func newNode(t *testing.T, name string) *node {
	n := &node{name: name, eng: newFakeEngine(t), reports: &fakeReports{}}
	n.m = New(Deps{
		Self: name, Reports: n.reports, StaleAfter: 3 * time.Second, StateDir: t.TempDir(),
		Sign:      func(r *http.Request) error { r.Header.Set("X-Test-Member", name); return nil },
		Verify:    func(r *http.Request) bool { return r.Header.Get("X-Test-Member") != "" },
		Logf:      t.Logf,
		SaveDelay: 10 * time.Millisecond,
	})
	n.m.SetModels([]Model{{Name: testModel, Engine: n.eng, Config: shareConfig(n.eng)}})
	n.srv = httptest.NewServer(n.m)
	t.Cleanup(func() { n.m.Close(); n.srv.Close() })
	return n
}

// sees makes n's capacity reports list peer serving the model in group.
func (n *node) sees(peer *node, group string, healthy int, at time.Time) {
	n.reports.mu.Lock()
	defer n.reports.mu.Unlock()
	n.reports.reps = append(n.reports.reps, capacity.Report{
		Node: peer.name, APIAddr: strings.TrimPrefix(peer.srv.URL, "http://"), Received: at,
		Models: []meshapi.ModelCapacity{{Name: testModel, Slots: 1, HealthyBackends: healthy, KVShareGroup: group}},
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	// Background saves and pushes of multi-MB test sessions take seconds
	// under -race on a loaded host (load 40-60 on teddy, 2026-11-15).
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTurnGoesToTheSpillHostAndImportsThere(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	b.sees(a, "g", 1, time.Now())
	ba := &fakeBackend{id: "m/0"}
	turn1 := kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}
	a.eng.set("sess-1", turn1)

	a.m.AfterLocal(testModel, "sess-1", 42, ba, 2000)
	waitFor(t, "the first push", func() bool { return b.m.Stats(testModel).Puts == 1 && a.m.Stats(testModel).Pushes == 1 })
	st := a.m.Stats(testModel)
	if st.Pushes != 1 || st.BytesSent != int64(len(turn1.Bytes())) || st.PushFailures != 0 {
		t.Fatalf("first turn: %+v", st)
	}

	// the next turn sends what changed only. A fresh report first: one
	// older than 3 s routes nothing, and under -race on a loaded host the
	// first push alone can take that long.
	a.sees(b, "g", 1, time.Now())
	turn2 := kvsharetest.Session{Model: 1, Config: 2, Cells: 2600, StateSeed: 2}
	a.eng.set("sess-1", turn2)
	a.m.AfterLocal(testModel, "sess-1", 42, ba, 2600)
	waitFor(t, "the second push", func() bool { return b.m.Stats(testModel).Puts == 2 && a.m.Stats(testModel).Pushes == 2 })
	sent := a.m.Stats(testModel).BytesSent - st.BytesSent
	if sent <= 0 || sent >= int64(len(turn2.Bytes())) {
		t.Fatalf("the second turn sent %d of %d bytes", sent, len(turn2.Bytes()))
	}
	if a.eng.deltas != 1 {
		t.Fatalf("%d engine deltas; the second save names the first, which the peer holds", a.eng.deltas)
	}
	waitFor(t, "the sender to delete its file", func() bool {
		left, _ := filepath.Glob(filepath.Join(a.eng.dir, "*"))
		return len(left) == 0
	})

	// the session spills to b: b imports its replica once per backend launch
	bb := &fakeBackend{id: "m/0"}
	ctx := context.Background()
	b.m.BeforeLocal(ctx, testModel, "sess-1", 42, bb)
	b.m.BeforeLocal(ctx, testModel, "sess-1", 42, bb)
	if _, n := b.eng.counts(); n != 1 {
		t.Fatalf("%d imports, want 1", n)
	}
	if b.eng.imports[0] != len(turn2.Bytes()) {
		t.Fatalf("imported %d bytes, want the second turn's %d", b.eng.imports[0], len(turn2.Bytes()))
	}
	bb.launched = bb.launched.Add(time.Second) // relaunched
	b.m.BeforeLocal(ctx, testModel, "sess-1", 42, bb)
	if _, n := b.eng.counts(); n != 2 {
		t.Fatalf("a respawned backend must import again: %d imports", n)
	}
	if left, _ := filepath.Glob(filepath.Join(b.eng.dir, "*")); len(left) != 0 {
		t.Fatalf("the receiver left %v", left)
	}
	if s := b.m.Stats(testModel); s.Imports != 2 || s.Replicas != 1 || s.StoreBytes == 0 {
		t.Fatalf("receiver stats %+v", s)
	}
	if c, ok := b.m.ImportCost(testModel); !ok || c < 1 {
		t.Fatalf("import cost %d %v after two imports", c, ok)
	}
	if _, ok := a.m.ImportCost(testModel); ok {
		t.Fatal("a node that imported nothing cannot say what an import costs")
	}
	// a session b holds nothing for imports nothing
	b.m.BeforeLocal(ctx, testModel, "sess-2", 43, bb)
	if _, n := b.eng.counts(); n != 2 {
		t.Fatal("no replica, no import")
	}
}

func TestShortConversationsStayHome(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 500, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 1, &fakeBackend{id: "m/0"}, 500)
	time.Sleep(100 * time.Millisecond)
	if saves, _ := a.eng.counts(); saves != 0 {
		t.Fatalf("%d saves below min_tokens", saves)
	}
	// and a short replica is not imported either
	b.m.mu.Lock()
	b.m.models[testModel].cfg.MinTokens = 3000
	b.m.mu.Unlock()
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 1, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	b.m.BeforeLocal(context.Background(), testModel, "s", 1, &fakeBackend{id: "m/0"})
	if _, n := b.eng.counts(); n != 0 {
		t.Fatal("a replica below min_tokens must not be imported")
	}
}

func TestLongSessionIDsAreNotShared(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	long := strings.Repeat("x", maxSessionID+1)
	a.eng.set(long, kvsharetest.Session{Model: 1, Config: 2, Cells: 20000, StateSeed: 1})
	a.m.AfterLocal(testModel, long, 1, &fakeBackend{id: "m/0"}, 20000)
	time.Sleep(100 * time.Millisecond)
	if saves, _ := a.eng.counts(); saves != 0 {
		t.Fatalf("%d saves of a session id Strata does not keep", saves)
	}
}

func TestANewTurnReplacesTheWaitingSave(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.m.d.SaveDelay = 200 * time.Millisecond
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	be := &fakeBackend{id: "m/0"}
	a.m.AfterLocal(testModel, "s", 7, be, 2000)
	a.m.AfterLocal(testModel, "s", 7, be, 2100)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	time.Sleep(300 * time.Millisecond)
	if saves, _ := a.eng.counts(); saves != 1 {
		t.Fatalf("%d saves for two quick turns, want 1", saves)
	}
	// a turn that starts on the backend drops the save outright
	a.m.AfterLocal(testModel, "s", 7, be, 2200)
	a.m.BeforeLocal(context.Background(), testModel, "s", 7, be)
	time.Sleep(400 * time.Millisecond)
	if saves, _ := a.eng.counts(); saves != 1 {
		t.Fatalf("a save of a session that already moved on ran: %d saves", saves)
	}
}

func TestNoTargetNoSave(t *testing.T) {
	for _, tc := range []struct {
		name    string
		group   string
		healthy int
		age     time.Duration
	}{
		{"another group", "other", 1, 0},
		{"no healthy backend", "g", 0, 0},
		{"stale report", "g", 1, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newNode(t, "a"), newNode(t, "b")
			a.sees(b, tc.group, tc.healthy, time.Now().Add(-tc.age))
			a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
			a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
			time.Sleep(100 * time.Millisecond)
			if saves, _ := a.eng.counts(); saves != 0 {
				t.Fatalf("%d saves with no member to send to", saves)
			}
		})
	}
}

func TestGoneSessionIsNotAFailure(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.m.AfterLocal(testModel, "unknown", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the save attempt", func() bool { s, _ := a.eng.counts(); return s == 1 })
	time.Sleep(50 * time.Millisecond)
	if s := a.m.Stats(testModel); s.PushFailures != 0 || s.Pushes != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestFailedImportServesColdAndRetries(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	b.eng.importErr = context.DeadlineExceeded
	be := &fakeBackend{id: "m/0"}
	b.m.BeforeLocal(context.Background(), testModel, "s", 7, be)
	if s := b.m.Stats(testModel); s.ImportFails != 1 || s.Imports != 0 {
		t.Fatalf("stats %+v", s)
	}
	b.eng.importErr = nil
	b.m.BeforeLocal(context.Background(), testModel, "s", 7, be)
	if s := b.m.Stats(testModel); s.Imports != 1 {
		t.Fatalf("a failed import must be tried again: %+v", s)
	}
}

func TestReceiverRefuses(t *testing.T) {
	b := newNode(t, "b")
	b.m.mu.Lock()
	b.m.models[testModel].cfg.MaxFileBytes = 1000
	b.m.mu.Unlock()
	digest := strings.Repeat("00", 32)
	for _, tc := range []struct {
		name, method, path string
		member             bool
		want               int
	}{
		{"not a member", http.MethodGet, meshapi.PathKVShareManifest + "?model=m&key=k", false, http.StatusForbidden},
		{"unknown model", http.MethodGet, meshapi.PathKVShareManifest + "?model=x&key=k", true, http.StatusNotFound},
		{"manifest of nothing", http.MethodGet, meshapi.PathKVShareManifest + "?model=m&key=k", true, http.StatusOK},
		{"bad key", http.MethodGet, meshapi.PathKVShareManifest + "?model=m&key=../x", true, http.StatusBadRequest},
		{"no size", http.MethodPut, meshapi.PathKVSharePut + "?model=m&key=k&digest=" + digest, true, http.StatusBadRequest},
		{"too large", http.MethodPut, meshapi.PathKVSharePut + "?model=m&key=k&size=1001&digest=" + digest, true, http.StatusRequestEntityTooLarge},
		{"not a delta", http.MethodPut, meshapi.PathKVSharePut + "?model=m&key=k&size=1000&digest=" + digest, true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, b.srv.URL+tc.path, strings.NewReader("junk"))
			if tc.member {
				req.Header.Set("X-Test-Member", "a")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestSetModelsKeepsUnchangedStores(t *testing.T) {
	a := newNode(t, "a")
	before := a.m.model(testModel)
	a.m.SetModels([]Model{{Name: testModel, Engine: a.eng, Config: shareConfig(a.eng)}})
	if a.m.model(testModel) != before {
		t.Fatal("an unchanged model must keep its store and counters")
	}
	if a.m.Group(testModel) != "g" || a.m.Group("x") != "" {
		t.Fatal("Group")
	}
	a.m.SetModels(nil)
	if a.m.Stats(testModel) != nil || a.m.Group(testModel) != "" {
		t.Fatal("a removed model shares nothing")
	}
}

// A reload that changes a model's settings keeps its store, what its
// backends imported and its counters; the store is not cleaned again.
func TestReloadKeepsTheStore(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 && a.m.Stats(testModel).Pushes == 1 })
	before := b.m.model(testModel)
	cfg := shareConfig(b.eng)
	cfg.MinTokens = 500
	b.m.SetModels([]Model{{Name: testModel, Engine: b.eng, Config: cfg}})
	after := b.m.model(testModel)
	if after == before || after.store != before.store || after.cfg.MinTokens != 500 {
		t.Fatal("the new settings must apply on the same store")
	}
	if s := b.m.Stats(testModel); s.Puts != 1 || s.Replicas != 1 {
		t.Fatalf("after the reload: %+v", s)
	}
}

func TestTwoModelsCannotShareAStore(t *testing.T) {
	a := newNode(t, "a")
	cfg := shareConfig(a.eng)
	cfg.StoreDir = t.TempDir()
	a.m.SetModels([]Model{{Name: "x", Engine: a.eng, Config: cfg}, {Name: "y", Engine: a.eng, Config: cfg}})
	if a.m.Group("x") == "" || a.m.Group("y") != "" {
		t.Fatal("the second model on one directory must not share")
	}
}

func TestModelNamesDoNotNestStores(t *testing.T) {
	a := newNode(t, "a")
	a.m.SetModels([]Model{{Name: "q", Engine: a.eng, Config: shareConfig(a.eng)}, {Name: "q/r", Engine: a.eng, Config: shareConfig(a.eng)}})
	if a.m.Group("q") == "" || a.m.Group("q/r") == "" {
		t.Fatal("both models share, each in its own directory")
	}
	if a.m.model("q").store.Dir == filepath.Dir(a.m.model("q/r").store.Dir) {
		t.Fatal("q/r's store nests in q's")
	}
}

// An imported replica the engine has since evicted is imported again: the
// record of what was sent is only a hint, and the engine is asked.
func TestAnEvictedReplicaIsImportedAgain(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })

	var held int64
	asked := 0
	ids := kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1}.IDs()
	h := fnv.New64a()
	h.Write(ids[:1995*4])
	wantHash := h.Sum64()
	b.eng.mu.Lock()
	b.eng.held = func(raw string, n int64, hash uint64) (int64, error) {
		asked++
		// the replica's resume point names the conversation: its checkpoint's
		// 1995 tokens and their hash, not the live 2000 that end in the reply
		if raw != "s" || n != 1995 || hash != wantHash {
			t.Errorf("asked about %q, %d tokens, %016x (want %016x)", raw, n, hash, wantHash)
		}
		return held, nil
	}
	b.eng.mu.Unlock()
	bb := &fakeBackend{id: "m/0"}
	ctx := context.Background()
	b.m.BeforeLocal(ctx, testModel, "s", 7, bb)
	if _, n := b.eng.counts(); n != 1 || asked != 0 {
		t.Fatalf("first import: %d imports, %d questions; a replica never given needs no question", n, asked)
	}
	held = 2600 // the turn ran here and the engine holds it, longer
	b.m.BeforeLocal(ctx, testModel, "s", 7, bb)
	if _, n := b.eng.counts(); n != 1 || asked != 1 {
		t.Fatalf("held: %d imports, %d questions", n, asked)
	}
	held = 0 // the conversation cache evicted it
	b.m.BeforeLocal(ctx, testModel, "s", 7, bb)
	if _, n := b.eng.counts(); n != 2 {
		t.Fatalf("evicted: %d imports, want the replica imported again", n)
	}
	held = 1500 // holds less than the replica resumes from: an older turn
	b.m.BeforeLocal(ctx, testModel, "s", 7, bb)
	if _, n := b.eng.counts(); n != 3 {
		t.Fatalf("shorter: %d imports, want 3", n)
	}
	// an engine that cannot say keeps the record's answer, as before
	b.eng.mu.Lock()
	b.eng.held = func(string, int64, uint64) (int64, error) { return 0, errors.New("400") }
	b.eng.mu.Unlock()
	b.m.BeforeLocal(ctx, testModel, "s", 7, bb)
	if _, n := b.eng.counts(); n != 3 {
		t.Fatalf("cannot say: %d imports, want no new one", n)
	}
}

// A replica the engine refuses as unusable here is removed, not imported
// again on every turn; a transient failure (above) keeps it.
func TestRejectedReplicaIsRemoved(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	b.eng.importErr = fmt.Errorf("%w: strata: 400 config fingerprint differs", engine.ErrReplicaRejected)
	be := &fakeBackend{id: "m/0"}
	b.m.BeforeLocal(context.Background(), testModel, "s", 7, be)
	if _, ok, _ := b.m.model(testModel).store.Info(storeKey(7)); ok {
		t.Fatal("the refused replica is still in the store")
	}
	b.m.BeforeLocal(context.Background(), testModel, "s", 7, be)
	if s := b.m.Stats(testModel); s.ImportFails != 1 {
		t.Fatalf("imported a refused replica again: %+v", s)
	}
}

// Each import assembles its own file: two turns of one session importing at
// once must not remove each other's file.
func TestConcurrentImportsUseTheirOwnFiles(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.sees(b, "g", 1, time.Now())
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push", func() bool { return b.m.Stats(testModel).Puts == 1 })
	b.eng.importDelay = 50 * time.Millisecond // the imports overlap
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.m.BeforeLocal(context.Background(), testModel, "s", 7, &fakeBackend{id: fmt.Sprintf("m/%d", i)})
		}()
	}
	wg.Wait()
	if s := b.m.Stats(testModel); s.ImportFails != 0 || s.Imports != 4 {
		t.Fatalf("concurrent imports failed: %+v", s)
	}
}

// kv_share.peers sends a member's replicas to the address it names, not the
// member's mesh address (the forges' LAN instead of the tailnet).
func TestPeersAddressIsUsedForTheMember(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	// b's mesh address leads nowhere; its peers entry is the real one.
	a.reports.mu.Lock()
	a.reports.reps = append(a.reports.reps, capacity.Report{
		Node: "b", APIAddr: "127.0.0.1:1", Received: time.Now(),
		Models: []meshapi.ModelCapacity{{Name: testModel, Slots: 1, HealthyBackends: 1, KVShareGroup: "g"}},
	})
	a.reports.mu.Unlock()
	cfg := shareConfig(a.eng)
	cfg.Peers = map[string]string{"b": strings.TrimPrefix(b.srv.URL, "http://")}
	a.m.SetModels([]Model{{Name: testModel, Engine: a.eng, Config: cfg}})
	a.eng.set("s", kvsharetest.Session{Model: 1, Config: 2, Cells: 2000, StateSeed: 1})
	a.m.AfterLocal(testModel, "s", 7, &fakeBackend{id: "m/0"}, 2000)
	waitFor(t, "the push over the peers address", func() bool { return b.m.Stats(testModel).Puts == 1 })
	if s := a.m.Stats(testModel); s.PushFailures != 0 {
		t.Fatalf("stats %+v", s)
	}
}
