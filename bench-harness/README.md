# bench-harness

Benchmark and soak harness used during the gfx906 llama.cpp fork work.
This is the repo-tracked snapshot of the scripts that live (and run from)
`/home/janit/gfx906-work/bench-harness/` on the production node. Results
land outside the repo at `/home/janit/gfx906-work/results/` because they
are large (per-sample CSVs, full container logs).

## Files

| file | purpose |
|---|---|
| `bench.py`              | Single-shot tok/s + RSS bench (one binary, one workload, N reps). Used during the Phase 1 strip-down to track regressions against the upstream baseline. |
| `runner.py`             | Bench runner: timestamps results, parses llama-server output, writes `summary.json` + `summary.md`. |
| `workloads.py`          | Workload definitions (model path, prompt, gen length, expected output shape). |
| `test_runner.py`        | Unit tests for `runner.py`. |
| `test_workloads.py`     | Unit tests for `workloads.py`. |
| `soak.py`               | Long-duration dual-cluster soak driver (parallel hammer of two viiwork instances at concurrency 1). Original 6h test from 2026-04-08 used this. |
| `soak_one.py`           | Single-cluster soak driver, configurable concurrency, polls per-pid RSS, per-GPU VRAM, and `/v1/status` `total_in_flight`. Imports prompts from `soak.py`. Used for the Apr 8/9 4h+4h A/B that produced milestone tag `milestone/gfx906-fork-4h-soak-2026-04-09`. |
| `soak_compare.py`       | Reads two `soak_one.py` CSVs (prod, fork) and writes a markdown comparison table. |
| `run_overnight_soak.sh` | A/B orchestrator: brings up `viiwork-soak-prod` -> waits for 5/5 healthy -> drives `soak_one.py` for $DURATION -> tears down -> repeats with `viiwork-soak-fork` -> writes `summary.md`. |
| `run_feature_soak.sh`   | Feature exploration soak: iterates a matrix of `backend.extra_args` configs (KV quantization, flash-attn, speculative decoding, prompt cache) over N minutes each, same pool as the A/B soak. |
| `run_feature_smoke.sh`  | Short 2-config smoke of `run_feature_soak.sh` to validate the pipeline before committing hours of GPU time. |
| `run_kv_cache_bench.sh` | 5-phase KV cache bench: baseline -> fa-only -> kv-q8 -> kv-q4 -> fa-kv-q8. Uses `configs/docker-compose.kv-bench.yaml` + `configs/viiwork.kv-bench-*.yaml`. |
| `run_kv_functional.sh`  | Per-phase functional check on a raw llama-server: confirms KV quantization doesn't break generation for a fixed prompt set. |
| `run_kv_perplexity.sh`  | Per-phase perplexity eval on a text corpus via `llama-perplexity`. |
| `common.sh`             | Shared `log()`, `healthy_count()`, `wait_healthy_viiwork()` sourced by the `run_*.sh` scripts. |

## Hardcoded paths to know about

These scripts assume the production node layout. If you're running this
from somewhere else, edit before launch:

- `run_overnight_soak.sh` :: `REPO_DIR=/home/janit/viiwork-private`
- `run_overnight_soak.sh` :: `HARNESS_DIR=/home/janit/gfx906-work/bench-harness`
- `run_overnight_soak.sh` :: `RESULTS_ROOT=/home/janit/gfx906-work/results/soak-ab`
- `soak_one.py`           :: `--out` default `/home/janit/gfx906-work/results/soak`
- `soak.py`               :: `--out` default `/home/janit/gfx906-work/results/soak`

## Typical use

5+5 minute smoke (validates pipeline before committing 8h of GPU time):

```
/home/janit/gfx906-work/bench-harness/run_overnight_soak.sh 5m smoke
```

Real overnight, detached so it survives session close:

```
nohup /home/janit/gfx906-work/bench-harness/run_overnight_soak.sh 4h overnight \
  > /tmp/soak-overnight.log 2>&1 &
disown
```

The orchestrator uses GPUs 5-9 and port 8091 (the soak compose files
under `configs/`) so it does not collide with the production viiwork on
GPUs 0-2 / port 8080. The legacy `viiwork-gfx906` instance on GPUs 4-6
is stopped automatically because it overlaps GPUs 5-6 of the soak pool.
