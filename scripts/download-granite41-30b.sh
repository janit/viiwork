#!/usr/bin/env bash
# Download Granite 4.1-30B Q4_K_M GGUF for the bring-up test.
#
# Source: bartowski/ibm-granite_granite-4.1-30b-GGUF
# Size:   ~17.8 GB
# Target: ./models/granite-4.1-30b-Q4_K_M.gguf
set -euo pipefail

REPO="bartowski/ibm-granite_granite-4.1-30b-GGUF"
FILE="ibm-granite_granite-4.1-30b-Q4_K_M.gguf"
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
