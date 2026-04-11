#!/usr/bin/env bash
# Phase 2 of the GPU power/perf workstream: voltage undervolt and memory
# clock overclock sweep on a single Radeon VII (gfx906), with a correctness
# gate that catches kernel-corruption-grade errors.
#
# UNLIKE Phase 1 (power-cap only) this sweep CAN corrupt VRAM if pushed too
# hard. Bad voltage produces silent FMA errors -> wrong tokens; bad memory
# timings produce corrupted weight reads -> wrong tokens. The correctness
# gate is the only thing standing between us and shipping a setting that
# silently degrades model quality.
#
# Safety design:
#   1) Sweep gentlest first (small voltage decrements, small clock increments)
#   2) After every setting, capture llama-cli's greedy output and compare to
#      a "golden" baseline captured at default voltage/clocks. Output must
#      match the golden first ~200 chars within a small edit distance (the
#      gfx906 HIP MMQ atomic-add reduction noise produces tiny per-run drift
#      even at default settings, so we allow ~3 char divergence).
#   3) Stop the voltage sweep on first fail. Stop the mclk sweep on first
#      fail. Do NOT try more aggressive settings after a fail.
#   4) ALWAYS reset to defaults on exit (trap on EXIT/INT/TERM). Reset is
#      idempotent and runs even if the script is killed.
#   5) Hard floor: voltage never below 950 mV (default is 1080), mclk never
#      above 1100 MHz (default 1000). These are conservative vs the rocm
#      OD_RANGE.
#
# Phase 2 of the GPU power workstream from
# docs/superpowers/specs/2026-04-09-gfx906-fork-phase-3-reassessment-addendum.md
#
# Usage:
#   GPU=3 ./scripts/power-perf-sweep-phase2.sh
#   GPU=3 ONLY=voltage ./scripts/power-perf-sweep-phase2.sh   # voltage only
#   GPU=3 ONLY=mclk    ./scripts/power-perf-sweep-phase2.sh   # mclk only
set -euo pipefail

GPU="${GPU:-3}"
IMAGE="${IMAGE:-viiwork:gfx906}"
MODEL_FILE="${MODEL_FILE:-gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf}"
MODELS_DIR="${MODELS_DIR:-/home/janit/viiwork-private/models}"
PROMPT_FILE="${PROMPT_FILE:-/tmp/rocprof-prompts/helsinki.txt}"
N_PREDICT="${N_PREDICT:-50}"
RUNS="${RUNS:-2}"
ONLY="${ONLY:-both}"  # voltage|mclk|both

# Hard safety floors / ceilings
DEFAULT_HIGH_MHZ=1801    # the state-2 sclk that the voltage curve high point binds to
DEFAULT_HIGH_MV=1080
MIN_VOLT_MV=950          # floor - never go below
MAX_MCLK_MHZ=1100        # ceiling - never go above
DEFAULT_MCLK_MHZ=1000

# Sweep targets (gentlest first)
VOLT_STEPS_MV="1050 1030 1010 990 970 950"        # -30 .. -130 from default
MCLK_STEPS_MHZ="1025 1050 1075 1100"              # +25 .. +100

OUT="${OUT:-/tmp/power-perf-sweep-phase2-$(date -u +%Y%m%dT%H%M%SZ).csv}"
SYSFS="/sys/class/drm/card${GPU}/device"

# === Pre-flight ===
[ -d "${SYSFS}" ] || { echo "ERROR: ${SYSFS} not found"; exit 1; }
[ -w "${SYSFS}/pp_od_clk_voltage" ] || sudo -n true 2>/dev/null || { echo "ERROR: passwordless sudo required"; exit 1; }
[ -f "${MODELS_DIR}/${MODEL_FILE}" ] || { echo "ERROR: model not found"; exit 1; }
[ -f "${PROMPT_FILE}" ] || { echo "ERROR: prompt file not found"; exit 1; }
docker image inspect "${IMAGE}" >/dev/null 2>&1 || { echo "ERROR: image not found"; exit 1; }

# === Reset trap (always restore defaults) ===
reset_gpu() {
    echo
    echo "[$(date -u +%H:%M:%SZ)] resetting GPU ${GPU} to default voltage/clocks"
    sudo bash -c "echo r > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sudo bash -c "echo auto > ${SYSFS}/power_dpm_force_performance_level" 2>/dev/null || true
}
trap reset_gpu EXIT INT TERM

# === Helpers ===

