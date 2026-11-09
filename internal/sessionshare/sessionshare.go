// Package sessionshare is kv_share on a node: a client session's engine
// state follows it to the member it would spill to, so that a turn which
// cannot run on the session's own host resumes warm on the next one instead
// of prefilling the whole conversation again.
//
// After a local turn of a session, the backend that ran it saves the
// conversation (engine.SessionSharer) and this node sends the file, as a
// delta against what the peer already holds (internal/kvshare), to the member
// of the model's group that SessionRank puts first after itself: the
// session's spill target when this node is its home, and its home when the
// turn spilled here. Before a session's turn runs on a local backend, a
// replica this node holds for it is imported into that backend's cache.
//
// Nothing here is on a client's path but the import, which a spilled turn
// pays once (about 1.5 s at 50k tokens against a 41 s prefill). A failed
// save, push or import leaves a cold prefill, never a wrong answer: the
// engine resumes only from a prompt prefix that matches what it parked.
//
// Design and measurements: docs/superpowers/specs/2026-10-22-kv-share-*.md.
package sessionshare

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// defaultSaveDelay lets an agent loop's next turn, which usually follows
	// at once, start before the save that would pause the backend.
	defaultSaveDelay = 2 * time.Second
	// pushTimeout bounds a save and its push: 1 GB at 1 Gbit/s is ~9 s.
	pushTimeout = 5 * time.Minute
	// importTimeout bounds what a spilled turn waits for its import.
	importTimeout = 30 * time.Second
	// heldTimeout bounds asking a backend whether it still holds a session;
	// past it the node trusts its own record, as with an engine that cannot
	// say.
	heldTimeout = 2 * time.Second
	// failLogEvery rate-limits repeated failures per (what, peer).
	failLogEvery = 10 * time.Minute
)

// Backend is a local backend as this package uses it; *supervisor.Backend
// satisfies it.
type Backend interface {
	ID() string
	Addr() string
}

// launcher is *supervisor.Backend's launch time: a relaunched backend has
// lost its cache, so a replica imported before must be imported again. (Not
// its respawn count, which the supervisor resets after a healthy spell.)
type launcher interface{ LaunchedAt() time.Time }

// Slotted is a Backend that carries the slot a request was given on it
// (route.Lease.Slot), which a SlotSharer model's turns need: the slot is
// what is restored before the turn and saved after it.
type Slotted interface {
	Backend
	Slot() (supervisor.SlotClaim, bool)
	// SlotHolds reports whether the claim's slot is idle and still holds
	// session's conversation (supervisor.Backend.SlotHolds).
	SlotHolds(c supervisor.SlotClaim, session uint64) bool
}

// maxImported bounds a model's record of imports; past it the record starts
// over, which costs at most one needless import per session.
const maxImported = 4096

// maxSessionID is the longest session id Strata keeps a conversation under;
// a longer one is no session to it, so there is nothing to save or resume.
const maxSessionID = 256

func sharable(raw string) bool { return raw != "" && len(raw) <= maxSessionID }

// Model is one configured model that shares sessions: by session
// (Engine, Strata) or by slot (Slots, llama.cpp); exactly one is set.
type Model struct {
	Name   string
	Engine engine.SessionSharer
	Slots  engine.SlotSharer
	Config engine.SessionShareConfig
}

// Deps is what a Manager needs of its node.
type Deps struct {
	Self string
	// Reports are the members' capacity reports (*capacity.Poller).
	Reports    route.Reports
	StaleAfter time.Duration
	// Sign marks and signs a request to a member as a forward (an empty
	// body: a put's file is named by its size and digest in the query).
	Sign func(*http.Request) error
	// Verify reports whether a request is a genuine forward from a member.
	Verify   func(*http.Request) bool
	StateDir string
	Logf     func(format string, args ...any)
	Now      func() time.Time
	// SaveDelay is how long a save waits after its turn; 0 is the default.
	SaveDelay time.Duration
}

// Manager runs kv_share for a node's models.
type Manager struct {
	d      Deps
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	models  map[string]*model
	stores  map[string]*kvshare.Store // by directory, for the process's life
	workers map[string]*worker        // by backend id
	logged  map[string]time.Time

	seq      atomic.Uint64 // makes each save's file name its own
	inflight int           // saves and sends running (under mu)
	idle     chan struct{} // closed when inflight drops to 0 (under mu)
}

type model struct {
	name  string
	eng   engine.SessionSharer
	slots engine.SlotSharer
	cfg   engine.SessionShareConfig
	store *kvshare.Store
	// fp is the model fingerprint from a backend's own weights file, for an
	// engine that fingerprints per backend (engine.ModelFingerprinter); 0
	// until a backend has answered. See modelID.
	fp atomic.Uint64
	// mismatched is when each peer that refused a replica as another
	// model's may be sent one again.
	mismatched sync.Map // peer name -> time.Time

	pushes, pushFails, bytesSent, puts, imports, importFails atomic.Int64
	locals, localFails                                       atomic.Int64 // copies kept by kv_share.local
	usage                                                    atomic.Pointer[kvshare.Usage]
	evicting                                                 atomic.Bool

	mu       sync.Mutex
	imported map[string]string    // backend generation [+ "/" + slot] + "/" + key -> digest
	turns    map[string]localTurn // backend generation + "/" + slot + "/" + key -> last local turn (SlotSharer)
	costs    []float64            // the last importCosts imports, ms per 1000 tokens
	// pending: a local copy of the session is saved but not yet in the
	// store; a restore waits for it rather than take the older copy.
	pending map[string]chan struct{}
}

