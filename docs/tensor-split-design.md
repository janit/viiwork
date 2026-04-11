# Tensor-Split Mode for viiwork — Design Note

**Status:** draft proposal
**Author:** tensor-split investigation (gfx906-fork-cutover sibling worktree)
**Date:** 2026-04-09
**Applies to:** branch `tensor-split-investigation`

## Background

viiwork's current architecture spawns **N independent `llama-server` processes**, one per GPU, each pinned via `ROCR_VISIBLE_DEVICES=<gpuid>`, all serving the same model independently. Requests are load-balanced across these per-GPU backends, giving N-way concurrency at single-GPU memory cost.

This works perfectly for any model that fits one GPU (≤ 16 GB on Radeon VII), which has been every model viiwork has run to date. It **cannot** run a model that exceeds single-GPU VRAM — and that's the entire reason this branch exists.

The tensor-split investigation (see `memory/smoke_test_results_2026_04_09.md` and `memory/gemma4_31b_results_2026_04_09.md`) confirmed that llama.cpp's `--tensor-split` + `--split-mode layer` works on the gfx906 stripped fork over the rig's PCIe gen1 x1 mining-rack risers, with surprisingly small throughput penalties (-13% TG at N=2, -25% at N=6 for the 26B-A4B MoE; -7% to -10% for Gemma 4 31B Dense). Layer-split is the right mode (row-split crashes, and is the wrong mode for this interconnect anyway). The **only** thing missing is the viiwork-side plumbing to spawn this configuration.

## What needs to change

A tensor-split deployment is structurally different from the existing replica deployment:

| Aspect | Current (replicas) | Tensor-split |
|---|---|---|
| llama-server processes | N (one per GPU) | **1** (spans N GPUs) |
| `ROCR_VISIBLE_DEVICES` | single int per process | **comma-separated list** |
| Per-process port | base_port + i | single port |
| Concurrent slots | N × parallel | **single slot** (one big request at a time) |
| Memory per GPU | full model | model / N |
| Total addressable model size | ≤ single GPU VRAM | ≤ N × single GPU VRAM |
| Health check loop | per-process | one process |
| Balancer behavior | pick lowest-latency idle | trivial (one backend) |
| `/v1/models` | one entry, N-way capacity | one entry, 1-way capacity |
| Failure recovery | restart one of N | restart the only one (longer outage) |

The two modes are not naturally interleaved on the same host. A given viiwork node should be either replica-mode or tensor-split-mode, not both. Mixed-mode would mean splitting the GPUs into two groups serving different models, which is already covered by running **two viiwork instances** on different ports (the existing multi-model-per-host pattern in `setup-node.sh`).

## Proposed config shape

The minimal-blast-radius option: a **boolean toggle on the `gpus` block** that switches the spawn semantics. No new top-level mode field, no schema break for existing configs.

```yaml
# viiwork.tensor-split.yaml
server:
  host: 0.0.0.0
  port: 8080

model:
  path: /models/gemma-4-31B-it-Q4_K_M.gguf
  context_size: 4096
  n_gpu_layers: 999
  parallel: 1                          # single slot — tensor-split serializes anyway

gpus:
  devices: [4, 5, 6, 7]                # explicit list; the model spans all of them
  base_port: 8081                      # single port assigned (base_port itself), no offsets
  tensor_split:
    enabled: true                      # the new toggle
    mode: layer                        # layer | row    (default: layer)
    weights: [1, 1, 1, 1]              # optional per-GPU split fractions; default = even
    main_gpu: 0                        # only used with split-mode=row; default 0

backend:
  binary: /usr/local/bin/llama-server
  extra_args: []
```

**Why this shape:**
- `gpus.devices` already exists and already takes a list. We reuse it instead of adding `tensor_split.devices`.
- `gpus.tensor_split.enabled: true` is the discriminator. Manager branches on this.
- `mode`, `weights`, `main_gpu` map 1:1 to llama.cpp's `--split-mode`, `--tensor-split`, `--main-gpu`. Don't invent new vocabulary.
- `parallel: 1` is enforced/defaulted in tensor-split mode regardless of what the user sets — single slot is a hardware property here, not a policy choice.
- Existing replica-mode configs are untouched; the absence of `gpus.tensor_split` means the default `enabled=false` and current behavior is preserved exactly.

### Alternative considered: top-level `mode:` field
```yaml
mode: tensor-split    # or "replicas" (default)
```
**Rejected.** It's a more invasive schema change, requires bumping config version, and forces every existing config to add an explicit `mode: replicas` line on next deploy. The boolean toggle hidden under `gpus` is strictly less disruptive and conveys the same intent.

### Alternative considered: `backends:` list
```yaml
backends:
  - kind: tensor-split
    devices: [4, 5, 6, 7]
    ...
  - kind: replica
    devices: [0]
    ...
```
**Rejected for now.** Most general, biggest refactor, and gives mixed-mode capability we don't need. Worth revisiting only if someone genuinely wants to run tensor-split *and* replicas in the same viiwork process — and even then the existing two-instances-on-different-ports pattern handles it.

