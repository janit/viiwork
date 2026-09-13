package supervisor

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// Phase is the engine-reported load phase ("loading"), "queued" while waiting
// for the load gate, or "".
func (b *Backend) Phase() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.phase
}

func (b *Backend) setPhase(p string) {
	b.mu.Lock()
	b.phase = p
	b.mu.Unlock()
}

// Respawns is the ladder's respawn count, 0 before the first launch.
func (b *Backend) Respawns() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ladder == nil {
		return 0
	}
	return b.ladder.Respawns()
}

// lastLoad is the last successful engine load since the current launch, with
// its time (zero when there is none).
func (b *Backend) lastLoad() (load engine.Load, decoded, remain int64, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.load, b.decoded, b.remain, b.loadAt
}

func (b *Backend) currentProcess() *Process {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.proc
}

// loop is the state of one backend's supervision goroutine.
type loop struct {
	b        *Backend
	ctx      context.Context
	gate     *loadGate
	nodePIDs func() []int
	ticket   *loadTicket // held while loading; nil once released

	quickRetryUsed bool
	loadFailLogged bool

	gpuDecided   bool
	gpuHealthyAt time.Time
	gpuLastErr   error
}

type next int

const (
	toLaunch next = iota
	toStarting
	toRunning
	toExit
)

// run supervises the backend until ctx ends. It holds a load ticket from the
// moment it starts waiting until the backend leaves starting. When ctx ends
// it returns without stopping the process: the owner stops it, so a removed
// model can drain first.
func (b *Backend) run(ctx context.Context, first *loadTicket, gate *loadGate, nodePIDs func() []int) {
	l := &loop{b: b, ctx: ctx, gate: gate, nodePIDs: nodePIDs, ticket: first}
	defer l.releaseTicket()

	b.setPhase("queued")
	if first.wait(ctx) != nil {
		return
	}
	n := toLaunch
	for n != toExit {
		switch n {
		case toLaunch:
			n = l.launch()
		case toStarting:
			n = l.starting()
		case toRunning:
			n = l.running()
		}
	}
}

func (l *loop) releaseTicket() {
	if l.ticket != nil {
		l.ticket.release()
		l.ticket = nil
	}
}

// sleep waits d, returning false if ctx ended first.
func (l *loop) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-l.ctx.Done():
		return false
	}
}

func (l *loop) newLadder() *Ladder {
	b := l.b
	timeout := b.model.StartupTimeout.Duration
	if timeout <= 0 {
		timeout = b.eng.DefaultStartupTimeout()
	}
	return NewLadder(LadderConfig{
		MaxFailures:    b.deps.Health.MaxFailures,
		RespawnGrace:   b.deps.Health.RespawnGrace.Duration,
		MaxRespawns:    b.deps.Health.MaxRespawns,
		StartupTimeout: timeout,
	}, b.deps.Now())
}

func (l *loop) launch() next {
	b := l.b
	err := b.launch()
	switch {
	case errors.Is(err, errCommand):
		b.mu.Lock()
		if b.ladder == nil {
			b.ladder = l.newLadder()
		}
		b.ladder.MarkDead(err.Error())
		b.mu.Unlock()
		b.setPhase("")
		b.emit("%v", err)
		l.releaseTicket()
		<-l.ctx.Done()
		return toExit

	case err != nil:
		// A start that fails outright (a missing binary) is a failed start:
		// observed as not ready and not alive after one probe interval.
		b.logf("start failed: %v", err)
		b.mu.Lock()
		if b.ladder == nil {
			b.ladder = l.newLadder()
		}
		b.mu.Unlock()
		if !l.sleep(b.deps.Timing.StartProbeInterval) {
			return toExit
		}
		return l.act(l.observe(false, false, 0), toLaunch)
	}

	b.mu.Lock()
	if b.ladder == nil {
		b.ladder = l.newLadder()
	} else {
		b.ladder.Restarted(b.deps.Now())
	}
	port := b.port
	b.mu.Unlock()
	l.gpuDecided, l.gpuHealthyAt, l.gpuLastErr = false, time.Time{}, nil
	b.emit("starting on port %d (gpus %v)", port, b.gpus)
	return toStarting
}

func (b *Backend) probe(ctx context.Context) (engine.Probe, error) {
	addr := b.Addr()
	if addr == "" {
		return engine.Probe{}, errors.New("backend has no running process")
	}
	ctx, cancel := context.WithTimeout(ctx, b.deps.Health.Timeout.Duration)
	defer cancel()
	return b.eng.Probe(ctx, addr)
}

