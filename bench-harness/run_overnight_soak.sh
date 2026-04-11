#!/usr/bin/env bash
# Overnight A/B soak orchestrator.
#
# Phase A: bring up viiwork-soak-prod (upstream image, GPUs 5-9, port 8091),
#          run soak_one.py for $DURATION at concurrency 5, tear down.
# Phase B: bring up viiwork-soak-fork (gfx906 fork image, same GPUs/port),
#          run soak_one.py for $DURATION at concurrency 5, tear down.
# Then write a markdown comparison via soak_compare.py.
#
# The two phases are STRICTLY sequential -- they share GPUs and port, so
# the orchestrator guarantees only one container is up at a time. The
# production viiwork on GPUs 0-2 (port 8080) is NOT touched. The legacy
# viiwork-gfx906 on GPUs 4-6 IS torn down at start because it overlaps
# with GPUs 5,6 of the soak pool.
#
# Smoke run:   ./run_overnight_soak.sh 5m
# Real run:    ./run_overnight_soak.sh 4h
#
# Outputs land in /home/janit/gfx906-work/results/soak-ab/<timestamp>/
set -euo pipefail

DURATION="${1:-5m}"
LABEL="${2:-overnight}"
REPO_DIR="/home/janit/viiwork-private"
HARNESS_DIR="/home/janit/gfx906-work/bench-harness"
RESULTS_ROOT="/home/janit/gfx906-work/results/soak-ab"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="${RESULTS_ROOT}/${TS}-${LABEL}-${DURATION}"
mkdir -p "${RUN_DIR}"

PROD_CSV="${RUN_DIR}/prod.csv"
FORK_CSV="${RUN_DIR}/fork.csv"
PROD_LOG="${RUN_DIR}/prod-driver.log"
FORK_LOG="${RUN_DIR}/fork-driver.log"
PROD_CONTAINER_LOG="${RUN_DIR}/prod-container.log"
FORK_CONTAINER_LOG="${RUN_DIR}/fork-container.log"
SUMMARY="${RUN_DIR}/summary.md"

BACKEND_PORTS=(9101 9102 9103 9104 9105)
GPUS=(5 6 7 8 9)
BASE_URL="http://localhost:8091"
# Concurrency 10 against 5 GPUs with max_in_flight_per_gpu=2 (set in
# viiwork.soak.yaml) keeps each GPU with one in-flight + one queued, so
# the handoff gap between requests is closed and we hit ~95% of
# single-GPU peak per GPU. Drives the slot allocator twice as hard per
# unit time as concurrency 5 -- surfaces RSS drift faster.
CONCURRENCY=10
INTERVAL="30s"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*" | tee -a "${RUN_DIR}/run.log"; }

teardown_all() {
  log "teardown: stopping any soak containers"
  # NB: do NOT pass --remove-orphans here. Without explicit project names
  # the soak compose files would share the default `viiwork-private`
  # project with production and a sweep would kill the prod container.
  # Each soak compose file now sets its own `name:` so this is belt-and-
  # braces, but the rule still stands.
  (cd "${REPO_DIR}" && docker compose -f docker-compose.soak-prod.yaml down >/dev/null 2>&1 || true)
  (cd "${REPO_DIR}" && docker compose -f docker-compose.soak-fork.yaml down >/dev/null 2>&1 || true)
}

wait_healthy() {
  # viiwork's manager spawns the 5 llama-server children sequentially,
  # each takes ~60-70 s to load gemma3-26B-A4B-Q3_K_XL into VRAM, so the
  # cluster needs ~5-6 min to reach all-healthy. 600 s timeout gives
  # plenty of headroom.
  local name="$1"
  local deadline=$(( $(date +%s) + 600 ))
  log "waiting for ${name} to report all 5 backends healthy via ${BASE_URL}/v1/status (up to 600s)"
  while (( $(date +%s) < deadline )); do
    if status_json=$(curl -fsS --max-time 3 "${BASE_URL}/v1/status" 2>/dev/null); then
      healthy=$(printf '%s' "$status_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(d.get("healthy_backends", 0))
')
      if [[ "$healthy" -ge 5 ]]; then
        log "${name}: ${healthy}/5 backends healthy"
        return 0
      fi
      log "${name}: ${healthy}/5 backends healthy, waiting..."
    fi
    sleep 10
  done
  log "ERROR: ${name} did not reach 5/5 healthy within 600s"
  docker logs --tail 200 "${name}" 2>&1 | tee -a "${RUN_DIR}/run.log" || true
  return 1
}

run_phase() {
  local phase="$1"        # prod | fork
  local compose="$2"      # docker-compose.soak-prod.yaml
  local container="$3"    # viiwork-soak-prod
  local csv_label="$4"    # prod | fork
  local csv_out="$5"
  local driver_log="$6"
  local container_log="$7"

  log "=== phase ${phase}: bringing up ${container} ==="
  (cd "${REPO_DIR}" && docker compose -f "${compose}" up -d)
  wait_healthy "${container}"

  log "phase ${phase}: starting soak driver, concurrency=${CONCURRENCY} duration=${DURATION}"
  (
    cd "${HARNESS_DIR}"
    python3 -u soak_one.py \
      --label "${csv_label}" \
      --duration "${DURATION}" \
      --interval "${INTERVAL}" \
      --base-url "${BASE_URL}" \
      --backend-ports "${BACKEND_PORTS[@]}" \
      --gpus "${GPUS[@]}" \
      --concurrency "${CONCURRENCY}" \
      --out "${RUN_DIR}"
  ) > "${driver_log}" 2>&1

  # soak_one writes <ts>-<label>.csv into --out; rename to fixed path
  produced=$(ls -1t "${RUN_DIR}"/*-"${csv_label}".csv 2>/dev/null | head -1)
  if [[ -n "${produced}" && "${produced}" != "${csv_out}" ]]; then
    mv "${produced}" "${csv_out}"
  fi

  log "phase ${phase}: capturing container logs"
  docker logs "${container}" > "${container_log}" 2>&1 || true

  log "phase ${phase}: tearing down ${container}"
  (cd "${REPO_DIR}" && docker compose -f "${compose}" down)
}

trap 'rc=$?; log "trap fired (rc=$rc), tearing down"; teardown_all; exit $rc' INT TERM ERR

log "overnight soak start: duration=${DURATION} label=${LABEL} run_dir=${RUN_DIR}"
log "GPU pool: ${GPUS[*]}; backend ports: ${BACKEND_PORTS[*]}; concurrency=${CONCURRENCY}"

# Pre-flight: stop legacy viiwork-gfx906 (uses GPUs 4-6, overlaps soak pool)
# and any leftover soak containers. Production viiwork (GPUs 0-2) untouched.
if docker ps --format '{{.Names}}' | grep -qx viiwork-gfx906; then
  log "stopping legacy viiwork-gfx906 (uses GPUs 4-6, overlaps soak pool)"
  (cd "${REPO_DIR}" && docker compose -f docker-compose.gfx906.yaml down)
fi
teardown_all

run_phase prod docker-compose.soak-prod.yaml viiwork-soak-prod prod \
  "${PROD_CSV}" "${PROD_LOG}" "${PROD_CONTAINER_LOG}"

run_phase fork docker-compose.soak-fork.yaml viiwork-soak-fork fork \
  "${FORK_CSV}" "${FORK_LOG}" "${FORK_CONTAINER_LOG}"

log "writing comparison summary"
python3 "${HARNESS_DIR}/soak_compare.py" "${PROD_CSV}" "${FORK_CSV}" > "${SUMMARY}"

log "done. summary: ${SUMMARY}"
cat "${SUMMARY}"
