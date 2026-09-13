// Package vllm is the vLLM engine: `vllm serve` backends that are ready when
// /health answers 200 and whose occupancy is read from the Prometheus text at
// /metrics.
//
// Written clean-room from docs/adding-an-engine.md, C7 and Task 2 of the v2.2
// plan, with the engine-specific knowledge (flag names, metric names and their
// aliases, the data-parallel aggregation rule) carried from
// github.com/janit/viiwork-nvidia's internal/vllm.
package vllm

import (
	"net/http"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// name is the engine's name in config and in the registry.
const name = "vllm"

func init() { engine.Register(New()) }

var _ engine.Engine = (*Engine)(nil)
var _ engine.OptionsValidator = (*Engine)(nil)

// Engine runs `vllm serve`. It holds no per-backend state: one Engine is
// registered from init and shared by every model on the node, and everything
// Command, Probe and Load need arrives in their arguments.
type Engine struct {
	client *http.Client
}

// New returns the engine for this host. Its HTTP client has no client-wide
// timeout: every probe and load poll is bounded by its caller's context, which
// is what the node sets from health.timeout and the 1 s load tick.
func New() *Engine {
	return &Engine{
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 30 * time.Second,
		}},
	}
}

func (e *Engine) Name() string { return name }

// DefaultStartupTimeout is 20 minutes (v2.2 plan, Decision 10). vLLM profiles
// the model, allocates the KV cache and captures CUDA graphs before it answers
// anything; a timeout tuned to llama.cpp's load time produces a permanent
// respawn loop that looks like a crash and is really impatience.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 20 * time.Minute }

// Command builds the `vllm serve` command line for one backend.
//
// Nothing here sets a device variable (Decision 11): gpu.PinningEnv strips the
// inherited ones and sets the vendor's for the whole node. vLLM therefore sees
// this backend's cards renumbered from zero, which is why the tensor-parallel
// size is simply how many it was given.
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return engine.Command{}, err
	}

	tp := len(s.GPUs)
	if tp == 0 {
		// vLLM does not implement engine.CPURunner, so config validation
		// already required gpus:. Guard anyway rather than emit
		// --tensor-parallel-size 0, which vLLM would reject with a stack trace
		// the operator has to read.
		return engine.Command{}, errNoGPUs
	}

	args := []string{
		"serve", s.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		// Without this vLLM advertises the model by its path, so a config
		// pointing at /models/foo would publish "/models/foo" to the mesh and
		// no client could ask for it by the name the fleet uses.
		"--served-model-name", s.Name,
		"--tensor-parallel-size", strconv.Itoa(tp),
		"--gpu-memory-utilization", strconv.FormatFloat(opts.GPUMemoryUtilization, 'f', -1, 64),
		// Always passed, from the node's own numbers (Decisions 1 and 2). v1
		// let a zero mean "take the checkpoint's limit"; a backend serving
		// more than the node advertises makes ctx a lie on every dashboard.
		"--max-model-len", strconv.Itoa(s.Context),
		"--max-num-seqs", strconv.Itoa(s.Parallel),
	}

	// Operator args last: vLLM's parser takes the last occurrence of a
	// repeated flag, so this is what lets an operator override a generated one
	// without this package growing a key for it.
	return engine.Command{Path: opts.Binary, Args: append(args, s.Args...)}, nil
}
