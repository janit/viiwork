package vllm

import (
	"fmt"

	"github.com/janit/viiwork/v2/internal/engine"
)

// Options is the `vllm:` block. It is deliberately two keys.
//
// v1's viiwork-nvidia carried max_model_len, max_num_seqs and extra_args here
// too. They are gone: v2 states them once, for every engine, as models[].context
// (per slot), models[].parallel and models[].args, and Decisions 1 and 2 of the
// v2.2 plan make them mandatory rather than "zero means take the checkpoint's
// limit". The rest of vLLM's long tail belongs in models[].args, per
// adding-an-engine.md: a mirrored YAML key per flag cannot express "let the
// engine decide".
type Options struct {
	Binary               string  `yaml:"binary"`
	GPUMemoryUtilization float64 `yaml:"gpu_memory_utilization"`
}

func defaultOptions() Options {
	return Options{Binary: "vllm", GPUMemoryUtilization: 0.90}
}

// ValidateOptions is engine.OptionsValidator. Called during config validation,
// before anything starts, so an operator learns about a bad value from
// `viiwork-accept config` rather than from a backend that will not load.
func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
	opts := defaultOptions()
	if err := engine.DecodeOptions(s, &opts); err != nil {
		// SPIKE FINDING F1: the house format wants "models[0].vllm: ..." and
		// DecodeOptions cannot produce the prefix, because Spec carries
		// neither the model index nor the engine name. Here — and ONLY here,
		// because this is the one method given a path — the engine can finish
		// the sentence. Command cannot; it has no path at all.
		return fmt.Errorf("%s.%s: %w", path, name, err)
	}
	if opts.Binary == "" {
		return fmt.Errorf("%s.%s.binary must not be empty", path, name)
	}
	if opts.GPUMemoryUtilization <= 0 || opts.GPUMemoryUtilization > 1 {
		return fmt.Errorf("%s.%s.gpu_memory_utilization %v must be in (0, 1]", path, name, opts.GPUMemoryUtilization)
	}
	return nil
}
