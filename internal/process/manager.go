package process

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/janit/viiwork/internal/activity"
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
	activity      *activity.Log
}

func NewManager(cfg *config.Config, logWriter io.Writer, sampler PowerSampler, tracker CostTracker, collector GPUCollector, actLog *activity.Log) *Manager {
	if actLog == nil {
		actLog = activity.NewLog() // no-op: no subscribers, events silently buffered
	}
	m := &Manager{
		cfg: cfg, logger: log.New(os.Stdout, "[manager] ", log.LstdFlags),
		failureCounts: make(map[int]int), respawnCounts: make(map[int]int),
		sampler: sampler, tracker: tracker, collector: collector, activity: actLog,
	}
	devices := cfg.GPUs.ResolvedDevices()
	m.Backends = make([]*Backend, len(devices))
	for i, gpuID := range devices {
		port := cfg.GPUs.BasePort + i
		addr := fmt.Sprintf("localhost:%d", port)
		m.Backends[i] = &Backend{
			GPUID: gpuID, ModelPath: cfg.Model.Path, Port: port,
			ContextSize: cfg.Model.ContextSize, NGPULayers: cfg.Model.NGPULayers, Parallel: cfg.Model.Parallel,
			Binary: cfg.Backend.Binary, ExtraArgs: cfg.Backend.ExtraArgs,
			HealthTimeout:   cfg.Health.Timeout.Duration,
			PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
			State: balancer.NewBackendState(gpuID, addr), LogWriter: logWriter,
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

	// Use a generous timeout for initial model loading — models can take minutes
	// to load into VRAM. The regular health timeout (120s) is too aggressive and
	// causes cascading concurrent loads when backends time out and the next starts.
	const startupTimeout = 10 * time.Minute

	// Start first backend and wait for it so the node can serve requests immediately.
	b0 := m.Backends[0]
	m.logger.Printf("starting backend gpu-%d on port %d", b0.GPUID, b0.Port)
	m.activity.Emit("backend", b0.GPUID, "loading model into VRAM (first — blocking until ready)")
	if err := b0.Start(); err != nil {
		m.activity.Emit("backend", b0.GPUID, "failed to start: %v", err)
		return fmt.Errorf("backend gpu-%d: %w", b0.GPUID, err)
	}
	t0 := time.Now()
	if err := m.waitForHealthy(ctx, b0, startupTimeout); err != nil {
		m.logger.Printf("WARNING: gpu-%d failed to become healthy: %v", b0.GPUID, err)
		m.activity.Emit("backend", b0.GPUID, "failed to become healthy: %v", err)
		b0.State.SetStatus(balancer.StatusUnhealthy)
	} else {
		m.activity.Emit("backend", b0.GPUID, "ready (loaded in %s)", time.Since(t0).Round(time.Second))
	}

	// Start remaining backends one at a time — each must become healthy (or fail)
	// before the next starts, preventing concurrent model loads that thrash I/O and CPU.
	if len(m.Backends) > 1 {
		remaining := m.Backends[1:]
		m.logger.Printf("starting %d more backend(s) in background...", len(remaining))
		m.activity.Emit("system", -1, "starting %d more backend(s) in background", len(remaining))
		go func() {
			for _, b := range remaining {
				m.logger.Printf("starting backend gpu-%d on port %d", b.GPUID, b.Port)
				m.activity.Emit("backend", b.GPUID, "loading model into VRAM")
				if err := b.Start(); err != nil {
					m.logger.Printf("ERROR: backend gpu-%d failed to start: %v", b.GPUID, err)
					m.activity.Emit("backend", b.GPUID, "failed to start: %v", err)
					continue
				}
				ts := time.Now()
				if err := m.waitForHealthy(ctx, b, startupTimeout); err != nil {
					m.logger.Printf("WARNING: gpu-%d failed to become healthy: %v", b.GPUID, err)
					m.activity.Emit("backend", b.GPUID, "failed to become healthy: %v", err)
					b.State.SetStatus(balancer.StatusUnhealthy)
				} else {
					m.activity.Emit("backend", b.GPUID, "ready (loaded in %s)", time.Since(ts).Round(time.Second))
				}
			}
			m.logger.Printf("all backends started")
			m.activity.Emit("system", -1, "all %d backends started", len(m.Backends))
		}()
	}

	return nil
}

func (m *Manager) waitForHealthy(ctx context.Context, b *Backend, timeout time.Duration) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(timeout)
	for {
		select {
		case <-ctx.Done(): return ctx.Err()
		case <-deadline: return fmt.Errorf("timeout waiting for gpu-%d after %v", b.GPUID, timeout)
		case <-ticker.C:
			if b.CheckHealth(ctx) {
				b.State.SetStatus(balancer.StatusHealthy)
				m.logger.Printf("gpu-%d is healthy", b.GPUID)
				return nil
			}
			if !b.IsRunning() {
				return fmt.Errorf("gpu-%d process died during startup", b.GPUID)
			}
		}
	}
}

func (m *Manager) RunHealthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Health.Interval.Duration)
	defer ticker.Stop()
	// Fast slot poll (1s) for live token progress, separate from slower health checks
	slotTicker := time.NewTicker(1 * time.Second)
	defer slotTicker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-slotTicker.C:
			for _, b := range m.Backends {
				if b.State.Status() == balancer.StatusHealthy {
					ss := b.ReadSlots(ctx)
					b.State.SetSlots(ss.NCtx, ss.NSlots, ss.NActive, ss.NDecoded, ss.NRemain)
				}
			}
		case <-ticker.C:
			for _, b := range m.Backends {
				st := b.State.Status()
				if st == balancer.StatusDead || st == balancer.StatusStarting { continue }
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
	b.State.SetRSSMB(b.ReadRSSMB())

	m.mu.Lock()
	defer m.mu.Unlock()
	if healthy {
		m.failureCounts[b.GPUID] = 0
		if b.State.Status() != balancer.StatusHealthy {
			b.State.SetStatus(balancer.StatusHealthy)
			m.respawnCounts[b.GPUID] = 0
			m.logger.Printf("gpu-%d recovered", b.GPUID)
			m.activity.Emit("backend", b.GPUID, "recovered")
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
			m.activity.Emit("backend", b.GPUID, "marked DEAD after %d respawn attempts", maxRespawnAttempts)
			return
		}
		m.logger.Printf("respawning gpu-%d (attempt %d/%d)", b.GPUID, m.respawnCounts[b.GPUID], maxRespawnAttempts)
		m.activity.Emit("backend", b.GPUID, "respawning (attempt %d/%d)", m.respawnCounts[b.GPUID], maxRespawnAttempts)
		b.Kill()
		b.Wait()
		if err := b.Start(); err != nil {
			m.logger.Printf("ERROR: failed to respawn gpu-%d: %v", b.GPUID, err)
			m.activity.Emit("backend", b.GPUID, "respawn failed: %v", err)
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
