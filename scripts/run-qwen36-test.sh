#!/usr/bin/env bash
# Qwen3.6-27B bring-up test runner.
#
# Mirrors scripts/run-qwen-test.sh (Qwen3.5-A3B) structure. Runs the full
# sequence and writes a report under bench-harness/results/. Leaves the
# container running on success or failure -- tear down manually with:
#   docker compose -f configs/docker-compose.qwen36-test.yaml down
#
# Safe to run standalone or via scheduler. Uses only read-only rocm-smi
# queries for GPU state (no power or clock changes).

set -u
set -o pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

REPORT_DIR="$REPO_DIR/bench-harness/results"
mkdir -p "$REPORT_DIR"
REPORT="$REPORT_DIR/qwen36-test-$(date +%Y%m%d).txt"
if [[ -e "$REPORT" ]]; then
    REPORT="$REPORT_DIR/qwen36-test-$(date +%Y%m%d-%H%M%S).txt"
fi

log() { printf '[%s] %s\n' "$(date -Iseconds)" "$*" | tee -a "$REPORT" ; }
section() { printf '\n=== %s ===\n' "$*" | tee -a "$REPORT" ; }
die() { log "ABORT: $*"; log "Report: $REPORT"; exit 1; }

section "Qwen3.6-27B bring-up test"
log "repo=$REPO_DIR"
log "report=$REPORT"
log "host=$(hostname) user=$(id -un) date=$(date -Iseconds)"

# ---------- Step 1: ensure no other qwen-test container is up ---------------
section "Step 1: bring down any other qwen-test containers (HSA one-at-a-time)"
for f in \
    configs/docker-compose.qwen-test.yaml \
    configs/docker-compose.qwen-test-q3km.yaml; do
    if [[ -f "$f" ]]; then
        log "compose down: $f"
        docker compose -f "$f" down 2>&1 | tee -a "$REPORT" || true
    fi
done

# ---------- Step 2: GPU idle check ------------------------------------------
# GPUs to test; must match gpus.devices in configs/viiwork.qwen36-test.yaml.
# Idle = VRAM used < 1 GB. We intentionally skip `rocm-smi --showpids`:
# its GPU(s) column reports KFD node IDs, not physical -d indices, so it
# produced false positives when unrelated containers were running on
# other cards (2026-04-22).
TARGET_GPUS=(0 1)
section "Step 2: confirm GPUs ${TARGET_GPUS[*]} idle (VRAM-based)"
rocm-smi --showpids 2>&1 >> "$REPORT" || true

for g in "${TARGET_GPUS[@]}"; do
    vram_json="$(rocm-smi -d "$g" --showmeminfo vram --json 2>/dev/null || echo '{}')"
    used="$(printf '%s' "$vram_json" \
        | jq -r --arg g "card$g" 'try .[$g]["VRAM Total Used Memory (B)"] catch "0" // "0"' 2>/dev/null \
        | head -1)"
    used="${used:-0}"
    log "GPU $g: VRAM used = $used bytes"
    if [[ "$used" =~ ^[0-9]+$ ]] && (( used > 1073741824 )); then
        die "GPU $g has >1 GB VRAM in use; aborting"
    fi
done
log "GPUs ${TARGET_GPUS[*]} idle. Proceeding."

# ---------- Step 3: Ensure GGUF present -------------------------------------
section "Step 3: ensure GGUF present"
GGUF="models/Qwen3.6-27B-Q4_K_M.gguf"
if [[ ! -f "$GGUF" ]]; then
    log "GGUF missing; downloading via scripts/download-qwen36.sh..."
    ./scripts/download-qwen36.sh 2>&1 | tee -a "$REPORT" || die "download failed"
fi
ls -lh "$GGUF" | tee -a "$REPORT"

