#!/usr/bin/env bash
# Sustained load benchmark for viiwork cluster
# Usage: ./scripts/bench-sustained.sh [URL] [CONCURRENCY] [DURATION_SECS]

set -euo pipefail

URL="${1:-http://gb1:8080}"
CONC="${2:-11}"
DURATION="${3:-60}"
MODEL="${MODEL:-qwen2.5-coder-14b-instruct-q6_k}"
MAX_TOKENS="${MAX_TOKENS:-256}"
PROMPT="${PROMPT:-Write a Python function that implements merge sort with detailed comments explaining each step.}"
RESULTS_DIR=$(mktemp -d)

COUNTER=0
OK_COUNT=0
FAIL_COUNT=0
TOTAL_TOKENS=0
TOTAL_MS=0
MIN_MS=999999
MAX_MS=0
START_TIME=$(date +%s)
END_TIME=$((START_TIME + DURATION))

# Atomic file-based counters
STATS="${RESULTS_DIR}/stats"
mkdir -p "$STATS"

request() {
    local id=$1
    local req_start=$(date +%s%N)

    local body
    body=$(curl -s -w "\n%{http_code}" --max-time 120 -X POST "${URL}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d '{
            "model": "'"${MODEL}"'",
            "messages": [{"role": "user", "content": "'"${PROMPT}"'"}],
            "max_tokens": '"${MAX_TOKENS}"',
            "temperature": 0.7
        }' 2>/dev/null) || true

    local req_end=$(date +%s%N)
    local http_code=$(echo "$body" | tail -1)
    local elapsed_ms=$(( (req_end - req_start) / 1000000 ))

    local completion_tokens=0
    if [[ "$http_code" == "200" ]]; then
        body=$(echo "$body" | sed '$d')
        completion_tokens=$(echo "$body" | grep -o '"completion_tokens":[0-9]*' | grep -o '[0-9]*' || echo 0)
    fi

    echo "${http_code},${elapsed_ms},${completion_tokens}" > "${STATS}/${id}.csv"
}

# Slot runner: keeps one request in flight, re-launches when done
run_slot() {
    local slot=$1
    local seq=0
    while (( $(date +%s) < END_TIME )); do
        seq=$((seq + 1))
        request "s${slot}_r${seq}"
    done
}

echo "========================================="
echo " viiwork sustained load benchmark"
echo " Target:      ${URL}"
echo " Model:       ${MODEL}"
echo " Concurrency: ${CONC}"
echo " Duration:    ${DURATION}s"
echo "========================================="
echo ""

# Check cluster
echo "Checking cluster..."
healthy=$(curl -s "${URL}/v1/status" | grep -o '"status":"healthy"' | wc -l)
echo "Healthy backends: ${healthy}"
echo ""
echo "Running... (${DURATION}s)"
echo ""

# Launch slots
slot_pids=()
for s in $(seq 1 "$CONC"); do
    run_slot "$s" &
    slot_pids+=($!)
done

# Progress ticker
elapsed=0
while (( elapsed < DURATION )); do
    sleep 5
    elapsed=$(( $(date +%s) - START_TIME ))
    completed=$(find "${STATS}" -name '*.csv' 2>/dev/null | wc -l)
    printf "  [%3ds/%ds] %d requests completed\n" "$elapsed" "$DURATION" "$completed"
done

# Wait for all slots to finish
for pid in "${slot_pids[@]}"; do
    wait "$pid" 2>/dev/null || true
done

WALL_END=$(date +%s)
WALL_SECS=$((WALL_END - START_TIME))

# Tally results
while IFS= read -r f; do
    [[ -f "$f" ]] || continue
    IFS=',' read -r code ms tokens < "$f"
    if [[ "$code" == "200" ]]; then
        ((OK_COUNT++)) || true
        ((TOTAL_TOKENS += tokens)) || true
        ((TOTAL_MS += ms)) || true
        (( ms < MIN_MS )) && MIN_MS=$ms
        (( ms > MAX_MS )) && MAX_MS=$ms
    else
        ((FAIL_COUNT++)) || true
    fi
done < <(find "${STATS}" -name '*.csv')

echo ""
echo "========================================="
echo " Results"
echo "========================================="

TOTAL_REQ=$((OK_COUNT + FAIL_COUNT))
echo "Total requests:  ${TOTAL_REQ}"
echo "Successful:      ${OK_COUNT}"
echo "Failed (429s):   ${FAIL_COUNT}"
echo ""

if (( OK_COUNT > 0 )); then
    AVG_MS=$((TOTAL_MS / OK_COUNT))
    RPS=$(echo "scale=2; ${OK_COUNT} / ${WALL_SECS}" | bc)
    TPS=$(echo "scale=1; ${TOTAL_TOKENS} * 1000 / ${TOTAL_MS} * ${CONC}" | bc 2>/dev/null || echo "n/a")
    AGG_TPS=$(echo "scale=1; ${TOTAL_TOKENS} / ${WALL_SECS}" | bc)

    echo "Latency:         min=${MIN_MS}ms  avg=${AVG_MS}ms  max=${MAX_MS}ms"
    echo "Requests/sec:    ${RPS}"
    echo "Total tokens:    ${TOTAL_TOKENS}"
    echo "Aggregate tok/s: ${AGG_TPS}"
    echo "Wall time:       ${WALL_SECS}s"
fi

echo ""
rm -rf "${RESULTS_DIR}"
echo "Done."
