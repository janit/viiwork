// Package engine is spec contract C2: the interface every inference engine
// (llamacpp, vllm, freetoken) implements, so one supervisor runs them all.
// Implementations live in subpackages and call Register from init.
package engine

import (
	"context"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// Spec is one backend of one model: the models[] entry plus the slice of
// GPUs this backend owns and the loopback port the supervisor assigned.
type Spec struct {
	Model  config.Model
	GPUs   []int
	Port   int
	Vendor gpu.Vendor
}

// Command is how to start a backend. Env is added on top of the
// supervisor's environment, which already carries the GPU pinning variables.
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
	// Progress is 0..1, or -1 when unknown.
	Progress float64
	// Reason explains why a previously ready backend is not ready.
	Reason string
}

// Load is a backend's current occupancy.
type Load struct {
	Slots      int // concurrent sequences this backend admits
	Busy       int // engine-reported running sequences
	Waiting    int // engine-internal queue
	CtxPerSlot int64
}

// Engine is implemented once per inference server. Name must equal one of
// config.EngineLlamaCpp, config.EngineVLLM or config.EngineFreeToken.
type Engine interface {
	Name() string
	// Command must make the backend answer to Spec.Model.Name as its model id.
	Command(Spec) (Command, error)
	// Probe returns an error only for a transport failure (refused, EOF, timeout).
	Probe(ctx context.Context, addr string) (Probe, error)
	Load(ctx context.Context, addr string) (Load, error)
	DefaultStartupTimeout() time.Duration
}
