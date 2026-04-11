package process

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/config"
)

// multiPartGGUFRe matches the llama.cpp multi-part naming convention used by
// bartowski / unsloth / canonical GGUF splits:
//
//	<prefix>-NNNNN-of-MMMMM.gguf
//
// where NNNNN is the 1-indexed part number and MMMMM is the total part count.
// Both fields are zero-padded 5-digit decimal. Capture groups: 1=prefix,
// 2=part number, 3=total parts.
var multiPartGGUFRe = regexp.MustCompile(`^(.+)-(\d{5})-of-(\d{5})\.gguf$`)

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
	if cfg.GPUs.TensorSplit.Enabled {
		// Tensor-split mode: a single backend spans all devices and serves on
		// gpus.base_port. The model lives partitioned across the GPUs in the
		// device list. Concurrency is single-slot — Model.Parallel is forced
		// to 1 by config.Validate.
		port := cfg.GPUs.BasePort
		addr := fmt.Sprintf("localhost:%d", port)
		// State.GPUID is the sentinel -1 meaning "tensor-split aggregate".
		// Downstream code that displays per-GPU labels should branch on this.
		m.Backends = []*Backend{{
			GPUID:           -1,
			GPUIDs:          devices,
			TensorSplit:     true,
			SplitMode:       cfg.GPUs.TensorSplit.Mode,
			SplitWeights:    cfg.GPUs.TensorSplit.Weights,
			MainGPU:         cfg.GPUs.TensorSplit.MainGPU,
			ModelPath:       cfg.Model.Path,
			Port:            port,
			ContextSize:     cfg.Model.ContextSize,
			NGPULayers:      cfg.Model.NGPULayers,
			Parallel:        1,
			Binary:          cfg.Backend.Binary,
			ExtraArgs:       cfg.Backend.ExtraArgs,
			HealthTimeout:   cfg.Health.Timeout.Duration,
			PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
			State:           balancer.NewBackendState(-1, addr),
			LogWriter:       logWriter,
		}}
	} else {
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
	}
	m.maybeAutoNoMmap()
	return m
}

// maybeAutoNoMmap inspects each backend's model file size and host RAM, and
// auto-injects --no-mmap into ExtraArgs if the model is larger than 80% of
// host RAM. This is the fix for the post-load mmap-on-NFS thrashing observed
// with Qwen3-235B-A22B Q3_K_M (100 GB on a 46 GB host): the kernel can't keep
// the whole file cached, llama.cpp's post-load metadata pass touches random
// pages, each miss is an NFS read, and the load gets stuck in
// folio_wait_bit_common for hours. --no-mmap fixes it cleanly because each
// tensor is then read once into a malloc'd buffer (a few MB max) and freed
// after upload to VRAM.
//
// If the user has already set --mmap or --no-mmap explicitly in
// backend.extra_args, this function respects their choice and (in the
// --mmap-explicit case) just logs a warning when the model is too big.
func (m *Manager) maybeAutoNoMmap() {
	totalRAMBytes := readTotalRAMBytes()
	if totalRAMBytes == 0 {
		return // can't read meminfo (non-Linux test env, sandboxing, etc.); skip silently
	}
	m.applyAutoNoMmap(totalRAMBytes)
}

