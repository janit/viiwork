#!/usr/bin/env bash
# Download Qwen3.5-35B-A3B Q4_K_M GGUF for the bring-up test.
#
# Source: unsloth/Qwen3.5-35B-A3B-GGUF (1.2M downloads, canonical)
# Size:   ~22 GB
# Target: ./models/Qwen3.5-35B-A3B-Q4_K_M.gguf
#
# Safe to run concurrently with GPU workloads -- pure network/disk.
set -euo pipefail

REPO="unsloth/Qwen3.5-35B-A3B-GGUF"
FILE="Qwen3.5-35B-A3B-Q4_K_M.gguf"
DEST_DIR="$(cd "$(dirname "$0")/.." && pwd)/models"

mkdir -p "$DEST_DIR"

if [[ -f "$DEST_DIR/$FILE" ]]; then
    echo "Already present: $DEST_DIR/$FILE"
    ls -lh "$DEST_DIR/$FILE"
    exit 0
fi

if command -v hf &>/dev/null; then
    hf download "$REPO" "$FILE" --local-dir "$DEST_DIR"
elif command -v huggingface-cli &>/dev/null; then
    huggingface-cli download "$REPO" "$FILE" --local-dir "$DEST_DIR"
else
    echo "Neither 'hf' nor 'huggingface-cli' is installed." >&2
    echo "Install with: pip install huggingface-hub" >&2
    echo "Or download manually:" >&2
    echo "  https://huggingface.co/$REPO/resolve/main/$FILE" >&2
    exit 1
fi

ls -lh "$DEST_DIR/$FILE"
