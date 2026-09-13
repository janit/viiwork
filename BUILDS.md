# Builds

viiwork is one Go binary that supervises inference engines. An image is that
binary dropped into a base that carries an engine's runtime — viiwork compiles
against none of them, because it only ever spawns and supervises, so **pinning
the base image is how the inference stack gets pinned.**

One image per engine, all under `docker/`, each named for what it carries.

| Image | Dockerfile | Engine | Make target |
|---|---|---|---|
| `viiwork:latest` (a.k.a. `viiwork`) | `docker/Dockerfile.rocm` | `llamacpp` on ROCm / gfx906 | `make docker-rocm` (aliases: `make docker`, `make docker-stable`) |
| — | `docker/Dockerfile.vllm` | `vllm` | `make docker-vllm` — **arrives in v2.2.0** |
| — | `docker/Dockerfile.freetoken` | `freetoken` | `make docker-freetoken` — **arrives in v2.2.0** |

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
