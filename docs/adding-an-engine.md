# Adding an Engine to viiwork

viiwork supervises inference engines. This is how you add one.

An engine package is **a command line, a readiness rule, an occupancy reading,
and the options that feed them.** Everything else — spawning, GPU pinning,
health ladders, respawns, routing, the mesh — belongs to the node and is already
written. If you find yourself implementing any of it, stop and read
"What the node already does" below.

> **Status.** This documents the engine API **as of v2.1.0**, and as of
> 2026-09-13 it is the API that exists: every declaration below was re-verified
> against the code on the `v2.1` branch after it was built. If you are working
> from a checkout that predates it, read "Writing against this before it ships"
> at the end.
>
> This document is the contract you write against. The design rationale behind
> each rule — why it is this way, and what went wrong when it was not — is kept
> with the project's internal specs (C7) and is not part of the published tree.
> Where the two disagree, C7 wins and this file is the bug.

## The shape of the job

```
internal/engine/<name>/
    <name>.go        Engine type, Name, Command, DefaultStartupTimeout, init registration
    health.go        Probe, Load, and any optional capability
    options.go       Options struct, defaults, ValidateOptions
    <name>_test.go   enginetest.Run plus your own tests
    testdata/        real engine output, captured verbatim
```

Plus one line in each of two files:

```go
// internal/node/node.go and cmd/viiwork-accept/main.go
_ "github.com/janit/viiwork/v2/internal/engine/<name>" // registers the <name> engine
```

`viiwork-accept` needs it because it validates a config file before the node
that will run it starts; an engine it cannot see is an engine it reports as
unknown.

**That is the entire footprint.** Nothing in `internal/config`,
`internal/supervisor`, `internal/node`, `internal/route` or `internal/proxy`
learns your engine's name. If your engine cannot be added without touching one
of them, that is a finding about the contract — say so rather than working
around it.

## The API

Everything you compile against. Copy these declarations into a stub if you are
working before v2.1.0 lands.

### `internal/engine`

```go
package engine

import (
    "context"
    "time"

    "github.com/janit/viiwork/v2/internal/gpu"
    "gopkg.in/yaml.v3"
)

// Spec is one backend of one model: what the node decided, plus this engine's
// own configuration block.
type Spec struct {
    Name     string      // the model's mesh-wide name; the backend MUST answer to it
    Path     string      // models[].path, uninterpreted by the node
    GPUs     []int       // this backend's cards, in config order; empty = CPU
    Port     int         // the loopback port the node assigned
    Vendor   gpu.Vendor  // informational, never a gate
    Context  int         // tokens PER SLOT, always >= 1
    Parallel int         // sequences this backend admits, always >= 1
    Backends int         // backends this model runs on this node, always >= 1
    Args     []string    // models[].args, the operator's own flags
    Options  yaml.Node   // this model's engine block, or a zero Node
}

// Command is how to start a backend. Env is added on top of the supervisor's
// environment, which already carries the GPU pinning variables.
type Command struct {
    Path string
    Args []string
    Env  []string
}

// Probe is the result of one readiness check.
type Probe struct {
    Ready    bool
    Phase    string  // engine-reported load phase, "" when it reports none
    Progress float64 // 0..1, or -1 when unknown. Never 0 to mean unknown.
    Reason   string  // why a backend is not ready, for the activity log
}

// Load is a backend's current occupancy.
type Load struct {
    Slots      int   // what the backend will actually admit
    Busy       int   // engine-reported running sequences
    Waiting    int   // engine-internal queue; 0 when the engine publishes none
    CtxPerSlot int64 // what the backend will actually serve, per slot
}

// Engine is implemented once per inference server.
type Engine interface {
    Name() string
    Command(Spec) (Command, error)
    Probe(ctx context.Context, addr string) (Probe, error)
    // s is the Spec Command was called with for this backend.
    Load(ctx context.Context, s Spec, addr string) (Load, error)
    DefaultStartupTimeout() time.Duration
}

// Register makes an engine available by name. Call it from init.
// An empty or duplicate name panics at init rather than failing at first use.
func Register(e Engine)

// Lookup returns the engine registered under name.
func Lookup(name string) (Engine, bool)

// Names returns every registered engine name, sorted. It is what config
// validation reports in "must be one of: ...".
func Names() []string

// DecodeOptions decodes this model's engine block into dst, rejecting any key
// dst does not define. Its errors name the key and the line the operator wrote
// it on, never a Go type:
//
//     unknown key gpu_memory_utilisation (line 12)
//     ratio: cannot unmarshal !!str `high` into float64 (line 14)
//
// Your ValidateOptions adds the path, so what the operator finally sees is
//
//     models[0].vllm: unknown key gpu_memory_utilisation (line 12)
//
// A zero Spec.Options leaves dst untouched, so set your defaults on dst first
// and decode over them.
func DecodeOptions(s Spec, dst any) error
```

