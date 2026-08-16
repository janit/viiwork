#!/usr/bin/env bash
# Download Soofi-S-30B-A3B Instruct-Preview GGUF (hybrid Mamba-2 / MoE).
#
# Source: Soofi-Project/Soofi-S-Instruct-Preview-GGUF
# Size:   ~25 GB (Q5_K_M)
# Target: /mnt/usbd/soofi-s-instruct-preview-Q5_K_M.gguf   (USB XFS, /dev/sda)
#
# Soofi-S is the German SOOFI consortium's sovereign foundation model:
# 31.6B total / 3.2B active, 52 layers interleaving 23 Mamba-2 sequence-mixing
# layers, 23 granular MoE layers and 6 GQA attention layers. German + English.
#
# ARCH: the GGUF declares `general.architecture = nemotron_h_moe` — it reuses
# an existing llama.cpp arch rather than registering a `soofi` one. Verified
# 2026-08-15: all four images on gb1 (viiwork:latest, :qwen-test,
# :glimmer-b10369, :qwen38-b10437) already have nemotron_h_moe in libllama.so,
# so NO new llama.cpp pin is needed. Do not grep the binaries for "soofi" —
# that string is absent by design and will mislead you into a pointless bump.
#
# QUANT CHOICE — read before "upgrading" this:
#   This arch's tensor columns (2688 / 1856 / 3712) are not divisible by 256,
#   so every K-quant tensor falls back to a non-K type:
#     Q6_K -> q8_0  (~32 GB, i.e. as large as Q8_0 for NO quality gain: skip it)
#     Q5_K_M -> q5_1 (~25 GB)  <- this script
#     Q4_K_M -> q4_1 (~21 GB)  <- fallback if Q5_K_M OOMs at warmup
#   This inverts the usual gfx906 "take the highest quant that fits" rule:
#   Q6_K is strictly worse than Q8_0 here, and Q8_0 does not fit 2x 16 GB.
#
# GATED REPO: Soofi-Project gates both the GGUF and the base repo with
# `gated: "manual"` — a human at the consortium approves each request. Until
# your account is on the list this script exits 1 with instructions. Request
# access at https://huggingface.co/Soofi-Project/Soofi-S-Instruct-Preview-GGUF
# There is no community requant to route around it (checked 2026-08-15:
# bartowski / mradermacher / unsloth all have nothing), and self-converting is
# blocked too because Soofi-S-Base carries the same manual gate.
set -euo pipefail

REPO="Soofi-Project/Soofi-S-Instruct-Preview-GGUF"
SRC_FILE="soofi-s-instruct-preview-Q5_K_M.gguf"
DST_FILE="soofi-s-instruct-preview-Q5_K_M.gguf"
DEST_DIR="/mnt/usbd"

mkdir -p "$DEST_DIR"

if [[ -f "$DEST_DIR/$DST_FILE" ]]; then
    echo "Already present: $DEST_DIR/$DST_FILE"
    ls -lh "$DEST_DIR/$DST_FILE"
    exit 0
fi

# Preflight the gate before starting a 25 GB transfer, so an unapproved token
# fails in one second with a useful message instead of mid-download.
if [[ -z "${HF_TOKEN:-}" ]]; then
    echo "HF_TOKEN is not set. This repo is gated; an anonymous fetch cannot work." >&2
    exit 1
fi

http_code=$(curl -sIL -o /dev/null -w '%{http_code}' --max-time 30 \
    -H "Authorization: Bearer $HF_TOKEN" \
    "https://huggingface.co/$REPO/resolve/main/$SRC_FILE" || echo 000)

if [[ "$http_code" == "403" ]]; then
    echo "HTTP 403 GatedRepo — this HF token is not on the authorized list." >&2
    echo >&2
    echo "Soofi-S is in closed beta and gated with manual approval. Fix:" >&2
    echo "  1. Visit https://huggingface.co/$REPO" >&2
    echo "  2. Accept the conditions and request access" >&2
    echo "  3. Wait for a consortium maintainer to approve (manual, not instant)" >&2
    echo "  4. Re-run this script" >&2
    exit 1
elif [[ "$http_code" != "200" ]]; then
    echo "Unexpected HTTP $http_code fetching $SRC_FILE — aborting." >&2
    exit 1
fi

echo "Gate check passed (HTTP 200). Downloading ~25 GB to $DEST_DIR ..."

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
