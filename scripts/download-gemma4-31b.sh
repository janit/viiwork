#!/usr/bin/env bash
# Download Gemma 4 31B-it Q5_K_S GGUF.
#
# Source: bartowski/google_gemma-4-31B-it-GGUF
# Size:   ~21.5 GB (Q5_K_S)
# Target: /mnt/p3700/llm-models/gemma-4-31B-it-Q5_K_S.gguf  (NFS p3700)
#
# Gemma 4 31B-it is a 33B dense Image-Text-to-Text model. Used here for
# text-only prose generation (weather/road/works summaries). Highest dense
# quality that fits in 2x Radeon VII (16GB) tensor-split at Q5_K_S.
#
# Arch token: `gemma4` (upstream llama.cpp master supports it).
set -euo pipefail

REPO="bartowski/google_gemma-4-31B-it-GGUF"
SRC_FILE="google_gemma-4-31B-it-Q5_K_S.gguf"
DST_FILE="gemma-4-31B-it-Q5_K_S.gguf"
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
