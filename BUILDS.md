# Builds

viiwork is one Go binary that supervises inference engines. An image is that
binary dropped into a base that carries an engine's runtime — viiwork compiles
against none of them, because it only ever spawns and supervises, so **pinning
the base image is how the inference stack gets pinned.**

One image per engine, all under `docker/`, each named for what it carries.

| Image | Dockerfile | Engine | Make target |
|---|---|---|---|
| `viiwork:latest` (a.k.a. `viiwork`) | `docker/Dockerfile.rocm` | `llamacpp` on ROCm / gfx906 | `make docker-rocm` (aliases: `make docker`, `make docker-stable`) |
| `viiwork-vllm:latest` | `docker/Dockerfile.vllm` | `vllm` | `make docker-vllm` |
| `viiwork-freetoken:latest` | `docker/Dockerfile.freetoken` | `freetoken` | `make docker-freetoken` |
| `viiwork-strata:latest` | `docker/Dockerfile.strata` | `strata` on ROCm / gfx906 | `make docker-strata` |
| `viiwork-strata-cuda:latest` | `docker/Dockerfile.strata-cuda` | `strata` on CUDA | `make docker-strata-cuda` |

**Named for the engine, never for a GPU vendor.** vLLM already has a ROCm build
and FreeToken may add AMD or Intel, so a second accelerator backend is a sibling
base image, not a fork — and `Spec.Vendor` is informational rather than a gate.

`make docker` builds the ROCm image. That is deliberate rather than historical:
Radeon VII is the core of this fleet, and the unqualified target pointing at it
is right. What was wrong before v2.1.0 was the *name* — a root `Dockerfile`
that silently meant "llama.cpp for gfx906" as though no other kind existed.

## `docker/Dockerfile.rocm`

Standard upstream llama.cpp from `ggml-org/llama.cpp`, pinned to a release tag
(currently `b11804`, which carries Kolibri-1, Qwen3.5/3.6/3.8 hybrid DeltaNet support and
MTP speculative decoding) and patched for the gfx906 FP8 header
incompatibility. Built on `rocm/dev-ubuntu-24.04:6.2.4-complete` — the last
ROCm with reliable gfx906 support.

**`--no-mmap` is gone in `b11371`.** `--mmap`, `--no-mmap`, `--mlock` and
`--direct-io` were deprecated in favour of `--load-mode` and then removed; the
server now exits with `invalid argument`. The node's own no-mmap rule (a model
at least 80% of host RAM) asks the binary's `--help` and generates
`--load-mode none` or `--no-mmap` accordingly, so it works on either build.
An operator's own `--no-mmap` in `models[].args` is passed as `--load-mode none`
to a build that no longer reads it, so a config written for `b10437` keeps
working on a `b11371` image; every other arg is passed through as written.

**The pin must stay at or above `b10430`:** the Qwen3.8 quants were cut with it,
and the older `b9222` rejects that architecture at load time with "unknown model
architecture" rather than anything self-explanatory.

The FP8 patch is required because ROCm 6.2+ ships `<hip/hip_fp8.h>` for all
architectures, but gfx906 has no FP8 hardware and the header fails to compile.

Build: `make docker`. To try a different llama.cpp release, override the pin —
and check the architectures your models need, since an older tag can refuse them
at load:

```bash
docker build --build-arg LLAMA_CPP_VERSION=<tag> -f docker/Dockerfile.rocm -t viiwork .
```

## `docker/Dockerfile.vllm`

The binary dropped into vLLM's own published image, `vllm/vllm-openai`, pinned
by `VLLM_VERSION` (currently `v0.32.0`). Nothing is rebuilt: that image already
carries a matched torch, CUDA and kernel set, and rebuilding the stack is how
you end up with a torch that disagrees with the driver.

Two things about it are load bearing:

- **`ENTRYPOINT []`.** The base image sets an entrypoint that launches one vLLM
  API server. viiwork is the process that runs here, and it starts `vllm serve`
  itself, once per backend, with the flags the engine builds. Leave the
  entrypoint in place and `CMD` reads as arguments to that server instead.
- **`ARG VLLM_VERSION` is declared before the first `FROM`.** An `ARG` written
  after a `FROM` belongs to that stage only, and the runtime stage's `FROM`
  would expand it to empty and fail with `invalid reference format`.

Build: `make docker-vllm`. About a minute once the base image is pulled; the
base is roughly 25 GB.

## `docker/Dockerfile.freetoken`

