package supervisor

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

// Model is one models[] entry: its backends, whether it admits requests, and
// its supervision loops.
type Model struct {
	cfg       config.Model
	deps      Deps
	backends  []*Backend
	admitting atomic.Bool
	loops     sync.WaitGroup

	loopMu sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool // no loop may start once set
}

func newModel(cfg config.Model, deps Deps) (*Model, error) {
	eng, ok := engine.Lookup(cfg.Engine)
	if !ok {
		return nil, fmt.Errorf("model %s: engine %q is not registered", cfg.Name, cfg.Engine)
	}
	deps = deps.withDefaults()
	m := &Model{cfg: cfg, deps: deps, ctx: context.Background()}
	for i := 0; i < cfg.Backends(); i++ {
		m.backends = append(m.backends, newBackend(cfg, i, eng, deps))
	}
	m.admitting.Store(true)
	return m, nil
}

func (m *Model) Name() string         { return m.cfg.Name }
func (m *Model) Config() config.Model { return m.cfg }
func (m *Model) Backends() []*Backend { return slices.Clone(m.backends) }
func (m *Model) Admitting() bool      { return m.admitting.Load() }

// setLoopContext derives the model's loop context from parent.
func (m *Model) setLoopContext(parent context.Context) {
	m.loopMu.Lock()
	defer m.loopMu.Unlock()
	m.ctx, m.cancel = context.WithCancel(parent)
}

func (m *Model) loopCtx() context.Context {
	m.loopMu.Lock()
	defer m.loopMu.Unlock()
	return m.ctx
}

// stopLoops cancels the model's loops and forbids starting new ones.
func (m *Model) stopLoops() {
	m.loopMu.Lock()
	defer m.loopMu.Unlock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
}

// startLoop runs b's supervision loop in a goroutine the model tracks, so a
// drain can wait for it to return before stopping processes. Once the model is
// closed it only releases the ticket: a model removed before its loops started
// must never start them afterwards.
func (m *Model) startLoop(ctx context.Context, b *Backend, first *loadTicket, gate *loadGate, nodePIDs func() []int) {
	m.loopMu.Lock()
	if m.closed {
		m.loopMu.Unlock()
		first.release()
		return
	}
	m.loops.Add(1)
	m.loopMu.Unlock()
	go func() {
		defer m.loops.Done()
		b.run(ctx, first, gate, nodePIDs)
	}()
}

// drain stops admitting at once, waits (polling every 50 ms) until nothing is
// in flight or grace has passed, then stops every backend concurrently and
// returns when all have stopped.
//
// Callers cancel the loops first. drain also closes the model and waits for
// its loops to have returned before stopping anything: a loop cancelled in the
// middle of a launch would otherwise start a process after the drain had
// stopped the backends, and that process would hold its GPUs with nothing left
// to stop it.
func (m *Model) drain(grace time.Duration) {
	m.admitting.Store(false)
	m.loopMu.Lock()
	m.closed = true
	m.loopMu.Unlock()
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && m.inFlight() > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	m.loops.Wait()
	var wg sync.WaitGroup
	for _, b := range m.backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.stop(m.deps.Timing.RespawnStopGrace)
		}()
	}
	wg.Wait()
}

func (m *Model) inFlight() int {
	n := 0
	for _, b := range m.backends {
		n += b.InFlight()
	}
	return n
}