### Capability interfaces

Optional, found by type assertion. Implement what your engine can answer
honestly and **nothing else** — a method that returns a plausible-looking zero
is worse than an absent one, because the mesh renders absent as "unknown" and
zero as fact.

```go
// OptionsValidator validates this engine's config block. path is the model's
// position ("models[0]"), so every error can name the field that is wrong.
// Called during config validation, before anything starts.
//
// The Spec it receives is BACKEND 0's, built before any backend exists: Name,
// Path, Context, Parallel, Args, Backends and Options are the model's; GPUs
// are the first backend's cards, so len(GPUs) is gpus_per_backend, which is
// not itself a Spec field; and Port is 0 and Vendor is "" because neither has
// been decided. A rule that reads Port or Vendor validates against nothing.
type OptionsValidator interface {
    ValidateOptions(path string, s Spec) error
}

// CPURunner is implemented by an engine that can serve with no GPUs.
// Not implementing it means `gpus:` is required for your engine.
type CPURunner interface {
    RunsOnCPU() bool
}

// TokenProgressReader is implemented by an engine that can report PER-REQUEST
// token progress. Cumulative counters are NOT this. Called in place of Load on
// every load tick.
type TokenProgressReader interface {
    LoadProgress(ctx context.Context, s Spec, addr string) (load Load, decoded, remain int64, err error)
}

// GPUBindingReader is implemented by an engine that reports which card it
// actually bound. Called once on the transition to healthy. A mismatch against
// the host inventory is REPORTED, never acted on. ok=false means the engine
// said nothing, which is not a mismatch.
type GPUBindingReader interface {
    BoundGPU(ctx context.Context, addr string) (uuid string, ok bool, err error)
}
```

### `internal/gpu`

```go
type Vendor string

const (
    VendorNVIDIA Vendor = "nvidia"
    VendorAMD    Vendor = "amd"
    VendorNone   Vendor = "none"
)
```

`Spec.Vendor` is **informational**. Use it if your engine spells a flag
differently per vendor. Never refuse to run because of it: engines are named for
the engine, not for a GPU vendor, and an engine that gains a second accelerator
backend should need no change here.

### `internal/engine/enginetest`

```go
package enginetest

// Run asserts the contract every engine must honour whatever it drives.
// Call it from your package's test with cases covering the Spec shapes you
// support. It spins up its own httptest servers for the generic Probe and Load
// assertions; you supply no server.
func Run(t *testing.T, e engine.Engine, cases ...Case)

type Case struct {
    Name string      // subtest name
    Spec engine.Spec // a Spec your engine should accept
    // Options is the block's CONTENTS as an operator would write them — the
    // lines under `vllm:`, not the header — dedented to column zero. When
    // non-empty it is parsed into Spec.Options, so a case reads like the
    // config file it stands for.
    Options string
}
```

What it checks: `Name()` is non-empty, lowercase and registered under that
name; `Command` puts `Spec.Name` on the command line, binds `127.0.0.1` and
`Spec.Port`, and appends `Spec.Args` **last**; `Command` does not panic on an
options block it cannot use (returning an error is right, and succeeding is
allowed for an engine that ignores options); `Probe` against a closed port
returns an **error**, and against a server that answered returns **no error**,
whatever it decides about readiness; `Load` reports no negative field, or an
error; `DefaultStartupTimeout` is positive.

> A note on that Probe pair, corrected after writing the kit: the universal rule
> is only that an answer is not a transport failure. Whether a 200 carrying
> nonsense means ready is the engine's own rule — llama.cpp reads a 200 as
> ready, FreeToken reads the document — so the kit does not assert readiness
> either way, only that answering is never reported as an error.

