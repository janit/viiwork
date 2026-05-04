#!/usr/bin/env bash
# Download EuroLLM-22B-Instruct-2512 Q5_K_M GGUF.
#
# Source: bartowski/utter-project_EuroLLM-22B-Instruct-2512-GGUF
# Size:   ~16 GB (Q5_K_M)
# Target: ./models_local/EuroLLM-22B-Instruct-2512-Q5_K_M.gguf
#
# EuroLLM-22B is a dense 22.6B Llama-recipe model (RMSNorm + SwiGLU + RoPE,
# GQA 48/8) trained on the 24 official EU languages plus Norwegian and
# Icelandic. Designed for tasks like structured-data → multilingual text
# (weather/road/works summaries in Nordic + Baltic languages).
#
# Q5_K_M is the quality sweet spot for TS=2 on gb1 (Radeon VII 16GB):
# splits to ~8GB per card with comfortable KV headroom at 16K context.
set -euo pipefail

REPO="bartowski/utter-project_EuroLLM-22B-Instruct-2512-GGUF"
SRC_FILE="utter-project_EuroLLM-22B-Instruct-2512-Q5_K_M.gguf"
DST_FILE="EuroLLM-22B-Instruct-2512-Q5_K_M.gguf"
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

# bartowski filenames use the upstream repo prefix; rename to a clean name.
if [[ -f "$DEST_DIR/$SRC_FILE" && ! -f "$DEST_DIR/$DST_FILE" ]]; then
    mv "$DEST_DIR/$SRC_FILE" "$DEST_DIR/$DST_FILE"
fi

ls -lh "$DEST_DIR/$DST_FILE"
