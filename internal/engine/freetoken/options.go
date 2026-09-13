package freetoken

import (
	"fmt"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
)

// Options is the `freetoken:` block.
//
// Deliberately short, and the source repo says why: FreeToken resolves dtype,
// attention backend, MoE cache size, KV capacity, page size and CUDA-graph
// sizes from the checkpoint and the card, and does it better than a config file
// can. What is generated is the set the NODE decides — where to listen, what to
// call the model on the mesh, and the knobs whose value is published to the
// mesh. Everything else belongs in models[].args.
type Options struct {
	Binary      string  `yaml:"binary"`
	MemoryRatio float64 `yaml:"memory_ratio"`
	MoEBackend  string  `yaml:"moe_backend"`
	// KVReserveTokens is SPIKE FINDING F7: it appears in v2.0's
	// config.FreeTokenOptions and in v2.2 Task 3's command line, but
	// `--kv-reserve-tokens` is emitted nowhere in viiwork-freetoken's Args and
	// the string does not occur anywhere in that repository. Kept because the
	// plan mandates it; unverifiable from the permitted sources.
	KVReserveTokens int `yaml:"kv_reserve_tokens"`
}

func defaultOptions() Options {
	return Options{Binary: "ft", MemoryRatio: 0.90, MoEBackend: "auto"}
}

// ValidateOptions is engine.OptionsValidator.
func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return fmt.Errorf("%s.%s: %w", path, name, err)
	}
	if opts.Binary == "" {
		return fmt.Errorf("%s.%s.binary must not be empty", path, name)
	}
	if opts.MemoryRatio <= 0 || opts.MemoryRatio > 1 {
		return fmt.Errorf("%s.%s.memory_ratio %v must be in (0, 1]", path, name, opts.MemoryRatio)
	}
	if strings.TrimSpace(opts.MoEBackend) == "" {
		return fmt.Errorf("%s.%s.moe_backend must not be empty", path, name)
	}
	if opts.KVReserveTokens < 0 {
		return fmt.Errorf("%s.%s.kv_reserve_tokens must be >= 0", path, name)
	}

	// Decision 7: the unit of deployment is the card. The engine has a
	// --tensor-parallel-size flag but rejects more than one --gpu entry.
	//
	// The rule is expressed as len(GPUs) because `gpus_per_backend` is not a
	// Spec field — C7's Spec carries GPUs []int and nothing else about the
	// split. That works because config.ModelSpec passes BackendGPUs(0): at
	// validation time Spec.GPUs is ONE BACKEND's cards, not the model's whole
	// gpus: list. The distinction is the whole rule. Were it the whole list,
	// this would reject the ordinary `gpus: [0,1,2]` + `gpus_per_backend: 1` —
	// three single-card backends, the exact topology this engine wants.
	//
	// That was spike finding F6, left open because it could not be settled
	// from the documents. It is settled now, and pinned by
	// internal/accept.TestFreeTokenAcceptsOneCardPerBackendAcrossManyCards
	// rather than by this comment.
	if len(s.GPUs) > 1 {
		return fmt.Errorf("%s.gpus_per_backend must be 1 for engine %s: the engine rejects more than one card per process (got a backend with %d)", path, name, len(s.GPUs))
	}
	return nil
}