// importCosts is how many recent imports the published cost is a median of.
const importCosts = 16

// ImportCost is this node's median time to import a replica into one of the
// model's backends, assembly included, per 1000 tokens; ok is false before
// the first import. Published on /v1/capacity, where the router prices a
// replica host with it.
func (m *Manager) ImportCost(name string) (msPer1k int, ok bool) {
	md := m.model(name)
	if md == nil {
		return 0, false
	}
	md.mu.Lock()
	c := slices.Clone(md.costs)
	md.mu.Unlock()
	if len(c) == 0 {
		return 0, false
	}
	slices.Sort(c)
	return max(int(c[len(c)/2]+0.5), 1), true
}

// New builds a manager with no models.
func New(d Deps) *Manager {
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SaveDelay <= 0 {
		d.SaveDelay = defaultSaveDelay
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{d: d, ctx: ctx, cancel: cancel, models: map[string]*model{}, stores: map[string]*kvshare.Store{}, workers: map[string]*worker{}, logged: map[string]time.Time{}}
}

// fileName is a save's own file name in the save directory: two saves of
// one session (the worker's and a flush's, or two displacements) must not
// write or remove each other's file.
func (m *Manager) fileName(prefix, key string) string {
	return prefix + "-" + key + "-" + strconv.FormatUint(m.seq.Add(1), 10) + ".bin"
}

// begin and end bracket a save or send, which Flush waits for.
func (m *Manager) begin() {
	m.mu.Lock()
	m.inflight++
	m.mu.Unlock()
}

func (m *Manager) end() {
	m.mu.Lock()
	m.inflight--
	if m.inflight == 0 && m.idle != nil {
		close(m.idle)
		m.idle = nil
	}
	m.mu.Unlock()
}

// waitIdle returns when no save or send is running, or ctx ends.
func (m *Manager) waitIdle(ctx context.Context) {
	m.mu.Lock()
	if m.inflight == 0 {
		m.mu.Unlock()
		return
	}
	if m.idle == nil {
		m.idle = make(chan struct{})
	}
	ch := m.idle
	m.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// markPending records that a local copy of key is on its way into the
// store; release ends it.
func (md *model) markPending(key string) (release func()) {
	ch := make(chan struct{})
	md.mu.Lock()
	md.pending[key] = ch
	md.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			md.mu.Lock()
			if md.pending[key] == ch {
				delete(md.pending, key)
			}
			md.mu.Unlock()
			close(ch)
		})
	}
}

// waitPending waits for a local copy of key on its way into the store.
func (md *model) waitPending(ctx context.Context, key string) {
	md.mu.Lock()
	ch := md.pending[key]
	md.mu.Unlock()
	if ch == nil {
		return
	}
	t := time.NewTimer(importTimeout)
	defer t.Stop()
	select {
	case <-ch:
	case <-ctx.Done():
	case <-t.C:
	}
}

// idleRetry is how often a save that waits for its backend to be idle
// looks again.
const idleRetry = 500 * time.Millisecond

// inFlight is a Backend that counts the requests it is serving.
type inFlight interface{ InFlight() int }

// save is the worker's run of job j. A save pauses every slot of its
// backend (llama-server runs it on its one task loop: no other slot decodes
// a token until the file is written, 1.8 s at 51k tokens on a 5090, ~18 s
// on a gfx906 host; a Strata save holds its FIFO), so a model without a
// group, whose copy guards only against a restart or a lost slot, saves
// only while its backend serves nothing else. The slot's reuse
// (saveDisplaced) and the node's stop (Flush) save it regardless.
func (m *Manager) save(ctx context.Context, w *worker, j job) {
	if md := m.model(j.model); md != nil && md.cfg.Group == "" {
		if b, ok := j.backend.(inFlight); ok && b.InFlight() > 0 {
			j.due = m.d.Now().Add(idleRetry)
			w.putBack(j)
			return
		}
	}
	m.push(ctx, j)
}

// lazyDue is a lazy save's due time: never, by the clock.
var lazyDue = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Flush runs every waiting save now, each backend's in turn and the
// backends at once, and returns when they are done or ctx ends. The node
// calls it while stopping, before the backends stop: a session whose save
// was still waiting (all of them, with lazy saves) is kept, and its replica
// host can take its next turn warm while this node is away.
func (m *Manager) Flush(ctx context.Context) {
	m.mu.Lock()
	ws := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		ws = append(ws, w)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, j := range w.takeAll() {
				if ctx.Err() != nil {
					return
				}
				m.push(ctx, j)
			}
		}()
	}
	wg.Wait()
	// Saves the workers had already started, and displaced sessions'
	// copies still being sent.
	m.waitIdle(ctx)
}

