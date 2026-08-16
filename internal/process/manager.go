package process

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	failureCounts map[string]int
	respawnCounts map[string]int
	// respawnDeferredAt tracks the wall-clock time at which a backend first
	// reached the failure threshold but had in-flight requests. While
	// nonzero, respawn is deferred so in-flight requests can drain. Cleared
	// on recover or once grace expires and the respawn proceeds.
	respawnDeferredAt map[string]time.Time
	sampler           PowerSampler
	tracker           CostTracker
	collector         GPUCollector
	activity          *activity.Log
	// now is wall-clock time. Overridable in tests.
	now func() time.Time
	// baseCtx is the lifetime context handed to StartAll. The respawn path
	// needs it to watch a restarted backend back to health from inside
	// handleHealthResult, which has no ctx of its own.
	baseCtx context.Context
}

func NewManager(cfg *config.Config, logWriter io.Writer, sampler PowerSampler, tracker CostTracker, collector GPUCollector, actLog *activity.Log) *Manager {
	if actLog == nil {
		actLog = activity.NewLog() // no-op: no subscribers, events silently buffered
	}
	m := &Manager{
		cfg: cfg, logger: log.New(os.Stdout, "[manager] ", log.LstdFlags),
		failureCounts: make(map[string]int), respawnCounts: make(map[string]int),
		respawnDeferredAt: make(map[string]time.Time),
		sampler:           sampler,
		tracker:           tracker,
		collector:         collector,
		activity:          actLog,
		now:               time.Now,
	}
	devices := cfg.GPUs.ResolvedDevices()
	if cfg.GPUs.TensorSplit.Enabled {
		// Tensor-split mode: partition devices into consecutive groups of
		// TensorSplit.GroupSize; spawn one backend per group on
		// base_port+i. group_size=0 (legacy) means a single group spanning
		// every device. Each backend honors Model.Parallel for in-process
		// concurrent slots (default 1; bump to share idle GPU compute
		// across requests at the cost of per-slot context). Multiple
		// groups give the balancer N tensor-split backends to route
		// across, identical in behavior to N replica backends but with
		// per-request speed scaled by the group's combined compute.
		groupSize := cfg.GPUs.TensorSplit.GroupSize
		if groupSize == 0 {
			groupSize = len(devices)
		}
		nGroups := len(devices) / groupSize
		m.Backends = make([]*Backend, nGroups)
		for i := 0; i < nGroups; i++ {
			group := devices[i*groupSize : (i+1)*groupSize]
			port := cfg.GPUs.BasePort + i
			addr := fmt.Sprintf("localhost:%d", port)
			// State.GPUID is the sentinel -1 meaning "tensor-split backend".
			// Downstream code that displays per-GPU labels branches on this.
			m.Backends[i] = &Backend{
				GPUID:           -1,
				GPUIDs:          append([]int(nil), group...),
				TensorSplit:     true,
				SplitMode:       cfg.GPUs.TensorSplit.Mode,
				SplitWeights:    cfg.GPUs.TensorSplit.Weights,
				MainGPU:         cfg.GPUs.TensorSplit.MainGPU,
				ModelPath:       cfg.Model.Path,
				Port:            port,
				ContextSize:     cfg.Model.ContextSize,
				NGPULayers:      cfg.Model.NGPULayers,
				Parallel:        cfg.Model.Parallel,
				Binary:          cfg.Backend.Binary,
				ExtraArgs:       cfg.Backend.ExtraArgs,
				Threads:         cfg.Backend.Threads,
				HealthTimeout:   cfg.Health.Timeout.Duration,
				PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
				State:           balancer.NewBackendState(-1, addr),
				LogWriter:       logWriter,
			}
			m.Backends[i].State.GPUIDs = append([]int(nil), group...)
		}
	} else {
		m.Backends = make([]*Backend, len(devices))
		for i, gpuID := range devices {
			port := cfg.GPUs.BasePort + i
			addr := fmt.Sprintf("localhost:%d", port)
			m.Backends[i] = &Backend{
				GPUID: gpuID, ModelPath: cfg.Model.Path, Port: port,
				ContextSize: cfg.Model.ContextSize, NGPULayers: cfg.Model.NGPULayers, Parallel: cfg.Model.Parallel,
				Binary: cfg.Backend.Binary, ExtraArgs: cfg.Backend.ExtraArgs,
				Threads:         cfg.Backend.Threads,
				HealthTimeout:   cfg.Health.Timeout.Duration,
				PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
				State: balancer.NewBackendState(gpuID, addr), LogWriter: logWriter,
			}
		}
	}
	m.maybeAutoNoMmap()
	m.applyDefaultThreads(runtime.NumCPU())
	return m
}

