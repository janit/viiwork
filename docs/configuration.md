# Configuration

One file per machine. `viiwork --config /etc/viiwork/viiwork.yaml` (the default
path; on a Mac, `~/.config/viiwork/viiwork.yaml`) is the only input — there are no command-line overrides. The node
validates the whole file at startup and on every reload, and names the offending
field when something is wrong. A viiwork 1.x file is refused with a pointer to
[migrating-to-v2.md](migrating-to-v2.md).

Copy [`viiwork.yaml.example`](../viiwork.yaml.example) and edit it.

- [Models](#models)
- [Tensor-split mode](#tensor-split-mode)
- [Reloading](#reloading)
- [GPU power limits](#gpu-power-limits)
- [Pipelines](#pipelines)
- [Client discovery](#client-discovery)
- [API listener and browser origins](#api-listener-and-browser-origins)
- [viiwork-parrot](#viiwork-parrot)
- [Environment variables](#environment-variables)
- [Host requirements](#host-requirements)

## Models

A machine's models are entries under `models:`. Each has an explicit `name` (the
model id clients send), its host GPU indices, and its slots:

```yaml
models:
  - name: Qwen3.8-27B
    engine: llamacpp
    path: /models/Qwen3.8-27B-UD-Q4_K_XL.gguf
    gpus: [0, 1]
    gpus_per_backend: 2      # one backend across both cards
    context: 49152           # tokens PER SLOT
    parallel: 2
  - name: granite-4.2-8b
    engine: llamacpp
    path: /models/granite-4.2-8b-Q4_K_M.gguf
    gpus: [2, 3, 4, 5]       # gpus_per_backend defaults to 1: four replicas
    context: 16384
    parallel: 2
```

A GPU belongs to at most one model. Every model on every machine is visible from
any node.

A model names its weights with `path` (a local file or directory) or `source`
(a viiwork-parrot catalog id, `viiwork-parrot:<id>`) — exactly one of the two.
A sourced model is resolved to a path by the host's viiwork-parrot before its
engine starts; see [Models from viiwork-parrot](models.md#models-from-viiwork-parrot).

**`models[].context` is the context of one slot**, for every engine: llama.cpp is
started with `--ctx-size context × parallel`. viiwork 1.x's `model.context_size`
was llama.cpp's total, divided across slots — carrying the old number over
multiplies VRAM use by `parallel`.

The engine named in `engine:` owns the YAML block below it (`llamacpp:`, `vllm:`,
`freetoken:`, `strata:`); see [Engines](../README.md#engines) in the README and
[adding-an-engine.md](adding-an-engine.md).

`startup_timeout` is per model, and its default comes from the engine: **10
minutes** for llama.cpp, **20** for vLLM, **30** for FreeToken, which fills a GPU
expert cache from host memory before it serves, and **30** for Strata. Size it from your observed
cold-load time: a backend
that has not answered by then is given up on and respawned, discarding whatever
it had already placed in VRAM. See [models.md](models.md) for both the gfx906
measurement behind that rule and the FreeToken offload case.

**For `engine: strata`, `path` is not a weights file.** It is Strata's own JSON
config, the file upstream's `setup.py` writes, and one file serves every backend
of the model: the node passes host, port and cards as flags, so the file's
`host`, `port` and `gpu` are ignored. Strata has no flag for the model name, the
context or the slot count, so a backend refuses to start unless the file agrees
with the `models[]` entry:

| In the Strata JSON | Must equal |
|---|---|
| `model_name`, or one of `aliases` | `name` |
| `--max-context` in `args` | `context` |
| `parallel` | the same number as the entry's `parallel` (absent is 1), from 1 to 8. Above 1 needs Strata v0.1.41, which lists its batch slots on `/slots`; an older build publishes one slot whatever the file says |
| `lazy_load`, `idle_unload_s` | off — an unloaded server looks dead to the node |
| `api_key` | not set — the node probes and forwards without a key, and the backend listens on loopback only |

The error names the key that disagrees. The file is read when a backend
launches: after editing it, `viiwork down` and `up` the model or restart the
node, because a reload (SIGHUP) relaunches only models whose entry in
`viiwork.yaml` changed. Keep the JSON where a backend cannot write it; it names
the program the server runs. `source:` is not supported for this engine.

The `strata:` block has three keys: `dir`, the Strata checkout (required, an
absolute path), `python` (default `python3`) and `warmup` (default `30s`,
`0s` to turn it off, at most `10m`): how long a backend waits after its model
has loaded before the node sends it work. It shows as phase `warming up`.
Engine tuning stays in the JSON's `args`, where upstream puts it. See
[models.md](models.md#strata-one-moe-family-across-several-cards).

## Tensor-split mode

For models that don't fit in a single GPU's VRAM, one backend spans several
GPUs. Set `gpus_per_backend` to the group size: a model's `gpus` are cut into
groups of that size, one backend per group.

```yaml
models:
  - name: gpt-oss-120b
    engine: llamacpp
    path: /models/gpt-oss-120b-MXFP4_MOE.gguf
    gpus: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]
    gpus_per_backend: 5      # two backends of five cards each
    context: 32768
    parallel: 2
    llamacpp:
      split_mode: layer      # "layer" is the only mode that works on gfx906
```

Trade-offs against one backend per GPU:

| | Replicas (`gpus_per_backend: 1`) | Split groups (`gpus_per_backend: N`) |
|---|---|---|
| Concurrency | N backends, each with `parallel` slots | one backend per group |
| Model size cap | Must fit in 1 GPU | Can span N GPUs |
| Throughput | Higher (parallel) | Lower (a group computes one card at a time) |
| Use case | Models ≤13GB on 16GB cards | Models >13GB that need 2+ cards |

On the reference fleet's gfx906 mining-rig topology (PCIe gen1 x1 risers), the
measured tensor-split penalty is -2 to -13% for 2-GPU and -7 to -20% for 4-GPU
splits. On PCIe gen3/4/5 the penalty is smaller.

Layer split runs a group's cards strictly sequentially, so **extra GPUs in a
group buy VRAM and context, never throughput** — for throughput run more
backends, not wider ones. The full rationale and the measurements behind it are
in [models.md](models.md); the design record is
[tensor-split-design.md](tensor-split-design.md).

## Reloading

`SIGHUP` (`docker kill -s HUP viiwork`) applies an edited model list: added
models start, removed ones drain and stop, and a changed entry restarts that
model only. The whole file is re-validated first, so a bad edit is refused
rather than half-applied.

`viiwork init` writes this file on a new machine, checked by the same
validation the node runs; see [setup.md](setup.md). To write it by hand, start
from `viiwork.yaml.example` and check it with `viiwork-accept config` (see
[operations.md](operations.md)). [migrating-to-v2.md](migrating-to-v2.md)
converts existing 1.x instances.

## GPU power limits

Optionally limit power draw per card:

```yaml
gpu:
  power_limit_watts: 180  # applied via rocm-smi when a backend starts
```

`scripts/power-perf-sweep.sh` measures tok/s, watts and temperature across cap
settings and recommends a value — see [operations.md](operations.md).

## Pipelines

Pipelines chain multiple LLM steps into virtual models. A consumer calls a
virtual model name (for example `localize-fi` or `improve-en`) and viiwork
executes a sequence of prompts across one or more real backend models.

Two pipeline types are included:

- **Localization** — translate, culturally adapt and QC text in a single
  request. Supports locale aliases and per-locale glossaries.
- **Text improvement** — generate text then rewrite it to remove AI writing
  patterns (de-slop).

Each step specifies a model, a Go template prompt and a temperature. Steps
execute sequentially, each step's output feeding the next. Configure pipelines
under `pipelines:` in `viiwork.yaml`, keyed by pipeline name:

```yaml
pipelines:
  tr:
    locales:
      fi:
        language: Finnish
    steps: []          # name, model, prompt, temperature, json_output per step
```

Every key, with its meaning, is in `internal/pipeline/config.go`
(`PipelineConfig`, `StepConfig`, `LocaleFileConfig`).

A pipeline is node-local: it is absent from capacity reports and status, and is
dispatched only on the node that received the request. It therefore cannot be an
alias target — see [mesh.md](mesh.md#aliases).

## Routing (`routing`)

Shown with the defaults; the section can be left out.

```yaml
routing:
  performance: true     # route by each host's measured time to first token
  queue_max: 64         # requests that may wait per model on this node; 0 = no queue
  queue_timeout: 20s    # how long one may wait before 429
  forward_retry: 1      # other routes tried after a refusal before the first byte
  stale_after: 3s       # a member's capacity report older than this is not routed on
```

`performance: false` restores the routing of v2.6 and earlier — a local backend
with a free slot, else the member with the most free slots — stops this node
publishing its scores, and makes it ignore session headers. What the scores
are and how a session header is used: [mesh.md](mesh.md#routing).

## Client discovery

`/v1/models` and `/v1/model/info` need no configuration. The OpenCode catalogue on
`/api.json` has a block of its own under `api:`, shown here with its defaults:

```yaml
api:
  catalog:
    enabled: true
    provider_id: viiwork       # the prefix a user types: viiwork/<model>
    provider_name: " viiwork" # OpenCode sorts by name; the space sorts it first
    base_url: ""              # empty: the address the request arrived on
    upstream: ""              # e.g. https://models.opencode.ai, without /api.json
    upstream_ttl: 1h          # must be positive when upstream is set
```

`base_url` is for a node behind a proxy that rewrites the host. `upstream` is
off by default, so a node makes no outbound request unless asked; set, it chains
a hosted catalogue back in so a client keeps its other providers. What each
field means to a client, and why the defaults are what they are:
[autodiscovery.md](autodiscovery.md).

## API listener and browser origins

```yaml
api:
  host: 0.0.0.0              # default: every interface
  port: 8086
  cors:
    # allow_origins: ["*.tail1234.ts.net", "localhost", "127.0.0.1"]
    allow_tailnet_ips: true  # also origins that are literal Tailscale IPs
```

**`api.host`** is the address the API binds. The default `0.0.0.0` reaches every
interface the machine has, and the node logs a warning saying so at startup,
because the API authenticates nothing. Set it to the machine's tailnet or LAN
address to narrow it; the node's own calls into its API (pipeline steps) follow
whatever address the listener bound. Health checks must then use that address.

**`api.cors.allow_origins`** is the browser-origin allowlist. Leave it out and
the node derives one at startup: this tailnet's own MagicDNS domain, read from
tailscaled over `mesh.tailnet.socket` (`*.tail1234.ts.net`), plus `localhost` and
`127.0.0.1`; without tailscaled, only the local origins. The resulting list is
logged. A list you write is used as written, and `allow_origins: []` turns CORS
off. Never use `*.ts.net`: it is every Tailscale customer's domain, Funnel pages
included.

Whatever the list, state-changing requests (`POST`, `PUT`, `PATCH`, `DELETE`)
from a browser origin it refuses are answered 403, and power and alias writes
must be `Content-Type: application/json`. The reasoning, and what it does and
does not stop: [security.md](security.md#browser-origins-cors).

## viiwork-parrot

A model with `source: viiwork-parrot:<id>` gets its weights from the host's
viiwork-parrot node instead of a local file — see
[Models from viiwork-parrot](models.md#models-from-viiwork-parrot). It has one
key:

```yaml
viiwork_parrot:
  api: 127.0.0.1:7950   # viiwork-parrot's API address; loopback only
```

`api` defaults to `127.0.0.1:7950`. It must be a loopback address or
`localhost` — viiwork-parrot's API is unauthenticated, so it is never reachable
from another machine. A change needs a restart: `viiwork_parrot` is captured at
start, and `SIGHUP` names it among the sections it did not apply.

## Rolling updates (`update`)

```yaml
update:
  enabled: false        # opt in to stage/activate/rollback over /v1/update
  source: https://github.com/janit/viiwork/releases/download
  confirm_timeout: 0    # 0: the sum of the models' startup timeouts + 10m
```

A request names a version; the node builds the URL from `source` itself.
`source` accepts only the value shown: releases are downloaded from GitHub and
nowhere else, and a download may be redirected only to GitHub's own asset
hosts (`*.githubusercontent.com`). Any other value is refused at start. The
key remains so existing files keep parsing. Changing any of these needs a
restart. See docs/releases.md.

## Environment variables

| Variable | Purpose |
|----------|---------|
| `VIIWORK_MESH_SECRET` | Mesh secret, base64 of exactly 32 bytes (`openssl rand -base64 32`). Unset requires `mesh.open: true`. See [mesh.md](mesh.md#secured-and-open) |
| `VIIWORK_MESH_SECRET_PREV` | The previous secret, while rotating |
| `VIIWORK_DEBUG=1` | Verbose `[debug]` logging on the request path. Off by default — these sit on hot paths, and on a host whose cores are shared with `llama-server` writing a line per request costs CPU that inference needs. Turn it on when diagnosing routing. |
| `ENTSOE_API_KEY` | ENTSO-E API key for cost tracking — see [power-and-energy.md](power-and-energy.md#cost-tracking) |
| `BMC_PASSWORD` | BMC password for out-of-band power control — see [power-and-energy.md](power-and-energy.md#power-control) |

The mesh secrets, `ENTSOE_API_KEY` and the BMC password (under whatever names
the config gives them) are the node's own: they are taken out of the
environment an engine process starts with. A model that needs one of those
names sets it in its own `env:`.

## Host requirements

- Linux with Docker and GPU device access, or an Apple Silicon Mac running
  natively ([macos.md](macos.md))
- **AMD / ROCm:** `amdgpu` kernel driver loaded (standard on modern kernels) and
  `/dev/kfd`, `/dev/dri` passed to the container. No ROCm installation is needed
  on the host — the image carries it.
- **NVIDIA / CUDA:** the NVIDIA container runtime, with GPUs granted through
  **CDI** (`--device nvidia.com/gpu=all`, after `nvidia-ctk cdi generate`). The
  images ship no `nvidia-smi` and no `libcuda` on purpose — the runtime injects
  them from the host so they always match the running driver. See
  [BUILDS.md](../BUILDS.md).
- GGUF model files; `hf download` (`pip install huggingface-hub`) fetches them
  from Hugging Face
- Optional: `/dev/ipmi0` passed through for chassis power readings — see
  [power-and-energy.md](power-and-energy.md)
