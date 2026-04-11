#!/usr/bin/env bash
# Sweep a Radeon VII's power cap across a range, measure tok/s + watts +
# temperature on the production model, and print a Pareto-frontier table
# with the best tok/s-per-Watt setting marked. Used by setup-node.sh as
# an opt-in step to find a per-node `power_limit_watts` value, and
# runnable standalone.
#
# Phase 1 of the GPU power/perf workstream from the Phase 3 reassessment
# addendum. Power-limit-only is safe (clamps clocks downward, cannot
# corrupt outputs). Phase 2 (voltage/memory clock) is gated on user
# explicit go-ahead and is NOT in this script.
#
# Defaults run a single GPU through 4 power settings × 4 runs each =
# ~10-15 minutes wall time. The script ALWAYS restores the default cap
# (250 W on Radeon VII) on exit, even if interrupted.
#
# Usage:
#   GPU=3 ./scripts/power-perf-sweep.sh
#   GPU=3 WATTS_LIST="180 210 250" RUNS=5 ./scripts/power-perf-sweep.sh
#   GPU=3 OUT=/tmp/sweep.csv ./scripts/power-perf-sweep.sh
#
# Env vars:
#   GPU            target GPU index, default 3
#   WATTS_LIST     space-separated power caps in watts, default "150 180 210 250"
#   RUNS           measured runs per setting, default 3
#   WARMUPS        discarded warmup runs per setting, default 1
#   N_PREDICT      tokens to generate per run, default 100
#   IMAGE          docker image with llama-cli, default viiwork:gfx906
#   MODEL_FILE     gguf filename inside MODELS_DIR, default gemma3-26B Q3_K_XL
#   PROMPT_FILE    host path to prompt file, default /tmp/rocprof-prompts/helsinki.txt
#   OUT            CSV output path, default /tmp/power-perf-sweep-<TS>.csv
set -euo pipefail

GPU="${GPU:-3}"
WATTS_LIST="${WATTS_LIST:-150 180 210 250}"
RUNS="${RUNS:-3}"
WARMUPS="${WARMUPS:-1}"
N_PREDICT="${N_PREDICT:-100}"
IMAGE="${IMAGE:-viiwork:gfx906}"
MODEL_FILE="${MODEL_FILE:-gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf}"
MODELS_DIR="${MODELS_DIR:-/home/janit/viiwork-private/models}"
PROMPT_FILE="${PROMPT_FILE:-/tmp/rocprof-prompts/helsinki.txt}"
OUT="${OUT:-/tmp/power-perf-sweep-$(date -u +%Y%m%dT%H%M%SZ).csv}"
DEFAULT_WATTS="${DEFAULT_WATTS:-250}"

ROCM_SMI="${ROCM_SMI:-/opt/rocm/bin/rocm-smi}"

# === Pre-flight checks ===
[ -f "${MODELS_DIR}/${MODEL_FILE}" ] || { echo "ERROR: model not found at ${MODELS_DIR}/${MODEL_FILE}"; exit 1; }
if [ ! -f "${PROMPT_FILE}" ]; then
    if [ "${PROMPT_FILE}" = "/tmp/rocprof-prompts/helsinki.txt" ] && [ -f "/home/janit/viiwork-private/bench-harness/soak.py" ]; then
        mkdir -p "$(dirname "${PROMPT_FILE}")"
        python3 -c "
import sys
sys.path.insert(0, '/home/janit/viiwork-private/bench-harness')
from soak import HELSINKI_PROMPT
open('${PROMPT_FILE}','w').write(HELSINKI_PROMPT)
"
        echo "  (auto-created ${PROMPT_FILE} from bench-harness/soak.py)"
    else
        echo "ERROR: prompt file not found at ${PROMPT_FILE}"
        exit 1
    fi