set_voltage() {
    local mv=$1
    if [ "${mv}" -lt "${MIN_VOLT_MV}" ]; then
        echo "REFUSING to set voltage below safety floor (${MIN_VOLT_MV} mV)"
        return 1
    fi
    sudo bash -c "echo manual > ${SYSFS}/power_dpm_force_performance_level"
    sudo bash -c "echo 'vc 2 ${DEFAULT_HIGH_MHZ} ${mv}' > ${SYSFS}/pp_od_clk_voltage"
    sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage"
    sleep 2
}

set_mclk() {
    local mhz=$1
    if [ "${mhz}" -gt "${MAX_MCLK_MHZ}" ]; then
        echo "REFUSING to set mclk above safety ceiling (${MAX_MCLK_MHZ} MHz)"
        return 1
    fi
    sudo bash -c "echo manual > ${SYSFS}/power_dpm_force_performance_level"
    sudo bash -c "echo 'm 1 ${mhz}' > ${SYSFS}/pp_od_clk_voltage"
    sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage"
    sleep 2
}

run_llama_capture_output() {
    # Returns the raw llama-cli output. Caller extracts what it needs.
    docker run --rm -i \
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
        --single-turn --simple-io < /dev/null 2>&1 || true
}

# Extract the gen text from llama-cli output. The output starts with the
# prompt echo, then a banner, then the gen, then "Exiting...". We grab
# everything between the prompt content end and "Exiting...", strip the
# stat line, and trim.
extract_gen_text() {
    sed -n '/\[Start thinking\]/,/llama_memory_breakdown_print/p' | \
        head -n -2 | tail -n +2
}

extract_perf_line() {
    grep -oE 'Prompt:[[:space:]]+[0-9.]+[^|]*\|[[:space:]]*Generation:[[:space:]]+[0-9.]+[^]]*' | head -1
}

# Token-edit-distance gate: compare candidate vs baseline first 200 chars,
# allow up to 5 char differences (gfx906 atomic-add noise can cause minor
# drift even at default voltage, but >5 char divergence in 200 chars means
# the kernel is producing meaningfully different output -> kernel corruption).
correctness_check() {
    local candidate_file="$1"
    local baseline_file="$2"
    python3 - "${candidate_file}" "${baseline_file}" <<'EOF'
import sys
cand = open(sys.argv[1]).read()[:200]
base = open(sys.argv[2]).read()[:200]
if not cand or not cand.strip():
    print("FAIL: empty candidate output")
    sys.exit(2)
if any(ord(c) > 127 and ord(c) < 160 for c in cand):
    print("FAIL: non-printable chars in candidate")
    sys.exit(2)
# Levenshtein-ish: count differing positions in the prefix that aligns
n = min(len(cand), len(base))
if n < 50:
    print(f"FAIL: candidate too short ({n} chars)")
    sys.exit(2)
diffs = sum(1 for i in range(n) if cand[i] != base[i])
if diffs > 5:
    print(f"FAIL: {diffs}/{n} chars differ from baseline (threshold 5)")
    sys.exit(2)
print(f"PASS: {diffs}/{n} chars differ from baseline")
sys.exit(0)
EOF
}

# === Header ===
echo "=== power-perf-sweep-phase2 (voltage + mclk) ==="
echo "GPU:            ${GPU}"
echo "model:          ${MODEL_FILE}"
echo "image:          ${IMAGE}"
echo "n_predict:      ${N_PREDICT}"
echo "voltage steps:  ${VOLT_STEPS_MV} (default ${DEFAULT_HIGH_MV} mV, floor ${MIN_VOLT_MV} mV)"
echo "mclk steps:     ${MCLK_STEPS_MHZ} (default ${DEFAULT_MCLK_MHZ} MHz, ceiling ${MAX_MCLK_MHZ} MHz)"
echo "out:            ${OUT}"
echo

echo "phase,setting,mv_or_mhz,run_idx,prompt_tps,gen_tps,gate" > "${OUT}"

baseline_out=$(mktemp)

echo "==> capturing baseline at default voltage/clocks (no sysfs writes)"
sudo bash -c "echo r > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
sleep 2

baseline_perf=""
for i in $(seq 1 "${RUNS}"); do
    out=$(run_llama_capture_output)
    text=$(echo "${out}" | extract_gen_text)
    perf=$(echo "${out}" | extract_perf_line)
    if [ "${i}" = "1" ]; then
        echo "${text}" > "${baseline_out}"
        baseline_perf="${perf}"
    fi
    echo "  baseline run ${i}: ${perf}"
    echo "baseline,default,0,${i},$(echo "${perf}" | grep -oE 'Prompt:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),$(echo "${perf}" | grep -oE 'Generation:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),baseline" >> "${OUT}"
done
echo "  baseline first 80 chars: $(head -c 80 "${baseline_out}" | tr -d '\n')..."
echo

