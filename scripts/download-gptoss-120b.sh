#!/usr/bin/env bash
# Download gpt-oss-120b MXFP4_MOE GGUF (split into 2 parts).
#
# Source:  bartowski/openai_gpt-oss-120b-GGUF, MXFP4_MOE subdir
# Size:    ~63.4 GB total (39.8 + 23.6)
# Target:  /mnt/p3700/llm-models/openai_gpt-oss-120b-MXFP4_MOE-{00001,00002}-of-00002.gguf
#
# MXFP4_MOE is OpenAI's native release format for the MoE weights — going
# below 4-bit re-quantizes already-quantized tensors. This is the lossless
# choice for gpt-oss-120b.
#
# Arch token: `gpt-oss` (upstream llama.cpp master since Aug 2025).
set -euo pipefail

REPO="bartowski/openai_gpt-oss-120b-GGUF"
DEST_DIR="/mnt/p3700/llm-models"
mkdir -p "$DEST_DIR"

FILES=(
  "openai_gpt-oss-120b-MXFP4_MOE/openai_gpt-oss-120b-MXFP4_MOE-00001-of-00002.gguf"
  "openai_gpt-oss-120b-MXFP4_MOE/openai_gpt-oss-120b-MXFP4_MOE-00002-of-00002.gguf"
)

dl() {
    local f="$1"
    local base="$(basename "$f")"
    if [[ -f "$DEST_DIR/$base" ]]; then
        echo "Already present: $base"
        return 0
    fi
    if command -v hf &>/dev/null; then
        hf download "$REPO" "$f" --local-dir "$DEST_DIR"
    else
        huggingface-cli download "$REPO" "$f" --local-dir "$DEST_DIR"
    fi
    # hf download lays files out under the repo's directory tree under DEST_DIR.
    # Flatten so both parts sit directly in /mnt/p3700/llm-models.
    if [[ -f "$DEST_DIR/$f" && ! -f "$DEST_DIR/$base" ]]; then
        mv "$DEST_DIR/$f" "$DEST_DIR/$base"
    fi
}

for f in "${FILES[@]}"; do
    dl "$f"
done

# Clean up the empty subdir if hf left one.
rmdir "$DEST_DIR/openai_gpt-oss-120b-MXFP4_MOE" 2>/dev/null || true

ls -lh "$DEST_DIR"/openai_gpt-oss-120b-MXFP4_MOE-*.gguf