fi
docker image inspect "${IMAGE}" >/dev/null 2>&1 || { echo "ERROR: image not found: ${IMAGE}"; exit 1; }
sudo -n true 2>/dev/null || { echo "ERROR: passwordless sudo required for rocm-smi --setpoweroverdrive"; exit 1; }

# === Cleanup trap: ALWAYS restore default cap on exit ===
restore_default() {
    echo
    echo "[$(date -u +%H:%M:%SZ)] restoring power cap to ${DEFAULT_WATTS}W on GPU ${GPU}"
    sudo "${ROCM_SMI}" --setpoweroverdrive "${DEFAULT_WATTS}" -d "${GPU}" --autorespond y >/dev/null 2>&1 || true
}
trap restore_default EXIT INT TERM

# === CSV header ===
echo "timestamp_utc,watts_setting,run_idx,phase,prompt_tps,gen_tps,peak_W,mean_W,peak_C,mean_C" > "${OUT}"

# === Helper: poll rocm-smi at 1 Hz, write watts,celsius lines to a file ===
poll_rocm_smi() {
    local out_file="$1"
    while true; do
        "${ROCM_SMI}" -P --showtemp -d "${GPU}" --json 2>/dev/null | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    g = d.get('card${GPU}', {})
    pw = g.get('Current Socket Graphics Package Power (W)', '0')
    tj = g.get('Temperature (Sensor junction) (C)') or g.get('Temperature (Sensor edge) (C)', '0')
    print(f'{pw},{tj}')
except Exception:
    pass
" >> "${out_file}"
        sleep 1
    done
}

# === Run one iteration ===
run_one_iteration() {
    local watts=$1 phase=$2 idx=$3
    local samples_file
    samples_file=$(mktemp)

    poll_rocm_smi "${samples_file}" &
    local poller_pid=$!

    local llama_out
    llama_out=$(docker run --rm -i \
        --device=/dev/kfd --device=/dev/dri \
        --group-add video --group-add render \
        --security-opt seccomp=unconfined \
        -e ROCR_VISIBLE_DEVICES="${GPU}" \
        -e HSA_OVERRIDE_GFX_VERSION=9.0.6 \
        -v "${MODELS_DIR}:/models:ro" \
        -v "$(dirname "${PROMPT_FILE}"):/prompts:ro" \
        --entrypoint /usr/local/bin/llama-cli \
        "${IMAGE}" \
        -m "/models/${MODEL_FILE}" \
        -f "/prompts/$(basename "${PROMPT_FILE}")" \
        -n "${N_PREDICT}" -ngl 999 --temp 0 \
        --single-turn --simple-io < /dev/null 2>&1 || true)

    kill "${poller_pid}" 2>/dev/null || true
    wait "${poller_pid}" 2>/dev/null || true

    local prompt_tps gen_tps
    prompt_tps=$(echo "${llama_out}" | grep -oE 'Prompt:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+' | head -1)
    gen_tps=$(echo "${llama_out}" | grep -oE 'Generation:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+' | head -1)
    prompt_tps=${prompt_tps:-0}
    gen_tps=${gen_tps:-0}

    local stats
    stats=$(python3 -c "
ws, cs = [], []
for line in open('${samples_file}'):
    parts = line.strip().split(',')
    if len(parts) == 2:
        try:
            ws.append(float(parts[0]))
            cs.append(float(parts[1]))
        except ValueError:
            pass
if ws:
    print(f'{max(ws):.1f},{sum(ws)/len(ws):.1f},{max(cs):.1f},{sum(cs)/len(cs):.1f}')
else:
    print('0,0,0,0')
")
    rm -f "${samples_file}"

    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ),${watts},${idx},${phase},${prompt_tps},${gen_tps},${stats}" >> "${OUT}"
    printf "  %s run %d: prompt=%6s t/s gen=%5s t/s peak=%sW mean=%sW peak=%s°C\n" \
        "${phase}" "${idx}" "${prompt_tps}" "${gen_tps}" \
        "$(echo "${stats}" | cut -d, -f1)" \
        "$(echo "${stats}" | cut -d, -f2)" \
        "$(echo "${stats}" | cut -d, -f3)"
}