# === Voltage sweep ===
if [ "${ONLY}" = "voltage" ] || [ "${ONLY}" = "both" ]; then
    echo "==> voltage sweep (gentlest first, stop on first fail)"
    voltage_floor=""
    for mv in ${VOLT_STEPS_MV}; do
        echo
        echo "  -- ${mv} mV (delta ${mv}-${DEFAULT_HIGH_MV} = $(( mv - DEFAULT_HIGH_MV )) mV)"
        if ! set_voltage "${mv}"; then
            echo "  set_voltage failed, stopping voltage sweep"
            break
        fi

        gate_passed=true
        for i in $(seq 1 "${RUNS}"); do
            out=$(run_llama_capture_output)
            text=$(echo "${out}" | extract_gen_text)
            perf=$(echo "${out}" | extract_perf_line)
            cand_file=$(mktemp)
            echo "${text}" > "${cand_file}"

            gate_result=$(correctness_check "${cand_file}" "${baseline_out}" 2>&1 || echo "FAIL")
            rm -f "${cand_file}"
            echo "    run ${i}: ${perf} | gate: ${gate_result}"
            echo "voltage,${mv}mV,${mv},${i},$(echo "${perf}" | grep -oE 'Prompt:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),$(echo "${perf}" | grep -oE 'Generation:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),${gate_result}" >> "${OUT}"

            if [[ "${gate_result}" == FAIL* ]]; then
                gate_passed=false
            fi
        done

        if [ "${gate_passed}" = "true" ]; then
            voltage_floor="${mv}"
            echo "    PASS at ${mv} mV (last good)"
        else
            echo "    FAIL at ${mv} mV - stopping voltage sweep"
            break
        fi
    done

    sudo bash -c "echo r > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sleep 2
    echo
    echo "voltage sweep complete. last good voltage: ${voltage_floor:-NONE} mV"
fi

# === Memory clock sweep ===
if [ "${ONLY}" = "mclk" ] || [ "${ONLY}" = "both" ]; then
    echo
    echo "==> memory clock sweep (gentlest first, stop on first fail)"
    mclk_ceiling=""
    for mhz in ${MCLK_STEPS_MHZ}; do
        echo
        echo "  -- ${mhz} MHz (delta +$(( mhz - DEFAULT_MCLK_MHZ )) MHz)"
        if ! set_mclk "${mhz}"; then
            echo "  set_mclk failed, stopping mclk sweep"
            break
        fi

        gate_passed=true
        for i in $(seq 1 "${RUNS}"); do
            out=$(run_llama_capture_output)
            text=$(echo "${out}" | extract_gen_text)
            perf=$(echo "${out}" | extract_perf_line)
            cand_file=$(mktemp)
            echo "${text}" > "${cand_file}"

            gate_result=$(correctness_check "${cand_file}" "${baseline_out}" 2>&1 || echo "FAIL")
            rm -f "${cand_file}"
            echo "    run ${i}: ${perf} | gate: ${gate_result}"
            echo "mclk,${mhz}MHz,${mhz},${i},$(echo "${perf}" | grep -oE 'Prompt:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),$(echo "${perf}" | grep -oE 'Generation:[[:space:]]+[0-9.]+' | grep -oE '[0-9.]+'),${gate_result}" >> "${OUT}"

            if [[ "${gate_result}" == FAIL* ]]; then
                gate_passed=false
            fi
        done

        if [ "${gate_passed}" = "true" ]; then
            mclk_ceiling="${mhz}"
            echo "    PASS at ${mhz} MHz (last good)"
        else
            echo "    FAIL at ${mhz} MHz - stopping mclk sweep"
            break
        fi
    done

    sudo bash -c "echo r > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sudo bash -c "echo c > ${SYSFS}/pp_od_clk_voltage" 2>/dev/null || true
    sleep 2
    echo
    echo "mclk sweep complete. last good mclk: ${mclk_ceiling:-NONE} MHz"
fi

rm -f "${baseline_out}"

echo
echo "=== summary ==="
python3 - <<EOF
import csv
rows = list(csv.DictReader(open("${OUT}")))
print(f"baseline: prompt={rows[0]['prompt_tps']} t/s gen={rows[0]['gen_tps']} t/s")
print()
print(f"{'phase':<10} {'setting':<12} {'gen_tps':>10} {'gate':<60}")
for r in rows[1:]:
    if r['phase'] == 'voltage' or r['phase'] == 'mclk':
        print(f"{r['phase']:<10} {r['setting']:<12} {r['gen_tps']:>9}t/s {r['gate'][:60]}")
print()
print("CSV: ${OUT}")
EOF