// applyAutoNoMmap is the testable inner function — pass an explicit RAM total.
func (m *Manager) applyAutoNoMmap(totalRAMBytes int64) {
	threshold := int64(float64(totalRAMBytes) * 0.8)
	for _, b := range m.Backends {
		modelBytes, err := modelTotalSize(b.ModelPath)
		if err != nil {
			continue // model path not statable from this process; skip
		}
		modelGiB := float64(modelBytes) / (1 << 30)
		ramGiB := float64(totalRAMBytes) / (1 << 30)
		if modelBytes < threshold {
			continue
		}
		if hasNoMmapArg(b.ExtraArgs) {
			continue // user already set --no-mmap, nothing to do
		}
		if hasExplicitMmapArg(b.ExtraArgs) {
			m.logger.Printf("WARNING: %s: model is %.1f GiB but --mmap was set explicitly; "+
				"with host RAM only %.1f GiB this risks page-cache thrashing on first load. "+
				"Consider --no-mmap if loads are slow.",
				b.label(), modelGiB, ramGiB)
			continue
		}
		// auto-inject --no-mmap. Make a fresh slice so we don't mutate any
		// shared cfg.Backend.ExtraArgs slice that other backends reference.
		newArgs := make([]string, 0, len(b.ExtraArgs)+1)
		newArgs = append(newArgs, b.ExtraArgs...)
		newArgs = append(newArgs, "--no-mmap")
		b.ExtraArgs = newArgs
		m.logger.Printf("auto-injected --no-mmap for %s: model is %.1f GiB > 80%% of host RAM (%.1f GiB). "+
			"Reading tensors directly into VRAM-bound malloc buffers instead of mmap'ing the file. "+
			"This avoids page-cache thrashing on slow filesystems like NFS.",
			b.label(), modelGiB, ramGiB)
	}
}

// modelTotalSize returns the total on-disk size of a model file. For
// single-file GGUFs it's just os.Stat. For multi-part GGUFs (named like
// <prefix>-00001-of-00003.gguf), it sums the sizes of all parts in the same
// directory — llama.cpp follows part references automatically when given
// part 1, so the actual on-disk footprint of "the model" is the sum across
// all parts. The auto --no-mmap logic uses this so that, e.g., a 100 GB
// model split into 3 parts of 37 GB each correctly triggers the auto-inject
// even though no single part exceeds the 80% RAM threshold.
//
// If the path doesn't match the multi-part pattern, falls back to single
// file. If the multi-part regex matches but globbing fails, returns just
// the single-file size — degrades gracefully.
func modelTotalSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	m := multiPartGGUFRe.FindStringSubmatch(base)
	if m == nil {
		// Single-file GGUF.
		return st.Size(), nil
	}
	prefix := m[1]
	totalParts := m[3]
	// Glob for all sibling parts: <prefix>-?????-of-<totalParts>.gguf
	pattern := filepath.Join(dir, fmt.Sprintf("%s-?????-of-%s.gguf", prefix, totalParts))
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return st.Size(), nil
	}
	var total int64
	for _, p := range matches {
		ps, err := os.Stat(p)
		if err != nil {
			continue // missing/unreadable part — skip but keep going
		}
		total += ps.Size()
	}
	if total == 0 {
		return st.Size(), nil
	}
	return total, nil
}

// readTotalRAMBytes parses /proc/meminfo and returns MemTotal in bytes.
// Returns 0 on any error so callers can skip the auto-detect silently.
func readTotalRAMBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		var kb int64
		// Format is e.g. "MemTotal:       46123456 kB"
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kb); err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// hasNoMmapArg returns true if --no-mmap is present in the args list.
func hasNoMmapArg(args []string) bool {
	for _, a := range args {
		if a == "--no-mmap" {
			return true
		}
	}
	return false
}