func (l *loop) observe(ready, alive bool, inFlight int) Action {
	b := l.b
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ladder.Observe(b.deps.Now(), ready, alive, inFlight)
}

func (l *loop) starting() next {
	b := l.b
	proc := b.currentProcess()
	ticker := time.NewTicker(b.deps.Timing.StartProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return toExit
		case <-ticker.C:
		case <-proc.Done():
		}

		alive := proc.Running()
		ready := false
		if alive {
			if p, err := b.probe(l.ctx); err == nil {
				ready = p.Ready
				if p.Phase != "" && p.Phase != b.Phase() {
					b.setPhase(p.Phase)
					b.emit("%s", p.Phase)
				}
			}
		}

		// Decision 4: a process that exits right after launch, before any
		// probe found it ready, most likely lost its port to another process
		// between listen-and-release and bind. One free retry on a new port.
		if !alive && !l.quickRetryUsed && b.deps.Now().Sub(b.launchTime()) < b.deps.Timing.QuickExitWindow {
			l.quickRetryUsed = true
			b.emit("exited during startup, retrying once on a new port")
			return toLaunch
		}

		act := l.observe(ready, alive, 0)
		if b.State() == StateHealthy {
			b.setPhase("")
			b.emit("ready (loaded in %s)", b.deps.Now().Sub(b.launchTime()).Round(time.Second))
			l.quickRetryUsed = false
			l.releaseTicket()
			l.gpuHealthyAt = b.deps.Now()
			l.checkGPU()
			l.checkGPUBinding()
			return toRunning
		}
		if act != ActionNone {
			return l.act(act, toStarting)
		}
	}
}

func (b *Backend) launchTime() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.launchedAt
}

func (l *loop) running() next {
	b := l.b
	proc := b.currentProcess()
	probeT := time.NewTicker(b.deps.Health.Interval.Duration)
	defer probeT.Stop()
	loadT := time.NewTicker(b.deps.Timing.LoadInterval)
	defer loadT.Stop()
	done := proc.Done()
	for {
		var act Action
		select {
		case <-l.ctx.Done():
			return toExit
		case <-probeT.C:
			act = l.probeTick(proc)
		case <-b.kick:
			act = l.probeTick(proc)
		case <-loadT.C:
			if b.State() == StateHealthy {
				l.loadTick()
			}
			continue
		case <-done:
			done = nil // closed channels stay ready; act on the exit once
			act = l.observe(false, false, b.InFlight())
		}
		if act != ActionNone {
			return l.act(act, toRunning)
		}
	}
}

func (l *loop) probeTick(proc *Process) Action {
	b := l.b
	rss := proc.RSSMB()
	b.mu.Lock()
	b.rss = rss
	b.mu.Unlock()
	l.checkGPU()
	alive := proc.Running()
	ready := false
	if alive {
		if p, err := b.probe(l.ctx); err == nil {
			ready = p.Ready
		}
	}
	return l.observe(ready, alive, b.InFlight())
}

func (l *loop) loadTick() {
	b := l.b
	addr := b.Addr()
	if addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, b.deps.Timing.LoadInterval)
	defer cancel()
	b.mu.Lock()
	spec := b.spec
	b.mu.Unlock()
	var (
		load            engine.Load
		decoded, remain int64
		err             error
	)
	if tpr, ok := b.eng.(engine.TokenProgressReader); ok {
		load, decoded, remain, err = tpr.LoadProgress(ctx, spec, addr)
	} else {
		load, err = b.eng.Load(ctx, spec, addr)
	}
	if err != nil {
		if !l.loadFailLogged && l.ctx.Err() == nil {
			b.logf("load failed: %v", err)
			l.loadFailLogged = true
		}
		return
	}
	l.loadFailLogged = false
	b.mu.Lock()
	b.load, b.decoded, b.remain, b.loadAt = load, decoded, remain, b.deps.Now()
	b.mu.Unlock()
}

// act carries out a ladder action. stay is where the loop continues on
// ActionNone.
func (l *loop) act(a Action, stay next) next {
	b := l.b
	switch a {
	case ActionRespawn:
		b.setPhase("")
		b.mu.Lock()
		attempt, cause := b.ladder.Respawns(), b.ladder.Cause()
		b.mu.Unlock()
		b.emit("respawning (attempt %d/%d): %s", attempt, b.deps.Health.MaxRespawns, cause)
		b.stop(b.deps.Timing.RespawnStopGrace)
		// Decision 5 (user, 2026-09-11): a respawn never waits for the load
		// gate. A ticket still held because the backend failed while starting
		// is kept; otherwise the relaunch holds none.
		return toLaunch
	case ActionMarkDead:
		b.stop(b.deps.Timing.RespawnStopGrace)
		l.releaseTicket()
		b.mu.Lock()
		cause := b.ladder.Cause()
		b.mu.Unlock()
		b.setPhase("")
		b.emit("marked dead after %d respawns: %s", b.deps.Health.MaxRespawns, cause)
		<-l.ctx.Done()
		return toExit
	}
	return stay
}

