#!/usr/bin/env bash
# KV cache optimization A/B/C/D benchmark.
#
# Tests 5 configurations sequentially on GPUs 5-9 (port 8091):
#   1. baseline  — no KV quantization, no flash attention
#   2. fa-only   — -fa only (isolates flash attention effect)
#   3. kv-q8     — --cache-type-k q8_0 --cache-type-v q8_0
#   4. kv-q4     — --cache-type-k q4_0 --cache-type-v q4_0
#   5. fa-kv-q8  — -fa --cache-type-k q8_0 --cache-type-v q8_0
#
# Each phase runs soak_one.py for $DURATION at concurrency 10 against
# 5 GPUs, sampling RSS/VRAM/tok-s every 30s. Phases are strictly
# sequential (shared GPUs and port).
#
# Usage:
#   ./run_kv_cache_bench.sh [DURATION]
#   ./run_kv_cache_bench.sh 1h          # real run (~5.5h total with startup)
#   ./run_kv_cache_bench.sh 10m         # smoke test (~1h total)
#
# Outputs: /home/janit/kv-bench-results/<timestamp>/
set -euo pipefail

DURATION="${1:-1h}"
REPO_DIR="/home/janit/viiwork-private"
HARNESS_DIR="${REPO_DIR}/bench-harness"
RESULTS_ROOT="/home/janit/kv-bench-results"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${RESULTS_ROOT}/${TS}-kv-bench-${DURATION}"
mkdir -p "${RUN_DIR}"

BACKEND_PORTS=(9101 9102 9103 9104 9105)
GPUS=(5 6 7 8 9)
BASE_URL="http://localhost:8091"
CONCURRENCY=10
INTERVAL="30s"
COMPOSE_FILE="configs/docker-compose.kv-bench.yaml"

# The 5 test configurations: label -> yaml config filename (relative to configs/,
# since the compose file's volume mount is resolved from its own directory)
declare -a PHASES=(baseline fa-only kv-q8 kv-q4 fa-kv-q8)
declare -A CONFIGS=(
  [baseline]="viiwork.kv-bench-baseline.yaml"
  [fa-only]="viiwork.kv-bench-fa-only.yaml"
  [kv-q8]="viiwork.kv-bench-q8.yaml"
  [kv-q4]="viiwork.kv-bench-q4.yaml"
  [fa-kv-q8]="viiwork.kv-bench-fa-q8.yaml"
)

LOG_FILE="${RUN_DIR}/run.log"
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

teardown() {
  log "teardown: stopping kv-bench container"
  (cd "${REPO_DIR}" && docker compose -f "${COMPOSE_FILE}" down >/dev/null 2>&1 || true)
}

wait_healthy() {
  local label="$1"
  if wait_healthy_viiwork "${BASE_URL}" 5 600 "${label}"; then
    return 0
  fi
  docker logs --tail 200 viiwork-kv-bench 2>&1 | tee -a "${RUN_DIR}/run.log" || true
  return 1
}

