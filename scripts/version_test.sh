#!/usr/bin/env sh
# Tests the changelog heading extraction in version.sh:
#
#	sh scripts/version_test.sh
set -eu
VIIWORK_VERSION_LIB=1
. "$(dirname "$0")/version.sh"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
fail=0

# check writes a changelog whose top heading is $1 and expects version $2.
check() {
	printf '# Changelog\n\n%s\n\nA change.\n\n## v1.0.0\n' "$1" >"$tmp"
	got=$(changelog_version "$tmp")
	if [ "$got" = "$2" ]; then
		printf 'ok    %s -> %s\n' "$1" "$got"
	else
		printf 'FAIL  %s -> %s, want %s\n' "$1" "$got" "$2"
		fail=1
	fi
}

check '## v1.8.1' v1.8.1
check '## v2.0.0-rc.1' v2.0.0-rc.1
check '## v2.0.0-alpha.1 (contracts)' v2.0.0-alpha.1
# beta1 has no dot before the digit, unlike rc.1/alpha.1 — the suffix pattern
# must not assume one.
check '## v2.0.0-beta1' v2.0.0-beta1

exit "$fail"
