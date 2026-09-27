#!/usr/bin/env bash
# REV-BUNKER-005/006 — the negative controls, as a script, so every claim in
# the evidence bundle is produced by the same files the probe uses.
#
#   sh sec-trio-controls.sh <frozen-repo> <005|006> [transcript]
#
# Each mode applies ONE mutation to a FROZEN tree, proves the mutation landed by
# sha256 (a "mutation" that came back byte-identical is not a mutation), runs the
# live wire probe, and asserts the specific probe cell that must turn RED — plus
# the cells that must STAY GREEN, which is what attributes the reddening to this
# row's defect rather than to a broken harness. It then restores the file from
# git and re-checks sha256 against the pre-mutation value: a restore that does
# not match aborts the run with exit 3.
#
#   negative-control-005.patch  registers the /graph routes with NO credential
#                               gate — the filed registration, and nothing else.
#   negative-control-006.patch  makes ArmThrottle a no-op — the filed arming
#                               (only a non-nil deny sink arms), and nothing else.
#
# exit: 0 = the control behaved (its row's cell RED, the other row's cells GREEN,
#       restore verified); 2 = infrastructure; 3 = restore failed.

set -u

REPO="${1:?usage: sh sec-trio-controls.sh <frozen-repo> <005|006> [transcript]}"
MODE="${2:?usage: sh sec-trio-controls.sh <frozen-repo> <005|006> [transcript]}"
OUT="${3:-/dev/stdout}"
PROBE="${PROBE:-/tmp/sec-trio-probe.sh}"

case "$MODE" in
005 | 006) ;;
*)
	echo "usage: sh $0 <frozen-repo> <005|006> [transcript]" >&2
	exit 2
	;;
esac

EV="$(cd -- "$(dirname -- "$PROBE")" && pwd)"
TREE=""
for cand in \
	"$EV/REV-BUNKER-005-006-007-live-negative-control-$MODE.patch" \
	"$EV/negative-control-$MODE.patch" \
	"/tmp/sec-trio-evidence/negative-control-$MODE.patch"; do
	[ -f "$cand" ] && TREE="$cand" && break
done
[ -f "$TREE" ] || { echo "missing control patch for $MODE" >&2; exit 2; }

case "$MODE" in
005) FILE="internal/server/server.go" ;;
006) FILE="internal/auth/interceptor.go" ;;
esac

say() { printf '%s\n' "$*" | tee -a "$OUT"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

cd "$REPO" || exit 2

if [ "$(git status --porcelain | wc -l)" != "0" ]; then
	say "REFUSING: $REPO is not a clean frozen tree ($(git status --porcelain | wc -l) dirty path(s))"
	exit 2
fi

BEFORE="$(hash "$FILE")"
say "negative control REV-BUNKER-$MODE"
say "  tree        : $REPO @ $(git rev-parse HEAD)"
say "  mutation    : $(basename "$TREE") applied to $FILE"
say "  pre-mutation sha256: $BEFORE"

if ! git apply "$TREE"; then
	say "FATAL: the control patch did not apply to $FILE"
	exit 2
fi
AFTER="$(hash "$FILE")"
if [ "$AFTER" = "$BEFORE" ]; then
	say "FATAL: the tree is byte-identical after applying the patch — not a mutation"
	exit 2
fi
say "  post-mutation sha256: $AFTER (differs: the mutation landed)"
say

PROBE_OUT="$EV/control-$MODE.txt"
if sh "$PROBE" "$REPO" "$PROBE_OUT"; then
	PROBE_RC=0
else
	PROBE_RC=$?
fi

T="$PROBE_OUT"
say "--- the probe under the mutation (transcript: $T)"
say "  probe exit: $PROBE_RC (1 = the probe reports FILED behaviour, which is what a control wants)"

if [ "$MODE" = "005" ]; then
	NAMED="the three unauthenticated /graph cells"
	MUST_RED='GET /graph/stats, no credentials|GET /graph/impact\?path=\.\.\., no credentials|GET /graph/related\?path=\.\.\., no credentials'
	MUST_STAY='6th failed auth is THROTTLED|GET /healthz, no credentials|POST /bunker.v1.Bunkerd/ServerInfo, valid token'
else
	NAMED="the 6th failed auth on the audit-off daemon"
	MUST_RED='6th failed auth is THROTTLED'
	MUST_STAY='GET /graph/stats, no credentials|GET /healthz, no credentials|POST /bunker.v1.Bunkerd/ServerInfo, valid token|POST /bunker.v1.Bunkerd/ServerInfo, no credentials'
fi

say
say "--- must be RED under the mutation: $NAMED"
RED_OK=1
while IFS= read -r pattern; do
	[ -n "$pattern" ] || continue
	if grep -E "\[CELL\].*$pattern" "$T" >/dev/null; then
		say "    [control RED] $(grep -E "\[CELL\].*$pattern" "$T" | head -1 | sed 's/^ *//')"
	else
		say "    [CONTROL FAILED] no [CELL] line for: $pattern — the cell cannot fail, so it proved nothing"
		RED_OK=0
	fi
done <<EOF
$(printf '%s' "$MUST_RED" | tr '|' '\n')
EOF

say
say "--- must STAY GREEN under the mutation (attribution: the reddening is this"
say "    row's defect, not a broken harness)"
STAY_OK=1
while IFS= read -r pattern; do
	[ -n "$pattern" ] || continue
	line="$(grep -E "\[ok\].*$pattern" "$T" | head -1)"
	if [ -n "$line" ]; then
		say "    [still ok] $(printf '%s' "$line" | sed 's/^ *//')"
	else
		say "    [CONTROL FAILED] a cell that must stay green is not green: $pattern"
		STAY_OK=0
	fi
done <<EOF
$(printf '%s' "$MUST_STAY" | tr '|' '\n')
EOF

say
say "--- restore (git checkout + sha256 re-check)"
git checkout -- "$FILE" || { say "RESTORE FAILED: git checkout"; exit 3; }
NOW="$(hash "$FILE")"
if [ "$NOW" != "$BEFORE" ]; then
	say "RESTORE FAILED: $FILE sha256=$NOW, expected $BEFORE"
	exit 3
fi
say "    $FILE restored byte-identical ($NOW)"
say "    tree dirty paths after restore: $(git status --porcelain | wc -l)"

if [ "$RED_OK" = "1" ] && [ "$STAY_OK" = "1" ]; then
	say "RESULT: CONTROL PASSED — REV-BUNKER-$MODE's cell turns RED when its fix is neutered"
	exit 0
fi
say "RESULT: CONTROL FAILED"
exit 1
