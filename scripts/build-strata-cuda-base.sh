#!/usr/bin/env bash
# Builds the CUDA 13 Strata image docker/Dockerfile.strata-cuda sits on, at the
# release docker/pins.env names, and tags it STRATA_CUDA_IMAGE.
#
# Upstream publishes ready-made images (ghcr.io/niko1221/strata:<version>-cuda13)
# some time after a release; 0.2.14 and 0.2.15 had none. For such a release
# this makes the same image from upstream's own tooling (then STRATA_CUDA_IMAGE
# is strata-cuda-base:<version without v>), and trusts nothing it does not
# check:
#   - the source is cloned at the tag and must be exactly STRATA_COMMIT;
#   - the engine package comes from tools/docker/fetch-engines.py (docker/ up
#     to 0.2.15), which checks the
#     signed update feed, the archive's size and sha256, and every file's hash;
#     its archive sha256 must also be STRATA_CUDA_ENGINE_SHA256;
#   - tools/docker/build.sh (upstream's) assembles the image. Since 0.2.16 it
#     carries engine/third_party/strata-gguf-py in its build context itself;
#     at 0.2.14 and 0.2.15 this script had to add it.
#
#   scripts/build-strata-cuda-base.sh
#
# Needs git, python3 and docker, and the internet. Nothing is pushed.
set -euo pipefail
cd "$(dirname "$0")/.."
. docker/pins.env

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git clone --quiet --depth 1 --branch "$STRATA_VERSION" https://github.com/Niko1221/Strata "$work/src"
got=$(git -C "$work/src" rev-parse HEAD)
if [ "$got" != "$STRATA_COMMIT" ]; then
	echo "build-strata-cuda-base: $STRATA_VERSION is $got, docker/pins.env says $STRATA_COMMIT" >&2
	exit 1
fi

cd "$work/src"
[ -f tools/docker/build.sh ] || { echo "build-strata-cuda-base: no tools/docker/build.sh at $STRATA_VERSION; this script follows the 0.2.16 layout" >&2; exit 1; }
sha=$(python3 -I tools/docker/fetch-engines.py --release-tag "$STRATA_VERSION" --list 2>/dev/null | awk -F'\t' '$1=="cuda13" {print $5}')
if [ "$sha" != "$STRATA_CUDA_ENGINE_SHA256" ]; then
	echo "build-strata-cuda-base: the feed lists the cuda13 engine as ${sha:-nothing}, docker/pins.env says $STRATA_CUDA_ENGINE_SHA256" >&2
	exit 1
fi
pkg=$(python3 -I tools/docker/fetch-engines.py --release-tag "$STRATA_VERSION" --flavors cuda13 --out "$work/pkgs" | awk -F'\t' '$1=="cuda13" {print $3}')
[ -d "$pkg" ] || { echo "build-strata-cuda-base: no engine package" >&2; exit 1; }

sh tools/docker/build.sh --flavor cuda --engine "$pkg" --tag "$STRATA_CUDA_IMAGE"

# What was built must be what was pinned.
built=$(docker run --rm --entrypoint cat "$STRATA_CUDA_IMAGE" /opt/strata/terminal/COMMIT | tr -d '[:space:]')
if [ "$built" != "$STRATA_COMMIT" ]; then
	echo "build-strata-cuda-base: the image's COMMIT is $built, not $STRATA_COMMIT" >&2
	exit 1
fi
echo "built $STRATA_CUDA_IMAGE at $STRATA_COMMIT"
