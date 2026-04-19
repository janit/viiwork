#!/usr/bin/env bash
# KV cache quality eval via llama-perplexity against wikitext-2 subset.
#
# Runs 4 configurations sequentially on a single GPU (default: GPU 5):
#   1. baseline  — f16 K and V cache (reference)
#   2. q8        — q8_0 K and V cache
#   3. q4        — q4_0 K and V cache
#   4. fa-q8     — flash-attn + q8_0 K and V cache
#
# Each config loads the model, processes the corpus in ctx-sized chunks,
# and prints "Final estimate: PPL = X ± Y". Lower PPL = better quality.
# A config is effectively lossless if its PPL is within ~0.5-1% of baseline.
#
# Usage:
#   ./run_kv_perplexity.sh [CTX] [GPU_ID]
#   ./run_kv_perplexity.sh            # default ctx=2048, gpu=5
#   ./run_kv_perplexity.sh 4096 5     # longer context on gpu 5
set -euo pipefail

CTX="${1:-2048}"
GPU_ID="${2:-5}"
MODEL="gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf"
CORPUS_HOST="/home/janit/kv-eval/wiki.test.subset.raw"
OUT_DIR="/home/janit/kv-eval/$(date -u +%Y%m%dT%H%M%SZ)-ppl-ctx${CTX}"
mkdir -p "$OUT_DIR"

PHASES=(baseline q8 q4 fa-q8)
declare -A EXTRA_ARGS=(
  [baseline]="--cache-type-k f16 --cache-type-v f16"
  [q8]="--cache-type-k q8_0 --cache-type-v q8_0"
  [q4]="--cache-type-k q4_0 --cache-type-v q4_0"
  [fa-q8]="-fa on --cache-type-k q8_0 --cache-type-v q8_0"
)

LOG_FILE="$OUT_DIR/run.log"
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

log "perplexity eval: ctx=$CTX gpu=$GPU_ID out=$OUT_DIR"
log "model=$MODEL corpus=$CORPUS_HOST ($(wc -c < "$CORPUS_HOST") bytes)"

for label in "${PHASES[@]}"; do
  args="${EXTRA_ARGS[$label]}"
  log "=== $label: $args ==="

  # shellcheck disable=SC2086
  docker run --rm \
    --name "kv-ppl-$label" \
    --device /dev/kfd --device /dev/dri \
    --group-add video --group-add render \
    -e "ROCR_VISIBLE_DEVICES=$GPU_ID" \
    -e "HSA_OVERRIDE_GFX_VERSION=9.0.6" \
    -v /home/janit/viiwork-private/models:/models:ro \
    -v /home/janit/kv-eval:/data:ro \
    --entrypoint llama-perplexity \
    viiwork:latest \
    -m "/models/$MODEL" -f /data/wiki.test.subset.raw \
    -c "$CTX" -ngl 99 --chunks 20 $args \
    > "$OUT_DIR/$label.log" 2>&1 \
    && log "$label: done" \
    || log "$label: FAILED (see $OUT_DIR/$label.log)"
done

log "--- results ---"
{
  echo "# KV perplexity eval (ctx=$CTX, gpu=$GPU_ID)"
  echo ""
  echo "| Config | Final PPL | ± err |"
  echo "|--------|-----------|-------|"
  base_ppl=""
  for label in "${PHASES[@]}"; do
    line=$(grep -E 'Final estimate: PPL' "$OUT_DIR/$label.log" 2>/dev/null | tail -1 || echo "")
    if [[ -z "$line" ]]; then
      echo "| $label | FAILED | - |"
      continue
    fi
    ppl=$(echo "$line" | grep -oE 'PPL = [0-9.]+' | awk '{print $NF}')
    err=$(echo "$line" | grep -oE '\+/- [0-9.]+' | awk '{print $NF}')
    echo "| $label | $ppl | ± $err |"
    [[ -z "$base_ppl" && "$label" == "baseline" ]] && base_ppl="$ppl"
  done
  echo ""
  if [[ -n "$base_ppl" ]]; then
    echo "## Delta vs baseline"
    echo ""
    echo "| Config | PPL delta | % delta |"
    echo "|--------|-----------|---------|"
    for label in "${PHASES[@]}"; do
      line=$(grep -E 'Final estimate: PPL' "$OUT_DIR/$label.log" 2>/dev/null | tail -1 || echo "")
      [[ -z "$line" ]] && { echo "| $label | FAILED | - |"; continue; }
      ppl=$(echo "$line" | grep -oE 'PPL = [0-9.]+' | awk '{print $NF}')
      delta=$(python3 -c "print(f'{$ppl - $base_ppl:+.4f}')")
      pct=$(python3 -c "print(f'{100*($ppl - $base_ppl)/$base_ppl:+.3f}%')")
      echo "| $label | $delta | $pct |"
    done
  fi
} | tee "$OUT_DIR/summary.md"

log "perplexity eval complete: $OUT_DIR/summary.md"