// applyDefaultThreads auto-derives a per-backend --threads value when the
// user hasn't set one, so N backends don't all default to llama-server's
// own nproc/2 default and oversubscribe the host. The chosen value is
// max(1, nproc / n_backends) — a fair-share split of the available
// logical CPUs. If --threads is already in extra_args, that wins and we
// don't touch the backend. A startup warning fires if the configured
// total still exceeds nproc.
//
// Field report on 4-core/8-SMT EPYC 3151 with 10 backends saw the default
// produce ~9 threads/backend × 10 backends = 90 threads on 8 SMT cores;
// backends thrashed and crashed under real long-prompt load.
func (m *Manager) applyDefaultThreads(nproc int) {
	if nproc < 1 || len(m.Backends) == 0 {
		return
	}
	derived := nproc / len(m.Backends)
	if derived < 1 {
		derived = 1
	}
	totalConfigured := 0
	autoCount := 0
	for _, b := range m.Backends {
		if hasThreadsArg(b.ExtraArgs) {
			totalConfigured += parseThreadsArg(b.ExtraArgs)
			continue
		}
		if b.Threads == 0 {
			b.Threads = derived
			autoCount++
		}
		totalConfigured += b.Threads
	}
	if autoCount > 0 {
		m.logger.Printf("auto-set --threads=%d on %d backend(s) (nproc=%d, n_backends=%d). "+
			"Override with backend.threads in viiwork.yaml or --threads in backend.extra_args.",
			derived, autoCount, nproc, len(m.Backends))
	}
	if totalConfigured > nproc {
		m.logger.Printf("WARNING: total backend threads = %d exceeds nproc = %d "+
			"(n_backends=%d). Backends will contend for CPU during prompt-eval, "+
			"which can starve /health responses and trigger respawn cascades. "+
			"Lower backend.threads or run fewer backends.",
			totalConfigured, nproc, len(m.Backends))
	}
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
	m.mu.Lock()
	m.baseCtx = ctx
	m.mu.Unlock()

	// Use a generous timeout for initial model loading — models can take many
	// minutes to load into VRAM. The regular health timeout (a few seconds) is
	// too aggressive and causes cascading concurrent loads when backends time
	// out and the next starts. Tensor-split mode with big models on slow PCIe
	// (e.g. mining-rig x1 risers) is the worst case: a 100 GB Q3_K_M MoE
	// streamed via NFS through page cache and then uploaded to 9 GPUs over
	// PCIe gen1 x1 took >10 min in measurement, so the previous 10 min limit
	// triggered respawn loops where each cycle re-loaded the model and timed
	// out again. 30 min covers the realistic worst case on this hardware.
	startupTimeout := m.startupTimeout()

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

// startupTimeout is the budget a backend gets to load its model and answer a
// health probe. Shared by the initial start and the respawn path.
func (m *Manager) startupTimeout() time.Duration {
	if m.cfg.GPUs.TensorSplit.Enabled {
		// Tensor-split additionally pays the cost of partitioning weights across
		// N devices and synchronizing the multi-GPU init path. Give it more.
		return 45 * time.Minute
	}
	return 30 * time.Minute
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
	m.handleHealthResult(b, healthy)
}

// handleHealthResult is the pure state-machine portion of checkAndManage,
// split out so the deferral / respawn decision tree can be exercised in
// tests without a live llama-server subprocess.
func (m *Manager) handleHealthResult(b *Backend, healthy bool) {
	// Key failure/respawn counters by label() not GPUID: tensor-split backends
	// all carry GPUID=-1, so keying by GPUID would collide and let any healthy
	// pair zero a dead pair's counter — preventing respawn forever.
	key := b.label()
	m.mu.Lock()
	defer m.mu.Unlock()
	if healthy {
		m.failureCounts[key] = 0
		if !m.respawnDeferredAt[key].IsZero() {
			delete(m.respawnDeferredAt, key)
		}
		// Clear any latched hard-failure flag — a successful probe means the
		// EOF signal was either stale or the backend has already recovered.
		b.State.ClearHardFailure()
		if b.State.Status() != balancer.StatusHealthy {
			b.State.SetStatus(balancer.StatusHealthy)
			m.respawnCounts[key] = 0
			m.logger.Printf("%s recovered", b.label())
			m.activity.Emit("backend", b.GPUID, "recovered")
		}
		return
	}
	// Hard-failure short-circuit: the proxy already saw a kernel-level
	// "process is gone" signal (EOF / connection refused) on the inference
	// path. The health probe just confirmed it. Skip the 3-strike ladder
	// and proceed to respawn on this tick.
	hardFail := b.State.HardFailureSeen()
	if hardFail {
		m.failureCounts[key] = m.cfg.Health.MaxFailures
		b.State.ClearHardFailure()
		m.logger.Printf("%s hard failure confirmed by probe; bypassing failure-count ladder", b.label())
	} else {
		m.failureCounts[key]++
	}
	b.State.SetStatus(balancer.StatusUnhealthy)
	m.logger.Printf("%s health check failed (%d/%d)", b.label(), m.failureCounts[key], m.cfg.Health.MaxFailures)
	if m.failureCounts[key] >= m.cfg.Health.MaxFailures {
		// shed-before-respawn: if in-flight requests are still draining and
		// the grace period hasn't expired, defer the kill so those requests
		// can finish instead of returning 502 to the client. Healthy
		// backends keep absorbing new requests because Pick filters on
		// StatusHealthy and this one is already Unhealthy.
		grace := m.cfg.Health.RespawnGrace.Duration
		if grace > 0 {
			inFlight := b.State.InFlight()
			if inFlight > 0 {
				deferredAt := m.respawnDeferredAt[key]
				if deferredAt.IsZero() {
					deferredAt = m.now()
					m.respawnDeferredAt[key] = deferredAt
					m.logger.Printf("%s respawn deferred: %d request(s) in flight, draining (grace %s)",
						b.label(), inFlight, grace)
					m.activity.Emit("backend", b.GPUID,
						"respawn deferred: %d in-flight, draining (grace %s)", inFlight, grace)
				}
				if m.now().Sub(deferredAt) < grace {
					return // keep waiting; will be re-evaluated next tick
				}
				m.logger.Printf("%s respawn grace expired (%s) with %d still in flight; forcing respawn",
					b.label(), grace, inFlight)
				m.activity.Emit("backend", b.GPUID,
					"respawn grace expired (%s) with %d still in flight", grace, inFlight)
			}
			delete(m.respawnDeferredAt, key)
		}
		m.failureCounts[key] = 0
		m.respawnCounts[key]++
		if m.respawnCounts[key] >= maxRespawnAttempts {
			b.State.SetStatus(balancer.StatusDead)
			m.logger.Printf("ERROR: %s marked DEAD after %d respawn attempts", b.label(), maxRespawnAttempts)
			m.activity.Emit("backend", b.GPUID, "marked DEAD after %d respawn attempts", maxRespawnAttempts)
			return
		}
		m.logger.Printf("respawning %s (attempt %d/%d)", b.label(), m.respawnCounts[key], maxRespawnAttempts)
		m.activity.Emit("backend", b.GPUID, "respawning (attempt %d/%d)", m.respawnCounts[key], maxRespawnAttempts)
		b.Kill()
		b.Wait()
		if err := b.Start(); err != nil {
			m.logger.Printf("ERROR: failed to respawn %s: %v", b.label(), err)
			m.activity.Emit("backend", b.GPUID, "respawn failed: %v", err)
			return
		}
		m.watchRespawn(b)
	}
}

// watchRespawn waits for a just-respawned backend to answer a health probe and
// puts it back in rotation.
//
// Start() leaves the backend in StatusStarting, and RunHealthLoop deliberately
// SKIPS Starting backends so a model that is merely still loading is not
// respawn-cascaded. Nothing else moves a backend out of Starting, so without
// this watcher a respawned backend loads normally, serves fine on its own port,
// and stays permanently invisible to the balancer. Observed on gb6 2026-07-28:
// ts-2,3 was respawned after an OOM kill at 04:55, answered /health and real
// completions on port 9602, and still sat out of rotation 6.8 hours later while
// the node reported 4/5 healthy and never logged another manager event.
//
// This mirrors what StartAll already does for the initial start.
//
// Caller must hold m.mu (handleHealthResult does): baseCtx is read under it.
func (m *Manager) watchRespawn(b *Backend) {
	ctx := m.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := m.startupTimeout()
	go func() {
		if err := m.waitForHealthy(ctx, b, timeout); err != nil {
			m.logger.Printf("WARNING: %s failed to become healthy after respawn: %v", b.label(), err)
			m.activity.Emit("backend", b.GPUID, "failed to become healthy after respawn: %v", err)
			// Unhealthy (not Starting) so RunHealthLoop resumes managing it and
			// the respawn ladder can try again.
			b.State.SetStatus(balancer.StatusUnhealthy)
		}
	}()
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