# === Main sweep ===
echo "=== power-perf-sweep ==="
echo "GPU:        ${GPU}"
echo "watts:      ${WATTS_LIST}"
echo "runs:       ${WARMUPS} warmup + ${RUNS} measured per setting"
echo "n_predict:  ${N_PREDICT}"
echo "model:      ${MODEL_FILE}"
echo "image:      ${IMAGE}"
echo "out:        ${OUT}"
echo

for watts in ${WATTS_LIST}; do
    echo "==> setting power cap to ${watts}W on GPU ${GPU}"
    sudo "${ROCM_SMI}" --setpoweroverdrive "${watts}" -d "${GPU}" --autorespond y >/dev/null 2>&1 || {
        echo "WARN: failed to set ${watts}W cap (continuing with current cap)"
    }
    sleep 5
    for i in $(seq 1 "${WARMUPS}"); do run_one_iteration "${watts}" warmup "${i}"; done
    for i in $(seq 1 "${RUNS}"); do run_one_iteration "${watts}" measure "${i}"; done
done

echo
echo "=== sweep complete ==="

# === Pareto analysis ===
python3 - <<EOF
import csv, statistics
out_path = "${OUT}"
rows = list(csv.DictReader(open(out_path)))
measured = [r for r in rows if r["phase"] == "measure"]
by_watts = {}
for r in measured:
    w = int(r["watts_setting"])
    by_watts.setdefault(w, []).append(r)

print()
print(f"{'watts':>6} {'gen_tps':>10} {'pe_tps':>10} {'mean_W':>10} {'peak_W':>10} {'peak_C':>9} {'gen/W':>10}")
print("-"*72)
table = []
for w in sorted(by_watts):
    rs = by_watts[w]
    g = statistics.mean(float(r["gen_tps"]) for r in rs)
    p = statistics.mean(float(r["prompt_tps"]) for r in rs)
    mw = statistics.mean(float(r["mean_W"]) for r in rs)
    pw = max(float(r["peak_W"]) for r in rs)
    pc = max(float(r["peak_C"]) for r in rs)
    eff = g/mw if mw > 0 else 0
    table.append((w, g, p, mw, pw, pc, eff))
    print(f"{w:>6} {g:>9.2f} {p:>9.2f} {mw:>9.1f} {pw:>9.1f} {pc:>8.1f} {eff:>9.4f}")

if not table:
    print("\nNo measured rows -- sweep failed")
    raise SystemExit(1)

knee_eff = max(table, key=lambda r: r[6])
knee_perf = max(table, key=lambda r: r[1])
print()
print(f"Best efficiency (gen tok/s per Watt): {knee_eff[0]}W cap")
print(f"  -> {knee_eff[1]:.2f} gen t/s @ {knee_eff[3]:.1f}W mean draw, {knee_eff[6]:.4f} t/s/W")
print(f"Best raw performance: {knee_perf[0]}W cap")
print(f"  -> {knee_perf[1]:.2f} gen t/s @ {knee_perf[3]:.1f}W mean draw")

peak_g = knee_perf[1]
viable = [r for r in table if r[1] >= peak_g * 0.95]
recommended = max(viable, key=lambda r: r[6]) if viable else knee_perf
print()
print(f"RECOMMENDED power_limit_watts: {recommended[0]}")
print(f"  Gives {recommended[1]:.2f} gen t/s ({recommended[1]/peak_g*100:.1f}% of peak)")
print(f"  Draws {recommended[3]:.1f}W mean ({recommended[3]/250*100:.0f}% of 250W default)")
print(f"  Efficiency {recommended[6]:.4f} t/s/W")
print()
print(f"To use: edit viiwork.yaml and set 'power_limit_watts: {recommended[0]}' under 'gpus:'")
print(f"CSV: {out_path}")
EOF
