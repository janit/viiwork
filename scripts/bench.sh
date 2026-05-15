#!/usr/bin/env bash
# Stress benchmark for viiwork cluster
# Usage: ./scripts/bench.sh [URL] [MAX_CONCURRENCY]
#
# Env:
#   PROMPT_SIZE=short|medium|long   pick a built-in prompt (default: short)
#   PROMPT_FILE=path/to/prompt.txt  use a custom prompt from a file
#   PROMPT="literal string"         use a literal prompt (overrides above)
#   PROMPT_SWEEP=1                  run each concurrency level at short, medium,
#                                   and long (overrides PROMPT_SIZE)
#   MODEL=<id>                      target model id
#   MAX_TOKENS=<n>                  max_tokens per request (default: 256)
#
# Short prompts (~80 chars) wildly under-predict real workloads — prompt-eval
# CPU is ~linear in token count, so a "passes at C=8" certificate from a short
# bench can mask backend crashes under real ~1k-char traffic. Use PROMPT_SWEEP=1
# (or PROMPT_SIZE=long) when validating a config for production.

set -euo pipefail

URL="${1:-http://gb1:8080}"
MAX_CONC="${2:-10}"
MODEL="${MODEL:-qwen2.5-coder-14b-instruct-q6_k}"
MAX_TOKENS="${MAX_TOKENS:-256}"
RESULTS_DIR=$(mktemp -d)

# Built-in prompts at three sizes. Sizes shown are approximate byte counts.
PROMPT_SHORT="Write a Python function that implements merge sort with detailed comments explaining each step."

read -r -d '' PROMPT_MEDIUM <<'EOF' || true
You are an experienced Python developer. Implement merge sort from scratch in
idiomatic Python. The function should take a list of comparable items and
return a new sorted list, leaving the input unchanged. Include detailed
docstrings, type hints, inline comments at every nontrivial branch, and a
short main block at the bottom that demonstrates the function on three inputs:
an already-sorted list, a reverse-sorted list, and a list with duplicates.
EOF

read -r -d '' PROMPT_LONG <<'EOF' || true
You are an experienced Python developer doing a code review for a junior
colleague. They have written a merge-sort implementation and want feedback on
correctness, readability, and performance. Please write the merge-sort function
yourself from scratch first, using idiomatic Python 3. The function should take
a list of comparable items and return a new sorted list, leaving the input
unchanged. Then walk through the algorithm step by step, explaining the
recursion, the merge step, the asymptotic complexity, why merge sort is stable,
and where it is preferable to quicksort or timsort. Include comprehensive
docstrings with parameter descriptions, return value, raises, and examples.
Add type hints throughout. Add inline comments at every nontrivial branch,
especially in the merge step where readers commonly get confused by the two
index variables. After the function, include a short main block at the bottom
that demonstrates the function on six inputs: an already-sorted list, a
reverse-sorted list, a list with duplicates, an empty list, a single-element
list, and a list of mixed positive and negative integers. For each, print the
input, the output, and a one-line description of what edge case it exercises.
Finally, list three improvements the junior could make to their version if
their implementation looked like the most common naive attempt: forgetting to
return a new list, using list slicing inefficiently, and not handling the empty
list as a base case.
EOF

select_prompt() {
    local size="$1"
    case "$size" in
        short) echo "$PROMPT_SHORT" ;;
        medium) echo "$PROMPT_MEDIUM" ;;
        long) echo "$PROMPT_LONG" ;;
        *) echo "ERROR: unknown PROMPT_SIZE=$size (use short, medium, or long)" >&2; exit 1 ;;
    esac
}

# Resolve the prompt(s) we'll exercise.
SWEEP="${PROMPT_SWEEP:-0}"
if [[ -n "${PROMPT:-}" ]]; then
    PROMPT_SIZES=(custom)
    PROMPT_TEXTS=("$PROMPT")
elif [[ -n "${PROMPT_FILE:-}" ]]; then
    [[ -r "$PROMPT_FILE" ]] || { echo "ERROR: cannot read PROMPT_FILE=$PROMPT_FILE" >&2; exit 1; }
    PROMPT_SIZES=(file)
    PROMPT_TEXTS=("$(<"$PROMPT_FILE")")
elif [[ "$SWEEP" == "1" ]]; then
    PROMPT_SIZES=(short medium long)
    PROMPT_TEXTS=("$PROMPT_SHORT" "$PROMPT_MEDIUM" "$PROMPT_LONG")
else
    SIZE="${PROMPT_SIZE:-short}"
    PROMPT_SIZES=("$SIZE")
    PROMPT_TEXTS=("$(select_prompt "$SIZE")")
    if [[ "$SIZE" == "short" && -z "${PROMPT_SIZE:-}" ]]; then
        echo "NOTE: defaulting to short prompts (~80 chars). Real workloads with"
        echo "      long prompts (~1k chars) stress CPU prompt-eval much harder."
        echo "      For production validation, set PROMPT_SIZE=long or PROMPT_SWEEP=1."
        echo ""
    fi
fi

# JSON-escape a string for inline embedding in the request body.
json_escape() {
    python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' <<<"$1" 2>/dev/null \
        || jq -Rs . <<<"$1" 2>/dev/null \
        || printf '%s' "\"$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/$/\\n/' | tr -d '\n')\""
}

request() {
    local id=$1
    local prompt_json=$2
    local start=$(date +%s%N)

    local http_code
    local body
    body=$(curl -s -w "\n%{http_code}" -X POST "${URL}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d '{
            "model": "'"${MODEL}"'",
            "messages": [{"role": "user", "content": '"${prompt_json}"'}],
            "max_tokens": '"${MAX_TOKENS}"',
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
    local prompt_json=$2
    local label=$3
    local wave_start=$(date +%s%N)

    echo "--- Concurrency: ${concurrency}  (prompt: ${label}) ---"

    pids=()
    for i in $(seq 1 "$concurrency"); do
        request "${label}_${concurrency}_${i}" "$prompt_json" &
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
        local f="${RESULTS_DIR}/${label}_${concurrency}_${i}.csv"
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
echo " Target:  ${URL}"
echo " Model:   ${MODEL}"
echo " Prompts: ${PROMPT_SIZES[*]} (max_tokens=${MAX_TOKENS})"
echo "========================================="
echo ""

# Check cluster is up
echo "Checking cluster..."
status=$(curl -s "${URL}/v1/status" 2>/dev/null) || { echo "ERROR: Cannot reach ${URL}"; exit 1; }
healthy=$(echo "$status" | grep -o '"status":"healthy"' | wc -l)
echo "Healthy backends: ${healthy}"
echo ""

# Ramp up concurrency for each prompt size
for idx in "${!PROMPT_SIZES[@]}"; do
    size_label="${PROMPT_SIZES[$idx]}"
    prompt_text="${PROMPT_TEXTS[$idx]}"
    prompt_json=$(json_escape "$prompt_text")
    chars=${#prompt_text}
    echo "=== Prompt: ${size_label} (~${chars} chars) ==="
    for c in 1 2 4 6 8 10; do
        if (( c > MAX_CONC )); then
            break
        fi
        run_wave "$c" "$prompt_json" "$size_label"
    done
    if (( MAX_CONC > 10 )); then
        run_wave "$MAX_CONC" "$prompt_json" "$size_label"
    fi
done

# Cleanup
rm -rf "${RESULTS_DIR}"

echo "Done."