Carried from `viiwork-freetoken`. There is no official FreeToken image, so the
engine is installed here into its own virtualenv on `PATH` — which is why the
build is slow (torch and its CUDA wheels) and the image is several GB.

- **It must be a *devel* CUDA base, not a runtime one.** FreeToken JIT-compiles
  its kernels on first use and needs `nvcc` on `PATH` **at run time**. A runtime
  base passes `docker build` and then fails on the first request.
- **The prebuilt kernel cache is best effort.** Upstream publishes a companion
  wheel whose `+cuXYZ` suffix must match the CUDA that *torch* was built for —
  not `CUDA_TAG`. The engine raises on a mismatch rather than falling back, so a
  wrong wheel is worse than none; no wheel is the ordinary case and the build
  continues.
- **`FREETOKEN_CHANNEL`** is `pypi` (a tagged release) or `nightly` (upstream's
  latest build of main, resolved and sha256-verified by
  `scripts/fetch-engine.py`). Nightly cannot be pinned — each publish deletes
  the previous wheels — so `FREETOKEN_COMMIT` makes a rebuild *fail* when the
  engine has moved rather than reproduce the old one. **Keep the image, not the
  build args**; `/opt/freetoken/engine.json` records the pair it holds.

Build: `make docker-freetoken`, off-peak.

### FreeToken 0.2.1 is the pin

`FREETOKEN_VERSION` defaults to `0.2.1`; 0.1.3 is still the floor, and Flint
needs 0.2.1. The kernel cache resolves to the published
`freetoken_kernel_cache-0.2.1+cu130` wheel. viiwork now also generates
`--enable-cache-report`, present since at least 0.1.3, so a FreeToken host
publishes a performance score. What an upgrading host has to check:

- **A GGUF with a fitting `*mmproj*.gguf` in its folder now builds the vision
  tower.** Add `--text-model-only` to `args:` for the text-only footprint.
- **GLM-5.2 FTWs from older builds load and route with a rounded bias.** Repair
  them with upstream's `scripts/ftw_hotfix.py`.
- **Add the two new cache volumes** from `docker/compose.freetoken.yaml` to an
  existing compose file, or each new container compiles again on its first
  request.

### FreeToken 0.2.0

`FREETOKEN_VERSION` moved to `0.2.0`, with 0.1.3 still the floor. The command
line viiwork generates is unchanged: every flag it passes (`--model`,
`--served-model-name`, `--host`, `--port`, `--moe-strategy`, `--memory-ratio`,
`--max-running-requests`, `--max-seq-len-override`, `--kv-reserve-tokens`) is
in 0.2.0's `ft serve`, and `/health` and `/v1/stats` are still where the probes
look. What an upgrading host has to check:

- **Old FTW checkpoints.** A Qwen3.5 / Qwen3.6 / Qwen3.8-27B FTW from an older
  build **loads without an error and answers wrongly** (its norm weights
  already hold the +1 that 0.2.0 now adds at run time). Experts converted with
  `--moe-backend fused` or `triton`, NVFP4 FTWs with fp16 global scales and
  Muse-Glimmer FTWs with a fused `qkvg_proj` are refused at load. Reconvert
  with `ft checkpoint`. HF checkpoints and GGUF files are not affected.
- **`FREETOKEN_API_KEY` in `ft serve`'s environment now turns authentication
  on.** The node already clears it for the backend (`FREETOKEN_API_KEY=`, which
  0.2.0 reads as unset) and refuses `--api-key` in `args:` and the variable in
  `env:` (v2.10.3, v2.10.4), so a node keeps probing and forwarding without a
  key. Behind a node older than v2.10.3, an inherited key would put every route
  but `/health` behind auth.
- **`--moe-strategy auto` no longer picks `hybrid`** from an `ft bench bw`
  profile, and profiles from older builds are ignored. A host that relied on it
  sets `moe_backend: hybrid` and reruns `ft bench bw --model <checkpoint>`.
- **0.2.0 serves GGUF files** (`ft serve --model <file>.gguf`; image input
  from GGUF came in 0.2.1). The node passes `models[].path` through as
  `--model`. The kernel cache resolves to the published
  `freetoken_kernel_cache-0.2.0+cu130` wheel. torch moves to 2.14.

### FreeToken 0.1.3 is the floor (viiwork 2.3.0)

`FREETOKEN_VERSION` defaults to `0.1.3` rather than to empty, so a rebuild
reproduces the inference stack instead of taking whatever PyPI has that day.
The floor is not advisory: 0.1.3 renamed `--moe-backend` to `--moe-strategy`,
viiwork generates the new spelling, and an older engine rejects it — every
backend on the node dies at load with an argparse error.

