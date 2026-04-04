#!/usr/bin/env bash
set -euo pipefail

# Kill all viiwork containers, remove images, and delete generated configs.
# Does NOT touch downloaded models in models/.

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

if [[ "${1:-}" != "--force" ]]; then
    echo "This will destroy all viiwork containers, images, configs, and .env."
    echo "Downloaded models in models/ will be kept."
    echo ""
    read -rp "Proceed? (y/n): " confirm
    [[ "$confirm" == "y" ]] || { echo "Aborted."; exit 0; }
    echo ""
fi

echo "==> Stopping containers..."
docker compose down --remove-orphans 2>/dev/null || true

echo "==> Removing viiwork containers (including stopped)..."
docker ps -a --filter "name=viiwork" -q | xargs -r docker rm -f 2>/dev/null || true

echo "==> Removing viiwork images..."
docker images --filter "reference=viiwork*" -q | xargs -r docker rmi -f 2>/dev/null || true
docker images --filter "reference=*/viiwork*" -q | xargs -r docker rmi -f 2>/dev/null || true

echo "==> Pruning dangling images, stopped containers, unused volumes and networks..."
docker image prune -f 2>/dev/null || true
docker container prune -f 2>/dev/null || true
docker volume prune -f 2>/dev/null || true
docker network prune -f 2>/dev/null || true

echo "==> Removing generated configs..."
rm -f viiwork.yaml viiwork-*.yaml docker-compose.yaml opencode.json
rm -rf bin/

echo "==> Removing .env..."
rm -f .env

echo ""
echo "=== Purged ==="
echo "  Kept: models/ (downloaded GGUFs)"
echo "  Removed: containers, images, configs, .env, bin/"
echo ""
echo "To start fresh: ./scripts/setup-node.sh"
