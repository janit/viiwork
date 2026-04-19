#!/usr/bin/env bash
set -euo pipefail

# Interactive deploy helper for compose configs under configs/.
#
# Usage:
#   scripts/deploy.sh                          # interactive: pick one or more, bring up
#   scripts/deploy.sh up gfx906 kv-bench       # up named configs
#   scripts/deploy.sh down gfx906              # down
#   scripts/deploy.sh logs gfx906              # follow logs (single config only)
#   scripts/deploy.sh list                     # show available
#
# Names are the compose filename minus the `docker-compose.` prefix and `.yaml`
# suffix (e.g. `configs/docker-compose.gfx906.yaml` -> `gfx906`).

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CONFIGS_DIR="${REPO_DIR}/configs"

if [[ ! -d "${CONFIGS_DIR}" ]]; then
    echo "error: ${CONFIGS_DIR} not found" >&2
    exit 1
fi

list_configs() {
    find "${CONFIGS_DIR}" -maxdepth 1 -type f -name 'docker-compose.*.yaml' -printf '%f\n' \
        | sort \
        | sed -e 's/^docker-compose\.//' -e 's/\.yaml$//'
}

compose_file_for() {
    local name="$1"
    local path="${CONFIGS_DIR}/docker-compose.${name}.yaml"
    if [[ ! -f "${path}" ]]; then
        echo "error: no compose file for '${name}' at ${path}" >&2
        echo "available:" >&2
        list_configs | sed 's/^/  /' >&2
        return 1
    fi
    printf '%s\n' "${path}"
}

resolve_names() {
    # Reads newline-separated names from stdin, prints full compose paths.
    while IFS= read -r name; do
        [[ -z "${name}" ]] && continue
        compose_file_for "${name}"
    done
}

do_up() {
    local name
    for name in "$@"; do
        local compose
        compose=$(compose_file_for "${name}") || return 1
        echo "+ docker compose -f ${compose#${REPO_DIR}/} up -d"
        (cd "${REPO_DIR}" && docker compose -f "${compose}" up -d)
    done
}

do_down() {
    local name
    for name in "$@"; do
        local compose
        compose=$(compose_file_for "${name}") || return 1
        echo "+ docker compose -f ${compose#${REPO_DIR}/} down"
        (cd "${REPO_DIR}" && docker compose -f "${compose}" down)
    done
}

do_logs() {
    if (( $# != 1 )); then
        echo "error: logs takes exactly one config name" >&2
        return 2
    fi
    local compose
    compose=$(compose_file_for "$1") || return 1
    (cd "${REPO_DIR}" && exec docker compose -f "${compose}" logs -f)
}

interactive_pick() {
    mapfile -t NAMES < <(list_configs)
    if (( ${#NAMES[@]} == 0 )); then
        echo "no compose configs found under configs/" >&2
        return 1
    fi

    echo "Available configs in configs/:"
    local i
    for i in "${!NAMES[@]}"; do
        printf '  %2d) %s\n' "$((i+1))" "${NAMES[i]}"
    done
    echo ""
    echo "Select one or more (space- or comma-separated numbers, or names):"
    read -rp "> " -a choices

    # Normalise: allow "1,2,3" as a single token too.
    local raw
    local -a picked=()
    for raw in "${choices[@]}"; do
        IFS=', ' read -ra parts <<< "${raw}"
        local part
        for part in "${parts[@]}"; do
            [[ -z "${part}" ]] && continue
            if [[ "${part}" =~ ^[0-9]+$ ]]; then
                local idx=$((part - 1))
                if (( idx < 0 || idx >= ${#NAMES[@]} )); then
                    echo "error: index out of range: ${part}" >&2
                    return 1
                fi
                picked+=("${NAMES[idx]}")
            else
                picked+=("${part}")
            fi
        done
    done

    if (( ${#picked[@]} == 0 )); then
        echo "nothing selected" >&2
        return 1
    fi

    echo ""
    echo "Action? [up/down/logs, default up]:"
    read -rp "> " action
    case "${action:-up}" in
        up)   do_up   "${picked[@]}" ;;
        down) do_down "${picked[@]}" ;;
        logs) do_logs "${picked[@]}" ;;
        *)    echo "unknown action: ${action}" >&2; return 2 ;;
    esac
}

cmd="${1:-}"
case "${cmd}" in
    list|ls)
        list_configs
        ;;
    up)
        shift
        (( $# == 0 )) && { echo "usage: deploy.sh up <name> [name...]" >&2; exit 2; }
        do_up "$@"
        ;;
    down)
        shift
        (( $# == 0 )) && { echo "usage: deploy.sh down <name> [name...]" >&2; exit 2; }
        do_down "$@"
        ;;
    logs)
        shift
        do_logs "$@"
        ;;
    help|-h|--help)
        sed -n '3,15p' "$0"
        ;;
    "")
        interactive_pick
        ;;
    *)
        echo "unknown command: ${cmd}" >&2
        sed -n '3,15p' "$0" >&2
        exit 2
        ;;
esac
