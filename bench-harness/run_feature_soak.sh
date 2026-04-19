#!/usr/bin/env bash
# Feature exploration soak: tests multiple llama-server flag combinations
# sequentially against the gfx906 fork image to find throughput improvements.
#
# Each config gets a 30-minute soak at concurrency 10 on 5 GPUs. The baseline
# is the current production config (no extra KV flags). Results land in
# /home/janit/gfx906-work/results/feature-explore/<timestamp>/
#
# Usage:
#   ./run_feature_soak.sh [duration_per_config]
#   ./run_feature_soak.sh 30m          # 30 min per config (default)
#   ./run_feature_soak.sh 5m           # quick smoke test
#
# Estimated total time at 30m: ~10 configs * (6min startup + 30min soak + 1min teardown) ~= 6h
set -euo pipefail

DURATION="${1:-30m}"
REPO_DIR="/home/janit/viiwork-private"
HARNESS_DIR="/home/janit/gfx906-work/bench-harness"
RESULTS_ROOT="/home/janit/gfx906-work/results/feature-explore"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${RESULTS_ROOT}/${TS}-feature-explore-${DURATION}"
mkdir -p "${RUN_DIR}"

IMAGE="viiwork:gfx906"
CONTAINER_NAME="viiwork-feature-test"
BASE_URL="http://localhost:8091"
BACKEND_PORTS=(9101 9102 9103 9104 9105)
GPUS=(5 6 7 8 9)
CONCURRENCY=10
INTERVAL="30s"

# Config matrix: name -> extra_args to pass to llama-server
# Each config is tested in sequence. The viiwork.yaml extra_args field
# is overridden per-config by generating a temporary config file.
declare -A CONFIGS
CONFIGS=(
  # Baseline: current production flags
  ["01-baseline"]='["--reasoning-format", "deepseek"]'
  # KV cache quantization: q8_0 (mild compression, likely lossless)
  ["02-kv-q8"]='["--reasoning-format", "deepseek", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0"]'
  # KV cache quantization: q4_0 (aggressive, ~4x less KV traffic)
  ["03-kv-q4"]='["--reasoning-format", "deepseek", "--cache-type-k", "q4_0", "--cache-type-v", "q4_0"]'
  # KV cache quantization: iq4_nl (information-optimal 4-bit)
  ["04-kv-iq4nl"]='["--reasoning-format", "deepseek", "--cache-type-k", "iq4_nl", "--cache-type-v", "iq4_nl"]'
  # KV cache: q5_1 keys (preserves attention quality), q4_0 values (aggressive)
  ["05-kv-q5k-q4v"]='["--reasoning-format", "deepseek", "--cache-type-k", "q5_1", "--cache-type-v", "q4_0"]'
  # Flash attention explicitly on + KV q4_0
  ["06-fa-on-kv-q4"]='["--reasoning-format", "deepseek", "--flash-attn", "on", "--cache-type-k", "q4_0", "--cache-type-v", "q4_0"]'
  # N-gram speculative decoding (draftless, pattern-matching on generated tokens)
  ["07-spec-ngram"]='["--reasoning-format", "deepseek", "--spec-type", "ngram-mod", "--draft-max", "16", "--draft-p-min", "0.75"]'
  # N-gram spec + KV q4_0 (stack both bandwidth wins)
  ["08-spec-kv-q4"]='["--reasoning-format", "deepseek", "--spec-type", "ngram-mod", "--draft-max", "16", "--draft-p-min", "0.75", "--cache-type-k", "q4_0", "--cache-type-v", "q4_0"]'
  # Host-memory prompt caching (reduces TTFT for repeated prefixes)
  ["09-cram"]='["--reasoning-format", "deepseek", "--cram", "2048"]'
  # Combined: cram + KV q4_0 + spec ngram (all three stacked)
  ["10-full-stack"]='["--reasoning-format", "deepseek", "--cram", "2048", "--cache-type-k", "q4_0", "--cache-type-v", "q4_0", "--spec-type", "ngram-mod", "--draft-max", "16", "--draft-p-min", "0.75"]'
)