Upgrading an existing host is two flags and a checkpoint question:

- **`models[].freetoken.moe_backend` does not change.** The operator key keeps
  its name (C1 freezes the YAML an operator writes); only the generated flag
  moved. `--nvfp4-backend` in `args:` does have to become
  `--quant-backend moe.nvfp4=<kernel>`.
- **Multimodal checkpoints now build their vision tower by default**, which is
  the change most likely to take a backend down. Qwen3.6, Qwen3.8-Flash-Next,
  Gemma-4, GLM-5.3-Flash, MiniMax-M3 and Muse-Glimmer-30B serve images out of
  the box; `--text-model-only` in `models[].args` restores the text-only
  footprint. It also restores the *load*, because an FTW converted before its
  family served images holds no vision encoder and `ft serve` refuses it.
- **Some older FTW checkpoints need repairing** before 0.1.3 loads them at all
  — NVFP4 dense exports missing the `input_scale` their scheme declares, and
  Qwen3.8-Flash-Next, whose PLE table now lives beside the FTW. Reconvert with
  `ft checkpoint`, or patch in place with upstream's `scripts/ftw_hotfix.py`.
- **`ft checkpoint --device` and `ft bench bw --device` are gone**; both take
  `--gpu <uuid|index>`. This only affects scripts you run by hand — viiwork
  spawns neither, and still pins `ft serve` with `CUDA_VISIBLE_DEVICES` rather
  than the new `ft serve --gpu`, because the node has already narrowed the
  child to one card and two layers pinning one backend is how it lands on a
  neighbour.

None of this changes the kernel-cache step: the URL it builds from the
installed release resolves to the published
`freetoken_kernel_cache-0.1.3+cu130` wheel unchanged.

## `docker/Dockerfile.strata` and `docker/Dockerfile.strata-cuda`

