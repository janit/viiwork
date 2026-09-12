package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/meshapi"
)

// errCommand wraps an engine.Command failure, which is a configuration bug no
// respawn fixes, so the loop can tell it apart from a failed start.
var errCommand = errors.New("cannot build command")

// Backend is one backend process of one model.
type Backend struct {
	model config.Model
	index int
	gpus  []int
	id    string
	eng   engine.Engine
	deps  Deps
	out   io.Writer
	kick  chan struct{}

	inFlight atomic.Int64

	mu         sync.Mutex
	proc       *Process
	port       int
	launchedAt time.Time
	ladder     *Ladder
	phase      string
	rss        int64
	load       engine.Load
	loadAt     time.Time // zero when no load has succeeded since the launch
	decoded    int64
	remain     int64

	powerLimitLogged bool
}

func newBackend(m config.Model, index int, eng engine.Engine, deps Deps) *Backend {
	deps = deps.withDefaults()
	id := meshapi.BackendID(m.Name, index)
	return &Backend{
		model: m,
		index: index,
		gpus:  m.BackendGPUs(index),
		id:    id,
		eng:   eng,
		deps:  deps,
		out:   logging.NewPrefixWriter(deps.Log, "["+id+"] "),
		kick:  make(chan struct{}, 1),
	}
}

func (b *Backend) ID() string  { return b.id }
func (b *Backend) Index() int  { return b.index }
func (b *Backend) GPUs() []int { return slices.Clone(b.gpus) }

// State is StateStarting until the first launch creates the ladder.
func (b *Backend) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ladder == nil {
		return StateStarting
	}
	return b.ladder.State()
}

// Addr is the backend's loopback address while its process runs, else "".
func (b *Backend) Addr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc == nil || !b.proc.Running() {
		return ""
	}
	return "127.0.0.1:" + strconv.Itoa(b.port)
}

// Acquire counts one request in flight on this backend.
func (b *Backend) Acquire() { b.inFlight.Add(1) }

// Release ends one in-flight request. An unpaired release is a router bug, so
// it panics rather than letting the count drift negative and invent capacity.
func (b *Backend) Release() {
	if b.inFlight.Add(-1) < 0 {
		b.inFlight.Add(1)
		panic("supervisor: Release without a matching Acquire on " + b.id)
	}
}

func (b *Backend) InFlight() int { return int(b.inFlight.Load()) }

// Slots is the engine's slot count from a fresh load, else the model's
// parallel. It is independent of health; the router checks health itself.
func (b *Backend) Slots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loadAt.IsZero() && b.deps.Now().Sub(b.loadAt) < b.deps.Timing.LoadStaleAfter {
		return b.load.Slots
	}
	return max(b.model.Parallel, 1)
}

// NoteHardFailure reports a transport failure on the inference path (EOF,
// refused). It latches on the ladder and kicks the loop to probe at once,
// which is what makes a crashed backend respawn immediately rather than on
// its next health tick.
func (b *Backend) NoteHardFailure() {
	b.mu.Lock()
	if b.ladder != nil {
		b.ladder.NoteHardFailure()
	}
	b.mu.Unlock()
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// PIDs is the process tree, nil when no process runs.
func (b *Backend) PIDs() []int {
	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.TreePIDs()
}

// gpuID is the GPU events are attributed to: the first, or -1 on CPU.
func (b *Backend) gpuID() int {
	if len(b.gpus) == 0 {
		return -1
	}
	return b.gpus[0]
}

// logf writes a supervisor line about this backend through its prefixed writer.
func (b *Backend) logf(format string, args ...any) {
	fmt.Fprintf(b.out, format+"\n", args...)
}

// emit sends an activity event whose message starts with the backend id.
func (b *Backend) emit(format string, args ...any) {
	b.deps.Events.Emit("backend", b.gpuID(), "%s: "+format, append([]any{b.id}, args...)...)
}

// launch starts a new process on a fresh loopback port.
func (b *Backend) launch() error {
	port, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("%s: taking a loopback port: %w", b.id, err)
	}
	b.applyPowerLimit()
	cmd, err := b.eng.Command(engine.Spec{Model: b.model, GPUs: slices.Clone(b.gpus), Port: port, Vendor: b.deps.Vendor})
	if err != nil {
		return fmt.Errorf("%w: %w", errCommand, err)
	}
	env := backendEnv(b.deps.Environ(), b.deps.Vendor, b.gpus, cmd.Env, b.model.Env)
	proc, err := StartProcess(cmd.Path, cmd.Args, env, b.out)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.proc, b.port, b.launchedAt = proc, port, b.deps.Now()
	b.load, b.loadAt, b.decoded, b.remain, b.rss = engine.Load{}, time.Time{}, 0, 0, 0
	b.mu.Unlock()
	return nil
}

func (b *Backend) applyPowerLimit() {
	if b.deps.PowerLimitWatts <= 0 || len(b.gpus) == 0 {
		return
	}
	for _, g := range b.gpus {
		err := gpu.SetPowerLimit(context.Background(), b.deps.Vendor, b.deps.Run, g, b.deps.PowerLimitWatts)
		switch {
		case err == nil:
		case errors.Is(err, gpu.ErrPowerLimitUnsupported):
			b.mu.Lock()
			logged := b.powerLimitLogged
			b.powerLimitLogged = true
			b.mu.Unlock()
			if !logged {
				b.logf("gpu.power_limit_watts is not applied on %s; set it in host provisioning", b.deps.Vendor)
			}
			return
		default:
			b.logf("WARNING: %v", err)
		}
	}
}

// stop stops the process, if any. The ladder and counters are untouched.
func (b *Backend) stop(grace time.Duration) {
	b.mu.Lock()
	p := b.proc
	b.mu.Unlock()
	if p == nil {
		return
	}
	p.Stop(grace)
	b.mu.Lock()
	if b.proc == p {
		b.proc, b.port = nil, 0
	}
	b.mu.Unlock()
}

// backendEnv is the pinning environment, then the engine's env in order, then
// the model's env as K=V sorted by key. Later entries win: os/exec keeps the
// last value of a duplicated key.
func backendEnv(environ []string, vendor gpu.Vendor, gpus []int, engineEnv []string, modelEnv map[string]string) []string {
	env := gpu.PinningEnv(environ, vendor, gpus)
	env = append(env, engineEnv...)
	keys := make([]string, 0, len(modelEnv))
	for k := range modelEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+modelEnv[k])
	}
	return env
}

// freeLoopbackPort takes a port by listening on 127.0.0.1:0 and releasing it.
// Another process can grab it in between, which the loop's quick-exit retry
// covers (Decision 4).
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return port, ln.Close()
}
