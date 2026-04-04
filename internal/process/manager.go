package process

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/config"
)

const maxRespawnAttempts = 3

type PowerSampler interface {
	Sample(ctx context.Context)
}

type CostTracker interface {
	Update(ctx context.Context)
}

type GPUCollector interface {
	Sample(ctx context.Context)
}

type Manager struct {
	Backends      []*Backend
	cfg           *config.Config
	logger        *log.Logger
	mu            sync.Mutex
	failureCounts map[int]int
	respawnCounts map[int]int
	sampler       PowerSampler
	tracker       CostTracker
	collector     GPUCollector
}

func NewManager(cfg *config.Config, logWriter io.Writer, sampler PowerSampler, tracker CostTracker, collector GPUCollector) *Manager {
	m := &Manager{
		cfg: cfg, logger: log.New(os.Stdout, "[manager] ", log.LstdFlags),
		failureCounts: make(map[int]int), respawnCounts: make(map[int]int),
		sampler: sampler, tracker: tracker, collector: collector,
	}
	m.Backends = make([]*Backend, cfg.GPUs.Count)
	for i := range cfg.GPUs.Count {
		port := cfg.GPUs.BasePort + i
		addr := fmt.Sprintf("localhost:%d", port)
		m.Backends[i] = &Backend{
			GPUID: i, ModelPath: cfg.Model.Path, Port: port,
			ContextSize: cfg.Model.ContextSize, NGPULayers: cfg.Model.NGPULayers,
			Binary: cfg.Backend.Binary, ExtraArgs: cfg.Backend.ExtraArgs,
			HealthTimeout:   cfg.Health.Timeout.Duration,
			PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
			State: balancer.NewBackendState(i, addr), LogWriter: logWriter,
		}
	}
	return m
}

func (m *Manager) States() []*balancer.BackendState {
	states := make([]*balancer.BackendState, len(m.Backends))
	for i, b := range m.Backends { states[i] = b.State }
	return states
}

func (m *Manager) StartAll(ctx context.Context) error {
	if len(m.Backends) == 0 {
		return nil
	}

	// Start first backend and wait for it so the node can serve requests immediately.
	b0 := m.Backends[0]
	m.logger.Printf("starting backend gpu-%d on port %d", b0.GPUID, b0.Port)
	if err := b0.Start(); err != nil {
		return fmt.Errorf("backend gpu-%d: %w", b0.GPUID, err)
	}
	if err := m.waitForHealthy(ctx, b0); err != nil {
		m.logger.Printf("WARNING: gpu-%d failed to become healthy: %v", b0.GPUID, err)
		b0.State.SetStatus(balancer.StatusUnhealthy)
	}

	// Start remaining backends in the background — they join the pool as they become healthy.
	if len(m.Backends) > 1 {
		remaining := m.Backends[1:]
		m.logger.Printf("starting %d more backend(s) in background...", len(remaining))
		go func() {
			for i, b := range remaining {
				if i > 0 {
					time.Sleep(2 * time.Second)
				}
				m.logger.Printf("starting backend gpu-%d on port %d", b.GPUID, b.Port)
				if err := b.Start(); err != nil {
					m.logger.Printf("ERROR: backend gpu-%d failed to start: %v", b.GPUID, err)
					continue
				}
				if err := m.waitForHealthy(ctx, b); err != nil {
					m.logger.Printf("WARNING: gpu-%d failed to become healthy: %v", b.GPUID, err)
					b.State.SetStatus(balancer.StatusUnhealthy)
				}
			}
			m.logger.Printf("all backends started")
		}()
	}

	return nil
}

func (m *Manager) waitForHealthy(ctx context.Context, b *Backend) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(120 * time.Second)
	for {
		select {
		case <-ctx.Done(): return ctx.Err()
		case <-timeout: return fmt.Errorf("timeout waiting for gpu-%d", b.GPUID)
		case <-ticker.C:
			if b.CheckHealth(ctx) {
				b.State.SetStatus(balancer.StatusHealthy)
				m.logger.Printf("gpu-%d is healthy", b.GPUID)
				return nil
			}
		}
	}
}

func (m *Manager) RunHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Health.Interval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
			for _, b := range m.Backends {
				if b.State.Status() == balancer.StatusDead { continue }
				m.checkAndManage(ctx, b)
			}
			if m.sampler != nil {
				m.sampler.Sample(ctx)
			}
			if m.tracker != nil {
				m.tracker.Update(ctx)
			}
			if m.collector != nil {
				m.collector.Sample(ctx)
			}
		}
	}
}

func (m *Manager) checkAndManage(ctx context.Context, b *Backend) {
	// Health check outside the lock to avoid holding mutex during network I/O
	healthy := b.CheckHealth(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if healthy {
		m.failureCounts[b.GPUID] = 0
		if b.State.Status() != balancer.StatusHealthy {
			b.State.SetStatus(balancer.StatusHealthy)
			m.respawnCounts[b.GPUID] = 0
			m.logger.Printf("gpu-%d recovered", b.GPUID)
		}
		return
	}
	m.failureCounts[b.GPUID]++
	b.State.SetStatus(balancer.StatusUnhealthy)
	m.logger.Printf("gpu-%d health check failed (%d/%d)", b.GPUID, m.failureCounts[b.GPUID], m.cfg.Health.MaxFailures)
	if m.failureCounts[b.GPUID] >= m.cfg.Health.MaxFailures {
		m.failureCounts[b.GPUID] = 0
		m.respawnCounts[b.GPUID]++
		if m.respawnCounts[b.GPUID] >= maxRespawnAttempts {
			b.State.SetStatus(balancer.StatusDead)
			m.logger.Printf("ERROR: gpu-%d marked DEAD after %d respawn attempts", b.GPUID, maxRespawnAttempts)
			return
		}
		m.logger.Printf("respawning gpu-%d (attempt %d/%d)", b.GPUID, m.respawnCounts[b.GPUID], maxRespawnAttempts)
		b.Kill()
		b.Wait()
		if err := b.Start(); err != nil {
			m.logger.Printf("ERROR: failed to respawn gpu-%d: %v", b.GPUID, err)
		}
	}
}

func (m *Manager) Shutdown(ctx context.Context) {
	m.logger.Println("shutting down all backends...")
	for _, b := range m.Backends { b.Stop() }
	done := make(chan struct{})
	go func() { for _, b := range m.Backends { b.Wait() }; close(done) }()
	select {
	case <-done:
		m.logger.Println("all backends stopped gracefully"); return
	case <-ctx.Done():
		m.logger.Println("shutdown context expired, sending SIGKILL to remaining backends")
		for _, b := range m.Backends { b.Kill() }
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		m.logger.Println("WARNING: some backends did not exit after SIGKILL")
	}
}