// Close stops the save workers and waits for them; a push in progress is
// cancelled.
func (m *Manager) Close() {
	// Under the lock: AfterLocal checks the context and starts a worker
	// under it, so no worker is added once Wait has begun.
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// SetModels replaces the models that share sessions, at start and on reload.
// A model whose settings did not change keeps its store and counters. A
// store is cleaned of incomplete replicas when it is first opened.
func (m *Manager) SetModels(models []Model) {
	next := map[string]*model{}
	m.mu.Lock()
	old := m.models
	m.mu.Unlock()
	used := map[string]string{} // store dir -> the model using it
	for _, mm := range models {
		if cur, ok := old[mm.Name]; ok && cur.cfg == mm.Config && cur.eng == mm.Engine && cur.slots == mm.Slots {
			next[mm.Name] = cur
			used[cur.store.Dir] = mm.Name
			continue
		}
		dir := mm.Config.StoreDir
		if dir == "" {
			if m.d.StateDir == "" {
				m.d.Logf("kv_share: %s: no state_dir and no kv_share.dir; not sharing its sessions", mm.Name)
				continue
			}
			// Escaped: a name with a slash must not nest in another model's store.
			dir = filepath.Join(m.d.StateDir, "kvshare", url.PathEscape(mm.Name))
		}
		dir = filepath.Clean(dir)
		if other, taken := used[dir]; taken {
			// A store's keys are sessions, which two models share.
			m.d.Logf("kv_share: %s: %s already holds %s's replicas; not sharing its sessions", mm.Name, dir, other)
			continue
		}
		st, u, err := m.openStore(dir)
		if err != nil {
			m.d.Logf("kv_share: %s: %v; not sharing its sessions", mm.Name, err)
			continue
		}
		used[dir] = mm.Name
		md := &model{name: mm.Name, eng: mm.Engine, slots: mm.Slots, cfg: mm.Config, store: st, imported: map[string]string{}, turns: map[string]localTurn{}, pending: map[string]chan struct{}{}}
		if cur, ok := old[mm.Name]; ok && cur.store == st {
			// Settings changed, the store did not: what the backends hold
			// and the counters carry over.
			cur.mu.Lock()
			for k, v := range cur.imported {
				md.imported[k] = v
			}
			for k, v := range cur.turns {
				md.turns[k] = v
			}
			md.costs = slices.Clone(cur.costs)
			cur.mu.Unlock()
			for _, c := range []struct{ to, from *atomic.Int64 }{
				{&md.pushes, &cur.pushes}, {&md.pushFails, &cur.pushFails}, {&md.bytesSent, &cur.bytesSent},
				{&md.puts, &cur.puts}, {&md.imports, &cur.imports}, {&md.importFails, &cur.importFails},
			} {
				c.to.Store(c.from.Load())
			}
		}
		md.usage.Store(&u)
		next[mm.Name] = md
		m.d.Logf("kv_share: %s: group %q, replicas in %s (%d held, %d MiB), sessions of %d tokens and more", mm.Name, mm.Config.Group, dir, u.Replicas, u.Bytes>>20, mm.Config.MinTokens)
	}
	m.mu.Lock()
	m.models = next
	m.mu.Unlock()
}

// openStore is the store on dir: the one this process already opened, else a
// new one, cleaned once. Two stores on one directory would not share their
// locks, and cleaning a directory in use could remove a put in progress.
func (m *Manager) openStore(dir string) (*kvshare.Store, kvshare.Usage, error) {
	m.mu.Lock()
	st, ok := m.stores[dir]
	m.mu.Unlock()
	if ok {
		u, err := st.Evict(-1)
		return st, u, err
	}
	st = kvshare.NewStore(dir)
	u, err := st.Clean()
	if err != nil {
		return nil, kvshare.Usage{}, err
	}
	m.mu.Lock()
	m.stores[dir] = st
	m.mu.Unlock()
	return st, u, nil
}

func (m *Manager) model(name string) *model {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.models[name]
}

// Group is the model's kv_share group on this node, "" for none: published
// on /v1/capacity so that members push only to members that share.
func (m *Manager) Group(name string) string {
	if md := m.model(name); md != nil {
		return md.cfg.Group
	}
	return ""
}

// Stats is the model's counters, nil when it shares nothing.
func (m *Manager) Stats(name string) *meshapi.KVShareStats {
	md := m.model(name)
	if md == nil {
		return nil
	}
	u := md.usage.Load()
	return &meshapi.KVShareStats{
		Group: md.cfg.Group, Pushes: md.pushes.Load(), PushFailures: md.pushFails.Load(), BytesSent: md.bytesSent.Load(),
		Puts: md.puts.Load(), Imports: md.imports.Load(), ImportFails: md.importFails.Load(),
		Replicas: u.Replicas, StoreBytes: u.Bytes,
	}
}

// storeKey is the file-safe name of a session: its routing hash, so the
// raw header never becomes a file name.
func storeKey(key uint64) string { return fmt.Sprintf("%016x", key) }

// failf logs a failure once per failLogEvery for each (what, peer): one peer
// whose engine refuses every file must not fill the log turn by turn.
func (m *Manager) failf(what, peer, format string, args ...any) {
	k := what + "\x00" + peer
	now := m.d.Now()
	m.mu.Lock()
	last, seen := m.logged[k]
	if seen && now.Sub(last) < failLogEvery {
		m.mu.Unlock()
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: "+format, args...)
		}
		return
	}
	m.logged[k] = now
	m.mu.Unlock()
	m.d.Logf("kv_share: "+format, args...)
}