## Code changes

### `internal/config/config.go`
Add a `TensorSplitConfig` type and embed in `GPUConfig`:

```go
type TensorSplitConfig struct {
    Enabled bool      `yaml:"enabled"`
    Mode    string    `yaml:"mode"`     // "layer" | "row"; default "layer"
    Weights []float64 `yaml:"weights"`  // optional per-GPU fractions
    MainGPU int       `yaml:"main_gpu"` // for mode=row only
}

type GPUConfig struct {
    Count           int                `yaml:"count"`
    Devices         []int              `yaml:"devices"`
    BasePort        int                `yaml:"base_port"`
    Offset          int                `yaml:"offset"`
    PowerLimitWatts int                `yaml:"power_limit_watts"`
    TensorSplit     TensorSplitConfig  `yaml:"tensor_split"`
}
```

`Validate()` additions:
- if `tensor_split.enabled` then require `len(devices) >= 2`
- if `tensor_split.mode == ""`, default to `"layer"`
- if `tensor_split.mode == "row"`, log a loud warning (we know it crashes on the gfx906 fork; let users opt in to the experiment)
- if `tensor_split.weights != nil` then require `len(weights) == len(devices)`
- if `tensor_split.enabled` then force `model.parallel = 1` (or warn and override)

### `internal/process/backend.go`
The `Backend` struct grows multi-GPU fields, but **only one is populated at a time**:

```go
type Backend struct {
    GPUID         int     // -1 if multi-GPU (tensor-split)
    GPUIDs        []int   // populated if tensor-split, empty otherwise
    TensorSplit   bool
    SplitMode     string
    SplitWeights  []float64
    MainGPU       int
    // ... (existing fields unchanged)
}
```

`buildArgs()` adds the tensor-split flags:
```go
if b.TensorSplit {
    args = append(args, "--split-mode", b.SplitMode)
    if len(b.SplitWeights) > 0 {
        args = append(args, "--tensor-split", joinFloats(b.SplitWeights, ","))
    } else {
        // even split: 1,1,1,...
        args = append(args, "--tensor-split", evenSplit(len(b.GPUIDs)))
    }
    if b.SplitMode == "row" {
        args = append(args, "--main-gpu", strconv.Itoa(b.MainGPU))
    }
}
```

`buildEnv()` produces the comma-separated `ROCR_VISIBLE_DEVICES`:
```go
if b.TensorSplit {
    env = append(env, fmt.Sprintf("ROCR_VISIBLE_DEVICES=%s", joinInts(b.GPUIDs, ",")))
} else {
    env = append(env, fmt.Sprintf("ROCR_VISIBLE_DEVICES=%d", b.GPUID))
}
```

`Start()` power-limit application loops over `GPUIDs` instead of single `GPUID` when multi-GPU. (Each GPU gets its own `rocm-smi --setpoweroverdrive ... -d <id>` call.)

### `internal/process/manager.go`
`NewManager`'s loop becomes a branch:

```go
if cfg.GPUs.TensorSplit.Enabled {
    // single backend spans all devices
    devices := cfg.GPUs.ResolvedDevices()
    port := cfg.GPUs.BasePort
    addr := fmt.Sprintf("localhost:%d", port)
    m.Backends = []*Backend{{
        GPUID:        -1,
        GPUIDs:       devices,
        TensorSplit:  true,
        SplitMode:    cfg.GPUs.TensorSplit.Mode,
        SplitWeights: cfg.GPUs.TensorSplit.Weights,
        MainGPU:      cfg.GPUs.TensorSplit.MainGPU,
        ModelPath:    cfg.Model.Path,
        Port:         port,
        ContextSize:  cfg.Model.ContextSize,
        NGPULayers:   cfg.Model.NGPULayers,
        Parallel:     1,                          // forced single-slot
        Binary:       cfg.Backend.Binary,
        ExtraArgs:    cfg.Backend.ExtraArgs,
        HealthTimeout:   cfg.Health.Timeout.Duration,
        PowerLimitWatts: cfg.GPUs.PowerLimitWatts,
        State:         balancer.NewBackendState(-1, addr),  // gpu_id=-1 means "tensor-split aggregate"
        LogWriter:     logWriter,
    }}
} else {
    // existing per-GPU loop, unchanged
    for i, gpuID := range devices { ... }
}
```

### `internal/balancer/`
`balancer.BackendState` already takes a single int for `GPUID`. Two options:

1. **Sentinel value `-1`** for tensor-split aggregate, treated as "the special backend". Pick logic: if any backend has `GPUID == -1`, that's the only choice. Trivial.
2. **Add a `GPUIDs []int` field** to `BackendState` for richer reporting. More invasive but exposes the multi-GPU shape to dashboard and `/v1/status`.

Recommend #1 for the initial cut, #2 as a follow-up when the dashboard work is done.

