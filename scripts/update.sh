#!/usr/bin/env bash
set -euo pipefail

# Update viiwork to the latest version.
# Rebuilds the Docker image and restarts the container with zero-downtime intent.
#
# Usage: ./scripts/update.sh [--no-restart]

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

NO_RESTART=false
for arg in "$@"; do
    case "$arg" in
        --no-restart) NO_RESTART=true ;;
    esac
done

echo "==> Pulling latest changes..."
git pull --ff-only

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
echo "==> Building Docker image (version: ${VERSION})..."
docker build --no-cache --build-arg VERSION="${VERSION}" -t viiwork .

if [ "$NO_RESTART" = true ]; then
    echo "==> Image built. Skipping restart (--no-restart)."
    exit 0
fi

echo "==> Restarting container..."
docker compose down
docker compose up -d

echo "==> Waiting for viiwork to start (this may take a few minutes while models load)..."
for i in $(seq 1 90); do
    if curl -sf http://localhost:8080/v1/models >/dev/null 2>&1; then
        echo "==> viiwork is up."
        exit 0
    fi
    sleep 2
done

echo "==> WARNING: viiwork did not respond within 3 minutes. Check logs with: docker compose logs"
exit 1