// target is where a session's replica goes: the first member after this
// node in SessionRank over the fresh members that serve the model in the
// same group with a healthy backend.
func (m *Manager) target(md *model, key uint64) (node, addr string) {
	if m.d.Reports == nil {
		return "", ""
	}
	now := m.d.Now()
	names := []string{m.d.Self}
	addrs := map[string]string{}
	for _, rep := range m.d.Reports.Reports() {
		if rep.Node == m.d.Self || now.Sub(rep.Received) > m.d.StaleAfter {
			continue
		}
		for _, mc := range rep.Models {
			if mc.Name == md.name && mc.KVShareGroup == md.cfg.Group && mc.HealthyBackends > 0 {
				names = append(names, rep.Node)
				addrs[rep.Node] = rep.APIAddr
				break
			}
		}
	}
	for _, n := range route.SessionRank(key, names) {
		if n != m.d.Self {
			return n, addrs[n]
		}
	}
	return "", ""
}

// generation names a backend's current launch.
func generation(b Backend) string {
	if l, ok := b.(launcher); ok {
		return fmt.Sprintf("%s@%d", b.ID(), l.LaunchedAt().UnixNano())
	}
	return b.ID()
}

// BeforeLocal runs before a session's turn goes to local backend b: it drops
// a save still waiting for the session on b (the turn makes it stale), and
// imports this node's replica of the session into b when b has not imported
// that replica yet. It returns when the import is done or has failed, and
// never fails the turn.
//
// A request that claimed a slot holding another session's conversation
// (supervisor.SlotClaim.Displaced) first saves that session, if its save is
// still waiting: otherwise the request would overwrite a conversation no
// copy has been taken of. Only the save is on this request's clock; the
// copy and the push follow in the background.
func (m *Manager) BeforeLocal(ctx context.Context, modelName, raw string, key uint64, b Backend) {
	m.mu.Lock()
	w := m.workers[b.ID()]
	md := m.models[modelName]
	m.mu.Unlock()
	if w == nil && md == nil {
		return // the common case for a model without kv_share: no allocation
	}
	if w != nil && md != nil && md.slots != nil {
		m.saveDisplaced(ctx, md, w, b)
	}
	if !sharable(raw) {
		return
	}
	k := storeKey(key)
	if w != nil {
		w.drop(modelName, k)
	}
	if md == nil {
		return
	}
	if md.slots != nil {
		// A copy of the session still on its way into the store is newer
		// than the one there: a slot that does not hold the session waits
		// for it (a displaced session's save, seconds at most).
		if sb, ok := b.(Slotted); ok {
			if c, ok := sb.Slot(); ok && !c.Held {
				md.waitPending(ctx, k)
			}
		}
	}
	rep, ok, err := md.store.Info(k)
	if err != nil || !ok || rep.Tokens < md.cfg.MinTokens {
		return
	}
	if md.slots != nil {
		m.restoreSlot(ctx, md, k, rep, b)
		return
	}
	gen := generation(b) + "/" + k
	md.mu.Lock()
	had, seen := md.imported[gen]
	md.mu.Unlock()
	if seen && had == rep.Version() && !m.evicted(ctx, md, b, raw, k, rep.Tokens) {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	name := "kvi-" + k + ".bin"
	path := filepath.Join(md.cfg.SaveDir, name)
	if _, err := md.store.Materialize(k, path); err != nil {
		md.importFails.Add(1)
		m.failf("materialize", "", "%s: assembling the replica of session %s: %v; serving it cold", modelName, k, err)
		return
	}
	defer os.Remove(path)
	if err := md.eng.ImportSession(ctx, b.Addr(), name); err != nil {
		md.importFails.Add(1)
		m.failf("import", b.ID(), "%s: backend %s refused the replica of session %s (%d tokens): %v; serving it cold", modelName, b.ID(), k, rep.Tokens, err)
		return
	}
	took := time.Since(start)
	m.imported(md, gen, rep, took)
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: imported session %s (%d tokens, %d MiB) into %s in %d ms", modelName, k, rep.Tokens, rep.Size>>20, b.ID(), took.Milliseconds())
	}
}

// evicted reports that backend b, which was given this replica before, no
// longer holds as much of the conversation: the engine's cache evicts on its
// own and says nothing, so the record of what was imported is only a hint.
// An engine that cannot say keeps the record's answer.
func (m *Manager) evicted(ctx context.Context, md *model, b Backend, raw, k string, tokens int64) bool {
	h, ok := md.eng.(engine.SessionHolder)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, heldTimeout)
	defer cancel()
	// the replica's own resume point names the conversation: the session's
	// latest branch there may be another one (a compaction on another host)
	n, sum, err := md.store.ResumeIDs(k)
	if err != nil {
		n, sum = 0, 0
	}
	held, err := h.SessionHeld(ctx, b.Addr(), raw, n, sum)
	if err != nil {
		return false
	}
	need := tokens
	if n > 0 {
		need = n // the engine holds what the replica would resume from
	}
	if held >= need {
		return false
	}
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: backend %s holds %d of the %d tokens it was given; importing again", md.name, b.ID(), held, tokens)
	}
	return true
}

