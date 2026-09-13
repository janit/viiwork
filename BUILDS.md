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

**Named for the engine, never for a GPU vendor.** vLLM already has a ROCm build
and FreeToken may add AMD or Intel, so a second accelerator backend is a sibling
base image, not a fork — and `Spec.Vendor` is informational rather than a gate.

`make docker` builds the ROCm image. That is deliberate rather than historical:
Radeon VII is the core of this fleet, and the unqualified target pointing at it
is right. What was wrong before v2.1.0 was the *name* — a root `Dockerfile`
that silently meant "llama.cpp for gfx906" as though no other kind existed.

## `docker/Dockerfile.rocm`

Standard upstream llama.cpp from `ggml-org/llama.cpp`, pinned to a release tag
(currently `b10437`, which carries Qwen3.5/3.6/3.8 hybrid DeltaNet support and
MTP speculative decoding) and patched for the gfx906 FP8 header
incompatibility. Built on `rocm/dev-ubuntu-24.04:6.2.4-complete` — the last
ROCm with reliable gfx906 support.

**The pin must stay at or above `b10430`:** the Qwen3.8 quants were cut with it,
and the older `b9222` rejects that architecture at load time with "unknown model
architecture" rather than anything self-explanatory.

Build: `make docker` (or `docker compose up -d`, whose `build:` directive
triggers it the first time).

## `docker/Dockerfile.vllm`

The binary dropped into vLLM's own published image, `vllm/vllm-openai`, pinned
by `VLLM_VERSION` (currently `v0.11.2`). Nothing is rebuilt: that image already
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

## Running either engine natively

Neither engine has to be in a container — `models[].vllm.binary` and
`models[].freetoken.binary` take an absolute path, so a virtualenv per engine
works and is the simpler shape for a FreeToken host. Two things bite, both
found bringing this up on teddy:

- **Python must be older than 3.14.** vLLM 0.11.2 requires `>=3.10,<3.14`, and a
  host whose `python3` is 3.14 cannot install it at all — pip reports only that
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
one additionally mounts the JIT kernel cache and the model cache: without those
two volumes every container start recompiles kernels and re-downloads weights.

## Test images

`docker/test/` holds one-off images that pin an unmerged llama.cpp PR for a
model bring-up — `Dockerfile.qwen-test`, `Dockerfile.granite-test`,
`Dockerfile.k2-test`. Same-week architectures sometimes need an *unmerged*
llama.cpp rather than a current one; pin the PR head in a dedicated test
Dockerfile rather than tracking `master`, and re-pin to a release tag once it
merges. They are not part of any release.

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
benchmarking apparatus rather than swept up: `configs/docker-compose.gfx906.yaml`
and the A/B arms in `bench-harness/run_feature_soak.sh` and
`run_overnight_soak.sh`. They need an image this repo no longer builds.

## Repo conventions

- **Every Dockerfile lives under `docker/`**, with test images under
  `docker/test/`. Nothing builds from the repo root: `docker build .` with no
  `-f` will not find a Dockerfile, and `make docker` is the documented path.
- `docker-compose.yaml` is the active per-node compose file at the repo root
  (gitignored; copy it from the example below).
- `configs/docker-compose.v2.example.yaml` is the example the README's Quick
  Start copies. It is a whole v2 node: host networking, `pid: host` for the
  on-GPU check, the state directory, and the stop grace a clean drain needs.
- All benchmark and experiment compose files and viiwork configs live under
  `configs/`; see `scripts/deploy.sh` for an interactive picker.

## See also

- `docs/adding-an-engine.md` — how an engine and its image get added
- `bench-harness/README.md` — the harness that produced the numbers above
