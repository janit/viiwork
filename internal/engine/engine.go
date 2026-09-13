// Package engine is viiwork's engine extension point: the interface every
// inference engine (llamacpp, and any engine added later) implements, so one
// supervisor runs them all. Implementations live in subpackages and call
// Register from init; nothing outside those subpackages learns an engine's
// name.
//
// docs/adding-an-engine.md is the implementer's guide: the five methods and
// what each must guarantee, the optional capabilities, and what the node does
// so an engine does not. (Its design rationale, contract C7, is kept with the
// project's internal specs and is not part of the published tree.)
//
// This package imports nothing from internal/config: an engine is handed what
// the node decided,
// plus its own configuration block, and config asks the registry what a valid
// engine is rather than the other way round.
package engine

import (
	"context"
	"time"

	"github.com/janit/viiwork/v2/internal/gpu"
	"gopkg.in/yaml.v3"
)

// Spec is one backend of one model: what the node decided, plus this engine's
// own configuration block.
type Spec struct {
	// Name is the model's mesh-wide name. The backend MUST answer to it as its
	// model id: a client asks for the name and reaches whichever node has a
	// free slot, so a backend advertising its file path instead is unreachable
	// through the mesh.
	Name string
	// Path is models[].path — a file, a directory or a hub id. The node does
	// not interpret it.
	Path string
	// GPUs are this backend's cards, in config order, as the host's SMI tool
	// numbers them. Empty means a CPU backend, which only an engine
	// implementing CPURunner is ever asked for.
	GPUs []int
	// Port is the loopback port the node assigned. Bind 127.0.0.1 and nothing
	// else: a backend on a routable interface is an unauthenticated inference
	// server on the tailnet.
	Port int
	// Vendor is the host's GPU vendor, already detected. It is INFORMATIONAL:
	// useful for an engine that spells a flag differently per vendor, and never
	// a gate. Engines are named for the engine, not for a GPU vendor, and one
	// that gains a second accelerator backend should need no change here.
	Vendor gpu.Vendor
	// Context is tokens PER SLOT, always >= 1. Engines translate it; whatever
	// is passed, the backend must actually serve this much per slot, because
	// it is what the node publishes to the mesh.
	Context int
	// Parallel is how many sequences this backend admits, always >= 1, and is
	// what Load.Slots must report.
	Parallel int
	// Backends is how many backends the whole model runs on this node, always
	// >= 1. It is here for host-level tuning that has to divide a shared
	// resource between them — llama.cpp's automatic --threads is a fair share
	// of the CPUs across a model's backends, and getting it from the node is
	// the difference between 10 backends running 10 fair shares and 10
	// backends each taking half the machine.
	Backends int
	// Args are models[].args: the operator's own flags, appended last so that
	// theirs wins — every engine in the fleet takes the last occurrence of a
	// repeated flag.
	Args []string
	// Options is this model's engine block exactly as written, or a zero Node
	// when the operator wrote none. Decode it with DecodeOptions.
	Options yaml.Node
}

// Command is how to start a backend. Env is added on top of the supervisor's
// environment, which already carries the GPU pinning variables.
type Command struct {
	Path string
	Args []string
	Env  []string
}

// Probe is the result of one readiness check.
type Probe struct {
	Ready bool
	// Phase is the engine-reported load phase, "" when the engine reports none.
	Phase string
	// Progress is 0..1, or -1 when unknown. Never 0 to mean unknown.
	Progress float64
	// Reason explains why a previously ready backend is not ready.
	Reason string
}

// Load is a backend's current occupancy, as the node publishes it to the mesh.
//
// Slots and CtxPerSlot are what the backend will ACTUALLY serve, which is not
// always what the node asked for. An engine that reports them — llama.cpp's
// /slots gives both — must return what the engine said, because that is the
// only way a value the engine silently clamped becomes visible instead of
// being echoed back as fact. An engine that reports neither returns
// s.Parallel and s.Context, which is the node's own intent and the best it can
// honestly say.
//
// This is why Load is handed its Spec. Without it an engine that cannot
// observe these has nowhere to get them, and the only workaround — remembering
// the Spec that Command was called with, keyed by address — misreports as soon
// as the node reassigns a port, which it does after a failed bind.
type Load struct {
	Slots      int // concurrent sequences this backend admits
	Busy       int // engine-reported running sequences
	Waiting    int // engine-internal queue; 0 when the engine publishes none
	CtxPerSlot int64
}

// Engine is implemented once per inference server.
type Engine interface {
	Name() string
	// Command must make the backend answer to Spec.Name as its model id.
	Command(Spec) (Command, error)
	// Probe returns an error only for a transport failure (refused, EOF,
	// timeout). An engine that answers and says "not ready" is a successful
	// probe reporting Ready: false.
	Probe(ctx context.Context, addr string) (Probe, error)
	// Load returns an error for "cannot say". Returning a zero to mean unknown
	// is a contract violation: absent is not zero, here as on the wire. The
	// Spec is the one Command was called with for this backend.
	Load(ctx context.Context, s Spec, addr string) (Load, error)
	DefaultStartupTimeout() time.Duration
}
