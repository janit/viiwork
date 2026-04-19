# Builds

viiwork ships in two parallel builds living in the same repo. Both run the same Go server, the same balancer, the same dashboard, the same API. They differ only in the llama.cpp binary that the server spawns under the hood.

| | Stable foundation | Experimental track |
|---|---|---|
| **Image tag** | `viiwork:latest` (a.k.a. `viiwork`) | `viiwork:gfx906` |
| **Dockerfile** | `Dockerfile` | `Dockerfile.gfx906` |
| **Make target** | `make docker` (alias: `make docker-stable`) | `make docker-gfx906` (alias: `make docker-experimental`) |
| **llama.cpp source** | Upstream `ggml-org/llama.cpp` @ pinned release tag | Local fork tree at `$GFX906_FORK` (default `~/gfx906-work/llama.cpp-gfx906`) |
| **Build dependencies** | Just the repo + Docker | Repo + Docker + the local fork tree (built separately) |
| **Production status** | Default, ships everywhere | Bake-in track, opt-in per node |
| **setup-node.sh choice** | Option 1 (default) | Option 2 |

## Stable foundation — `viiwork:latest`

The default. Standard upstream llama.cpp from `ggml-org/llama.cpp`, built from the repo's `Dockerfile`. Pinned to a specific release tag (currently `b8660`) and patched for the gfx906 FP8 header incompatibility. This is the build every viiwork node has run since day one and what every new node should start on unless there's a reason to do otherwise.

Build: `make docker` or `docker compose up -d` (compose's `build:` directive triggers the build automatically the first time).

## Experimental track — `viiwork:gfx906`

A gfx906-specialized fork of llama.cpp with non-HIP backends, unused model architectures, unused quant formats, half the HIP MMQ instances, the grammar parser, and several sampler strategies removed. The fork is the subject of `docs/superpowers/specs/2026-04-07-gfx906-llama-cpp-fork-design.md` and lives in its own repo (`llama.cpp-gfx906`) on the dev node, not in this tree.

What it has shown so far:

| Measurement | Result | Source |
|---|---|---|
| Single-GPU bench parity vs upstream | -0.07 % (within noise) | `gfx906-work/results/20260408T085054Z-cutover-viiwork-gfx906/` |
| 4 h sustained A/B vs upstream, concurrency 10 | **+3.0 %** sustained tok/s, 0 failures over 5455 vs 5298 requests | milestone tag `milestone/gfx906-fork-4h-soak-2026-04-09` |
| RSS drift over 4 h | Bounded — both builds reach steady state in ~5 min and oscillate inside a ~2 GiB band, no upward trend | same milestone |
| VRAM drift over 4 h | -2 MiB / +0 MiB | same milestone |
| Binary size | -73 % lines in `llama-model.cpp`, -28 % in `llama-sampler.cpp`, ~370 source files removed | commit `49f51d5` |

What it's still missing before promotion:

- **24 h formal soak** under production load to formally clear the spec's `≤5% RSS drift` exit gate. The 4 h A/B soak (bounded RSS, flat VRAM) and the 6 h extreme stress test (4,979 reqs, 0 failures across stable+fork, replica+tensor-split) both showed stable memory — this is likely a formality, not a risk. TODO: run and record.
- **Phase 3 kernel work — PAUSED at hard-stop.** Profile-guided kernel selection showed conservative headroom of only +9 pp (below the +15 % spec floor) after PMC corrections. `mul_mat_vec_q` is at 12 % of HBM peak bandwidth; R-mode (row tensor parallelism) is demoted. The failed experiment images are preserved (`viiwork:gfx906-mmq64-experiment-2026-04-09`, `viiwork:gfx906-hipgraphs-experiment-2026-04-09`). See `docs/superpowers/specs/2026-04-09-gfx906-fork-phase-3-reassessment.md` and its addendum. TODO: decide whether to pursue a different kernel angle or accept that the strip-down is the entire win.
- Canary deploy on one production node alongside the cluster.

Build: `make docker-gfx906` (or the alias `make docker-experimental`). Requires the fork tree at `$GFX906_FORK`. Override with `make docker-gfx906 GFX906_FORK=/path/to/llama.cpp-gfx906` if you keep the fork somewhere else.

## When to use which

- **New node, fresh setup:** stable. Pick option 1 in `setup-node.sh` (or just press Enter — it's the default).
- **Existing production node:** stable. Don't switch unless you're explicitly canarying the experimental build, and only after you've talked to whoever owns that node.
- **Dev / test / one-off bench rig:** either. The experimental build is +3 % sustained throughput and the same memory profile in the soak, so for non-customer-facing work the experimental build is fine and arguably preferable.
- **Canary:** one node on experimental, the rest on stable, watch metrics for 48 h. The `scripts/switch-node-build.sh` helper flips a single node between tracks in place without re-running full setup.

## Switching a running node between tracks

```bash
./scripts/switch-node-build.sh
```

Prompts you for the target build, edits `docker-compose.yaml` in place, and restarts the stack. Bails if the target image doesn't exist locally and tells you how to obtain it.

## Image distribution

The stable image is built from this repo and any node can build it directly. The experimental image is built from a separate fork tree that doesn't live in this repo, so for nodes without that tree the image has to be transferred:

```bash
# On a node that has built it:
docker save viiwork:gfx906 | ssh OTHER_NODE 'docker load'

# Or via a registry once the fork has a published image:
docker pull <registry>/viiwork:gfx906
```

A registry-pushed experimental image is on the to-do list but not yet set up.

## Rollback

Switching back to stable is symmetric:

```bash
./scripts/switch-node-build.sh   # pick option 1
```

Or manually: edit the `image:` line in `docker-compose.yaml` from `viiwork:gfx906` back to `viiwork`, restore the `build: .` line, then `docker compose up -d`. Stable rebuilds from the repo's `Dockerfile` so no cross-node transfer is needed.

## Repo conventions

- The two `Dockerfile`s live at the repo root. Both tracks are first-class citizens.
- `docker-compose.yaml` is the active per-node compose file at the repo root (gitignored, generated by `setup-node.sh`).
- `docker-compose.yaml.example` is the stable-track example at the repo root.
- `configs/docker-compose.gfx906.yaml` is the experimental-track example.
- `configs/docker-compose.soak-{prod,fork}.yaml` are the dedicated A/B soak compose files used by `bench-harness/run_overnight_soak.sh`. Not for normal use.
- All benchmark/experiment compose + viiwork configs live under `configs/`; see `scripts/deploy.sh` for an interactive picker.

## See also

- `docs/superpowers/specs/2026-04-07-gfx906-llama-cpp-fork-design.md` — full design rationale for the experimental track
- `docs/superpowers/plans/2026-04-07-gfx906-fork-phase-0-1.md` — Phase 0+1 implementation plan (the strip-down work that produced `viiwork:gfx906`)
- `bench-harness/README.md` — the harness that produced the milestone numbers above
- Tag `milestone/gfx906-fork-4h-soak-2026-04-09` — committed proof of the 4 h A/B result