// AfterLocal runs after a session's turn on local backend b completed: a
// conversation of MinTokens or more is saved and pushed to the session's
// replica host, later and in the background (see worker).
func (m *Manager) AfterLocal(modelName, raw string, key uint64, b Backend, promptTokens int64) {
	if !sharable(raw) {
		return
	}
	md := m.model(modelName)
	if md == nil || promptTokens < md.cfg.MinTokens {
		return
	}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	w := m.workers[b.ID()]
	if w == nil {
		w = newWorker()
		m.workers[b.ID()] = w
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			w.run(m.ctx, func(ctx context.Context, j job) { m.save(ctx, w, j) })
		}()
	}
	m.mu.Unlock()
	j := job{model: modelName, key: storeKey(key), hash: key, raw: raw, backend: b, due: m.d.Now().Add(m.d.SaveDelay)}
	if md.cfg.LazySaves {
		j.due = lazyDue // run by saveDisplaced or Flush only
	}
	if md.slots != nil {
		sb, ok := b.(Slotted)
		if !ok {
			return
		}
		if j.slot, j.slotted = sb.Slot(); !j.slotted {
			return
		}
		md.mu.Lock()
		if len(md.turns) >= maxImported {
			clear(md.turns)
		}
		md.turns[slotGen(b, j.slot, j.key)] = localTurn{at: m.d.Now(), tokens: promptTokens}
		md.mu.Unlock()
	}
	w.add(j)
}

// push saves one session from its backend and sends it to its replica host.
func (m *Manager) push(ctx context.Context, j job) {
	md := m.model(j.model)
	if md == nil {
		return
	}
	m.begin()
	defer m.end()
	var peer, addr string
	if md.cfg.Group != "" {
		peer, addr = m.target(md, j.hash)
	}
	if md.slots != nil {
		// A peer that refused this node's replicas as another model's gets
		// none for a while; a local copy is still kept.
		if until, ok := md.mismatched.Load(peer); ok && peer != "" && m.d.Now().Before(until.(time.Time)) {
			peer, addr = "", ""
		}
		if (peer == "" || addr == "") && !md.cfg.Local {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, pushTimeout)
		defer cancel()
		m.pushSlot(ctx, md, j, peer, addr)
		return
	}
	hasPeer := peer != "" && addr != ""
	if !hasPeer && !md.cfg.Local {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	var t kvshare.Transport
	var man *kvshare.Manifest
	if hasPeer {
		t = kvshare.NewHTTPTransport(kvshare.HTTPOptions{
			Query: url.Values{"model": {j.model}},
			Sign:  m.d.Sign,
		})
		// What the peer holds first: a peer that cannot answer costs no
		// save (which pauses the backend) unless the save is kept here too,
		// and the save id it holds is the base the engine may write a
		// delta against.
		var err error
		if man, err = t.Manifest(ctx, addr, j.key); err != nil {
			md.pushFails.Add(1)
			m.failf("push", peer, "%s: asking %s what it holds of session %s: %v", j.model, peer, j.key, err)
			if !md.cfg.Local {
				return
			}
			t = nil
		}
	}
	// The base of an engine delta: this node's own copy when it keeps one
	// (always its own last save, unless a peer's replica has replaced it),
	// else what the peer holds.
	var base string
	if md.cfg.Local {
		if rep, ok, err := md.store.Info(j.key); err == nil && ok {
			base = rep.SaveID
		}
	} else if man != nil {
		base = man.SaveID
	}
	start := time.Now()
	name := m.fileName("kvs", j.key)
	path := filepath.Join(md.cfg.SaveDir, name)
	saved, err := md.eng.SaveSession(ctx, j.backend.Addr(), j.raw, name, base)
	if errors.Is(err, engine.ErrSessionGone) {
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: session %s is no longer held by %s; nothing to send", j.model, j.key, j.backend.ID())
		}
		return
	}
	// The engine writes the file; whatever it wrote goes, whatever happens.
	defer os.Remove(path)
	if err != nil {
		md.pushFails.Add(1)
		m.failf("save", j.backend.ID(), "%s: saving session %s from %s: %v", j.model, j.key, j.backend.ID(), err)
		return
	}
	savedAt := time.Now()
	kept := false
	if md.cfg.Local {
		kept = m.keepSession(ctx, md, j, path, saved, savedAt.Sub(start))
	}
	if t == nil {
		return
	}
	var res kvshare.PushResult
	kind := "diff"
	switch {
	case !saved.Delta:
		res, err = kvshare.PushSave(ctx, t, addr, j.key, path, man, saved.ID)
	case man != nil && man.SaveID == base:
		kind = "engine delta"
		res, err = kvshare.PushEngineDelta(ctx, t, addr, j.key, path)
	case kept:
		// A delta against this node's copy, which the peer does not hold:
		// the whole file, rebuilt from the copy, as a diff against what
		// the peer has.
		full := filepath.Join(md.cfg.SaveDir, m.fileName("kvm", j.key))
		if _, err = md.store.Materialize(j.key, full); err == nil {
			// A replica from a member may have replaced the copy since it
			// was kept: send the file only under the id it was saved as.
			if rep, ok, ierr := md.store.Info(j.key); ierr != nil || !ok || rep.SaveID != saved.ID {
				err = fmt.Errorf("the copy of session %s changed before it was sent", j.key)
			} else {
				res, err = kvshare.PushSave(ctx, t, addr, j.key, full, man, saved.ID)
			}
		}
		os.Remove(full)
	default:
		err = fmt.Errorf("a delta against %s, which neither %s nor this node holds", base, peer)
	}
	if err == nil {
		md.bytesSent.Add(res.Plan.Data)
		md.pushes.Add(1)
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: session %s to %s (%s): %d of %d MiB in %d ms (save %d ms)", j.model, j.key, peer, kind,
				res.Plan.Data>>20, res.Plan.Size>>20, time.Since(savedAt).Milliseconds(), savedAt.Sub(start).Milliseconds())
		}
		return
	}
	md.pushFails.Add(1)
	m.failf("push", peer, "%s: sending session %s to %s: %v", j.model, j.key, peer, err)
}

