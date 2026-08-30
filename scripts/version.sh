#!/usr/bin/env sh
# Print the version string to stamp into a build (-X main.version=...).
#
# This exists because the two repositories know their version in different
# ways, and the obvious `git describe` is right in only one of them.
#
# The public repo is tagged by scripts/publish.sh, so a release build there sits
# exactly on its tag and describe is authoritative. The private repo
# deliberately carries no tags at all — they are created on the public repo at
# publish time — so describe there reports the newest tag it happens to still
# see. That went stale at v1.5.2 and quietly stamped every build of v1.5.3,
# v1.6.0 and v1.6.1 with the wrong release number, which is the number an
# operator reads off /v1/status when a host misbehaves.
#
# CHANGELOG.md's top heading is the one place the private tree does know its own
# version, and it is updated as part of cutting a release, so it cannot drift
# from what was shipped the way a tag the repo does not carry can.
set -eu
cd "$(dirname "$0")/.."

# An exact tag wins: that is a release build, and the tag is the whole answer.
if tag=$(git describe --tags --exact-match --dirty 2>/dev/null); then
	printf '%s\n' "$tag"
	exit 0
fi

version=$(sed -n 's/^## \(v[0-9][0-9.]*\).*/\1/p' CHANGELOG.md 2>/dev/null | head -1)

# No changelog heading either — an unpacked tarball, or a tree this script was
# copied into. Fall back rather than fail a build over a version string.
if [ -z "$version" ]; then
	version=$(git describe --tags --always 2>/dev/null || echo dev)
fi

# Between releases the heading names the version being prepared, so the sha is
# what distinguishes one build of it from another.
# if-statements rather than `[ ... ] && x=y`: under set -e a false test as the
# last command of an AND-list exits the script, which would fail every build
# outside a git checkout — an unpacked release tarball, say.
sha=$(git rev-parse --short HEAD 2>/dev/null || true)
if [ -n "$sha" ]; then
	version="${version}-g${sha}"
fi
if [ -n "$(git status --porcelain 2>/dev/null || true)" ]; then
	version="${version}-dirty"
fi

printf '%s\n' "$version"
