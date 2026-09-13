package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

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
		if err := checkEngineBlock(i, cfg.Models[i]); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

// checkEngineBlock keeps Parse strict about model keys. Model.Options collects
// everything the struct does not define, so without this a typo would be
// silently carried instead of rejected. A model may carry one block, named for
// its own engine; anything else is an unknown key.
//
// This is deliberately not a registry lookup. Whether a name belongs to a real
// engine is Validate's question, answered by whoever registered it; whether a
// key belongs on this model is a question about this file alone, and answering
// it here keeps parsing self-contained and independent of which engines a given
// binary happens to link in.
func checkEngineBlock(i int, m Model) error {
	keys := make([]string, 0, len(m.Options))
	for key := range m.Options {
		keys = append(keys, key)
	}
	sort.Strings(keys) // a deterministic message when more than one is wrong
	for _, key := range keys {
		if key == m.Engine {
			continue
		}
		if m.Engine == "" {
			return fmt.Errorf("models[%d]: unknown key %q (models[%d].engine is not set, so no block is allowed)", i, key, i)
		}
		return fmt.Errorf("models[%d]: unknown key %q (a model may only carry a block named for its own engine, %q)", i, key, m.Engine)
	}
	return nil
}

// applyModelDefaults fills the per-model defaults that are the node's to
// decide. An engine's own defaults are the engine's: it sets them on its
// options struct and decodes the operator's block over them, so that this
// package never holds a value only one engine understands.
func applyModelDefaults(m *Model) {
	if m.GPUsPerBackend == 0 {
		m.GPUsPerBackend = 1
	}
	if m.Parallel == 0 {
		m.Parallel = 1
	}
}