// modelID is the model fingerprint of backend b's weights: the file the
// backend runs, for an engine that fingerprints per backend (a model with a
// source has no path until a backend fetched it, so the fingerprint from
// SessionShare is 0), else the one SessionShare gave. 0 matches nothing.
func (md *model) modelID(b Backend) uint64 {
	f, ok := md.slots.(engine.ModelFingerprinter)
	if !ok {
		return md.cfg.Model
	}
	if p, ok := b.(interface{ ModelPath() string }); ok && p.ModelPath() != "" {
		if id, err := f.ModelFingerprint(p.ModelPath()); err == nil && id != 0 {
			md.fp.Store(id)
			return id
		}
	}
	if id := md.fp.Load(); id != 0 {
		return id
	}
	return md.cfg.Model
}

// localTurn is a session's last turn in one slot here: when it ended and
// how many prompt tokens it had.
type localTurn struct {
	at     time.Time
	tokens int64
}

// ownID is the model fingerprint this node restores, as far as it knows
// without a backend at hand: a backend's, once one answered (modelID), else
// SessionShare's; 0 when it cannot say.
func (md *model) ownID() uint64 {
	if id := md.fp.Load(); id != 0 {
		return id
	}
	return md.cfg.Model
}

// mismatchBackoff is how long a peer that refused this node's replicas as
// another model's gets none: each would cost a save, which stalls the
// backend, for a file the peer cannot use.
const mismatchBackoff = 10 * time.Minute

// keepSession is keepLocal for a SessionSharer model (Strata): the save, a
// whole file or an engine delta against this node's copy, goes into the
// store, and the backend is recorded as holding that version. ok is false
// when the copy was not kept.
func (m *Manager) keepSession(ctx context.Context, md *model, j job, path string, saved engine.SavedSession, saveTook time.Duration) bool {
	start := time.Now()
	var res kvshare.PushResult
	var err error
	if saved.Delta {
		res, err = kvshare.PushEngineDelta(ctx, kvshare.StoreTransport(md.store), "", j.key, path)
	} else {
		if fi, serr := os.Stat(path); serr == nil {
			md.makeRoom(fi.Size(), j.key)
		}
		res, err = kvshare.PushSaveAs(ctx, kvshare.StoreTransport(md.store), "", j.key, path, nil, saved.ID, kvshare.Identity{})
	}
	md.sweep()
	if err != nil {
		md.localFails.Add(1)
		m.failf("local", "", "%s: keeping session %s: %v", j.model, j.key, err)
		return false
	}
	md.locals.Add(1)
	if rep, ok, err := md.store.Info(j.key); err == nil && ok {
		md.mu.Lock()
		if len(md.imported) >= maxImported {
			clear(md.imported)
		}
		md.imported[generation(j.backend)+"/"+j.key] = rep.Version()
		md.mu.Unlock()
	}
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: kept session %s from %s: %d of %d MiB written in %d ms (save %d ms, delta %v)", j.model, j.key, j.backend.ID(),
			res.Plan.Data>>20, res.Plan.Size>>20, time.Since(start).Milliseconds(), saveTook.Milliseconds(), saved.Delta)
	}
	return true
}

// slotGen names a slot of a backend's current launch, for one session.
func slotGen(b Backend, c supervisor.SlotClaim, key string) string {
	return generation(b) + "/" + strconv.Itoa(c.Index) + "/" + key
}

