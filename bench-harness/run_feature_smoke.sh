#!/usr/bin/env bash
# Quick 2-config smoke test to validate the feature soak harness.
# Tests baseline + kv-q4 at 5m each. ~25 min total.
set -euo pipefail

DURATION="5m"
REPO_DIR="/home/janit/viiwork-private"
HARNESS_DIR="/home/janit/gfx906-work/bench-harness"
RESULTS_ROOT="/home/janit/gfx906-work/results/feature-explore"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${RESULTS_ROOT}/${TS}-smoke-2config-${DURATION}"
mkdir -p "${RUN_DIR}"

IMAGE="viiwork:gfx906"
CONTAINER_NAME="viiwork-feature-test"
BASE_URL="http://localhost:8091"
BACKEND_PORTS=(9101 9102 9103 9104 9105)
GPUS=(5 6 7 8 9)
CONCURRENCY=10
INTERVAL="30s"

declare -A CONFIGS
CONFIGS=(
  ["01-baseline"]='["--reasoning-format", "deepseek"]'
  ["03-kv-q4"]='["--reasoning-format", "deepseek", "--cache-type-k", "q4_0", "--cache-type-v", "q4_0"]'
)

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

  local config_file
  config_file=$(generate_config "${extra_args}")

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

  docker logs "${CONTAINER_NAME}" > "${config_dir}/container.log" 2>&1 || true
  log "config ${name}: done, tearing down"
  teardown
}

trap 'rc=$?; log "trap fired (rc=$rc), tearing down"; teardown; exit $rc' INT TERM ERR

log "smoke test: ${#SORTED_CONFIGS[@]} configs x ${DURATION}"
log "configs: ${SORTED_CONFIGS[*]}"

teardown

for name in "${SORTED_CONFIGS[@]}"; do
  run_config "${name}" "${CONFIGS[${name}]}"
done

log "smoke complete. results in ${RUN_DIR}"
ls -la "${RUN_DIR}"/*/
