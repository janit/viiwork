// Package llamacpp is the llama.cpp engine: llama-server backends that are
// ready when /health answers 200 and whose occupancy is read from /slots. The
// command line and host tuning (auto --threads, auto --load-mode none, the malloc
// environment) are lifted from the v1 process manager.
package llamacpp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/hostinfo"
)

func init() { engine.Register(New()) }

var _ engine.Engine = (*Engine)(nil)

// Engine runs llama-server. The function fields fix the host in tests.
type Engine struct {
	client    *http.Client
	nproc     func() int
	totalRAM  func() int64
	modelSize func(path string) (int64, error)
	// help reports what a llama-server binary's --help says about the
	// spellings this engine chooses between. nil in tests means the pinned
	// build.
	help func(binary string) helpFacts
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
		totalRAM:  hostinfo.TotalRAMBytes,
		modelSize: modelTotalSize,
		help:      cachedHelp,
	}
}

func (e *Engine) Name() string { return Name }

// DisplayName is the project's own spelling.
func (e *Engine) DisplayName() string { return "llama.cpp" }

// UsageReporting: llama-server always carries cached_tokens in usage; on
// 2,436 overnight requests prompt_tokens-cached_tokens equalled timings.prompt_n
// (performance-routing spike §1).
func (e *Engine) UsageReporting() engine.UsageReporting {
	return engine.UsageReporting{Unasked: false, CachedTokens: true}
}

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
	o, err := options(s)
	if err != nil {
		return engine.Command{}, fmt.Errorf("llamacpp: model %s: %w", s.Name, err)
	}

	// A viiwork-parrot folder model resolves to a directory. A missing path
	// is left to llama-server, as before; only an existing directory is
	// refused here, because it can never load.
	if fi, err := os.Stat(s.Path); err == nil && fi.IsDir() {
		return engine.Command{}, fmt.Errorf("llamacpp: model %s: %s is a directory; llama.cpp loads a GGUF file, so a folder model needs an engine that takes a directory (vllm, freetoken)", s.Name, s.Path)
	}

	parallel := max(s.Parallel, 1)

	args := []string{
		"--model", s.Path,
		// --alias makes the backend answer to the model's name (C7).
		"--alias", s.Name,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		// v2 context is per slot; llama.cpp divides --ctx-size across slots.
		"--ctx-size", strconv.Itoa(s.Context * parallel),
		"--parallel", strconv.Itoa(parallel),
	}
	if len(s.GPUs) == 0 {
		args = append(args, "--n-gpu-layers", "0")
	} else {
		args = append(args, "--n-gpu-layers", "-1")
	}
	// --slots enables /slots, which Load reads. --log-verbosity 1 keeps
	// llama-server's errors (a model that cannot load says why, in the node's
	// log) and drops its info lines: the per-request ones the once-a-second
	// load poll would flood. --log-disable silenced the errors too.
	// An older build reads --log-verbosity as a threshold where 1 also turns
	// debug on, request and response bodies included: prompts would reach the
	// node's log. Such a build keeps --log-disable.
	facts := pinnedBuild
	if e.help != nil {
		facts = e.help(o.Binary)
	}
	if facts.levelLogs {
		args = append(args, "--slots", "--log-verbosity", "1")
	} else {
		args = append(args, "--slots", "--log-disable")
	}

	if len(s.GPUs) >= 2 {
		mode := o.SplitMode
		if mode == "" {
			mode = SplitLayer
		}
		args = append(args, "--split-mode", mode, "--tensor-split", tensorSplit(o.SplitWeights, len(s.GPUs)))
		if mode == SplitRow {
			args = append(args, "--main-gpu", strconv.Itoa(o.MainGPU))
		}
	}

	if !hasAnyArg(s.Args, "--threads", "-t") {
		threads := o.Threads
		if threads == 0 {
			threads = autoThreads(e.nproc(), max(s.Backends, 1))
		}
		args = append(args, "--threads", strconv.Itoa(threads))
	}

	if !hasAnyArg(s.Args, "--mmap", "--no-mmap", "--load-mode", "-lm") {
		if size, err := e.modelSize(s.Path); err == nil && needsNoMmap(size, e.totalRAM()) {
			if facts.loadMode {
				args = append(args, "--load-mode", "none")
			} else {
				args = append(args, "--no-mmap")
			}
		}
	}

	// Operator args last, so their flag wins. One of them is translated: a
	// --no-mmap written for a build that read it makes a build that only has
	// --load-mode exit at argument parsing, under a config that did not
	// change. The node knows which the binary reads.
	for _, a := range s.Args {
		if a == "--no-mmap" && facts.loadMode && !facts.noMmap {
			args = append(args, "--load-mode", "none")
			continue
		}
		args = append(args, a)
	}
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