// restoreSlot is BeforeLocal for a SlotSharer model: the replica goes into
// the slot this turn was given, unless that slot already holds the session
// as recent as the replica (its own turns ran there since the replica
// arrived, or this very replica was restored into it). A replica of another
// model or configuration is never restored.
func (m *Manager) restoreSlot(ctx context.Context, md *model, k string, rep kvshare.Replica, b Backend) {
	sb, ok := b.(Slotted)
	if !ok {
		return
	}
	c, ok := sb.Slot()
	if !ok {
		return
	}
	if id := md.modelID(b); id == 0 || rep.Model != id || rep.Config != md.cfg.Config {
		m.failf("identity", "", "%s: the replica of session %s is of another model or configuration (%016x/%016x, this node %016x/%016x); serving it cold",
			md.name, k, rep.Model, rep.Config, id, md.cfg.Config)
		return
	}
	gen := slotGen(b, c, k)
	if c.Held {
		md.mu.Lock()
		had, last := md.imported[gen], md.turns[gen]
		md.mu.Unlock()
		// A replica that arrived after the slot's last turn here is not
		// necessarily newer: a peer's push of an earlier turn can land after
		// this host ran the next one. It must also hold more of the
		// conversation than that turn did (turns only append; after a
		// compaction elsewhere the slot's copy is merely served cold).
		if had == rep.Version() || !rep.Updated.After(last.at) || rep.Tokens <= last.tokens {
			return
		}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	name := "kvi-" + k + ".bin"
	path := filepath.Join(md.cfg.SaveDir, name)
	if _, err := md.store.Materialize(k, path); err != nil {
		md.importFails.Add(1)
		m.failf("materialize", "", "%s: assembling the replica of session %s: %v; serving it cold", md.name, k, err)
		return
	}
	defer os.Remove(path)
	assembled := time.Now()
	if err := md.slots.RestoreSlot(ctx, b.Addr(), c.Index, name); err != nil {
		md.importFails.Add(1)
		m.failf("import", b.ID(), "%s: backend %s refused the replica of session %s (%d tokens) into slot %d: %v; serving it cold", md.name, b.ID(), k, rep.Tokens, c.Index, err)
		return
	}
	m.imported(md, gen, rep, time.Since(start))
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: restored session %s (%d tokens, %d MiB) into %s slot %d in %d ms (assembly %d ms)", md.name, k, rep.Tokens, rep.Size>>20, b.ID(), c.Index, time.Since(start).Milliseconds(), assembled.Sub(start).Milliseconds())
	}
}

// pushSlot is push for a SlotSharer model: the slot the turn ran in is saved
// if it still holds the session and is idle (another request may have taken
// it, or the session's next turn may be running there, whose own save
// follows), and the file goes out stamped with this node's identity: to the
// session's replica host when there is one (peer), and into this node's own
// store when the model keeps local copies.
func (m *Manager) pushSlot(ctx context.Context, md *model, j job, peer, addr string) {
	sb, ok := j.backend.(Slotted)
	if !ok || !j.slotted || !j.claimed && !sb.SlotHolds(j.slot, j.hash) {
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: slot %d of %s no longer holds session %s idle; nothing to send", j.model, j.slot.Index, j.backend.ID(), j.key)
		}
		return
	}
	var t kvshare.Transport
	var man *kvshare.Manifest
	if peer != "" && addr != "" {
		t = kvshare.NewHTTPTransport(kvshare.HTTPOptions{
			Query: url.Values{"model": {j.model}},
			Sign:  m.d.Sign,
		})
	}
	// What the peer holds first: a peer that cannot answer costs no save
	// (which pauses the backend), unless the save is kept here too. Not for
	// a displaced session, whose save a request is waiting for: its peer is
	// asked after the save.
	if t != nil && !j.claimed {
		var err error
		if man, err = t.Manifest(ctx, addr, j.key); err != nil {
			md.pushFails.Add(1)
			m.failf("push", peer, "%s: asking %s what it holds of session %s: %v", j.model, peer, j.key, err)
			if !md.cfg.Local {
				return
			}
			t = nil
		}
	}
	start := time.Now()
	name := m.fileName("kvs", j.key)
	path := filepath.Join(md.cfg.SaveDir, name)
	release := func() {}
	if md.cfg.Local {
		release = md.markPending(j.key)
	}
	err := md.slots.SaveSlot(ctx, j.backend.Addr(), j.slot.Index, name)
	if errors.Is(err, engine.ErrSessionGone) {
		release()
		return
	}
	if err != nil {
		release()
		os.Remove(path)
		md.pushFails.Add(1)
		m.failf("save", j.backend.ID(), "%s: saving session %s from %s slot %d: %v", j.model, j.key, j.backend.ID(), j.slot.Index, err)
		return
	}
	// Nothing reserves the slot between the check and the save: a request
	// claimed in between may have run first, and llama-server holds a save
	// on a busy slot until it is free, so the file would be that request's
	// conversation under this session's key. The slot still holding the
	// session, idle, after the save proves no other request ran there. (A
	// claimed slot is busy with a request that waits for this save.)
	if !j.claimed && !sb.SlotHolds(j.slot, j.hash) {
		release()
		os.Remove(path)
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: slot %d of %s was taken during the save of session %s; dropping the file", j.model, j.slot.Index, j.backend.ID(), j.key)
		}
		return
	}
	if j.claimed {
		// The request that claimed the slot waits for the save only.
		m.mu.Lock()
		if m.ctx.Err() != nil {
			m.mu.Unlock()
			release()
			os.Remove(path)
			return
		}
		m.wg.Add(1)
		m.inflight++ // under mu, as begin
		m.mu.Unlock()
		go func() {
			defer m.wg.Done()
			defer m.end()
			ctx, cancel := context.WithTimeout(m.ctx, pushTimeout)
			defer cancel()
			if t != nil {
				var err error
				if man, err = t.Manifest(ctx, addr, j.key); err != nil {
					md.pushFails.Add(1)
					m.failf("push", peer, "%s: asking %s what it holds of session %s: %v", j.model, peer, j.key, err)
					t = nil
				}
			}
			m.sendSlot(ctx, md, j, t, peer, addr, man, path, start, release)
		}()
		return
	}
	m.sendSlot(ctx, md, j, t, peer, addr, man, path, start, release)
}

