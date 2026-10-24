#!/bin/sh
# Verify the exact commit field emitted by bunker --version output.
#
# This is deliberately stricter than a substring search: deployment must not
# accept a stale binary whose arbitrary output happens to contain the expected
# SHA. Exactly one commit field is required, and its value must match exactly.
set -eu

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
	printf 'usage: %s EXPECTED_COMMIT\n' "$0" >&2
	exit 2
fi

expected=$1
awk -v expected="$expected" '
$1 == "commit:" {
    count++
    if (NF == 2 && $2 == expected) {
        matches++
    }
}
END {
    exit (count == 1 && matches == 1) ? 0 : 1
}
'
