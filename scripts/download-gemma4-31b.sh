#!/usr/bin/env bash
# Download Gemma 4 31B-it QAT GGUF (Quantization-Aware Training, Q4).
#
# Source: unsloth/gemma-4-31B-it-qat-GGUF
# Size:   ~17.3 GB (QAT UD-Q4_K_XL)
# Target: /mnt/p3700/llm-models/gemma-4-31B-it-qat-UD-Q4_K_XL.gguf  (NFS p3700)
#
# Gemma 4 31B-it is a 33B dense Image-Text-to-Text model. Used here for
# text-only prose generation (weather/road/works summaries). This is the QAT
# checkpoint: Google quantization-aware-trained the weights for Q4, so the
# int4 quant lands at near-bf16 quality while fitting in 2x Radeon VII (16GB)
# under tensor-split — replacing the old PTQ Q5_K_S at lower VRAM.
#
# NOTE: Google's own day-one GGUF (google/gemma-4-31B-it-qat-q4_0-gguf) is a
# broken conversion — it detokenizes garbage and leaks foreign special tokens
# on gfx906 llama.cpp builds. Use Unsloth's clean requant below instead.
#
# Arch token: `gemma4`. Needs viiwork:latest (llama.cpp b9222+); the older
# viiwork:qwen-test build mishandles the gemma4 QAT tokenizer.
set -euo pipefail

REPO="unsloth/gemma-4-31B-it-qat-GGUF"
SRC_FILE="gemma-4-31B-it-qat-UD-Q4_K_XL.gguf"
DST_FILE="gemma-4-31B-it-qat-UD-Q4_K_XL.gguf"
DEST_DIR="/mnt/p3700/llm-models"

mkdir -p "$DEST_DIR"

if [[ -f "$DEST_DIR/$DST_FILE" ]]; then
    echo "Already present: $DEST_DIR/$DST_FILE"
    ls -lh "$DEST_DIR/$DST_FILE"
    exit 0
fi

if command -v hf &>/dev/null; then
    hf download "$REPO" "$SRC_FILE" --local-dir "$DEST_DIR"
elif command -v huggingface-cli &>/dev/null; then
    huggingface-cli download "$REPO" "$SRC_FILE" --local-dir "$DEST_DIR"
else
    echo "Neither 'hf' nor 'huggingface-cli' is installed." >&2
    echo "Install with: pip install huggingface-hub" >&2
    echo "Or download manually:" >&2
    echo "  https://huggingface.co/$REPO/resolve/main/$SRC_FILE" >&2
    exit 1
fi

if [[ -f "$DEST_DIR/$SRC_FILE" && ! -f "$DEST_DIR/$DST_FILE" ]]; then
    mv "$DEST_DIR/$SRC_FILE" "$DEST_DIR/$DST_FILE"
fi

ls -lh "$DEST_DIR/$DST_FILE"
