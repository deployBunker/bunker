#!/bin/sh
# RELEASE-026 regression tests for scripts/board-jsonl-guard.sh.
#
# The corruption this row was filed for (BFS/RELEASE-026) was stash-conflict
# markers (<<<<<<< / ======= / >>>>>>>) left inside .coding-hermes/board/*.jsonl.
# These tests run the guard ONLY against scratch copies in a mktemp -d dir —
# never against the live board.
set -u

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
GUARD="$ROOT/scripts/board-jsonl-guard.sh"

pass=0
fail=0
ok() { printf 'PASS: %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL: %s\n' "$1"; fail=$((fail + 1)); }

SCRATCH=$(mktemp -d) || exit 2
trap 'rm -rf "$SCRATCH"' EXIT

# A small, valid board fixture.
valid_board() {
	printf '%s\n' \
		'{"id":"OPS-001","status":"closed"}' \
		'{"id":"OPS-002","status":"open"}'
}

# Case 1: a valid board copy passes (exit 0).
printf '%s\n' '{"id":"OPS-001","status":"closed"}' '{"id":"OPS-002","status":"open"}' > "$SCRATCH/valid.jsonl"
out=$("$GUARD" "$SCRATCH/valid.jsonl" 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
	ok "valid board copy passes (rc=0): $out"
else
	bad "valid board copy should pass, got rc=$rc: $out"
fi

# Case 2: a leading stash-conflict block is rejected with a 'conflict marker' diagnostic.
{
	printf '%s\n' '{"id":"OPS-001","status":"closed"}'
	printf '%s\n' '<<<<<<< HEAD'
	printf '%s\n' '{"id":"OPS-002","status":"open"}'
	printf '%s\n' '>>>>>>> abc1234'
} > "$SCRATCH/conflict-lead.jsonl"
out=$("$GUARD" "$SCRATCH/conflict-lead.jsonl" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'conflict marker'; then
	ok "leading conflict markers rejected (rc=$rc): $(printf '%s' "$out" | grep 'conflict marker' | head -n1)"
else
	bad "leading conflict markers: expected rc!=0 + 'conflict marker', got rc=$rc: $out"
fi

# Case 3: a trailing conflict block after valid rows is rejected.
{
	printf '%s\n' '{"id":"OPS-001","status":"closed"}'
	printf '%s\n' '{"id":"OPS-002","status":"open"}'
	printf '%s\n' '======='
} > "$SCRATCH/conflict-tail.jsonl"
out=$("$GUARD" "$SCRATCH/conflict-tail.jsonl" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'conflict marker'; then
	ok "trailing conflict marker rejected (rc=$rc): $(printf '%s' "$out" | grep 'conflict marker' | head -n1)"
else
	bad "trailing conflict marker: expected rc!=0 + 'conflict marker', got rc=$rc: $out"
fi

# Case 4: a plain invalid JSON line is still rejected.
{
	printf '%s\n' '{"id":"OPS-001","status":"closed"}'
	printf '%s\n' 'not json at all'
} > "$SCRATCH/invalid.jsonl"
out=$("$GUARD" "$SCRATCH/invalid.jsonl" 2>&1)
rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'not valid JSON'; then
	ok "invalid JSON line rejected (rc=$rc): $(printf '%s' "$out" | grep 'not valid JSON' | head -n1)"
else
	bad "invalid JSON line: expected rc!=0 + 'not valid JSON', got rc=$rc: $out"
fi

# Case 5: no-argument mode checks both default board files — against a scratch
# tree with a marker in events.jsonl it must fail.
(
	CD_SCRATCH=$(mktemp -d) || exit 2
	mkdir -p "$CD_SCRATCH/.coding-hermes/board"
	printf '%s\n' '{"id":"OPS-001"}' > "$CD_SCRATCH/.coding-hermes/board/tasks.jsonl"
	{
		printf '%s\n' '{"id":"E-001"}'
		printf '%s\n' '<<<<<<< Updated upstream'
	} > "$CD_SCRATCH/.coding-hermes/board/events.jsonl"
	cd "$CD_SCRATCH" || exit 2
	"$GUARD" 2>&1
)
out=$?
if [ "$out" -ne 0 ]; then
	ok "default no-arg mode fails on marker in events.jsonl (rc=$out)"
else
	bad "default no-arg mode should fail on marker in events.jsonl, got rc=0"
fi

# Case 6: a missing file is fail-closed (rc!=0), never skipped.
out=$("$GUARD" "$SCRATCH/does-not-exist.jsonl" 2>&1)
rc=$?
if [ "$rc" -ne 0 ]; then
	ok "missing file is fail-closed (rc=$rc): $out"
else
	bad "missing file should be fail-closed, got rc=0"
fi

if [ "$fail" -ne 0 ]; then
	printf 'FAIL: board-jsonl-guard regression: %d case(s) failed, %d passed\n' "$fail" "$pass"
	exit 1
fi
printf 'PASS: board-jsonl-guard regression (%d cases)\n' "$pass"
exit 0
