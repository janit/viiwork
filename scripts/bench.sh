#!/usr/bin/env bash
# Stress benchmark for viiwork cluster
# Usage: ./scripts/bench.sh [URL] [MAX_CONCURRENCY]

set -euo pipefail

URL="${1:-http://gb1:8080}"
MAX_CONC="${2:-10}"
MODEL="qwen2.5-coder-14b-instruct-q6_k"
RESULTS_DIR=$(mktemp -d)
PROMPT="Write a Python function that implements merge sort with detailed comments explaining each step."

request() {
    local id=$1
    local start=$(date +%s%N)

    local http_code
    local body
    body=$(curl -s -w "\n%{http_code}" -X POST "${URL}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d '{
            "model": "'"${MODEL}"'",
            "messages": [{"role": "user", "content": "'"${PROMPT}"'"}],
            "max_tokens": 256,
            "temperature": 0.7
        }' 2>/dev/null) || true

    local end=$(date +%s%N)
    http_code=$(echo "$body" | tail -1)
    body=$(echo "$body" | sed '$d')

    local elapsed_ms=$(( (end - start) / 1000000 ))

    # Extract token counts from response
    local prompt_tokens=0 completion_tokens=0
    if echo "$body" | grep -q '"usage"'; then
        prompt_tokens=$(echo "$body" | grep -o '"prompt_tokens":[0-9]*' | grep -o '[0-9]*' || echo 0)
        completion_tokens=$(echo "$body" | grep -o '"completion_tokens":[0-9]*' | grep -o '[0-9]*' || echo 0)
    fi

    echo "${id},${http_code},${elapsed_ms},${prompt_tokens},${completion_tokens}" > "${RESULTS_DIR}/${id}.csv"
}

run_wave() {
    local concurrency=$1
    local wave_start=$(date +%s%N)

    echo "--- Concurrency: ${concurrency} ---"

    pids=()
    for i in $(seq 1 "$concurrency"); do
        request "${concurrency}_${i}" &
        pids+=($!)
    done

    for pid in "${pids[@]}"; do
        wait "$pid" 2>/dev/null || true
    done

    local wave_end=$(date +%s%N)
    local wave_ms=$(( (wave_end - wave_start) / 1000000 ))

    # Collect results
    local total_ok=0 total_fail=0 total_tokens=0
    local min_ms=999999 max_ms=0 sum_ms=0

    for i in $(seq 1 "$concurrency"); do
        local f="${RESULTS_DIR}/${concurrency}_${i}.csv"
        if [[ -f "$f" ]]; then
            IFS=',' read -r id code ms pt ct < "$f"
            if [[ "$code" == "200" ]]; then
                ((total_ok++)) || true
                ((total_tokens += ct)) || true
                ((sum_ms += ms)) || true
                (( ms < min_ms )) && min_ms=$ms
                (( ms > max_ms )) && max_ms=$ms
            else
                ((total_fail++)) || true
                echo "  FAIL: request ${id} -> HTTP ${code}"
            fi
        fi
    done

    if (( total_ok > 0 )); then
        local avg_ms=$(( sum_ms / total_ok ))
        local tps=0
        if (( wave_ms > 0 )); then
            tps=$(echo "scale=1; ${total_tokens} * 1000 / ${wave_ms}" | bc)
        fi
        printf "  OK: %d/%d  |  Latency: min=%dms avg=%dms max=%dms  |  Wall: %.1fs  |  Tokens: %d (%.1f tok/s)\n" \
            "$total_ok" "$concurrency" "$min_ms" "$avg_ms" "$max_ms" \
            "$(echo "scale=1; ${wave_ms}/1000" | bc)" \
            "$total_tokens" "$tps"
    else
        echo "  All requests failed."
    fi

    if (( total_fail > 0 )); then
        echo "  Failures: ${total_fail}"
    fi
    echo ""
}

echo "========================================="
echo " viiwork stress benchmark"
echo " Target: ${URL}"
echo " Model:  ${MODEL}"
echo " Prompt: 256 max tokens, coding task"
echo "========================================="
echo ""

# Check cluster is up
echo "Checking cluster..."
status=$(curl -s "${URL}/v1/status" 2>/dev/null) || { echo "ERROR: Cannot reach ${URL}"; exit 1; }
healthy=$(echo "$status" | grep -o '"status":"healthy"' | wc -l)
echo "Healthy backends: ${healthy}"
echo ""

# Ramp up concurrency
for c in 1 2 4 6 8 10; do
    if (( c > MAX_CONC )); then
        break
    fi
    run_wave "$c"
done

# If max > 10, also test max
if (( MAX_CONC > 10 )); then
    run_wave "$MAX_CONC"
fi

# Cleanup
rm -rf "${RESULTS_DIR}"

echo "Done."
