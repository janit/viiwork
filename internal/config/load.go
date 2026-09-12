package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ErrV1Config is returned for a viiwork 1.x file. v2 replaced the layout
// wholesale, so guessing at a translation would be worse than stopping.
var ErrV1Config = errors.New("v1 config: see docs/migrating-to-v2.md")

// v1Keys are top-level keys that exist only in the v1 layout, checked in this
// order so the error names the same key every time.
var v1Keys = []string{"server", "model", "gpus", "backend", "balancer", "peers"}

// Parse decodes a v2 config strictly (unknown keys are errors), rejects v1
// files, and fills defaults. It does not validate; see Validate and Load.
func Parse(data []byte) (*Config, error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	for _, k := range v1Keys {
		if _, ok := top[k]; ok {
			return nil, fmt.Errorf("found top-level %q: %w", k, ErrV1Config)
		}
	}

	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	for i := range cfg.Models {
		applyModelDefaults(&cfg.Models[i])
	}
	return &cfg, nil
}

// applyModelDefaults fills per-model defaults and creates the options block
// for the model's own engine. A block for a different engine is left as
// written, so Validate can reject it.
func applyModelDefaults(m *Model) {
	if m.GPUsPerBackend == 0 {
		m.GPUsPerBackend = 1
	}
	if m.Parallel == 0 {
		m.Parallel = 1
	}
	switch m.Engine {
	case EngineLlamaCpp:
		if m.LlamaCpp == nil {
			m.LlamaCpp = &LlamaCppOptions{}
		}
		if m.LlamaCpp.Binary == "" {
			m.LlamaCpp.Binary = "llama-server"
		}
		if m.LlamaCpp.SplitMode == "" {
			m.LlamaCpp.SplitMode = SplitLayer
		}
	case EngineVLLM:
		if m.VLLM == nil {
			m.VLLM = &VLLMOptions{}
		}
		if m.VLLM.Binary == "" {
			m.VLLM.Binary = "vllm"
		}
		if m.VLLM.GPUMemoryUtilization == 0 {
			m.VLLM.GPUMemoryUtilization = 0.90
		}
	case EngineFreeToken:
		if m.FreeToken == nil {
			m.FreeToken = &FreeTokenOptions{}
		}
		if m.FreeToken.Binary == "" {
			m.FreeToken.Binary = "ft"
		}
		if m.FreeToken.MemoryRatio == 0 {
			m.FreeToken.MemoryRatio = 0.90
		}
		if m.FreeToken.MoEBackend == "" {
			m.FreeToken.MoEBackend = "auto"
		}
	}
}
