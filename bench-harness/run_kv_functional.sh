#!/usr/bin/env bash
# KV cache FUNCTIONAL quality eval. Runs a fixed prompt suite through
# llama-server with three KV configurations and compares outputs.
#
# Uses one GPU (default: 5) and a single llama-server process per phase.
# Determinism via temperature=0, seed=0, top_k=1.
#
# Usage:
#   ./run_kv_functional.sh [GPU_ID]
set -euo pipefail

GPU_ID="${1:-5}"
PORT=9201
MODEL="/models/gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf"
PROMPTS="/home/janit/kv-eval/prompts.hydrated.json"
OUT_DIR="/home/janit/kv-eval/$(date -u +%Y%m%dT%H%M%SZ)-functional"
mkdir -p "$OUT_DIR"

PHASES=(baseline q8 q4)
declare -A CACHE_ARGS=(
  [baseline]=""
  [q8]="--cache-type-k q8_0 --cache-type-v q8_0"
  [q4]="--cache-type-k q4_0 --cache-type-v q4_0"
)

LOG_FILE="$OUT_DIR/run.log"
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

teardown() {
  docker rm -f kv-ft 2>/dev/null || true
}

wait_healthy() {
  local deadline=$(( $(date +%s) + 300 ))
  while (( $(date +%s) < deadline )); do
    if curl -fsS --max-time 2 "http://localhost:${PORT}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 3
  done
  return 1
}

run_phase() {
  local label="$1"
  local cache_args="${CACHE_ARGS[$label]}"
  log "=== phase $label: $cache_args ==="

  teardown

  # shellcheck disable=SC2086
  docker run -d --rm --name kv-ft \
    --device /dev/kfd --device /dev/dri \
    --group-add video --group-add render \
    -e "ROCR_VISIBLE_DEVICES=$GPU_ID" \
    -e "HSA_OVERRIDE_GFX_VERSION=9.0.6" \
    -p "${PORT}:8080" \
    -v /home/janit/viiwork-private/models:/models:ro \
    --entrypoint llama-server \
    viiwork:latest \
    -m "$MODEL" -c 8192 -ngl 99 --host 0.0.0.0 --port 8080 \
    $cache_args \
    >/dev/null

  if ! wait_healthy; then
    log "$label: FAILED to become healthy in 300s"
    docker logs kv-ft > "$OUT_DIR/$label-container.log" 2>&1 || true
    teardown
    return 1
  fi
  log "$label: healthy, running prompts"

  python3 - "$label" "$OUT_DIR" "$PROMPTS" "$PORT" <<'PYEOF'
import json, sys, urllib.request, time

label, out_dir, prompts_path, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
prompts = json.load(open(prompts_path))
results = []
for p in prompts:
    body = {
        "model": "any",
        "messages": [
            {"role": "system", "content": p["system"]},
            {"role": "user", "content": p["user"]},
        ],
        "temperature": 0.0,
        "top_k": 1,
        "seed": 0,
        "max_tokens": p["max_tokens"],
    }
    t0 = time.time()
    req = urllib.request.Request(
        f"http://localhost:{port}/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=180) as r:
        resp = json.loads(r.read())
    elapsed = time.time() - t0
    choice = resp.get("choices", [{}])[0]
    msg = choice.get("message", {})
    text = msg.get("content", "") or ""
    reasoning = msg.get("reasoning_content", "") or ""
    full = (reasoning + "\n===\n" + text).strip() if reasoning else text
    finish = choice.get("finish_reason", "")
    usage = resp.get("usage", {})
    results.append({
        "id": p["id"],
        "output": full,
        "content": text,
        "reasoning": reasoning,
        "finish_reason": finish,
        "usage": usage,
        "elapsed_s": round(elapsed, 2),
    })
    print(f"  [{label}] {p['id']}: {elapsed:.1f}s, {usage.get('completion_tokens','?')} toks, finish={finish}")

with open(f"{out_dir}/{label}.json","w") as f:
    json.dump(results, f, indent=2)
PYEOF

  log "$label: done, tearing down"
  docker logs kv-ft > "$OUT_DIR/$label-container.log" 2>&1 || true
  teardown
  sleep 10
}

trap 'teardown' EXIT INT TERM

log "functional eval: gpu=$GPU_ID out=$OUT_DIR"

for phase in "${PHASES[@]}"; do
  run_phase "$phase" || log "phase $phase failed; continuing"
done

log "--- comparison ---"
python3 - "$OUT_DIR" <<'PYEOF'
import json, sys, os
from pathlib import Path

out_dir = Path(sys.argv[1])
configs = ["baseline", "q8", "q4"]
data = {}
for c in configs:
    p = out_dir / f"{c}.json"
    if p.exists():
        data[c] = {r["id"]: r for r in json.load(open(p))}

if "baseline" not in data:
    print("baseline missing, cannot compare")
    sys.exit(0)

base = data["baseline"]
ids = list(base.keys())

out = [f"# KV functional eval comparison\n\nOutput dir: `{out_dir}`\n"]
out.append("## Per-prompt outputs\n")
for pid in ids:
    out.append(f"### {pid}\n")
    for c in configs:
        if c not in data or pid not in data[c]:
            out.append(f"**{c}**: MISSING\n")
            continue
        r = data[c][pid]
        text = r["output"].strip()
        match = "EXACT" if c == "baseline" else ("EXACT" if text == base[pid]["output"].strip() else "DIFF")
        out.append(f"**{c}** ({match}, {r['elapsed_s']}s, {r['usage'].get('completion_tokens','?')} toks):\n\n```\n{text}\n```\n")

# summary table
out.append("\n## Match summary vs baseline\n")
out.append("| Prompt | q8 | q4 |")
out.append("|--------|----|----|")
for pid in ids:
    row = [pid]
    for c in ["q8", "q4"]:
        if c not in data or pid not in data[c]:
            row.append("—")
            continue
        m = "EXACT" if data[c][pid]["output"].strip() == base[pid]["output"].strip() else "DIFF"
        row.append(m)
    out.append("| " + " | ".join(row) + " |")

# aggregate stats
for c in ["q8", "q4"]:
    if c not in data:
        continue
    matches = sum(1 for pid in ids if pid in data[c] and data[c][pid]["output"].strip() == base[pid]["output"].strip())
    total = sum(1 for pid in ids if pid in data[c])
    out.append(f"\n**{c}**: {matches}/{total} exact match vs baseline")

summary = "\n".join(out)
print(summary)
(out_dir / "summary.md").write_text(summary + "\n")
PYEOF

log "functional eval complete: $OUT_DIR/summary.md"
