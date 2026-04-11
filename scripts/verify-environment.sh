#!/usr/bin/env bash
# Verify the gfx906 dev environment for the llama.cpp fork work.
# Lives in viiwork temporarily; will move into the fork repo as scripts/verify-gpu.sh.
set -euo pipefail

echo "== ROCm version =="
cat /opt/rocm/.info/version 2>/dev/null || dpkg -l | grep -E '^ii\s+rocm' | head -5 || true

echo "== HIP info =="
/opt/rocm/bin/hipconfig --version 2>/dev/null || true
/opt/rocm/bin/hipconfig --platform 2>/dev/null || true

echo "== GPUs visible to ROCr =="
/opt/rocm/bin/rocminfo 2>/dev/null | grep -E "Name:|Marketing Name:|gfx" | head -30 || true

echo "== rocm-smi product list =="
/opt/rocm/bin/rocm-smi --showproductname 2>/dev/null | grep -E "Card Series|GFX Version" | head -40 || true

echo "== rocprof =="
which rocprof && rocprof --version 2>&1 || echo "rocprof not found"

echo "== Tools =="
for tool in cmake git python3 hipcc gh docker; do
  if command -v "$tool" >/dev/null; then
    echo "$tool: $($tool --version 2>&1 | head -1)"
  else
    echo "$tool: MISSING"
  fi
done

echo "== Required env vars =="
echo "HSA_OVERRIDE_GFX_VERSION=${HSA_OVERRIDE_GFX_VERSION:-NOT SET}"
echo "ROCR_VISIBLE_DEVICES=${ROCR_VISIBLE_DEVICES:-NOT SET}"

echo "== Disk space (for builds + models) =="
df -h /home /tmp /opt 2>/dev/null | grep -v "tmpfs\|devtmpfs" || true

echo "== Running viiwork containers (will share GPUs) =="
docker ps --filter "ancestor=viiwork" --format "{{.ID}} {{.Status}} {{.Names}}" 2>/dev/null || echo "docker not accessible"

echo "== Free GPUs (idle, low VRAM%) =="
/opt/rocm/bin/rocm-smi --showmemuse 2>/dev/null | grep "VRAM%" | awk -F: '{gsub(/ /,"",$2); if ($2+0 < 5) print $0}' || true

echo "== DONE =="
