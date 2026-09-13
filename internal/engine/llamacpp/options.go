package llamacpp

import (
	"fmt"

	"github.com/janit/viiwork/v2/internal/engine"
)

// Name is how this engine is named in models[].engine and how its
// configuration block is named. An engine owns its name; nothing outside this
// package spells it.
const Name = "llamacpp"

// Split modes llama.cpp accepts for a multi-GPU backend.
const (
	SplitLayer = "layer"
	SplitRow   = "row"
)

// Options is the llamacpp: block of a models[] entry.
type Options struct {
	Binary       string    `yaml:"binary"`
	SplitMode    string    `yaml:"split_mode"`
	SplitWeights []float64 `yaml:"split_weights"`
	MainGPU      int       `yaml:"main_gpu"`
	Threads      int       `yaml:"threads"`
}

// defaults are what an operator gets when they write no block at all, or leave
// a key out of one. They live here rather than in the config package because
// only this engine knows what "llama-server" and "layer" mean.
func defaults() Options {
	return Options{Binary: "llama-server", SplitMode: SplitLayer}
}

// options decodes a spec's block over the defaults.
func options(s engine.Spec) (Options, error) {
	o := defaults()
	if err := engine.DecodeOptions(s, &o); err != nil {
		return Options{}, err
	}
	return o, nil
}

// RunsOnCPU declares llama.cpp the one engine that serves without a GPU, which
// is why a models[] entry may omit gpus: only for this engine.
func (e *Engine) RunsOnCPU() bool { return true }

// ValidateOptions enforces the rules that were in internal/config until
// v2.1.0. Every message names the field, and the field is named as the
// operator wrote it — gpus_per_backend rather than len(Spec.GPUs), which is
// the same number seen from the other side.
func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
	o, err := options(s)
	if err != nil {
		return fmt.Errorf("%s.%s: %w", path, Name, err)
	}
	if o.SplitMode != SplitLayer && o.SplitMode != SplitRow {
		return fmt.Errorf("%s.llamacpp.split_mode %q must be layer or row", path, o.SplitMode)
	}
	perBackend := len(s.GPUs)
	if len(o.SplitWeights) > 0 {
		if perBackend < 2 {
			return fmt.Errorf("%s.llamacpp.split_weights needs gpus_per_backend >= 2", path)
		}
		if len(o.SplitWeights) != perBackend {
			return fmt.Errorf("%s.llamacpp.split_weights has %d entries, gpus_per_backend is %d", path, len(o.SplitWeights), perBackend)
		}
	}
	// A CPU backend has no cards but still has a main_gpu of 0, so the range
	// is at least one wide.
	if n := max(perBackend, 1); o.MainGPU < 0 || o.MainGPU >= n {
		return fmt.Errorf("%s.llamacpp.main_gpu %d must be 0..%d", path, o.MainGPU, n-1)
	}
	if o.Threads < 0 {
		return fmt.Errorf("%s.llamacpp.threads must be >= 0 (0 = auto)", path)
	}
	return nil
}