The kit is itself tested against a deliberately broken engine, in a subprocess,
so that "my engine passes" is evidence rather than decoration.

## What the node already does

The most common mistake when porting an engine from a standalone supervisor is
reimplementing this half. None of it belongs in your package.

| The node owns | So you never write |
| --- | --- |
| Spawning, the process group, SIGTERM then SIGKILL after a grace | process management, zombie reaping, "kill the children too" |
| GPU pinning: stripping inherited device variables, then `CUDA_DEVICE_ORDER=PCI_BUS_ID` + `CUDA_VISIBLE_DEVICES`, or `ROCR_VISIBLE_DEVICES` | anything in `Command.Env` about device selection |
| Port assignment, and a free retry if the engine fails to bind | port scanning or collision handling |
| The health ladder: starting, healthy, unhealthy after `health.max_failures`, respawn, dead after `health.max_respawns` | retry logic, backoff, failure counting |
| Serialising loads node-wide so two models do not thrash the disk | load queueing |
| The on-GPU PID check, which condemns a backend on no card of ours | anything that kills a backend |
| Draining in-flight requests before a respawn, a removal or shutdown | graceful shutdown |
| Alias resolution, routing, the origin queue, peer forwarding | anything about where a request goes |
| `/v1/models`, `/v1/capacity`, the dashboards, the mesh | anything on the wire |

### When your methods are called

| | When | Timeout | On error |
| --- | --- | --- | --- |
| `Command` | once per backend launch and per respawn | none | the backend fails to start; the error is logged and counted |
| `Probe` | every 1 s while `starting`; every `health.interval` (default 5 s) once healthy | `health.timeout`, default 10 s, as the context deadline | counts one failure toward `health.max_failures` (default 3) |
| `Load` | every 1 s while healthy | 1 s, as the context deadline | logged once, last good value kept; after 3 s stale the node falls back to `Slots = Parallel`, `Busy =` its own in-flight count |
| `DefaultStartupTimeout` | at launch | — | overridden by `models[].startup_timeout` when non-zero |
| `BoundGPU` | once, on the transition to healthy | short | logged, nothing else |

Two consequences worth internalising:

- **While `starting`, a failed probe is the expected case.** It does not count
  toward the failure ladder; only the startup timeout ends that patience. Return
  `Ready: false` with a `Phase` and `Progress` if your engine reports them — an
  operator watching a large model load then sees movement instead of silence.
- **A `Load` error is better than a guess.** The node degrades to its own
  in-flight count within 3 seconds, which is honest. A zero published as fact
  makes the router send a burst at a saturated backend.

## Writing it

### Command

```go
func (e *Engine) Command(s engine.Spec) (engine.Command, error) {
    opts := defaultOptions()               // your defaults, first
    if err := engine.DecodeOptions(s, &opts); err != nil {
        return engine.Command{}, err
    }
    args := []string{
        "serve", s.Path,
        "--host", "127.0.0.1",
        "--port", strconv.Itoa(s.Port),
        "--served-model-name", s.Name,     // REQUIRED: see below
        // ... whatever your engine needs, derived from s and opts
    }
    // Operator args LAST, so their flag wins: every engine in the fleet takes
    // the last occurrence of a repeated flag.
    return engine.Command{Path: opts.Binary, Args: append(args, s.Args...)}, nil
}
```

Three things `enginetest` enforces and the fleet depends on:

1. **The backend must answer to `s.Name`.** A client asks for `Qwen3.8-27B` and
   reaches whichever node has a free slot; a backend advertising its file path
   instead is unreachable through the mesh.
2. **Bind `127.0.0.1` and `s.Port`.** A backend on a routable interface is an
   unauthenticated inference server on the tailnet.
3. **`s.Args` go last.**

`s.Backends` is how many backends the whole model runs on this node, for
host-level tuning that divides a shared resource between them: llama.cpp's
automatic `--threads` is a fair share of the CPUs across a model's backends, and
without it ten backends each take half the machine.

`s.Context` is **per slot**. Translate it to whatever your engine wants —
llama.cpp gets `--ctx-size s.Context*s.Parallel`, an engine with a per-sequence
limit gets `s.Context` directly. Whatever you pass, the backend must actually
serve `s.Context` per slot, because that is the number the node publishes to the
mesh.

