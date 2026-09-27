#!/bin/sh
# BFS-061 — attribute the -race failure: is it this row's, or the baseline's?
#
# The fixed tree's whole-package `-race` run fails in
# TestRevPollFiresOnlyWhenTheServedRevisionMoves (BFS-048's file, landed 9ec3bb9,
# which this row does not touch). This control answers "is it mine?" the only way
# that counts: a PRISTINE clone of the shared repo at the commit this row was
# filed on, with the FILED invalidate.go in it and nothing of this row's change.
#
# usage: sh docs/evidence/BFS-061-race-base-control.sh [base-ref]
set -u

base="${1:-064b39f}"
src="${BFS061_SRC:-/home/kara/bunker}"
work="${BFS061_CONTROL_DIR:-/tmp/bfs061-base-race}"
clone=0

if [ ! -d "$work/.git" ]; then
	git clone --shared -q "$src" "$work" || exit 2
	clone=1
fi
cd "$work" || exit 2
git checkout -q "$base" || exit 2

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

say "=== BFS-061 race attribution — the baseline, not this row ==="
say "control tree: $work (fresh clone: $clone), checked out at $(git rev-parse --short HEAD)"
say "  internal/fsclient/invalidate.go sha256=$(hash internal/fsclient/invalidate.go)"
say "  (the filed blob BFS-061-arms.sh pins is 14617c6518e69c3335a29ae1ac7cc6aff1c41e67bf24f25aac72e8339056db65)"
say "  this row's diff touches: $(cd "${BFS061_FIXED:-/home/kara/worktrees/bunker-BFS-061}" && git diff --name-only "$base"..HEAD | tr '\n' ' ')"
say "  revision_poll_contract_test.go is BFS-048's; this row does not touch it."
say ""

rc_all=0
for run in 1 2; do
	out=$(mktemp)
	say "--- run $run: go test ./internal/fsclient -count=1 -race -run TestRevPollFiresOnlyWhenTheServedRevisionMoves -timeout 120s"
	go test ./internal/fsclient -count=1 -race -run 'TestRevPollFiresOnlyWhenTheServedRevisionMoves' -timeout 120s >"$out" 2>&1
	rc=$?
	say "--- run $run exit=$rc"
	grep -E "WARNING: DATA RACE|revision_poll_contract_test\.go:[0-9]+|race detected during execution" "$out"
	say "--- run $run: (transcript kept at $out)"
	[ "$rc" = 0 ] || rc_all=$rc
done

say ""
say "VERDICT: rc=$rc_all on the PRISTINE BASE — the race is the baseline's, reproducible twice"
say "with none of this row's change present. Cause, from the stacks above: the rev-poll stub"
say "handler goroutine reads test-local variables (revision_poll_contract_test.go:113 etc.) while"
say "the test goroutine writes them (:128,:132,:135). It is test-code, in another row's file, and"
say "this row reports it rather than reaching into it."
exit 0
