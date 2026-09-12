// Package llamacpp is the llama.cpp engine: llama-server backends that are
// ready when /health answers 200 and whose occupancy is read from /slots. The
// command line and host tuning (auto --threads, auto --no-mmap, the malloc
// environment) are lifted from the v1 process manager.
package llamacpp

import (
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

func init() { engine.Register(New()) }

var _ engine.Engine = (*Engine)(nil)

// Engine runs llama-server. The function fields fix the host in tests.
type Engine struct {
	client    *http.Client
	nproc     func() int
	totalRAM  func() int64
	modelSize func(path string) (int64, error)
}

// New returns the engine for this host. Its HTTP client keeps up to 100 idle
// connections (v1's healthClient) and has no client-wide timeout: every probe
// and load poll is bounded by its caller's context.
func New() *Engine {
	return &Engine{
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConns:    100,
			IdleConnTimeout: 30 * time.Second,
		}},
		nproc:     runtime.NumCPU,
		totalRAM:  readTotalRAMBytes,
		modelSize: modelTotalSize,
	}
}

func (e *Engine) Name() string { return config.EngineLlamaCpp }

// DefaultStartupTimeout is the spec's 10 minutes. Large split models on slow
// risers need more, set per model with startup_timeout.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 10 * time.Minute }

// mallocEnv is v1's heap-fragmentation fix for long-running llama-server
// processes: allocations over 64 KB use mmap and are returned on free, the
// heap is trimmed aggressively, and arenas are capped.
var mallocEnv = []string{
	"MALLOC_MMAP_THRESHOLD_=65536",
	"MALLOC_TRIM_THRESHOLD_=65536",
	"MALLOC_ARENA_MAX=4",
}

// Command builds the llama-server command line for one backend. The model's
// own args come last, so an operator's flag wins: llama.cpp takes the last
// occurrence of a repeated flag.
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
	m := s.Model
	o := m.LlamaCpp
	if o == nil {
		return engine.Command{}, fmt.Errorf("llamacpp: model %s has no llamacpp options (its config was not parsed)", m.Name)
	}
	parallel := max(m.Parallel, 1)

	args := []string{
		"--model", m.Path,
		// --alias makes the backend answer to the model's name (spec C2).
		"--alias", m.Name,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		// v2 context is per slot; llama.cpp divides --ctx-size across slots.
		"--ctx-size", strconv.Itoa(m.Context * parallel),
		"--parallel", strconv.Itoa(parallel),
	}
	if len(s.GPUs) == 0 {
		args = append(args, "--n-gpu-layers", "0")
	} else {
		args = append(args, "--n-gpu-layers", "-1")
	}
	// --slots enables /slots, which Load reads; --log-disable stops
	// per-request logging, which the once-a-second load poll would flood.
	args = append(args, "--slots", "--log-disable")

	if len(s.GPUs) >= 2 {
		mode := o.SplitMode
		if mode == "" {
			mode = config.SplitLayer
		}
		args = append(args, "--split-mode", mode, "--tensor-split", tensorSplit(o.SplitWeights, len(s.GPUs)))
		if mode == config.SplitRow {
			args = append(args, "--main-gpu", strconv.Itoa(o.MainGPU))
		}
	}

	if !hasAnyArg(m.Args, "--threads", "-t") {
		threads := o.Threads
		if threads == 0 {
			threads = autoThreads(e.nproc(), m.Backends())
		}
		args = append(args, "--threads", strconv.Itoa(threads))
	}

	if !hasAnyArg(m.Args, "--mmap", "--no-mmap") {
		if size, err := e.modelSize(m.Path); err == nil && needsNoMmap(size, e.totalRAM()) {
			args = append(args, "--no-mmap")
		}
	}

	args = append(args, m.Args...)
	return engine.Command{
		Path: o.Binary,
		Args: args,
		Env:  slices.Clone(mallocEnv),
	}, nil
}

// tensorSplit is the configured weights with the shortest exact decimal (1.0
// prints as 1), or an even 1,1,... split across n GPUs.
func tensorSplit(weights []float64, n int) string {
	parts := make([]string, 0, max(len(weights), n))
	if len(weights) > 0 {
		for _, w := range weights {
			parts = append(parts, strconv.FormatFloat(w, 'f', -1, 64))
		}
	} else {
		for range n {
			parts = append(parts, "1")
		}
	}
	return strings.Join(parts, ",")
}

func hasAnyArg(args []string, flags ...string) bool {
	for _, a := range args {
		if slices.Contains(flags, a) {
			return true
		}
	}
	return false
}
