#!/usr/bin/env bash
# Switch the running viiwork node between the stable foundation
# (viiwork:latest) and the experimental track (viiwork:gfx906) in
# place, without re-running setup-node.sh.
#
# Edits docker-compose.yaml's `image:` line and `build:` directive,
# verifies the target image exists locally, then restarts the stack.
# Bails before touching anything if the target image is missing.
#
# See BUILDS.md for the full comparison of the two builds and when
# each is appropriate.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yaml}"

if [ ! -f "${COMPOSE_FILE}" ]; then
    echo "ERROR: ${COMPOSE_FILE} not found in $(pwd)."
    echo "Run scripts/setup-node.sh first."
    exit 1
fi

# Detect current track from the first `image:` line in the compose file.
# Both single-instance and multi-instance compose files written by
# setup-node.sh use the same image across all services on a node.
current_image=$(grep -m1 -E '^[[:space:]]*image:[[:space:]]' "${COMPOSE_FILE}" | awk '{print $2}')
case "${current_image}" in
    viiwork|viiwork:latest)
        current_track="stable"
        ;;
    viiwork:gfx906)
        current_track="experimental"
        ;;
    *)
        echo "ERROR: ${COMPOSE_FILE} does not look like a viiwork compose file."
        echo "Found image: '${current_image}' on the first service."
        exit 1
        ;;
esac

echo "=== viiwork node build switcher ==="
echo ""
echo "Current build: ${current_track}  (image: ${current_image})"
echo ""
echo "Switch to:"
echo "  1) Stable foundation    (viiwork:latest, upstream llama.cpp)"
echo "  2) Experimental track   (viiwork:gfx906, stripped gfx906 fork)"
echo "  q) Cancel"
read -rp "Select [1/2/q]: " choice

case "${choice}" in
    1|stable)
        target_track="stable"
        target_image="viiwork"
        target_build_line="    build: ."
        ;;
    2|experimental|gfx906|fork)
        target_track="experimental"
        target_image="viiwork:gfx906"
        # Experimental images have no in-tree build context (the fork
        # tree lives outside this repo on the dev node only). Drop the
        # `build:` line so docker compose doesn't try to rebuild and
        # fail.
        target_build_line=""
        ;;
    q|Q|"")
        echo "cancelled"
        exit 0
        ;;
    *)
        echo "unknown choice: ${choice}"
        exit 1
        ;;
esac

if [ "${target_track}" = "${current_track}" ]; then
    echo "Already on ${current_track}. Nothing to do."
    exit 0
fi

# Verify target image exists locally before touching anything. The
# experimental image is the common failure case -- the user has to
# build or transfer it before switching to it.
if ! docker image inspect "${target_image}" >/dev/null 2>&1; then
    echo ""
    echo "ERROR: target image ${target_image} not found locally."
    echo ""
    if [ "${target_track}" = "experimental" ]; then
        echo "The experimental image must exist on this host before switching."
        echo "Build it on a node that has the fork tree:"
        echo "    make docker-gfx906   # or: make docker-experimental"
        echo ""
        echo "Or transfer it from another node:"
        echo "    docker save viiwork:gfx906 | ssh $(hostname) 'docker load'"
    else
        echo "Build the stable image with:"
        echo "    make docker          # or: make docker-stable"
    fi
    exit 2
fi

# Backup the existing compose file before editing
backup="${COMPOSE_FILE}.bak.$(date +%Y%m%dT%H%M%S)"
cp "${COMPOSE_FILE}" "${backup}"
echo ""
echo "Backed up ${COMPOSE_FILE} -> ${backup}"

# Rewrite all `image: viiwork...` lines to the target image.
sed -i -E "s|^([[:space:]]*image:[[:space:]]+)viiwork(:[^[:space:]]+)?$|\1${target_image}|" "${COMPOSE_FILE}"

# Handle the `build: .` line. Two cases:
#   1. switching to stable: ensure each service has `    build: .`
#      after its `image:` line (compose's first-time-build path)
#   2. switching to experimental: remove any `    build: .` lines so
#      compose doesn't try to rebuild from this repo's Dockerfile
if [ "${target_track}" = "experimental" ]; then
    sed -i '/^[[:space:]]*build:[[:space:]]*\.[[:space:]]*$/d' "${COMPOSE_FILE}"
else
    # Add `    build: .` after every `image: viiwork` line that doesn't
    # already have one immediately following it. Use awk for the lookahead.
    awk '
        /^[[:space:]]*image:[[:space:]]+viiwork(:latest)?$/ {
            print
            getline next_line
            if (next_line ~ /^[[:space:]]*build:[[:space:]]*\.[[:space:]]*$/) {
                print next_line
            } else {
                print "    build: ."
                print next_line
            }
            next
        }
        { print }
    ' "${COMPOSE_FILE}" > "${COMPOSE_FILE}.tmp" && mv "${COMPOSE_FILE}.tmp" "${COMPOSE_FILE}"
fi

echo "Rewrote ${COMPOSE_FILE}: ${current_track} -> ${target_track}"
echo ""
echo "Diff:"
diff -u "${backup}" "${COMPOSE_FILE}" || true
echo ""

read -rp "Restart the stack now? [y/N]: " restart
case "${restart}" in
    y|Y|yes)
        echo "Stopping current containers..."
        docker compose -f "${COMPOSE_FILE}" down
        echo "Starting on ${target_track}..."
        docker compose -f "${COMPOSE_FILE}" up -d
        echo ""
        echo "=== Switched to ${target_track} (${target_image}) ==="
        echo "Tail logs with:  docker compose logs -f"
        ;;
    *)
        echo "Skipped restart. Apply manually with:"
        echo "    docker compose -f ${COMPOSE_FILE} down"
        echo "    docker compose -f ${COMPOSE_FILE} up -d"
        ;;
esac
