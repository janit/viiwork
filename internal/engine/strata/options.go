package strata

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

const name = "strata"

// Options is the `strata:` block. Three keys, because Strata keeps its own
// configuration in a JSON file (models[].path) and everything about the model
// and the engine's tuning already lives there. What is left for viiwork to
// know is how to start the server: which Python, and where the checkout is.
type Options struct {
	// Python runs serve/server.py. An image built from upstream's Dockerfile
	// keeps its packages in a venv, so there it is /opt/strata/.venv/bin/python.
	Python string `yaml:"python"`
	// Dir is the Strata checkout. It becomes PYTHONPATH, which is all
	// `python -m serve.server` needs: the server finds its own files relative
	// to itself and starts the engine in the JSON's "cwd".
	Dir string `yaml:"dir"`
	// Warmup is how long a backend's /health must have said loaded before the
	// node sends it work ("30s"; "0s" turns it off). See WarmUp.
	Warmup time.Duration `yaml:"warmup"`
}

// defaultWarmup and maxWarmup bound the `warmup` key. The ceiling is there so
// that a typo ("30m") cannot keep every backend out of service for a load's
// length each time it starts.
const (
	defaultWarmup = 30 * time.Second
	maxWarmup     = 10 * time.Minute
)

// ValidateOptions is engine.OptionsValidator: the block and the rules about
// the model entry. The Strata JSON itself is checked in Command, at launch:
// config validation also runs in viiwork-accept, where that file may not be
// the one the node will read (accept only checks that the path exists).
func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
	_, err := options(path, s)
	return err
}

// options decodes the block and applies every rule that needs no file. path
// is the model's position ("models[0]"), so each error names its field.
func options(path string, s engine.Spec) (Options, error) {
	opts := Options{Python: "python3", Warmup: defaultWarmup}
	if err := engine.DecodeOptions(s, &opts); err != nil {
		return opts, fmt.Errorf("%s.%s: %w", path, name, err)
	}
	if strings.TrimSpace(opts.Python) == "" {
		return opts, fmt.Errorf("%s.%s.python must not be empty", path, name)
	}
	if !filepath.IsAbs(opts.Dir) {
		return opts, fmt.Errorf("%s.%s.dir is required and must be absolute: the Strata checkout that holds serve/server.py (got %q)", path, name, opts.Dir)
	}
	if opts.Warmup < 0 || opts.Warmup > maxWarmup {
		return opts, fmt.Errorf("%s.%s.warmup must be between 0s and %s (got %s)", path, name, maxWarmup, opts.Warmup)
	}
	if s.Path == "" {
		// source: would hand the path to the fetch step, and this file names
		// the program the server runs. It has to be one the operator wrote.
		return opts, fmt.Errorf("%s.path is required for engine %s: Strata's JSON config (source: is not supported)", path, name)
	}
	if s.Parallel < 1 || s.Parallel > parallelMax {
		return opts, fmt.Errorf("%s.parallel must be between 1 and %d for engine %s: the engine's batch window holds %d requests (got %d)", path, parallelMax, name, parallelMax, s.Parallel)
	}
	for _, flag := range []string{"--config", "--api-key"} {
		if slices.ContainsFunc(s.Args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") }) {
			return opts, fmt.Errorf("%s.args must not carry %s for engine %s: the node passes the config it checked, and probes without a key", path, flag, name)
		}
	}
	return opts, nil
}
