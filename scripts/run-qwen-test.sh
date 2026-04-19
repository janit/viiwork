#!/usr/bin/env bash
# Qwen3.5-35B-A3B bring-up test runner.
#
# Runs the full 10-step sequence and writes a full report under
# bench-harness/results/. Leaves the container running on success or
# failure -- tear down manually with:
#   docker compose -f configs/docker-compose.qwen-test.yaml down
#
# Safe to run standalone or via scheduler. Uses only read-only rocm-smi
# queries for GPU state (no power or clock changes).

set -u
set -o pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

REPORT_DIR="$REPO_DIR/bench-harness/results"
mkdir -p "$REPORT_DIR"
REPORT="$REPORT_DIR/qwen-test-$(date +%Y%m%d).txt"
if [[ -e "$REPORT" ]]; then
    REPORT="$REPORT_DIR/qwen-test-$(date +%Y%m%d-%H%M%S).txt"
fi

log() { printf '[%s] %s\n' "$(date -Iseconds)" "$*" | tee -a "$REPORT" ; }
section() { printf '\n=== %s ===\n' "$*" | tee -a "$REPORT" ; }
die() { log "ABORT: $*"; log "Report: $REPORT"; exit 1; }

section "Qwen3.5-35B-A3B bring-up test"
log "repo=$REPO_DIR"
log "report=$REPORT"
log "host=$(hostname) user=$(id -un) date=$(date -Iseconds)"

# ---------- Step 2: GPU idle check ------------------------------------------
section "Step 2: confirm GPUs 7-9 idle"
PID_OUT="$(rocm-smi --showpids 2>&1 || true)"
printf '%s\n' "$PID_OUT" >> "$REPORT"
# Parse: look for any line where the GPU(s) column references 7, 8, or 9.
busy_via_pids="$(printf '%s\n' "$PID_OUT" \
    | awk '/^[[:space:]]*[0-9]+[[:space:]]+[^[:space:]]+/ {print}' \
    | awk '{for (i=3; i<=NF; i++) if ($i ~ /[789]/) print $i}' \
    | tr ',' '\n' | awk '/^[789]$/' | sort -u | tr '\n' ' ')"
if [[ -n "${busy_via_pids// }" ]]; then
    log "FAIL: GPUs busy per rocm-smi --showpids: $busy_via_pids"
    die "GPUs 7/8/9 not idle"
fi

# VRAM cross-check (> 1 GB on any target GPU means something is loaded there).
for g in 7 8 9; do
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
log "GPUs 7-9 idle. Proceeding."

# ---------- Step 3: Ensure GGUF present -------------------------------------
section "Step 3: ensure GGUF present"
GGUF="models/Qwen3.5-35B-A3B-Q4_K_M.gguf"
if [[ ! -f "$GGUF" ]]; then
    log "GGUF missing; downloading via scripts/download-qwen35.sh..."
    ./scripts/download-qwen35.sh 2>&1 | tee -a "$REPORT" || die "download failed"
fi
ls -lh "$GGUF" | tee -a "$REPORT"

# ---------- Step 4: Ensure docker image present -----------------------------
section "Step 4: ensure viiwork:qwen-test image present"
if ! docker image inspect viiwork:qwen-test >/dev/null 2>&1; then
    log "Image missing; building (can take 10+ min)..."
    docker build -t viiwork:qwen-test -f Dockerfile.qwen-test . 2>&1 \
        | tee -a "$REPORT" | tail -20
    docker image inspect viiwork:qwen-test >/dev/null 2>&1 || die "docker build failed"
fi
docker image inspect --format '{{.Id}} {{.Size}} bytes' viiwork:qwen-test | tee -a "$REPORT"

# ---------- Step 5: compose up ----------------------------------------------
section "Step 5: docker compose up"
# Make sure any stale container from a prior run is gone (non-destructive
# for production since we use a dedicated project name).
docker compose -f configs/docker-compose.qwen-test.yaml down 2>/dev/null || true
docker compose -f configs/docker-compose.qwen-test.yaml up -d 2>&1 | tee -a "$REPORT"
sleep 3

# ---------- Step 6: wait for ready (or definite failure) up to 5 min --------
section "Step 6: wait for server ready (max 300s)"
DEADLINE=$(( $(date +%s) + 300 ))
READY=0
LOG_SCRATCH="$(mktemp)"
while (( $(date +%s) < DEADLINE )); do
    docker logs --tail=1000 viiwork-qwen-test > "$LOG_SCRATCH" 2>&1 || true
    if grep -qE "HTTP server is listening|server is listening|main: server is listening|listening on 0\.0\.0\.0:|all backends started" "$LOG_SCRATCH"; then
        READY=1; break
    fi
    if grep -qiE "unsupported architecture|unknown model architecture|failed to load model|error loading model|segmentation fault|sigsegv|error while loading model|terminate called" "$LOG_SCRATCH"; then
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
    curl -sS --max-time 10 http://localhost:8092/v1/models 2>&1 \
        | tee -a "$REPORT" | jq . 2>/dev/null | tee -a "$REPORT" || log "probe failed"

    section "Step 8: chat completion"
    curl -sS --max-time 180 -i http://localhost:8092/v1/chat/completions \
        -H 'Content-Type: application/json' \
        -d '{"model":"Qwen3.5-35B-A3B-Q4_K_M","messages":[{"role":"user","content":"Hello. Reply in one short sentence."}],"max_tokens":64,"temperature":0}' \
        2>&1 | tee -a "$REPORT" || log "chat completion failed"
fi

# ---------- Step 9: rocm-smi snapshot ---------------------------------------
section "Step 9: rocm-smi snapshot"
rocm-smi 2>&1 | tee -a "$REPORT" || true
rocm-smi -d 7 8 9 --showmeminfo vram --showuse 2>&1 | tee -a "$REPORT" || true

# ---------- Step 10: leave container running --------------------------------
section "Done"
log "Container left running. Inspect with:"
log "  docker logs viiwork-qwen-test"
log "  curl -s http://localhost:8092/v1/models | jq"
log "Tear down when done:"
log "  docker compose -f configs/docker-compose.qwen-test.yaml down"
log "Report: $REPORT"
