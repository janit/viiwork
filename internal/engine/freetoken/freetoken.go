// Package freetoken is the FreeToken engine: `ft serve` backends, one per
// card, whose readiness is a FIELD IN A JSON DOCUMENT rather than an HTTP
// status, and whose occupancy is read from /v1/stats.
//
// Written clean-room from docs/adding-an-engine.md, C7 and Task 3 of the v2.2
// plan, with the engine-specific knowledge carried from
// github.com/janit/viiwork-freetoken's internal/freetoken.
package freetoken

import (
	"net/http"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

const name = "freetoken"

func init() { engine.Register(New()) }

var (
	_ engine.Engine           = (*Engine)(nil)
	_ engine.OptionsValidator = (*Engine)(nil)
	_ engine.GPUBindingReader = (*Engine)(nil)
)

// Engine runs `ft serve`. It holds no per-backend state: one Engine is
// registered from init and shared by every model on the node, and everything
// Command, Probe and Load need arrives in their arguments.
type Engine struct {
	client *http.Client
}

func New() *Engine {
	return &Engine{
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 30 * time.Second,
		}},
	}
}

func (e *Engine) Name() string { return name }

// DefaultStartupTimeout is 30 minutes (v2.2 plan, Decision 10). FreeToken loads
// weights, sizes its cache pools and captures CUDA graphs before it serves, and
// on an offload MoE backend it then fills a GPU expert cache from host memory.
// On the models this engine exists to run, that is minutes to tens of minutes.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 30 * time.Minute }

// Command builds the `ft serve` command line.
//
// Which card to take is NOT here (Decision 11). The node's pinning sets
// CUDA_DEVICE_ORDER=PCI_BUS_ID and CUDA_VISIBLE_DEVICES, which is correct on
// both engine generations; --gpu is correct on only the newer one, so an engine
// package that generated it would break the release the fleet actually runs.
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return engine.Command{}, err
	}
	if len(s.GPUs) == 0 {
		return engine.Command{}, errNoGPUs
	}
	if len(s.GPUs) > 1 {
		// The engine rejects more than one card per process. Fail here rather
		// than let it die at load with a Python traceback.
		return engine.Command{}, errTooManyGPUs(len(s.GPUs))
	}

	args := []string{
		"serve",
		"--model", s.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		// Without this FreeToken advertises the model by the basename of
		// --model, so a config pointing at /models/DSV4-Flash-NVFP4 would
		// publish "DSV4-Flash-NVFP4" to the mesh and no client could ask for
		// it by the name the fleet uses.
		"--served-model-name", s.Name,
		"--memory-ratio", strconv.FormatFloat(opts.MemoryRatio, 'f', -1, 64),
		// Decisions 1 and 2: both always passed, from the node's own numbers.
		"--max-running-requests", strconv.Itoa(s.Parallel),
		"--max-seq-len-override", strconv.Itoa(s.Context),
		// Builds after 0.1.2 call this --moe-strategy and keep --moe-backend
		// as a deprecated alias that logs a warning. The old spelling stays
		// because 0.1.2 knows no other.
		"--moe-backend", opts.MoEBackend,
	}
	if opts.KVReserveTokens > 0 {
		// Task 3: the reserve flag only when non-zero. See F7 — this flag is
		// not emitted anywhere in the source repo.
		args = append(args, "--kv-reserve-tokens", strconv.Itoa(opts.KVReserveTokens))
	}

	// Operator args last: argparse takes the last occurrence.
	return engine.Command{Path: opts.Binary, Args: append(args, s.Args...)}, nil
}