// helpFacts is what a llama-server's --help says about the spellings this
// engine has to choose between: whether --log-verbosity takes levels
// (1 = errors only), whether it lists --load-mode, which replaced
// --mmap/--no-mmap, and whether it still lists --no-mmap. b10437 reads both
// spellings, b11371 only the new one.
type helpFacts struct{ levelLogs, loadMode, noMmap bool }

// pinnedBuild is the build viiwork pins (docker/pins.env).
var pinnedBuild = helpFacts{levelLogs: true, loadMode: true}

// unknownBuild is what is assumed of a binary whose --help could not be read.
// The node's own flag is the pinned build's (b11371 exits on --no-mmap, so
// "unknown" read as "old" would kill every large model at load, on every
// respawn), an operator's --no-mmap is left as written (nothing showed the
// binary stopped reading it), and logging is --log-disable, which every build
// reads.
var unknownBuild = helpFacts{loadMode: true, noMmap: true}

// helpRetry is how long a failed probe stands before the binary is asked
// again: long enough that the backends of one model do not each wait out a
// hanging --help in turn, short enough that a repaired binary is seen.
var helpRetry = 30 * time.Second

var (
	helpCache  sync.Map // binary path -> helpFacts
	helpFailed sync.Map // binary path -> time.Time of the last failed probe
	helpLocks  sync.Map // binary path -> *sync.Mutex: one probe at a time per binary
)

// cachedHelp asks a binary's --help once per process. Callers that arrive
// while a probe of the same binary runs wait for it instead of starting
// their own.
func cachedHelp(binary string) helpFacts {
	if v, ok := helpCache.Load(binary); ok {
		return v.(helpFacts)
	}
	mu, _ := helpLocks.LoadOrStore(binary, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if v, ok := helpCache.Load(binary); ok {
		return v.(helpFacts)
	}
	if at, ok := helpFailed.Load(binary); ok && time.Since(at.(time.Time)) < helpRetry {
		return unknownBuild
	}
	out := helpOutput(binary)
	f := helpFacts{levelLogs: levelList(out), loadMode: loadModeFlag(out), noMmap: strings.Contains(out, "--no-mmap")}
	if !f.loadMode && !f.noMmap {
		// Not a llama-server --help: the binary is missing, failed to start
		// (a library it finds only with models[].env) or timed out. That says
		// nothing about the build, so it is not remembered beyond helpRetry.
		helpFailed.Store(binary, time.Now())
		return unknownBuild
	}
	helpFailed.Delete(binary)
	helpCache.Store(binary, f)
	return f
}

// helpLimit is how much of a --help is kept: llama-server's is about 30 KB.
const helpLimit = 1 << 20

// helpOutput runs binary --help and returns what it printed, at most
// helpLimit bytes, or "" when it could not be run.
func helpOutput(binary string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--help")
	cmd.WaitDelay = time.Second
	out := &capWriter{left: helpLimit}
	cmd.Stdout, cmd.Stderr = out, out
	_ = cmd.Run() // some builds exit non-zero after printing; the text is the evidence
	return out.b.String()
}

// capWriter keeps the first left bytes written to it and discards the rest.
type capWriter struct {
	b    strings.Builder
	left int
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := min(len(p), w.left)
	w.b.Write(p[:n])
	w.left -= n
	return len(p), nil
}

// levelList reports whether a llama-server --help lists --log-verbosity's
// levels, as builds do since verbosity became levels ("1: error").
func levelList(help string) bool {
	return strings.Contains(help, "- 1: error")
}

// loadModeFlag reports whether a llama-server --help lists --load-mode.
func loadModeFlag(help string) bool {
	return strings.Contains(help, "--load-mode")
}