run_phase() {
  local label="$1"
  local config="$2"
  local csv_out="${RUN_DIR}/${label}.csv"
  local driver_log="${RUN_DIR}/${label}-driver.log"
  local container_log="${RUN_DIR}/${label}-container.log"

  log "=== phase ${label}: config=${config} ==="

  # Bring up container with the right config
  (cd "${REPO_DIR}" && VIIWORK_KV_BENCH_CONFIG="${config}" docker compose -f "${COMPOSE_FILE}" up -d)

  if ! wait_healthy "${label}"; then
    log "SKIP ${label}: failed to reach healthy state"
    docker logs viiwork-kv-bench > "${container_log}" 2>&1 || true
    (cd "${REPO_DIR}" && docker compose -f "${COMPOSE_FILE}" down)
    # Write a marker so the comparison script knows this phase failed
    echo "FAILED_TO_START" > "${csv_out}"
    return 0  # don't abort the whole run
  fi

  log "phase ${label}: starting soak, concurrency=${CONCURRENCY} duration=${DURATION}"
  (
    cd "${HARNESS_DIR}"
    python3 -u soak_one.py \
      --label "${label}" \
      --duration "${DURATION}" \
      --interval "${INTERVAL}" \
      --base-url "${BASE_URL}" \
      --backend-ports ${BACKEND_PORTS[@]} \
      --gpus ${GPUS[@]} \
      --concurrency "${CONCURRENCY}" \
      --out "${RUN_DIR}"
  ) > "${driver_log}" 2>&1

  # soak_one writes <ts>-<label>.csv; rename to fixed path
  produced=$(ls -1t "${RUN_DIR}"/*-"${label}".csv 2>/dev/null | head -1)
  if [[ -n "${produced}" && "${produced}" != "${csv_out}" ]]; then
    mv "${produced}" "${csv_out}"
  fi

  log "phase ${label}: capturing container logs"
  docker logs viiwork-kv-bench > "${container_log}" 2>&1 || true

  log "phase ${label}: tearing down"
  (cd "${REPO_DIR}" && docker compose -f "${COMPOSE_FILE}" down)

  # Brief cooldown between phases for GPU thermals
  log "phase ${label}: done, 30s cooldown"
  sleep 30
}

write_summary() {
  local out="${RUN_DIR}/summary.md"
  log "writing comparison summary"
  python3 - "${RUN_DIR}" > "${out}" <<'PYEOF'
import csv, json, sys, os
from pathlib import Path

run_dir = Path(sys.argv[1])
phases = ["baseline", "fa-only", "kv-q8", "kv-q4", "fa-kv-q8"]

print("# KV Cache Benchmark Results\n")
print(f"Run directory: `{run_dir}`\n")

results = {}
for phase in phases:
    csv_path = run_dir / f"{phase}.csv"
    if not csv_path.exists() or csv_path.read_text().strip() == "FAILED_TO_START":
        print(f"## {phase}: FAILED TO START\n")
        results[phase] = None
        continue

    rows = []
    with open(csv_path) as f:
        reader = csv.DictReader(f)
        for row in reader:
            rows.append(row)

    if not rows:
        print(f"## {phase}: NO DATA\n")
        results[phase] = None
        continue

    tps_vals = [float(r["tps_window"]) for r in rows if float(r.get("tps_window", 0)) > 0]
    rss_vals = [int(r["rss_mb"]) for r in rows if int(r.get("rss_mb", 0)) > 0]
    vram_vals = [int(r["vram_mb"]) for r in rows if int(r.get("vram_mb", 0)) > 0]

    req_ok = int(rows[-1].get("req_ok", 0))
    req_fail = int(rows[-1].get("req_fail", 0))

    results[phase] = {
        "tps_mean": sum(tps_vals) / len(tps_vals) if tps_vals else 0,
        "tps_p50": sorted(tps_vals)[len(tps_vals)//2] if tps_vals else 0,
        "tps_min": min(tps_vals) if tps_vals else 0,
        "tps_max": max(tps_vals) if tps_vals else 0,
        "rss_start": rss_vals[0] if rss_vals else 0,
        "rss_end": rss_vals[-1] if rss_vals else 0,
        "rss_max": max(rss_vals) if rss_vals else 0,
        "vram_start": vram_vals[0] if vram_vals else 0,
        "vram_end": vram_vals[-1] if vram_vals else 0,
        "vram_max": max(vram_vals) if vram_vals else 0,
        "req_ok": req_ok,
        "req_fail": req_fail,
        "samples": len(rows),
    }

# Comparison table
print("## Throughput Comparison\n")
print("| Config | Mean tok/s | P50 tok/s | Min tok/s | Max tok/s | Requests | Failures |")
print("|--------|-----------|-----------|-----------|-----------|----------|----------|")
for phase in phases:
    r = results.get(phase)
    if r is None:
        print(f"| {phase} | FAILED | - | - | - | - | - |")
    else:
        print(f"| {phase} | {r['tps_mean']:.1f} | {r['tps_p50']:.1f} | {r['tps_min']:.1f} | {r['tps_max']:.1f} | {r['req_ok']} | {r['req_fail']} |")

# Relative to baseline
baseline = results.get("baseline")
if baseline and baseline["tps_mean"] > 0:
    print("\n## Relative to Baseline\n")
    print("| Config | tok/s delta | VRAM delta (end) |")
    print("|--------|------------|-----------------|")
    for phase in phases:
        r = results.get(phase)
        if r is None:
            print(f"| {phase} | FAILED | - |")
        else:
            tps_delta = ((r["tps_mean"] / baseline["tps_mean"]) - 1) * 100
            vram_delta = r["vram_end"] - baseline["vram_end"]
            sign = "+" if tps_delta >= 0 else ""
            vsign = "+" if vram_delta >= 0 else ""
            print(f"| {phase} | {sign}{tps_delta:.1f}% | {vsign}{vram_delta} MiB |")

# Memory table
print("\n## Memory (RSS + VRAM, all 5 GPUs)\n")
print("| Config | RSS start | RSS end | RSS max | VRAM start | VRAM end | VRAM max |")
print("|--------|-----------|---------|---------|------------|----------|----------|")
for phase in phases:
    r = results.get(phase)
    if r is None:
        print(f"| {phase} | FAILED | - | - | - | - | - |")
    else:
        print(f"| {phase} | {r['rss_start']} MiB | {r['rss_end']} MiB | {r['rss_max']} MiB | {r['vram_start']} MiB | {r['vram_end']} MiB | {r['vram_max']} MiB |")

print()
PYEOF
  cat "${out}"
}

trap 'rc=$?; log "trap fired (rc=$rc), tearing down"; teardown; exit $rc' INT TERM ERR

log "KV cache benchmark start: duration=${DURATION} run_dir=${RUN_DIR}"
log "GPU pool: ${GPUS[*]}; backend ports: ${BACKEND_PORTS[*]}; concurrency=${CONCURRENCY}"
log "Phases: ${PHASES[*]}"

# Pre-flight: stop any overlapping containers
teardown
# Also stop legacy soak containers if they exist
(cd "${REPO_DIR}" && docker compose -f configs/docker-compose.soak-prod.yaml down >/dev/null 2>&1 || true)
(cd "${REPO_DIR}" && docker compose -f configs/docker-compose.soak-fork.yaml down >/dev/null 2>&1 || true)

for phase in "${PHASES[@]}"; do
  run_phase "${phase}" "${CONFIGS[$phase]}"
done

write_summary

log "KV cache benchmark complete. Results: ${RUN_DIR}/"