// hasExplicitMmapArg returns true if --mmap (the affirmative form) is present
// in the args list. We use this to detect when the user has explicitly opted
// IN to mmap so we can log a warning instead of overriding their choice.
func hasExplicitMmapArg(args []string) bool {
	for _, a := range args {
		if a == "--mmap" {
			return true
		}
	}
	return false
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

	// Use a generous timeout for initial model loading — models can take many
	// minutes to load into VRAM. The regular health timeout (a few seconds) is
	// too aggressive and causes cascading concurrent loads when backends time
	// out and the next starts. Tensor-split mode with big models on slow PCIe
	// (e.g. mining-rig x1 risers) is the worst case: a 100 GB Q3_K_M MoE
	// streamed via NFS through page cache and then uploaded to 9 GPUs over
	// PCIe gen1 x1 took >10 min in measurement, so the previous 10 min limit
	// triggered respawn loops where each cycle re-loaded the model and timed
	// out again. 30 min covers the realistic worst case on this hardware.
	startupTimeout := 30 * time.Minute
	if m.cfg.GPUs.TensorSplit.Enabled {
		// Tensor-split additionally pays the cost of partitioning weights across
		// N devices and synchronizing the multi-GPU init path. Give it more.
		startupTimeout = 45 * time.Minute
	}

	// Start first backend and wait for it so the node can serve requests immediately.
	b0 := m.Backends[0]
	m.logger.Printf("starting backend %s on port %d", b0.label(), b0.Port)
	m.activity.Emit("backend", b0.GPUID, "loading model into VRAM (first — blocking until ready)")
	if err := b0.Start(); err != nil {
		m.activity.Emit("backend", b0.GPUID, "failed to start: %v", err)
		return fmt.Errorf("backend %s: %w", b0.label(), err)
	}
	t0 := time.Now()
	if err := m.waitForHealthy(ctx, b0, startupTimeout); err != nil {
		m.logger.Printf("WARNING: %s failed to become healthy: %v", b0.label(), err)
		m.activity.Emit("backend", b0.GPUID, "failed to become healthy: %v", err)
		b0.State.SetStatus(balancer.StatusUnhealthy)
	} else {
		m.activity.Emit("backend", b0.GPUID, "ready (loaded in %s)", time.Since(t0).Round(time.Second))
	}

	// Start remaining backends one at a time — each must become healthy (or fail)
	// before the next starts, preventing concurrent model loads that thrash I/O and CPU.
	// In tensor-split mode there's only one backend total, so this branch is skipped.
	if len(m.Backends) > 1 {
		remaining := m.Backends[1:]
		m.logger.Printf("starting %d more backend(s) in background...", len(remaining))
		m.activity.Emit("system", -1, "starting %d more backend(s) in background", len(remaining))
		go func() {
			for _, b := range remaining {
				m.logger.Printf("starting backend %s on port %d", b.label(), b.Port)
				m.activity.Emit("backend", b.GPUID, "loading model into VRAM")
				if err := b.Start(); err != nil {
					m.logger.Printf("ERROR: backend %s failed to start: %v", b.label(), err)
					m.activity.Emit("backend", b.GPUID, "failed to start: %v", err)
					continue
				}
				ts := time.Now()
				if err := m.waitForHealthy(ctx, b, startupTimeout); err != nil {
					m.logger.Printf("WARNING: %s failed to become healthy: %v", b.label(), err)
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
		case <-deadline: return fmt.Errorf("timeout waiting for %s after %v", b.label(), timeout)
		case <-ticker.C:
			if b.CheckHealth(ctx) {
				b.State.SetStatus(balancer.StatusHealthy)
				m.logger.Printf("%s is healthy", b.label())
				return nil
			}
			if !b.IsRunning() {
				return fmt.Errorf("%s process died during startup", b.label())
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
			m.logger.Printf("%s recovered", b.label())
			m.activity.Emit("backend", b.GPUID, "recovered")
		}
		return
	}
	m.failureCounts[b.GPUID]++
	b.State.SetStatus(balancer.StatusUnhealthy)
	m.logger.Printf("%s health check failed (%d/%d)", b.label(), m.failureCounts[b.GPUID], m.cfg.Health.MaxFailures)
	if m.failureCounts[b.GPUID] >= m.cfg.Health.MaxFailures {
		m.failureCounts[b.GPUID] = 0
		m.respawnCounts[b.GPUID]++
		if m.respawnCounts[b.GPUID] >= maxRespawnAttempts {
			b.State.SetStatus(balancer.StatusDead)
			m.logger.Printf("ERROR: %s marked DEAD after %d respawn attempts", b.label(), maxRespawnAttempts)
			m.activity.Emit("backend", b.GPUID, "marked DEAD after %d respawn attempts", maxRespawnAttempts)
			return
		}
		m.logger.Printf("respawning %s (attempt %d/%d)", b.label(), m.respawnCounts[b.GPUID], maxRespawnAttempts)
		m.activity.Emit("backend", b.GPUID, "respawning (attempt %d/%d)", m.respawnCounts[b.GPUID], maxRespawnAttempts)
		b.Kill()
		b.Wait()
		if err := b.Start(); err != nil {
			m.logger.Printf("ERROR: failed to respawn %s: %v", b.label(), err)
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
