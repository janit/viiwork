package config

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// This package must know nothing about which engines exist, so its tests
// register their own rather than importing real ones. The three names match
// testdata/full.yaml, which is kept as a v2.0.0-era config file: that it still
// parses and validates byte for byte is the evidence the YAML surface did not
// move when engines took ownership of their blocks.
//
// None of them implements engine.OptionsValidator, so a block is captured and
// carried but not inspected — which is what an engine with no rules looks like.
// pickyEngine exists to prove an engine's own error reaches the operator.

type fakeEngine struct {
	name string
	cpu  bool
}

func (f fakeEngine) Name() string { return f.name }
func (f fakeEngine) Command(s engine.Spec) (engine.Command, error) {
	return engine.Command{Path: "/bin/true", Args: []string{"--alias", s.Name}}, nil
}
func (f fakeEngine) Probe(context.Context, string) (engine.Probe, error) {
	return engine.Probe{Ready: true, Progress: -1}, nil
}
func (f fakeEngine) Load(context.Context, engine.Spec, string) (engine.Load, error) {
	return engine.Load{Slots: 1}, nil
}
func (f fakeEngine) DefaultStartupTimeout() time.Duration { return time.Minute }
func (f fakeEngine) RunsOnCPU() bool                      { return f.cpu }

// pickyEngine rejects one value, so that a test can show an engine's own
// validation reaching the operator with the field named.
type pickyEngine struct{ fakeEngine }

func (pickyEngine) ValidateOptions(path string, s engine.Spec) error {
	var o struct {
		Knob float64 `yaml:"knob"`
	}
	if err := engine.DecodeOptions(s, &o); err != nil {
		return fmt.Errorf("%s.picky: %w", path, err)
	}
	if o.Knob > 1 {
		return fmt.Errorf("%s.picky.knob %v must be <= 1", path, o.Knob)
	}
	return nil
}

var errNotRegistered = errors.New("not registered")

func init() {
	engine.Register(fakeEngine{name: "llamacpp", cpu: true})
	engine.Register(fakeEngine{name: "vllm"})
	engine.Register(fakeEngine{name: "freetoken"})
	engine.Register(pickyEngine{fakeEngine{name: "picky"}})
}

// Names used by the tests in place of the constants this package used to hold.
const (
	testEngineCPU  = "llamacpp"
	testEngineGPU  = "vllm"
	testEngineMoE  = "freetoken"
	testEnginePick = "picky"
)

var _ = errNotRegistered