### `internal/proxy/handler.go`
- `/v1/models` should report the model as available with `n_slots: 1` (vs `n_slots: N*parallel` in replica mode). The dashboard currently shows per-GPU rows; in tensor-split mode it should show a single "tensor-split (4 GPUs)" row.
- The 429 backpressure path triggers when in-flight ≥ max-in-flight; with single-slot tensor-split, max-in-flight should default to 1 (or a small queue depth).
- `X-GPU-Backend` header should report something like `tensor-split:4-7` instead of a single GPU id.

### `internal/peer/`
No changes needed for the cross-node mesh — peers see a viiwork node as a single endpoint. The tensor-split node advertises one model with one slot, and peer routing is based on model name, not slot count. **But:** the mesh latency-aware picker should weigh tensor-split nodes appropriately given they serialize. Easiest fix: bump the latency window or add a "single-slot penalty" multiplier when picking. Defer to a follow-up.

### `internal/gpu/` and `internal/power/`
No structural changes. The collectors sample all visible GPUs regardless of how viiwork has them grouped. Dashboard will show all N GPUs as busy when one tensor-split request is in flight, which is correct.

## Migration / backward compat

- Configs without `gpus.tensor_split` continue to work exactly as before (default `enabled=false`).
- Existing `viiwork.yaml`, `viiwork.gfx906.yaml`, `viiwork.soak.yaml` unchanged.
- New `viiwork.tensor-split.yaml.example` checked in alongside the existing examples.
- `setup-node.sh` gets a new option: "tensor-split mode for one big model" vs the existing "N replicas of one small model". Both produce a docker-compose plus config.
- The MCP server (`cmd/viiwork-mcp`) needs no changes — it talks to viiwork's HTTP API, which is unchanged.

## Open questions

1. **Concurrency model.** Single-slot tensor-split serializes requests. Should viiwork explicitly queue requests at the proxy layer (return 429 immediately) or let llama-server's internal slot machinery do it (incoming requests block until the slot frees)? Recommend: queue at the proxy with a configurable depth, return `Retry-After` past depth — matches existing replica-mode backpressure semantics.
2. **Failure recovery.** If the single tensor-split backend dies, there's no failover within the node. Restart attempts already exist (max 3 in `manager.respawnCounts`); reuse that path. Mesh peers can pick up the model if they have it. Document this as a known limitation: "tensor-split nodes have no per-node redundancy."
3. **Power limit application.** Currently `gpus.power_limit_watts` is one number applied to each GPU. In tensor-split mode it still applies to each of the N GPUs in the group. Confirmed correct, but mention in docs.
4. **Pipelines.** `pipeline.PipelineConfig` is per-model. In tensor-split mode there's only one model per viiwork process, so pipeline support carries over unchanged. No work needed.
5. **Bench harness.** The `bench.sh` and `bench-sustained.sh` scripts already hit the OpenAI-compatible HTTP API and don't care about backend topology. They will produce meaningful results against tensor-split mode immediately, with the caveat that effective concurrency is 1 instead of N×parallel.

## Testing plan

1. **Unit tests** for `config.Validate` covering:
   - `tensor_split.enabled` requires `devices` of length ≥ 2
   - `tensor_split.weights` length must match `devices` length
   - default `mode` is `"layer"`
   - `parallel` is forced to 1 with a logged override
2. **Unit test** for `Backend.buildArgs()` and `buildEnv()` in tensor-split mode — verify correct flags and `ROCR_VISIBLE_DEVICES`.
3. **Integration test** (build tag `integration`) using mock httptest backends to confirm Manager only starts one backend in tensor-split mode and the proxy routes there.
4. **Bench validation** against the gfx906 fork on the actual rig: load Gemma 4 31B Q4_K_M with `gpus: { devices: [4,5], tensor_split: { enabled: true } }`, run `bench.sh`, confirm tok/s within ±5% of the manual `docker run llama-server --tensor-split` numbers from `memory/gemma4_31b_results_2026_04_09.md` (16.2 tok/s @ N=2 long-prompt).

## Estimated implementation cost

- config.go + tests: ~100 LOC
- backend.go + manager.go branching: ~80 LOC
- proxy headers / status / models updates: ~50 LOC
- example config + docs: ~30 LOC
- integration test: ~80 LOC
- **Total: ~340 LOC for the minimal viable cut.** Mesh-balancer single-slot weighting and dashboard tensor-split-aware row are deferred to follow-ups.

## Appendix — what we deliberately are NOT changing

- The `gpus.count` semantics. Replica mode keeps using `count` as before.
- The balancer's adaptive mode. With one backend it trivially always picks that one.
- The peer registry's model-based routing. It already aggregates by model name, not by per-GPU slots.
- The dashboard's GPU history rings. They're per-physical-GPU and stay that way.
- The cost / power / IPMI samplers. They're already host-wide, not per-backend.
- The MCP server. It's a thin HTTP client; nothing to change.