[Strata](https://github.com/Niko1221/Strata) at the tag `STRATA_VERSION` pins in
`docker/pins.env` (currently `v0.2.3`), with `viiwork` added. Two files rather
than one with a build argument, because the gfx906 image compiles the engine
and the CUDA image does not, and the two lay Strata out differently: the
model's block and the JSON name different paths on each
([docs/models.md](docs/models.md#strata-one-moe-family-across-several-cards)
has the table).

**`Dockerfile.strata` is the gfx906 build** (Radeon VII, Instinct MI50 / MI60).

- **The base is a community image**, `mixa3607/rocm-gfx906:7.14-complete`
  (pinned by digest as `STRATA_ROCM_BASE` in `docker/pins.env`), the one
  upstream's own `docs/AMD_HIP.md` documents. AMD's ROCm no longer ships
  gfx906 libraries, so this image does not sit on the ROCm 6.2.4 base the
  llama.cpp image uses.
- **The checkout is `/opt/strata`**, built in place as `docs/AMD_HIP.md`
  does: `strata: {dir: /opt/strata}` and the JSON's `exe`
  `/opt/strata/build-906/strata`, as at 0.1.x. Upstream's 0.2 Terminal and
  engine packages are release archives this build does not make.
- **`docker/strata/gfx906-v0.2.3.patch` carries one fix**: HIP aliases in
  `strata_hip.h` for `cudaDeviceGetStreamPriorityRange` and
  `cudaStreamCreateWithPriority`, which `src/core/dflash2.cpp` (new in 0.2)
  calls on every build; without them the engine does not compile (built
  without the patch on 2026-10-24 to check). The v0.1.45 patch's other three
  fixes are gone: the two CMake ones are upstream since 0.2.1, and the cuBLAS
  aliases were for the Qwen3.6 engine, which a gfx906 build no longer
  compiles (`strata --families` lists `qwen4exp`, Qwen3.8-Flash-Next). The
  patch is named for the version and must be re-cut when `STRATA_VERSION`
  changes; `scripts/pins_test.sh` checks that the file the Dockerfile names
  exists.
- **v0.2.3 is compiled, not yet served on a card.** v0.1.45 served
  Qwen3.8-Flash-Next under viiwork on 2026-10-18, one backend on four Radeon
  VIIs and an MI60 (a 104k-token prompt read at 959 tok/s).
- The image has no group named `render`. Give the container the host's render
  group by number, as `docker/compose.strata.yaml` does (`RENDER_GID`); with
  the name the container does not start.

**`Dockerfile.strata-cuda` sits on upstream's image.** Since 0.2.1 upstream
publishes ready-made images, so this is the same shape as `Dockerfile.vllm`:
`STRATA_CUDA_IMAGE` (`ghcr.io/niko1221/strata:0.2.3-cuda13`, held by digest;
its `/opt/strata/terminal/COMMIT` is `STRATA_COMMIT`) with `viiwork` added.
Nothing is compiled, and the legacy builder works.

- **cuda13 needs NVIDIA driver 580 or newer.** Its engine is built for sm_75,
  86, 89 and 120 plus PTX: RTX 20 to 50 and the A4000.
- **The layout is upstream's 0.2 one**: the server and its Python in the
  Terminal (`strata: {dir: /opt/strata/terminal, python:
  /opt/strata/terminal/python/bin/python3}`) and the engine in a component
  package, `/opt/strata/components/engine/0.1.48-linux-x86_64-cuda-cuda13/`,
  which is the JSON's `exe` directory and its `lib_dirs`. With the 0.1.x
  paths (`/opt/strata`, `/opt/strata/.venv/bin/python`,
  `/opt/strata/engine/strata`) the backend dies at launch: **moving a CUDA
  node from v2.11.0-beta1's image means editing its `strata:` block and
  JSON in the same step.**
- **No curl and no `python3` on PATH.** A compose healthcheck has to use
  `/opt/strata/terminal/python/bin/python3` (urllib) instead.
- Served under viiwork on an RTX A4000 on 2026-10-24 (load about 50 s;
  chat, tool call and JSON output correct; decode 46 to 53 tok/s once the
  expert cache was warm, the same as the kv_share fork on the card beside
  it). It needs GPUs granted like the other two NVIDIA images (below).

What an operator will not guess, for either image:

- **The model is not in the image.** Mount the pack, the GGUF shards and the
  MTP layer, and write the Strata JSON config that `models[].path` names.
  Preparing a model is upstream's `setup.py`; viiwork does none of it.
- **With `STRATA_ARENA_MMAP=1` the first start writes a 24 GB `experts.bin`
  into the pack**, as root, and takes about 6 minutes instead of 3.5. The pack
  volume must be writable, and the JSON's `args` need `--pcie-frac 0`.
  Backends of one model share that file; two loaded used 36 GB of host RAM.
- Strata wants `ipc: host` and `ulimits.memlock: -1`; the compose example sets
  both.

`docker/compose.strata.yaml` is a whole gfx906 node on this image.

## Running either engine natively

Neither engine has to be in a container — `models[].vllm.binary` and
`models[].freetoken.binary` take an absolute path, so a virtualenv per engine
works and is the simpler shape for a FreeToken host. Two things bite, both
found bringing this up on teddy:

- **Python must be older than 3.15.** vLLM 0.32.0 requires `>=3.11,<3.15` (0.11.2
  required `<3.14`), and a host whose `python3` is newer cannot install it at all — pip reports only that
  no version satisfies the requirement, without saying why. `uv python install
  3.12` and `uv venv --python 3.12` is the quickest fix and touches no system
  package. Give each engine its own venv: they pull different torch builds.
- **FreeToken needs `ninja` and `nvcc` on the BACKEND's PATH**, not just yours.
  It shells out to both by name to JIT-compile kernels, and without them the
  backend dies with `FileNotFoundError: [Errno 2] No such file or directory:
  'ninja'` after the API server has already started — so the failure looks like
  a crash on first load rather than a missing dependency. viiwork passes
  `models[].env` to the child, so:

  ```yaml
  env:
    PATH: /opt/engines/freetoken/bin:/usr/local/cuda/bin:/usr/local/bin:/usr/bin:/bin
  ```

  This is the host-side twin of why `docker/Dockerfile.freetoken` needs a
  *devel* CUDA base: the toolchain is a run-time dependency, not a build-time
  one.

## GPU access for the two NVIDIA images

Neither image contains `nvidia-smi` or `libcuda`, deliberately: the container
runtime injects them from the host so they always match the running driver. A
copy baked in would be whatever was current on build day.

So the container must be granted GPUs explicitly. Two ways, and the choice
matters on a busy machine:

| | Needs | Restarts the Docker daemon |
|---|---|---|
| **CDI** | `nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml` | no |
| nvidia runtime | `nvidia-ctk runtime configure --runtime=docker` | **yes** — bounces every container on the host |

CDI is the better default. Docker 25+ reads `/etc/cdi` and `/var/run/cdi`
natively; `docker info` lists them under "CDI spec directories". Then:

```bash
docker run --rm --device nvidia.com/gpu=all viiwork-vllm nvidia-smi
```

`docker/compose.vllm.yaml` and `docker/compose.freetoken.yaml` use the compose
spelling of the same thing, which needs `capabilities` alongside `device_ids`:

```yaml
deploy:
  resources:
    reservations:
      devices:
        - driver: cdi
          capabilities: [gpu]
          device_ids: ["nvidia.com/gpu=all"]
```

Both compose files also set `ipc: host` — required for vLLM tensor parallelism,
whose shards talk over shared memory — and mount `node.state_dir` as a host
directory so the alias table survives recreating the container. The FreeToken
one additionally mounts the JIT kernel caches (`~/.triton`,
`~/.cache/flashinfer`) and the model cache: without those volumes every new
container recompiles kernels on its first request and re-downloads weights.

## Test images

A bring-up that only needs a **newer upstream llama.cpp** needs no Dockerfile of
its own: `docker/Dockerfile.rocm` takes the ref as a build argument, and any tag
or branch `git clone --branch` accepts will do.

```bash
docker build --build-arg LLAMA_CPP_VERSION=master \
  --build-arg VERSION=$(scripts/version.sh) \
  -t viiwork:llama-master -f docker/Dockerfile.rocm .
```

That image ships `llama-server` and `llama-perplexity`; it does not build
`llama-cli`, which viiwork never runs. For an interactive smoke test, use
`llama-server` directly.

`docker/test/` holds one-off images for what the build argument cannot reach —
today `Dockerfile.k2-test`, which builds a fork at a pinned commit. Same-week
architectures sometimes need an *unmerged* llama.cpp rather than a current one;
pin the PR head in a dedicated test Dockerfile rather than tracking `master`,
and re-pin to a release tag once it merges. They are not part of any release.

The earlier `Dockerfile.qwen-test` and `Dockerfile.granite-test` were
`Dockerfile.rocm` with the ref set to `master` and were removed in favour of the
command above.

## The gfx906 fork track is retired

Until v2.1.0 this file described a second first-class build: `viiwork:gfx906`,
a gfx906-specialised fork of llama.cpp with unused architectures, quant formats
and backends stripped out. It was **retired on 2026-08-30** and removed from
this repo in v2.1.0.

It is worth saying why, because the numbers were good: a 4 h A/B soak measured
**+3.0 % sustained tok/s** with bounded RSS and flat VRAM over 5455 requests.
What killed it was not performance. The fork's base was ~2000 build tags behind,
and the architecture prune that produced the win is exactly what made it unable
to load three of the fleet's five models. No host ran it, and the published repo
advertised a build track that no outside reader could build, because
`Dockerfile.gfx906` needed a `--build-context` pointing at a fork tree that was
never cloned.

The full record — measurements, the phase-2 kernel hard-stop, what was salvaged
— is kept with the project's internal notes rather than here.

Two things still reference the retired image and are left as historical
benchmarking apparatus rather than swept up: the v1 compose files in the
unpublished `configs/private/v1-archive/` and the A/B arms in
`bench-harness/run_feature_soak.sh` and `run_overnight_soak.sh`. They need an
image this repo no longer builds.

## Repo conventions

- **Every Dockerfile lives under `docker/`**, with test images under
  `docker/test/`. Nothing builds from the repo root: `docker build .` with no
  `-f` will not find a Dockerfile, and `make docker` is the documented path.
- `docker-compose.yaml` is the active per-node compose file at the repo root
  (gitignored; copy it from the example below).
- `configs/docker-compose.v2.example.yaml` is the example the README's Quick
  Start copies. It is a whole v2 node: host networking, `pid: host` for the
  on-GPU check, the state directory, and the stop grace a clean drain needs.
- The v1 benchmark and experiment layouts (one `viiwork.*.yaml` and one
  compose file per model) are refused by a v2 node. They are kept, unconverted,
  as benchmark provenance in `configs/private/v1-archive/` together with the
  v1 helpers that drove them (`deploy.sh`, `run-qwen*-test.sh`); that directory
  is never published. A v2 layout is a `models:` entry in the machine's one
  `viiwork.yaml` — see `docs/migrating-to-v2.md`.

## See also

- `docs/adding-an-engine.md` — how an engine and its image get added
- `docs/models.md` — what Strata measured on Radeon VII, and its config
- `bench-harness/README.md` — the harness that produced the numbers above
