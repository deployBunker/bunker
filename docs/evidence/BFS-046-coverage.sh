#!/bin/sh
# BFS-046 — the COVERAGE FLOOR over the invalidation packages, as a NUMBER.
#
# The floor is a NUMBER that fails the run when the invalidation packages lose
# coverage, and it is stated against a MEASURED value rather than a round number
# chosen to look tidy. Measured on the tree this row tested:
#
#   internal/invalidation       82.3% of statements
#   internal/fsclient           69.8%
#   internal/server/webdav      81.3%
#   MERGED (the three)          75.5%
#
# The enforced floors sit just below those measurements, so a regression trips
# them and ordinary churn does not. A floor nobody can measure is not a floor:
# the numbers and the command that produced them are printed by this script, and
# the merged figure is the one the row states.
#
# usage: sh docs/evidence/BFS-046-coverage.sh

set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

FLOOR_INVALIDATION=82.0
FLOOR_FSCLIENT=69.0
FLOOR_WEBDAV=81.0
FLOOR_MERGED=75.0

profile=$(mktemp)
trap 'rm -f "$profile"' EXIT

say() { printf '%s\n' "$*"; }

say "BFS-046 coverage floor — $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo no-git) · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
say ""

# The per-package figures come from `go test -cover` (what CI gates report) and
# the merged figure from the merged profile (the number the row states).
say "--- per package: go test -cover ./internal/invalidation/... ./internal/fsclient/... ./internal/server/webdav/..."
out=$(go test -covermode=atomic -coverprofile="$profile" \
	./internal/invalidation/... ./internal/fsclient/... ./internal/server/webdav/... 2>&1)
printf '%s\n' "$out"
if ! printf '%s\n' "$out" | grep -q '^ok '; then
	say "COVERAGE RUN FAILED: at least one package did not pass, so its coverage figure is meaningless"
	exit 1
fi

# Pull each package's figure out of the run output.
pct() { printf '%s\n' "$out" | awk -v p="$1" '$0 ~ p { for (i = 1; i <= NF; i++) if ($i == "coverage:") print $(i+1) }' | tr -d '%' | head -1; }

inv=$(pct 'internal/invalidation')
cli=$(pct 'internal/fsclient')
wdb=$(pct 'internal/server/webdav')

merged=$(go tool cover -func="$profile" | awk '/^total:/ { print $NF }' | tr -d '%')
if [ -z "$merged" ]; then
	say "COVERAGE PROFILE FAILED: go tool cover produced no total"
	exit 1
fi

say ""
say "--- the numbers"
say "  internal/invalidation   ${inv}%   floor ${FLOOR_INVALIDATION}%"
say "  internal/fsclient       ${cli}%   floor ${FLOOR_FSCLIENT}%"
say "  internal/server/webdav  ${wdb}%   floor ${FLOOR_WEBDAV}%"
say "  MERGED (all three)      ${merged}%   floor ${FLOOR_MERGED}%"
say ""

rc=0
below() { # <label> <value> <floor>
	awk -v v="$2" -v f="$3" 'BEGIN { exit !(v + 0 < f + 0) }' && {
		say "FLOOR BREACHED: $1 is $2% and the floor is $3%"
		rc=1
	}
}
below internal/invalidation "$inv" "$FLOOR_INVALIDATION"
below internal/fsclient "$cli" "$FLOOR_FSCLIENT"
below internal/server/webdav "$wdb" "$FLOOR_WEBDAV"
below MERGED "$merged" "$FLOOR_MERGED"

if [ "$rc" -eq 0 ]; then
	say "floor held: MERGED ${merged}% >= ${FLOOR_MERGED}% and every package is above its own floor"
else
	say "floor NOT held (see the FLOOR BREACHED lines above)"
fi
exit "$rc"