# ---------- Step 4: Ensure docker image present -----------------------------
section "Step 4: ensure viiwork:qwen-test image present"
if ! docker image inspect viiwork:qwen-test >/dev/null 2>&1; then
    log "Image missing; building (can take 10+ min)..."
    docker build -t viiwork:qwen-test -f Dockerfile.qwen-test . 2>&1 \
        | tee -a "$REPORT" | tail -20
    docker image inspect viiwork:qwen-test >/dev/null 2>&1 || die "docker build failed"
else
    log "Image present. NOTE: if last built before Qwen3.6 arch support landed"
    log "upstream, rebuild with: docker build --no-cache -t viiwork:qwen-test -f Dockerfile.qwen-test ."
fi
docker image inspect --format '{{.Id}} {{.Size}} bytes' viiwork:qwen-test | tee -a "$REPORT"

# ---------- Step 5: compose up ----------------------------------------------
section "Step 5: docker compose up"
docker compose -f configs/docker-compose.qwen36-test.yaml down 2>/dev/null || true
docker compose -f configs/docker-compose.qwen36-test.yaml up -d 2>&1 | tee -a "$REPORT"
sleep 3

# ---------- Step 6: wait for ready (or definite failure) up to 20 min -------
# Qwen3.6-27B cold load on gfx906 is ~6 min per backend. viiwork starts
# backends sequentially (next spawns after prior becomes healthy), so
# with 2 GPUs expect ~12 min before both are up. Budget 20 min.
section "Step 6: wait for server ready (max 1200s)"
DEADLINE=$(( $(date +%s) + 1200 ))
READY=0
LOG_SCRATCH="$(mktemp)"
while (( $(date +%s) < DEADLINE )); do
    docker logs --tail=1000 viiwork-qwen36-test > "$LOG_SCRATCH" 2>&1 || true
    if grep -qE "HTTP server is listening|server is listening|main: server is listening|listening on 0\.0\.0\.0:|all backends started" "$LOG_SCRATCH"; then
        READY=1; break
    fi
    if grep -qiE "unsupported architecture|unknown model architecture|failed to load model|error loading model|segmentation fault|sigsegv|error while loading model|terminate called|no ROCm-capable device" "$LOG_SCRATCH"; then
        READY=2; break
    fi
    sleep 5
done
section "Full container log (tail 1000)"
cat "$LOG_SCRATCH" >> "$REPORT"
rm -f "$LOG_SCRATCH"

case "$READY" in
    1) log "STATUS: READY" ;;
    2) log "STATUS: DEFINITE_FAILURE (error pattern seen in logs)" ;;
    0) log "STATUS: TIMEOUT after 300s" ;;
esac

# ---------- Steps 7 & 8: probe + chat, only if ready ------------------------
if [[ "$READY" == "1" ]]; then
    section "Step 7: /v1/models probe"
    curl -sS --max-time 10 http://localhost:8094/v1/models 2>&1 \
        | tee -a "$REPORT" | jq . 2>/dev/null | tee -a "$REPORT" || log "probe failed"

    section "Step 8: chat completion"
    curl -sS --max-time 180 -i http://localhost:8094/v1/chat/completions \
        -H 'Content-Type: application/json' \
        -d '{"model":"Qwen3.6-27B-Q4_K_M","messages":[{"role":"user","content":"Hello. Reply in one short sentence."}],"max_tokens":64,"temperature":0}' \
        2>&1 | tee -a "$REPORT" || log "chat completion failed"
fi

# ---------- Step 9: rocm-smi snapshot ---------------------------------------
section "Step 9: rocm-smi snapshot"
rocm-smi 2>&1 | tee -a "$REPORT" || true
rocm-smi -d "${TARGET_GPUS[@]}" --showmeminfo vram --showuse 2>&1 | tee -a "$REPORT" || true

# ---------- Step 10: leave container running --------------------------------
section "Done"
log "Container left running. Inspect with:"
log "  docker logs viiwork-qwen36-test"
log "  curl -s http://localhost:8094/v1/models | jq"
log "Tear down when done:"
log "  docker compose -f configs/docker-compose.qwen36-test.yaml down"
log "Report: $REPORT"
