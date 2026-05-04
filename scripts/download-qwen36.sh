#!/usr/bin/env bash
# Download Qwen3.6-27B Q4_K_M GGUF for the bring-up test.
#
# Source: unsloth/Qwen3.6-27B-GGUF
# Size:   ~16.8 GB (Q4_K_M)
# Target: ./models_local/Qwen3.6-27B-Q4_K_M.gguf
#         (local NVMe, not the NFS-backed models/ — Qwen3.6 is the go-to
#         local agent and must stay reachable when the NAS is down)
#
# Qwen3.6-27B is a 27B dense hybrid model (Gated-DeltaNet + Gated-Attention,
# same arch family as 3.5-A3B). Unlike 3.5-A3B (MoE, 3B active), all 27B
# params are active per token, so this is compute-bound on gfx906 rather
# than memory-streaming-bound.
#
# Safe to run concurrently with GPU workloads -- pure network/disk.
set -euo pipefail

REPO="unsloth/Qwen3.6-27B-GGUF"
FILE="Qwen3.6-27B-Q4_K_M.gguf"
DEST_DIR="$(cd "$(dirname "$0")/.." && pwd)/models_local"

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