### Probe

The single most common porting bug: **an HTTP 200 is not readiness.** Engines
differ, and they differ in ways that break silently.

```go
func (e *Engine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
    resp, err := e.get(ctx, addr, "/health")
    if err != nil {
        return engine.Probe{}, err   // transport failure: refused, EOF, timeout
    }
    defer resp.Body.Close()
    // ... decide readiness from the status, the body, or both
}
```

Return an error **only** for a transport failure. An engine that answers and
says "still loading" is a *successful* probe reporting `Ready: false` — if you
return an error there, the node cannot tell "loading" from "gone".

If your engine answers 200 while still loading (some do, for the whole many
minutes a large model takes), readiness is in the body and a 200 alone is a
trap: the node would advertise the backend and every routed request would come
back 503.

### Load

```go
func (e *Engine) Load(ctx context.Context, s engine.Spec, addr string) (engine.Load, error) {
    // scrape whatever your engine publishes
    if !foundRunningGauge {
        return engine.Load{}, fmt.Errorf("...: no running-sequence gauge in this build")
    }
    return engine.Load{
        Slots:      s.Parallel,   // or what the engine reports, if it reports it
        Busy:       running,
        Waiting:    waiting,      // 0 if your engine publishes no queue depth
        CtxPerSlot: int64(s.Context),
    }, nil
}
```

**`Slots` and `CtxPerSlot` are what the backend will ACTUALLY serve**, which is
not always what the node asked for. Report what your engine says when it says
it — llama.cpp's `/slots` gives both, and it returns those rather than the
config, because a value the engine silently clamped is only visible if somebody
reports the real one. Fall back to `s.Parallel` and `s.Context` when your engine
reports neither. Never return zero for either: the mesh reads a number as fact,
and "0 slots" takes the model out of routing.

> `Load` is handed its `Spec`, and that is a correction. The signature was
> `Load(ctx, addr)` in v2.1.0's first cut, which left these two fields
> unfillable by any engine that cannot observe them. Two independent clean-room
> engines reached the same workaround — a map from address to the `Spec`
> `Command` was called with — and it misreports as soon as the node reassigns a
> port, which it does after a failed bind. One parameter removed the class.

If a gauge is missing from this build of your engine — they get renamed between
versions — return an error, and consider probing a list of known names rather
than one.

### Options

Your engine owns its YAML block, named for the engine:

```yaml
models:
  - name: granite-4.2-8b
    engine: myengine          # names both the engine and the block below
    path: /models/granite-4.2-8b
    gpus: [0]
    gpus_per_backend: 1
    context: 16384            # per slot
    parallel: 8
    args: ["--some-operator-flag"]
    myengine:                 # captured verbatim, decoded by your package
      binary: myengine
      some_knob: 0.85
```

```go
type Options struct {
    Binary   string  `yaml:"binary"`
    SomeKnob float64 `yaml:"some_knob"`
}

func defaultOptions() Options { return Options{Binary: "myengine", SomeKnob: 0.9} }

func (e *Engine) ValidateOptions(path string, s engine.Spec) error {
    opts := defaultOptions()
    if err := engine.DecodeOptions(s, &opts); err != nil {
        return err
    }
    if opts.SomeKnob <= 0 || opts.SomeKnob > 1 {
        return fmt.Errorf("%s.myengine.some_knob %v must be in (0, 1]", path, opts.SomeKnob)
    }
    return nil
}
```

Two rules: **every error names the field that is wrong**, and an unknown key is
an error rather than a shrug — `DecodeOptions` handles the second for you.

Keep the block small. The right home for an engine's long tail of tuning flags
is `models[].args`, not a mirrored YAML key per flag: a key with a zero value
cannot express "let the engine decide", and most engines resolve most of their
own settings from the checkpoint and the hardware better than a config file can.
Promote a flag to a key when the *node* is the one deciding it, or when its
value is published to the mesh.

## Testing

**No test may need a GPU, your engine's binary, or the network.** Every engine
viiwork has ever had was developed on a machine that could not run it.