// checkGPU runs the on-GPU check while it is undecided (spec "GPU vendor",
// Decision 2). A wrong verdict condemns the backend; the next observation in
// the same probe tick acts on it.
func (l *loop) checkGPU() {
	b := l.b
	if l.gpuDecided {
		return
	}
	if b.deps.Vendor == gpu.VendorNone || len(b.gpus) == 0 {
		l.gpuDecided = true
		return
	}
	if b.deps.Now().Sub(l.gpuHealthyAt) > b.deps.Timing.GPUCheckWindow {
		b.logf("on-GPU check unknown: %v", l.gpuLastErr)
		l.gpuDecided = true
		return
	}
	pidGPUs, err := gpu.ProcessGPUs(l.ctx, b.deps.Vendor, b.deps.Run)
	if err != nil {
		l.gpuLastErr = err
		return
	}
	backendPIDs := b.PIDs()
	switch gpu.CheckAssignment(pidGPUs, l.nodePIDs(), backendPIDs, b.gpus) {
	case gpu.VerdictOK:
		b.logf("on assigned GPUs %v", b.gpus)
	case gpu.VerdictUnknown:
		b.logf("on-GPU check unknown: no backend PID of this node appears in the %s output (docker nodes need pid: host)", smiTool(b.deps.Vendor))
	case gpu.VerdictWrong:
		b.mu.Lock()
		b.ladder.Condemn("not on assigned GPU")
		b.mu.Unlock()
		b.emit("not on assigned GPU (want %v, holds %v)", b.gpus, heldGPUs(pidGPUs, backendPIDs))
	}
	l.gpuDecided = true
}

// checkGPUBinding compares the card the engine says it bound against the card
// this node pinned it to. Once per launch, on the transition to healthy, which
// re-checks after a respawn for free.
//
// Worth doing because the failure it catches is otherwise silent: pinning
// happens through CUDA_VISIBLE_DEVICES, whose index CUDA resolves in whatever
// order CUDA_DEVICE_ORDER selects, and getting that wrong produces a backend
// that loads, serves and answers every probe while the GPU panel attributes its
// load, its VRAM and its wattage to a neighbouring card. On a host recording
// energy the misattribution is written to disk.
//
// A mismatch is REPORTED, NOT ACTED ON. The backend is serving correctly and
// taking it out of the mesh would trade a wrong label for a lost GPU. That is
// deliberately unlike the on-GPU PID check above, which condemns, because that
// one detects a backend holding no card of ours at all.
//
// Four cases are silently "unknown" rather than a mismatch: an engine that does
// not implement the capability, one that reports no card (an older build), a
// host whose inventory cannot be read or does not carry UUIDs (every non-NVIDIA
// vendor today), and a multi-card backend, which has no single expected card.
// A check that guessed would cry wolf on every node in the fleet.
func (l *loop) checkGPUBinding() {
	b := l.b
	r, ok := b.eng.(engine.GPUBindingReader)
	if !ok || len(b.gpus) != 1 {
		return
	}
	addr := b.Addr()
	if addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(l.ctx, b.deps.Health.Timeout.Duration)
	defer cancel()
	got, reported, err := r.BoundGPU(ctx, addr)
	if err != nil || !reported || got == "" {
		return
	}
	inv, err := gpu.Inventory(ctx, b.deps.Vendor, b.deps.Run)
	if err != nil || len(inv) == 0 {
		return
	}
	want := ""
	for _, id := range inv {
		if id.Index == b.gpus[0] {
			want = id.UUID
		}
	}
	if want == "" || want == got {
		return
	}
	b.logf("pinned to GPU %d (%s) but the engine bound %s — check models[].gpus and CUDA_DEVICE_ORDER", b.gpus[0], want, got)
	b.emit("is on the wrong GPU: expected %s, engine bound %s", want, got)
}

func heldGPUs(pidGPUs map[int][]int, pids []int) []int {
	held := []int{}
	for _, pid := range pids {
		held = append(held, pidGPUs[pid]...)
	}
	slices.Sort(held)
	return slices.Compact(held)
}

func smiTool(v gpu.Vendor) string {
	if v == gpu.VendorNVIDIA {
		return "nvidia-smi"
	}
	return "rocm-smi"
}
