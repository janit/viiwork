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
	_ engine.UsageReporter    = (*Engine)(nil)
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

// DisplayName is the project's own spelling.
func (e *Engine) DisplayName() string { return "FreeToken" }

// DefaultStartupTimeout is 30 minutes (v2.2 plan, Decision 10). FreeToken loads
// weights, sizes its cache pools and captures CUDA graphs before it serves, and
// on an offload MoE backend it then fills a GPU expert cache from host memory.
// On the models this engine exists to run, that is minutes to tens of minutes.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 30 * time.Minute }

// UsageReporting: measured on 0.2.1. A stream carries usage only when the client
// sets include_usage, and with --enable-cache-report, which Command always
// passes, prompt_tokens_details.cached_tokens appears whenever any prompt token
// came from cache, so its absence means zero (server/openai_api.py, _usage).
func (e *Engine) UsageReporting() engine.UsageReporting {
	return engine.UsageReporting{Unasked: false, CachedTokens: true}
}

// Command builds the `ft serve` command line.
//
// Which card to take is NOT here (Decision 11). The node's pinning sets
// CUDA_DEVICE_ORDER=PCI_BUS_ID and CUDA_VISIBLE_DEVICES before the child
// starts. 0.1.3 added --gpu, taking an nvidia-smi index or a UUID, and that is
// a reason to keep generating nothing: an index means whatever ordering the
// process was given, and the node has already narrowed that to one card. Two
// layers pinning the same backend is how a backend ends up on a neighbour.
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
		// 0.1.3 renamed this from --moe-backend, which survives as a
		// deprecated alias that warns on every start. The operator key is
		// still moe_backend, because C1 freezes the YAML an operator writes;
		// only the generated flag moved.
		"--moe-strategy", opts.MoEBackend,
		// Off by default upstream, where every cached count then reads 0 and
		// performance routing could never price a FreeToken host. Present
		// since at least 0.1.3, this package's floor.
		"--enable-cache-report",
	}
	if opts.KVReserveTokens > 0 {
		// Only when non-zero, and zero is not "no reserve": the engine's own
		// default is 8192 tokens, so omitting the flag leaves that floor in
		// place rather than removing it. Spike finding F7 — which recorded
		// this flag as unverifiable — is closed: 0.1.3 documents it.
		args = append(args, "--kv-reserve-tokens", strconv.Itoa(opts.KVReserveTokens))
	}

	// Operator args last: argparse takes the last occurrence.
	//
	// Since main after 0.1.3 the server reads FREETOKEN_API_KEY when --api-key
	// is absent, and a key keys every route but /health: one inherited from
	// the node's environment would turn the node's probes and forwards into
	// 401s. Empty is "off" to the engine (resolve_api_key).
	return engine.Command{Path: opts.Binary, Args: append(args, s.Args...), Env: []string{"FREETOKEN_API_KEY="}}, nil
}
