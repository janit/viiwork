#!/usr/bin/env bash
# Shared helpers for bench-harness/run_*.sh scripts.
#
# Callers may set before sourcing:
#   LOG_FILE  (optional) path where log() mirrors output via tee

log() {
    if [[ -n "${LOG_FILE:-}" ]]; then
        printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*" | tee -a "${LOG_FILE}"
    else
        printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"
    fi
}

# Reads /v1/status JSON on stdin, prints healthy_backends count (0 if missing).
healthy_count() {
    python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(0)
    sys.exit(0)
print(d.get("healthy_backends", 0))
'
}

# wait_healthy_viiwork <base_url> <required_count> [timeout_seconds] [label]
# Polls <base_url>/v1/status until healthy_backends >= required_count.
# Returns 0 on success, 1 on timeout. Logs progress via log().
wait_healthy_viiwork() {
    local base_url="$1"
    local required="$2"
    local timeout="${3:-600}"
    local label="${4:-backends}"
    local deadline=$(( $(date +%s) + timeout ))
    log "waiting for ${label}: ${required} healthy via ${base_url}/v1/status (up to ${timeout}s)"
    while (( $(date +%s) < deadline )); do
        if status_json=$(curl -fsS --max-time 3 "${base_url}/v1/status" 2>/dev/null); then
            local healthy
            healthy=$(printf '%s' "$status_json" | healthy_count)
            if (( healthy >= required )); then
                log "${label}: ${healthy}/${required} healthy"
                return 0
            fi
            log "${label}: ${healthy}/${required} healthy, waiting..."
        fi
        sleep 10
    done
    log "ERROR: ${label} did not reach ${required}/${required} healthy within ${timeout}s"
    return 1
}
