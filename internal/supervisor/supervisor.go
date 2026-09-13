package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/meshapi"
)

var errShutDown = errors.New("supervisor is shut down")

// Supervisor runs a node's model list.
type Supervisor struct {
	deps       Deps
	gate       *loadGate
	base       context.Context
	cancelBase context.CancelFunc

	mu       sync.Mutex
	models   []*Model // in the order of the last Apply
	draining map[*Model]chan struct{}
	shutdown bool
}

func New(deps Deps) *Supervisor {
	base, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		deps:       deps.withDefaults(),
		gate:       newLoadGate(),
		base:       base,
		cancelBase: cancel,
		draining:   map[*Model]chan struct{}{},
	}
}

// Apply makes the running models match models, diffing by name. It is atomic
// on error and returns without waiting for loads or drains.
//
// An unchanged entry keeps its processes. A removed one leaves Models() at
// once and drains in the background, its PIDs still in PIDs() because they
// still hold GPUs. A changed one is removed and added. New models start only
// after every drain in progress has stopped its processes, so a new process
// never loads onto a GPU the old one still holds; their load tickets are
// enqueued round-robin by backend index (Decision 5) before any loop runs.
func (s *Supervisor) Apply(models []config.Model) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return errShutDown
	}

	current := map[string]*Model{}
	for _, m := range s.models {
		current[m.Name()] = m
	}
	seen := map[string]bool{}
	for _, cfg := range models {
		if seen[cfg.Name] {
			return fmt.Errorf("model %s is listed twice", cfg.Name)
		}
		seen[cfg.Name] = true
		if old, ok := current[cfg.Name]; ok && old.cfg.Equal(cfg) {
			continue
		}
		if _, ok := engine.Lookup(cfg.Engine); !ok {
			return fmt.Errorf("model %s: engine %q is not registered", cfg.Name, cfg.Engine)
		}
	}

	next := make([]*Model, 0, len(models))
	var started []*Model
	for _, cfg := range models {
		if old, ok := current[cfg.Name]; ok && old.cfg.Equal(cfg) {
			next = append(next, old)
			delete(current, cfg.Name)
			continue
		}
		m, err := newModel(cfg, s.deps)
		if err != nil {
			return err // unreachable: the engine was looked up above
		}
		next = append(next, m)
		started = append(started, m)
	}
	for _, old := range current { // removed or changed
		s.drainLocked(old, s.deps.Health.RespawnGrace.Duration)
	}
	s.models = next

	if len(started) > 0 {
		for _, m := range started {
			m.setLoopContext(s.base)
		}
		waits := make([]chan struct{}, 0, len(s.draining))
		for _, done := range s.draining {
			waits = append(waits, done)
		}
		go s.start(started, waits)
	}
	return nil
}

// start waits for the drains, then enqueues load tickets round-robin by
// backend index across the started models and starts their loops.
func (s *Supervisor) start(models []*Model, drains []chan struct{}) {
	for _, done := range drains {
		<-done
	}
	type pending struct {
		m      *Model
		b      *Backend
		ticket *loadTicket
	}
	var queue []pending
	for i := 0; ; i++ {
		added := false
		for _, m := range models {
			if bs := m.Backends(); i < len(bs) {
				queue = append(queue, pending{m, bs[i], s.gate.enqueue()})
				added = true
			}
		}
		if !added {
			break
		}
	}
	for _, p := range queue {
		p.m.startLoop(p.m.loopCtx(), p.b, p.ticket, s.gate, s.PIDs)
	}
}

// drainLocked cancels a model's loops and drains it in a tracked goroutine.
func (s *Supervisor) drainLocked(m *Model, grace time.Duration) chan struct{} {
	if done, ok := s.draining[m]; ok {
		return done
	}
	done := make(chan struct{})
	s.draining[m] = done
	m.stopLoops()
	go func() {
		m.drain(grace)
		s.mu.Lock()
		delete(s.draining, m)
		s.mu.Unlock()
		close(done)
	}()
	return done
}

func (s *Supervisor) Models() []*Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Model(nil), s.models...)
}

func (s *Supervisor) Model(name string) (*Model, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.models {
		if m.Name() == name {
			return m, true
		}
	}
	return nil, false
}

// PIDs is every backend process tree on the node, draining models included:
// the on-GPU check needs every PID that can appear in SMI output.
func (s *Supervisor) PIDs() []int {
	s.mu.Lock()
	models := append([]*Model(nil), s.models...)
	for m := range s.draining {
		models = append(models, m)
	}
	s.mu.Unlock()
	var pids []int
	for _, m := range models {
		for _, b := range m.Backends() {
			pids = append(pids, b.PIDs()...)
		}
	}
	return pids
}

func (s *Supervisor) views(m *Model) []backendView {
	now := s.deps.Now()
	bs := m.Backends()
	views := make([]backendView, len(bs))
	for i, b := range bs {
		views[i] = b.view(now, s.deps.Timing.LoadStaleAfter)
	}
	return views
}

// Status is every current model's meshapi status, in Apply order.
func (s *Supervisor) Status() []meshapi.ModelStatus {
	models := s.Models()
	out := make([]meshapi.ModelStatus, 0, len(models))
	for _, m := range models {
		out = append(out, modelStatus(m.cfg, s.views(m)))
	}
	return out
}

// Capacity is every current model's capacity report, in Apply order.
func (s *Supervisor) Capacity() []meshapi.ModelCapacity {
	models := s.Models()
	out := make([]meshapi.ModelCapacity, 0, len(models))
	for _, m := range models {
		out = append(out, modelCapacity(m.cfg, s.views(m)))
	}
	return out
}

// Shutdown stops admitting everywhere, cancels every loop (processes keep
// serving), drains every model concurrently with health.respawn_grace capped
// at the time left before ctx's deadline, and waits for all drains. A second
// call returns at once.
func (s *Supervisor) Shutdown(ctx context.Context) {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	s.shutdown = true
	grace := s.deps.Health.RespawnGrace.Duration
	if deadline, ok := ctx.Deadline(); ok {
		grace = min(grace, time.Until(deadline))
	}
	for _, m := range s.models {
		m.admitting.Store(false)
	}
	s.cancelBase()
	for _, m := range s.models {
		s.drainLocked(m, max(grace, 0))
	}
	s.models = nil
	waits := make([]chan struct{}, 0, len(s.draining))
	for _, done := range s.draining {
		waits = append(waits, done)
	}
	s.mu.Unlock()
	for _, done := range waits {
		<-done
	}
}
