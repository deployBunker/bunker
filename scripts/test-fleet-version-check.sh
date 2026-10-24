#!/bin/sh
# Fixture-driven tests for scripts/fleet-version-check.sh.
set -u

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
CHECK="$ROOT/scripts/fleet-version-check.sh"

pass=0
fail=0
ok() { printf 'PASS: %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL: %s\n' "$1"; fail=$((fail + 1)); }

run_case() {
	name=$1
	expected=$2
	want_rc=$3
	fixture=$4
	printf '%s\n' "$fixture" | "$CHECK" "$expected" >/dev/null 2>&1
	got_rc=$?
	if [ "$got_rc" -eq "$want_rc" ]; then
		ok "$name"
	else
		bad "$name (expected rc $want_rc, got $got_rc)"
	fi
}

run_case "exact commit field is accepted" "abc1234" 0 \
	'bunkerd 0.2.1
  commit:     abc1234
  built:      2026-10-24T00:00:00Z'
run_case "a stale commit is rejected" "abc1234" 1 \
	'bunkerd 0.2.1
  commit:     old5678
  built:      2026-10-02T00:00:00Z'
run_case "substring in another field is rejected" "abc1234" 1 \
	'bunkerd 0.2.1
  commit:     old5678
  built:      abc1234-is-not-the-commit'
run_case "duplicate commit fields are rejected" "abc1234" 1 \
	'bunkerd 0.2.1
  commit:     abc1234
  commit:     abc1234'
run_case "missing commit field is rejected" "abc1234" 1 \
	'bunkerd 0.2.1
  built:      2026-10-24T00:00:00Z'

if [ "$fail" -ne 0 ]; then
	printf 'FAIL: %d case(s), %d passed\n' "$fail" "$pass"
	exit 1
fi
printf 'PASS: fleet-version-check (%d cases)\n' "$pass"
