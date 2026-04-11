#!/usr/bin/env bash
# Wrap llama-cli inside the viiwork:gfx906 image with rocprof and
# capture stats CSV + hip-trace + full SQLite db. Used for the Phase 2
# kernel selection work in the gfx906 fork.
#
# Usage:
#   rocprof_capture.sh <label> <prompt_file> <n_predict>
#
# Example:
#   rocprof_capture.sh gen-dominated   /tmp/rocprof-prompts/short.txt    400
#   rocprof_capture.sh prompt-eval     /tmp/rocprof-prompts/helsinki.txt 20
#
# Pins to GPU 3 (the dev/bench card on gb1). Output lands in
# /home/janit/gfx906-work/results/rocprof-phase2/<label>.{stats,hip_stats,copy_stats}.csv
# plus the .db / .json / .sysinfo.txt artefacts.
set -euo pipefail

LABEL="${1:?usage: $0 <label> <prompt_file> <n_predict>}"
PROMPT_FILE="${2:?usage: $0 <label> <prompt_file> <n_predict>}"
N_PREDICT="${3:?usage: $0 <label> <prompt_file> <n_predict>}"

MODEL="${MODEL:-/models/gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf}"
GPU="${GPU:-3}"
IMAGE="${IMAGE:-viiwork:gfx906}"
OUT_DIR="${OUT_DIR:-/home/janit/gfx906-work/results/rocprof-phase2}"
MODELS_DIR="${MODELS_DIR:-/home/janit/viiwork-private/models}"
PROMPTS_DIR="$(dirname "$(realpath "${PROMPT_FILE}")")"
PROMPT_BASENAME="$(basename "${PROMPT_FILE}")"

mkdir -p "${OUT_DIR}"
# Pre-clean any prior files for this label so the capture is fresh
rm -f "${OUT_DIR}/${LABEL}".{stats,hip_stats,copy_stats}.csv \
      "${OUT_DIR}/${LABEL}".{db,json,sysinfo.txt}

echo "[$(date -u +%H:%M:%SZ)] starting capture: label=${LABEL} n_predict=${N_PREDICT} gpu=${GPU}"
echo "[$(date -u +%H:%M:%SZ)] prompt: ${PROMPT_FILE} ($(wc -c <"${PROMPT_FILE}") bytes)"

docker run --rm -i \
    --device=/dev/kfd --device=/dev/dri \
    --group-add video --group-add render \
    --security-opt seccomp=unconfined \
    -e ROCR_VISIBLE_DEVICES="${GPU}" \
    -e HSA_OVERRIDE_GFX_VERSION=9.0.6 \
    -v "${MODELS_DIR}:/models:ro" \
    -v "${PROMPTS_DIR}:/prompts:ro" \
    -v "${OUT_DIR}:/out" \
    --entrypoint rocprof \
    "${IMAGE}" \
    --stats --hip-trace -o "/out/${LABEL}.csv" \
    /usr/local/bin/llama-cli -m "${MODEL}" \
        -f "/prompts/${PROMPT_BASENAME}" \
        -n "${N_PREDICT}" -ngl 999 --temp 0 \
        --single-turn --simple-io < /dev/null 2>&1 \
    | grep -vE '^scan ops data|^dump json|^from_val' || true

echo "[$(date -u +%H:%M:%SZ)] capture done: ${LABEL}"
ls -la "${OUT_DIR}/${LABEL}".{stats,hip_stats,copy_stats}.csv 2>&1 | grep -v 'No such'