// sendSlot keeps and sends the file a slot was saved to, then removes it.
func (m *Manager) sendSlot(ctx context.Context, md *model, j job, t kvshare.Transport, peer, addr string, man *kvshare.Manifest, path string, start time.Time, release func()) {
	defer os.Remove(path)
	savedAt := time.Now()
	if md.cfg.Local {
		m.keepLocal(ctx, md, j, path, savedAt.Sub(start))
	}
	release()
	if t == nil {
		return
	}
	res, err := kvshare.PushSaveAs(ctx, t, addr, j.key, path, man, "", kvshare.Identity{Model: md.modelID(j.backend), Config: md.cfg.Config})
	if se := (*kvshare.StatusError)(nil); errors.As(err, &se) && se.Code == http.StatusConflict {
		md.mismatched.Store(peer, m.d.Now().Add(mismatchBackoff))
	}
	if err != nil {
		md.pushFails.Add(1)
		m.failf("push", peer, "%s: sending session %s to %s: %v", j.model, j.key, peer, err)
		return
	}
	md.bytesSent.Add(res.Plan.Data)
	md.pushes.Add(1)
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: session %s to %s (diff): %d of %d MiB in %d ms (save %d ms)", j.model, j.key, peer,
			res.Plan.Data>>20, res.Plan.Size>>20, time.Since(savedAt).Milliseconds(), savedAt.Sub(start).Milliseconds())
	}
}

// saveDisplaced saves the session whose conversation b's claimed slot
// holds, when that session's save is still waiting in w (see BeforeLocal).
func (m *Manager) saveDisplaced(ctx context.Context, md *model, w *worker, b Backend) {
	sb, ok := b.(Slotted)
	if !ok {
		return
	}
	c, ok := sb.Slot()
	if !ok || c.Displaced == 0 {
		return
	}
	j, ok := w.takeSlot(md.name, c.Displaced, c.Index)
	if !ok {
		return
	}
	j.claimed = true
	var peer, addr string
	if md.cfg.Group != "" {
		peer, addr = m.target(md, j.hash)
	}
	if (peer == "" || addr == "") && !md.cfg.Local {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	m.pushSlot(ctx, md, j, peer, addr)
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: saved session %s from %s slot %d before reusing the slot, in %d ms", md.name, j.key, b.ID(), c.Index, time.Since(start).Milliseconds())
	}
}

// keepLocal puts the session file at path, just saved from j's slot, into
// this node's own store, and records that the slot holds that version: the
// session's next turn there must not restore what the slot already has.
func (m *Manager) keepLocal(ctx context.Context, md *model, j job, path string, saveTook time.Duration) {
	fi, err := os.Stat(path)
	if err != nil {
		md.localFails.Add(1)
		m.failf("local", "", "%s: keeping session %s: %v", j.model, j.key, err)
		return
	}
	if fi.Size() > md.cfg.MaxBytes {
		return
	}
	md.makeRoom(fi.Size(), j.key)
	start := time.Now()
	res, err := kvshare.SaveLocal(ctx, md.store, j.key, path, kvshare.Identity{Model: md.modelID(j.backend), Config: md.cfg.Config})
	md.sweep()
	if err != nil {
		md.localFails.Add(1)
		m.failf("local", "", "%s: keeping session %s: %v", j.model, j.key, err)
		return
	}
	md.locals.Add(1)
	if rep, ok, err := md.store.Info(j.key); err == nil && ok {
		md.mu.Lock()
		if len(md.imported) >= maxImported {
			clear(md.imported)
		}
		md.imported[slotGen(j.backend, j.slot, j.key)] = rep.Version()
		md.mu.Unlock()
	}
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: kept session %s from %s slot %d: %d of %d MiB written in %d ms (save %d ms)", j.model, j.key, j.backend.ID(), j.slot.Index,
			res.Plan.Data>>20, res.Plan.Size>>20, time.Since(start).Milliseconds(), saveTook.Milliseconds())
	}
}

// makeRoom evicts when the store looks full, so it makes way for a file of
// size under key; never at the cost of that key's replica. Eviction walks
// every replica, so a put that fits skips it.
func (md *model) makeRoom(size int64, key string) {
	if u := md.usage.Load(); u.Bytes+size > md.cfg.MaxBytes {
		if u, err := md.store.EvictExcept(md.cfg.MaxBytes-size, key); err == nil {
			md.usage.Store(&u)
		}
	}
}

// sweep brings the store under its bound off the caller's clock, one sweep
// at a time: a sweep waits for every other replica's lock.
func (md *model) sweep() {
	if md.evicting.CompareAndSwap(false, true) {
		go func() {
			defer md.evicting.Store(false)
			if u, err := md.store.Evict(md.cfg.MaxBytes); err == nil {
				md.usage.Store(&u)
			}
		}()
	}
}

// imported records that gen now holds rep, and what bringing it there cost.
func (m *Manager) imported(md *model, gen string, rep kvshare.Replica, took time.Duration) {
	md.mu.Lock()
	if len(md.imported) >= maxImported {
		clear(md.imported)
	}
	md.imported[gen] = rep.Version()
	if rep.Tokens >= 1000 {
		md.costs = append(md.costs, float64(took.Milliseconds())*1000/float64(rep.Tokens))
		if len(md.costs) > importCosts {
			md.costs = md.costs[1:]
		}
	}
	md.mu.Unlock()
	md.imports.Add(1)
}
