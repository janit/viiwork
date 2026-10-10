// Package strata is the Strata engine: one Python server per backend
// (serve/server.py), which starts the C++ engine itself from a JSON config.
// viiwork launches and probes the Python server only.
//
// models[].path is that JSON config, written by the operator or by upstream's
// setup.py, and one file serves every backend of a model: host, port and cards
// arrive as flags. Strata has no flag for the model name, the context or the
// slot count, so Command reads the file and refuses to start a backend whose
// file disagrees with the node (config.go).
//
// Written against docs/adding-an-engine.md; design in
// docs/superpowers/specs/2026-10-04-strata-engine-design.md.
package strata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
)

func init() { engine.Register(New()) }

var (
	_ engine.Engine              = (*Engine)(nil)
	_ engine.OptionsValidator    = (*Engine)(nil)
	_ engine.TokenProgressReader = (*Engine)(nil)
	_ engine.UsageReporter       = (*Engine)(nil)
	_ engine.ReasoningSeparator  = (*Engine)(nil)
	_ engine.PerfKeyer           = (*Engine)(nil)
	_ engine.WarmUpper           = (*Engine)(nil)
)

// Engine holds no per-backend state: one is registered from init and shared
// by every model on the node.
type Engine struct {
	client *http.Client
}

func New() *Engine {
	return &Engine{
		// Strata's server speaks HTTP/1.0 and closes every connection, so
		// there is nothing to pool. No client-wide timeout: every probe and
		// load poll is bounded by its caller's context.
		client: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
	}
}

func (e *Engine) Name() string { return name }

// DefaultStartupTimeout is 30 minutes. Loads on ten Radeon VIIs took 3.5
// minutes, and about 6 on the first start that writes the mapped expert file;
// the rest is room for a slow disk.
func (e *Engine) DefaultStartupTimeout() time.Duration { return 30 * time.Minute }

// UsageReporting: measured on v0.1.39. A stream ends with usage on its final
// chunk whether or not the client asked, and prompt_tokens_details.cached_tokens
// is there with a zero when nothing came from cache.
func (e *Engine) UsageReporting() engine.UsageReporting {
	return engine.UsageReporting{Unasked: true, CachedTokens: true}
}

// SeparatesReasoning: measured on v0.1.39. Reasoning streams as
// delta.reasoning_content with no <think> tags and the answer follows as
// delta.content, in every reply that has one.
func (e *Engine) SeparatesReasoning() bool { return true }

// WarmUp is the block's `warmup`, 30 seconds unless the operator says
// otherwise. Seen on gb3 with v0.1.39 (2026-10-05, three times): a long prompt
// that reached a backend within 30 seconds of /health saying loaded failed at
// its first checkpoint ("saving a checkpoint part failed") and the server lost
// its slot, so the backend was reloaded; a backend whose first long prompt
// came 108 seconds after loaded read it. The wait turned out not to prevent
// it: on gfx906 a checkpoint's copy on the default stream is refused while
// another thread captures its prompt graphs, which is a matter of the first
// long prompt and not of time. docker/strata's gfx906 patch fixes it from
// v0.1.40.1 and upstream has the fix since v0.1.42; an older build without
// that patch still fails that way, with or without this wait. Options that do not validate give no warm-up: Command is what
// refuses them.
func (e *Engine) WarmUp(s engine.Spec) time.Duration {
	opts, err := options("model", s)
	if err != nil {
		return 0
	}
	return opts.Warmup
}

// PerfKey is the content of the Strata JSON: the pack, the KV format and the
// layer split that decide a backend's speed are all in there, and the node's
// entry only names the file. A file that cannot be read adds nothing; Command
// is what refuses it. The key is read at start and on reload and follows the
// file, not the running backend: the file is edited and then the model is
// relaunched (`viiwork down` and `up`), and a reload between the two files the
// old backend's samples under the new key until the relaunch.
func (e *Engine) PerfKey(s engine.Spec) string {
	raw, err := readConfigFile(s.Path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// Command builds the server's command line.
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
	opts, err := options("model", s)
	if err != nil {
		return engine.Command{}, err
	}
	if len(s.GPUs) == 0 {
		return engine.Command{}, errors.New("strata: a backend needs at least one GPU")
	}
	conf, err := readConfig(s.Path)
	if err != nil {
		return engine.Command{}, err
	}
	if err := conf.check(s); err != nil {
		return engine.Command{}, fmt.Errorf("strata: %s: %w", s.Path, err)
	}

	args := []string{
		"-m", "serve.server", "--engine", "strata",
		"--config", s.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.Port),
		"--gpu", gpuList(s),
	}
	return engine.Command{
		Path: opts.Python,
		Args: append(args, s.Args...), // operator args last: argparse takes the last occurrence
		Env: []string{
			"PYTHONPATH=" + opts.Dir,
			// Without it `-m` puts the node's working directory ahead of
			// PYTHONPATH, and a serve/ found there would run instead.
			"PYTHONSAFEPATH=1",
			// The load stages only go to stdout; unbuffered, they reach the
			// node's log as they happen rather than when a buffer fills.
			"PYTHONUNBUFFERED=1",
		},
	}, nil
}

// gpuList is the server's --gpu value, which replaces the "gpu" key of the
// JSON. Unlike the other engines this one has to name cards, because Strata's
// server builds the engine's device variables itself from that list, under
// whatever the node already pinned:
//
//   - Radeon: the node set ROCR_VISIBLE_DEVICES to this backend's cards, and
//     HIP numbers inside that set. So 0..n-1.
//   - Otherwise: the server OVERWRITES CUDA_VISIBLE_DEVICES with the list (and
//     sets CUDA_DEVICE_ORDER=PCI_BUS_ID, the node's own order). So the real
//     indexes; 0..n-1 would put every backend on the node's first cards.
//
// This is Spec.Vendor spelling a flag, not gating anything.
func gpuList(s engine.Spec) string {
	ids := make([]string, len(s.GPUs))
	for i, g := range s.GPUs {
		if s.Vendor == gpu.VendorAMD {
			g = i
		}
		ids[i] = strconv.Itoa(g)
	}
	return strings.Join(ids, ",")
}
