#!/usr/bin/env bash
set -euo pipefail

# Stop all viiwork containers, remove old images, rebuild from scratch.
# Docker build cache is preserved.

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

echo "==> Stopping containers..."
docker compose down --remove-orphans 2>/dev/null || true

echo "==> Removing viiwork images..."
docker images --filter "reference=viiwork*" -q | xargs -r docker rmi -f 2>/dev/null || true
docker images --filter "reference=*/viiwork*" -q | xargs -r docker rmi -f 2>/dev/null || true

echo "==> Removing dangling images..."
docker image prune -f 2>/dev/null || true

echo "==> Removing stopped containers..."
docker container prune -f 2>/dev/null || true

echo "==> Removing unused volumes..."
docker volume prune -f 2>/dev/null || true

echo "==> Removing unused networks..."
docker network prune -f 2>/dev/null || true

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
echo "==> Building viiwork (version: ${VERSION})..."
docker build --build-arg VERSION="${VERSION}" -t viiwork .

echo "==> Starting containers..."
docker compose up -d

echo "==> Waiting for viiwork to start (this may take a few minutes)..."
for i in $(seq 1 90); do
    if curl -sf http://localhost:8080/v1/models >/dev/null 2>&1; then
        echo "==> viiwork is up."
        exit 0
    fi
    sleep 2
done

echo "==> WARNING: viiwork did not respond within 3 minutes. Check: docker compose logs"
exit 1
