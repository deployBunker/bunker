#!/bin/bash
# ══════════════════════════════════════════════════════════════════════════════
# test-runner-stranded-reap.sh — fixture-driven test for
# scripts/runner-stranded-reap.sh (INT-CI-081 acceptance 3).
#
# The reaper's whole risk is WHO it kills, so the test drives the real script
# against a synthetic process table (RUNNER_REAP_PS_SOURCE) with an injected
# kill binary (RUNNER_REAP_KILL_CMD) — no real process is ever signalled.
#
# Cells:
#   1. a stranded Runner.Worker tree (worker + bash + bunkerd children) that is
#      NOT an ancestor of self is reaped leaf-first (child before parent);
#   2. this job's own worker (an ancestor of self) is never touched;
#   3. a Runner.Worker younger than --min-age is left alone;
#   4. the Runner.Listener and PID 1 are never matched;
#   5. --dry-run reports the same set and kills nothing;
#   6. an unreadable process table is fail-open by default (exit 0) and
#      fail-closed under --strict (exit 1).
#
# Exit codes: 0 all cells pass, 1 otherwise.
# ══════════════════════════════════════════════════════════════════════════════
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REAP="$HERE/runner-stranded-reap.sh"
[ -x "$REAP" ] || { echo "FAIL: $REAP is not executable"; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
KILL_LOG="$WORK/killed.log"
: > "$KILL_LOG"

# Kill stub: records "<sig> <pid>" and exits 0 (nothing real is killed).
KILL_STUB="$WORK/kill-stub.sh"
cat > "$KILL_STUB" <<'EOF'
#!/bin/bash
printf '%s %s\n' "$1" "$2" >> "$RUNNER_REAP_KILL_LOG"
exit 0
EOF
chmod +x "$KILL_STUB"

# Synthetic table: pid ppid etimes args
FIXTURE="$WORK/ps.txt"
cat > "$FIXTURE" <<'EOF'
1 0 999999 /sbin/init splash
10 1 500 /opt/actions-runner/bin/Runner.Listener run --startuptype service
100 1 300 /opt/actions-runner/bin.2.337.0/Runner.Worker spawnclient 157 160
101 100 280 bash scripts/root-suite.sh
102 101 275 /home/bunker/.cache/bin/bunkerd --config /etc/bunkerd/config.yaml
200 1 40 /opt/actions-runner/bin/Runner.Worker spawnclient 300 400
201 200 39 bash scripts/runner-stranded-reap.sh
202 1 20 /opt/actions-runner/bin/Runner.Worker spawnclient 500 600
EOF

FAILS=0
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }
pass() { echo "ok: $*"; }

run_reap() {
	RUNNER_REAP_PS_SOURCE="$FIXTURE" \
	RUNNER_REAP_KILL_CMD="$KILL_STUB" \
	RUNNER_REAP_KILL_LOG="$KILL_LOG" \
	RUNNER_REAP_SELF_PID=201 \
	RUNNER_REAP_MIN_AGE=30 \
		bash "$REAP" "$@"
}

# ── cell 1-4: real reap of exactly the stranded tree ────────────────────────
out=$(run_reap 2>&1); rc=$?
[ "$rc" -eq 0 ] || fail "reap exited $rc (want 0): $out"
killed=$(sort -k2,2n "$KILL_LOG")

for pid in 100 101 102; do
	if grep -q -- "-TERM $pid\$" "$KILL_LOG"; then
		pass "stranded pid $pid signalled"
	else
		fail "stranded pid $pid NOT signalled; killed=$(printf '%s ' $killed)"
	fi
done
for pid in 1 10 200 201 202; do
	if grep -q -- " $pid\$" "$KILL_LOG"; then
		fail "pid $pid must never be signalled; killed=$(printf '%s ' $killed)"
	fi
done
pass "self (201), own worker (200), listener (10) and init (1) untouched"
if grep -q -- "-TERM 202\$" "$KILL_LOG"; then
	fail "a worker younger than --min-age (202) was reaped"
else
	pass "young worker (202, etimes 20 < 30) left alone"
fi

# Leaf-first ordering: the child bunkerd (102) before its bash parent (101)
# before the worker (100).
order=$(grep -o -- '^\-TERM [0-9]*' "$KILL_LOG" | awk '{print $2}' | paste -sd' ')
[ "$order" = "102 101 100" ] || fail "kill order '$order', want '102 101 100' (leaf-first)"

# ── cell 5: dry run reports, kills nothing ──────────────────────────────────
: > "$KILL_LOG"
dry=$(run_reap --dry-run 2>&1); rc=$?
[ "$rc" -eq 0 ] || fail "dry run exited $rc (want 0): $dry"
if [ -s "$KILL_LOG" ]; then
	fail "dry run signalled processes: $(cat "$KILL_LOG")"
else
	pass "dry run killed nothing"
fi
case "$dry" in
*"stranded worker 100"*) pass "dry run names the stranded worker" ;;
*) fail "dry run did not name worker 100: $dry" ;;
esac
case "$dry" in
*would\ be\ reaped*) pass "dry run prints the would-reap line" ;;
*) fail "dry run missing summary line: $dry" ;;
esac

# ── cell 6: unreadable table fail-open / --strict fail-closed ───────────────
missing="$WORK/absent-ps.txt"
out=$(RUNNER_REAP_PS_SOURCE="$missing" RUNNER_REAP_SELF_PID=201 bash "$REAP" 2>&1); rc=$?
[ "$rc" -eq 0 ] || fail "missing ps source should be fail-open (exit 0), got $rc: $out"
out=$(RUNNER_REAP_PS_SOURCE="$missing" RUNNER_REAP_SELF_PID=201 bash "$REAP" --strict 2>&1); rc=$?
[ "$rc" -eq 1 ] || fail "--strict should exit 1 on a fatal error, got $rc: $out"
pass "missing process table: fail-open by default, exit 1 under --strict"

# ── cell 7: a clean table is a no-op ────────────────────────────────────────
clean="$WORK/ps-clean.txt"
cat > "$clean" <<'EOF'
1 0 999999 /sbin/init splash
10 1 500 /opt/actions-runner/bin/Runner.Listener run --startuptype service
200 1 40 /opt/actions-runner/bin/Runner.Worker spawnclient 300 400
201 200 39 bash scripts/runner-stranded-reap.sh
EOF
: > "$KILL_LOG"
out=$(RUNNER_REAP_PS_SOURCE="$clean" RUNNER_REAP_KILL_CMD="$KILL_STUB" RUNNER_REAP_KILL_LOG="$KILL_LOG" \
	RUNNER_REAP_SELF_PID=201 bash "$REAP" 2>&1); rc=$?
[ "$rc" -eq 0 ] || fail "clean table exited $rc: $out"
[ -s "$KILL_LOG" ] && fail "clean table signalled processes: $(cat "$KILL_LOG")" || pass "clean table is a no-op"
case "$out" in
*"no stranded Runner.Worker"*) pass "clean table reports no stranded worker" ;;
*) fail "clean table output unexpected: $out" ;;
esac

if [ "$FAILS" -eq 0 ]; then
	echo "PASS: runner-stranded-reap.sh (all cells)"
	exit 0
fi
echo "FAILED: $FAILS cell(s)"
exit 1