```go
func TestConformance(t *testing.T) {
    enginetest.Run(t, New(),
        enginetest.Case{
            Name: "single card",
            Spec: engine.Spec{Name: "m", Path: "/models/m", GPUs: []int{0},
                Port: 18080, Context: 8192, Parallel: 4, Vendor: gpu.VendorNVIDIA},
            Options: "binary: myengine\nsome_knob: 0.85\n",
        },
    )
}
```

Then your own tests, for what is specific to your engine:

- **Command line: golden tests.** The exact argument slice, for each Spec shape
  you support. This is where a flag rename shows up.
- **Probe and Load: `httptest` servers replaying `testdata/`.** Capture real
  output from the engine version the fleet will run, verbatim, including the
  awkward cases: a body from an older version with different metric names, a
  multi-process engine reporting several series, a document with a field absent.
- **Cover the absent case explicitly.** A test that only ever sees a healthy
  engine will not catch publishing a zero as fact.

The node-level test (one per release, not per engine) runs the real supervisor
against a fake binary that asserts the flags it was given and then serves your
fixtures. If you want your engine covered there, say so — it lives in
`internal/node` and needs a mode added to the test helper.

## Packaging

One image per engine under `docker/`. The Go binary is built CGO-off and dropped
into whatever base image carries your engine's runtime; viiwork compiles against
none of it, because it only ever spawns and supervises. Pinning the base image
is how the inference stack gets pinned.

Name the file for the engine, not the hardware: `docker/Dockerfile.<name>`. An
engine that gains a second accelerator backend gains a sibling base image, not a
fork.

If your engine JIT-compiles kernels, needs a toolchain at **run** time, or keeps
a cache it would otherwise rebuild on every container start, say so in the
Dockerfile's header comment and give the compose example the volumes it needs. A
reviewer cannot infer it and an operator will not discover it until the first
cold start.

## Done when

- [ ] `Options` with defaults, and `ValidateOptions` if any value can be wrong
- [ ] The five methods, and any capability your engine can answer honestly
- [ ] `engine.Register(New())` from `init`
- [ ] `enginetest.Run` passes
- [ ] Golden command-line tests, and `httptest` tests over captured `testdata/`
- [ ] Blank import in `internal/node/node.go` and `cmd/viiwork-accept`
- [ ] `docker/Dockerfile.<name>` and a compose example, if the runtime needs one
- [ ] A model block in `viiwork.yaml.example` and a row in README's engine table
- [ ] `gofmt`, `go vet`, `go test ./...` clean

If any step forced an edit to `internal/config`, `internal/supervisor` or
`internal/proxy`, write down what and why — that is a finding about the
contract, and the contract is what gets fixed.

## Writing against this before it ships

The API above lands in **v2.1.0**. To write an engine in parallel:

**Guaranteed not to move** — write against these freely:

- The five `Engine` methods, their names and their signatures.
- `Spec`'s field names and meanings, `Command`, `Probe`, `Load`.
- `Register` / `Lookup` / `Names`, and registration from `init`.
- The four capability interfaces and their semantics.
- The rule that an engine's YAML block is named for the engine and owned by it.
- Everything in "What the node already does".

**May still move**, so keep it at arm's length:

- `DecodeOptions`'s exact error string. Depend on it returning an error, not on
  its wording.
- `enginetest.Case`'s fields, which have not been exercised by a second engine
  yet. Keep your cases in one function so they are cheap to reshape.
- Whether `Spec` gains a field. It can only gain one — nothing is being removed
  — so a struct literal with field names will keep compiling. It has already
  gained `Backends` since this document was first written, for exactly that
  reason: llama.cpp's thread tuning needed it on contact with the code.

**To compile today**, copy the declarations in "The API" into a local stub
package and build against that. The real packages now exist on the `v2.1`
branch, so once you can pull it, delete the stub and change the import path;
nothing else should change. One field did move while this was being built —
`Spec` gained `Backends` — and it is in the listing above.

**For the vLLM engine specifically**, the decisions are already made and written
down in the project's internal v2.2 plan — ask for them before starting, because
several are counter-intuitive. The short version: always pass
`--max-model-len` and `--max-num-seqs` from `Spec.Context` and `Spec.Parallel`;
return an error from `Load` when the running gauge is absent rather than zero;
`CtxPerSlot` is `Spec.Context` with no round trip to the engine; do not implement
`TokenProgressReader`; default startup timeout 20 minutes; set nothing about
devices in `Command.Env`.