# Sorted config names for deterministic order
SORTED_CONFIGS=($(printf '%s\n' "${!CONFIGS[@]}" | sort))

LOG_FILE="${RUN_DIR}/run.log"
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

generate_config() {
  local extra_args="$1"
  local config_file="${RUN_DIR}/viiwork-test.yaml"
  cat > "${config_file}" <<YAML
server:
  host: 0.0.0.0
  port: 8091

model:
  path: /models/gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf
  context_size: 13337
  n_gpu_layers: -1

gpus:
  devices: [5, 6, 7, 8, 9]
  base_port: 9101

backend:
  binary: llama-server
  extra_args: ${extra_args}

health:
  interval: 5s
  timeout: 3s
  max_failures: 3

balancer:
  strategy: adaptive
  latency_window: 30s
  high_load_threshold: 14
  max_in_flight_per_gpu: 2
YAML
  echo "${config_file}"
}

teardown() {
  log "teardown: stopping feature test container"
  docker stop "${CONTAINER_NAME}" >/dev/null 2>&1 || true
  docker rm "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}

wait_healthy() {
  if wait_healthy_viiwork "${BASE_URL}" 5 600 "${CONTAINER_NAME}"; then
    return 0
  fi
  docker logs --tail 100 "${CONTAINER_NAME}" 2>&1 | tee -a "${RUN_DIR}/run.log" || true
  return 1
}

run_config() {
  local name="$1"
  local extra_args="$2"
  local config_dir="${RUN_DIR}/${name}"
  mkdir -p "${config_dir}"

  log "=== config ${name}: extra_args=${extra_args} ==="

  # Generate config
  local config_file
  config_file=$(generate_config "${extra_args}")

  # Start container
  teardown
  docker run -d \
    --name "${CONTAINER_NAME}" \
    --network host \
    --device /dev/kfd:/dev/kfd \
    --device /dev/dri:/dev/dri \
    --group-add video \
    --group-add render \
    -v "${REPO_DIR}/models:/models" \
    -v "${config_file}:/etc/viiwork/viiwork.yaml" \
    "${IMAGE}"

  if ! wait_healthy; then
    log "SKIP ${name}: failed to start healthy"
    docker logs "${CONTAINER_NAME}" > "${config_dir}/container.log" 2>&1 || true
    teardown
    return 0
  fi

  # Run soak
  log "config ${name}: starting soak driver, concurrency=${CONCURRENCY} duration=${DURATION}"
  local driver_log="${config_dir}/driver.log"
  (
    cd "${HARNESS_DIR}"
    python3 -u soak_one.py \
      --label "${name}" \
      --duration "${DURATION}" \
      --interval "${INTERVAL}" \
      --base-url "${BASE_URL}" \
      --backend-ports "${BACKEND_PORTS[@]}" \
      --gpus "${GPUS[@]}" \
      --concurrency "${CONCURRENCY}" \
      --out "${config_dir}"
  ) > "${driver_log}" 2>&1

  # Capture logs
  docker logs "${CONTAINER_NAME}" > "${config_dir}/container.log" 2>&1 || true

  # Teardown
  log "config ${name}: done, tearing down"
  teardown
}

trap 'rc=$?; log "trap fired (rc=$rc), tearing down"; teardown; exit $rc' INT TERM ERR

log "feature exploration soak start: ${#SORTED_CONFIGS[@]} configs x ${DURATION} each"
log "GPU pool: ${GPUS[*]}; concurrency=${CONCURRENCY}; image=${IMAGE}"
log "configs: ${SORTED_CONFIGS[*]}"

# Pre-flight: stop anything on the soak GPUs
if docker ps --format '{{.Names}}' | grep -qx viiwork-gfx906; then
  log "stopping legacy viiwork-gfx906 (overlaps soak GPUs)"
  (cd "${REPO_DIR}" && docker compose -f configs/docker-compose.gfx906.yaml down 2>/dev/null || true)
fi
(cd "${REPO_DIR}" && docker compose -f configs/docker-compose.soak-prod.yaml down 2>/dev/null || true)
(cd "${REPO_DIR}" && docker compose -f configs/docker-compose.soak-fork.yaml down 2>/dev/null || true)
teardown

for name in "${SORTED_CONFIGS[@]}"; do
  run_config "${name}" "${CONFIGS[${name}]}"
done

# Write comparison summary
log "writing comparison summary"
python3 - "${RUN_DIR}" <<'PYTHON'
import csv
import sys
from pathlib import Path

run_dir = Path(sys.argv[1])
results = []

for config_dir in sorted(run_dir.iterdir()):
    if not config_dir.is_dir():
        continue
    csvs = list(config_dir.glob("*.csv"))
    if not csvs:
        continue
    csv_file = csvs[0]
    rows = list(csv.DictReader(open(csv_file)))
    if not rows:
        continue

    tok_samples = [float(r.get("tok_s", 0)) for r in rows if float(r.get("tok_s", 0)) > 0]
    req_ok = sum(int(r.get("req_ok_delta", 0)) for r in rows)
    req_fail = sum(int(r.get("req_fail_delta", 0)) for r in rows)

    if tok_samples:
        tok_mean = sum(tok_samples) / len(tok_samples)
        tok_sorted = sorted(tok_samples)
        tok_median = tok_sorted[len(tok_sorted) // 2]
        tok_p5 = tok_sorted[int(len(tok_sorted) * 0.05)]
        tok_p95 = tok_sorted[int(len(tok_sorted) * 0.95)]
    else:
        tok_mean = tok_median = tok_p5 = tok_p95 = 0

    # RSS drift
    rss_vals = [int(r.get("rss_total_mb", 0)) for r in rows if int(r.get("rss_total_mb", 0)) > 0]
    rss_start = rss_vals[0] if rss_vals else 0
    rss_end = rss_vals[-1] if rss_vals else 0
    rss_drift = rss_end - rss_start

    # VRAM drift
    vram_vals = [int(r.get("vram_total_mb", 0)) for r in rows if int(r.get("vram_total_mb", 0)) > 0]
    vram_start = vram_vals[0] if vram_vals else 0
    vram_end = vram_vals[-1] if vram_vals else 0
    vram_drift = vram_end - vram_start

    results.append({
        "config": config_dir.name,
        "req_ok": req_ok,
        "req_fail": req_fail,
        "tok_mean": tok_mean,
        "tok_median": tok_median,
        "tok_p5": tok_p5,
        "tok_p95": tok_p95,
        "rss_drift_mb": rss_drift,
        "vram_drift_mb": vram_drift,
    })

if not results:
    print("No results found.")
    sys.exit(0)

baseline = next((r for r in results if "baseline" in r["config"]), results[0])

print("# Feature Exploration Results\n")
print(f"| config | req_ok | fails | tok/s mean | tok/s median | tok/s p5 | tok/s p95 | delta vs baseline | RSS drift | VRAM drift |")
print(f"|---|---|---|---|---|---|---|---|---|---|")
for r in results:
    delta = ((r["tok_mean"] / baseline["tok_mean"]) - 1) * 100 if baseline["tok_mean"] > 0 else 0
    delta_str = f"+{delta:.1f}%" if delta >= 0 else f"{delta:.1f}%"
    print(f"| {r['config']} | {r['req_ok']} | {r['req_fail']} | {r['tok_mean']:.1f} | {r['tok_median']:.1f} | {r['tok_p5']:.1f} | {r['tok_p95']:.1f} | {delta_str} | {r['rss_drift_mb']:+d} MiB | {r['vram_drift_mb']:+d} MiB |")

print(f"\nBaseline tok/s mean: {baseline['tok_mean']:.1f}")
PYTHON

log "done. results in ${RUN_DIR}"
