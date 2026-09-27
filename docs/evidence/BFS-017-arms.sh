#!/usr/bin/env bash
# BFS-017-arms.sh — the cells that prove the gate's three verdicts.
#
# THE RULE (BFS-046's, kept): every claim below appears with the arm that turns
# it RED. A cell with no mutation is a cell with no evidence.
#
# The defect is a CLASSIFICATION defect, so every cell below runs the REAL
# `gitreins guard` on a FIXTURE repository (two tiny Go packages): an arm costs
# seconds, not the 195s bunker suite, and needs no busy machine. Three shapes of
# the guard's lie are pinned, all three measured on the installed gitreins
# 0.15.0 before this file was written:
#
#   S1 timeout      the go_tests lane is killed by its own wall budget
#                   (guards.test_timeout) -> "Tier 1 Guards: FAIL", exit 1,
#                   while the guard's own diagnostics say the opposite:
#                   "first_failing_test: none detected". A CLOCK reported as a
#                   code failure.  [guard_manager.check_go_tests]
#   S2 swallowed    the guard's OVERALL budget (guards.hook_timeout) is exceeded
#                   and the early return builds its result with passed=True,
#                   DISCARDING the collected lanes: a test that failed in this
#                   very run is reported as a PASS, exit 0, while the evidence
#                   document still carries `go_tests passed: false`.
#                   [guard_manager._timeout_result]
#   S3 dropped      the same early return, taken before the tests lane: the lane
#                   is simply ABSENT from the evidence, no skipped step is
#                   recorded, and the run is a PASS, exit 0 — the false green.
#
# usage: bash docs/evidence/BFS-017-arms.sh <red|green|control|contention|mutations|times|all>

set -u

mode="${1:-}"
case "$mode" in
red | green | control | contention | mutations | hook | once | times | all) ;;
*)
	printf 'usage: %s <red|green|control|contention|mutations|hook|once|times|all>\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
gate="$root/scripts/gitreins-guard.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/bfs017-arms-XXXXXX")

hog_pids=""
HOGS="${BFS017_HOGS:-32}"
HOG_SECONDS="${BFS017_HOG_SECONDS:-60}"

stop_hogs() {
	for p in $hog_pids; do kill -9 "$p" 2>/dev/null || true; done
	wait 2>/dev/null || true
	hog_pids=""
}

cleanup() {
	stop_hogs
	if [ -n "${probe:-}" ] && [ -e "$probe" ]; then
		git -C "$root" reset -q -- "$probe" 2>/dev/null || true
		rm -f "$probe"
	fi
	printf 'scratch kept: %s\n' "$work"
}
trap cleanup EXIT

say() { printf '%s\n' "$*"; }

# now_ms — a millisecond clock. `date +%s%3N` is NOT portable: GNU date has no
# sub-second width specifier and prints `1790528127661429292`, a 19-digit
# concatenation that silently turns every timing in this file into arithmetic
# on a nonsense number (measured on this host: `date +%s%3N` =
# 1790528127661429292). EPOCHREALTIME is bash's own microsecond clock; the
# `date +%s%N` fallback is truncated to milliseconds.
now_ms() {
	if [ -n "${EPOCHREALTIME:-}" ]; then
		local t="${EPOCHREALTIME/./}"
		printf '%s' "${t:0:13}"
		return 0
	fi
	local n
	n="$(date +%s%N 2>/dev/null || true)"
	case "$n" in
	'' | *[!0-9]*) printf '%s000' "$(date +%s)" ;;
	*) printf '%s' "${n:0:13}" ;;
	esac
}

# start_hogs N — N bounded CPU hogs, children of THIS shell (never detached),
# killed by PID in stop_hogs. No `pkill -f`: five workers in this fleet have
# hung on a self-matching pattern.
start_hogs() {
	n="$1"
	i=0
	while [ "$i" -lt "$n" ]; do
		(
			end=$((SECONDS + HOG_SECONDS))
			while [ "$SECONDS" -lt "$end" ]; do :; done
		) &
		hog_pids="$hog_pids $!"
		i=$((i + 1))
	done
	sleep 1
}

say "BFS-017 arms — mode=$mode"
say "host: $(nproc) cpu, loadavg $(cut -d' ' -f1-3 /proc/loadavg), $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
say "gate: $gate"
say ""

# ── the fixture repository ──────────────────────────────────────────────────
# A real git repo (committed, so golangci-lint's --new-from-rev lane is real)
# with a real .gitreins/config.yaml. The `slow` package burns a FIXED number of
# iterations (BFS017_WORK) rather than sleeping: a sleep is immune to CPU
# contention, so a contended run would never exceed a budget and the contention
# cell would prove nothing.
mkgate_fixture() { # <dir>
	# NOTE: `local` matters — this function used to assign the GLOBAL `fx`, so
	# building a second fixture silently repointed the caller's `fx` at it and
	# the M1 cell then graded the wrong tree (measured: M1 ran against the S3
	# fixture and "failed" for the wrong reason).
	local fx="$1"
	mkdir -p "$fx/slow" "$fx/fail" "$fx/failonly" "$fx/.gitreins"
	cat >"$fx/go.mod" <<'GO'
module bfs017fixture

go 1.21
GO
	cat >"$fx/slow/work_test.go" <<'GO'
package slow

import (
	"os"
	"strconv"
	"testing"
)

// TestWork burns BFS017_WORK dependent arithmetic steps. The wall time is a
// function of the work AND of the CPU the process is given — the controllable
// quantity the BFS-017 contention cells measure.
func TestWork(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("BFS017_WORK"))
	if err != nil || n <= 0 {
		t.Fatalf("BFS017_WORK must be a positive integer, got %q", os.Getenv("BFS017_WORK"))
	}
	s := 0
	for i := 0; i < n; i++ {
		s = (s + i) % 1000003
	}
	if s < 0 {
		t.Fatalf("impossible accumulator: %d", s)
	}
}
GO
	cat >"$fx/fail/genuine_test.go" <<'GO'
package fail

import "testing"

// TestGenuineFailure is a real defect. The BFS-017 control cells assert that it
// is still reported as a FAILURE — quietly, under contention, and in the same
// run as a budget hit.
func TestGenuineFailure(t *testing.T) {
	t.Fatalf("BFS-017 control: this failure is real and must be reported as one")
}
GO
	cat >"$fx/failonly/genuine_test.go" <<'GO'
package failonly

import "testing"

// A second, independent failing package (used by the arms that need the tests
// lane to FAIL without any slow package in the same module).
func TestOnlyFailure(t *testing.T) {
	t.Fatalf("BFS-017: this failure is real")
}
GO
	git -C "$fx" init -q 2>/dev/null
	git -C "$fx" add -A 2>/dev/null
	git -C "$fx" -c user.email=bfs017@example.invalid -c user.name=bfs017 commit -qm "fixture base" 2>/dev/null
}

fixture_config() { # <fixture> <test_timeout> <hook_timeout> [build] [lint]
	mkdir -p "$1/.gitreins"
	cat >"$1/.gitreins/config.yaml" <<YML
# BFS-017 fixture: the shapes under test, nothing else.
guards:
  secrets: true
  go:
    build: ${4:-true}
    lint: ${5:-true}
    tests: true
  test_mode: full
  test_timeout: $2
  hook_timeout: $3
YML
}

stage_probe() { # <fixture> — a staged .go edit, so the Go lanes actually run
	printf '\n// staged probe (BFS-017 arms)\n' >>"$1/slow/work_test.go"
	git -C "$1" add -A 2>/dev/null
}

# mkbig_package <fixture> — one package with a very large function body, so
# `go build ./...` takes seconds and the pre-tests lanes cross a small overall
# budget DETERMINISTICALLY. That is what makes the S3 cell (a lane dropped by
# the overall budget) a cell rather than a coin toss on how fast the host is.
# NOTE: the first version emitted ONE chained expression and made `go vet` fail
# with "exceeded max nesting depth" — a broken fixture that would have made the
# arm prove nothing. Statements, not nesting.
mkbig_package() {
	mkdir -p "$1/big"
	{
		printf 'package big\n\n// Big adds 120000 numbers one statement at a time.\nfunc Big() int {\n	s := 0\n'
		i=0
		while [ "$i" -lt 120000 ]; do
			printf '	s += %d\n' "$i"
			i=$((i + 1))
		done
		printf '	return s\n}\n'
	} >"$1/big/big.go"
	git -C "$1" add -A 2>/dev/null
}

verdict_of() { sed -n 's/^GUARD VERDICT: \([A-Z-]*\).*/\1/p' "$1" | head -1; }

pass=0
fail_count=0
declare_ok() {
	say "OK   $*"
	pass=$((pass + 1))
}
declare_bad() {
	say "FAIL $*"
	fail_count=$((fail_count + 1))
}

# guard_doc <fixture> <out.json> <env...> — run the bare guard, capture the
# evidence document, and print the shape it produced.
guard_doc() {
	fx="$1"
	out="$2"
	shift 2
	(cd "$fx" && env "$@" gitreins guard --json) >"$out" 2>"$out.err"
	guard_rc=$?
	doc_lanes="$(jq -r '[.checks[]?.id] | join(",")' "$out" 2>/dev/null)"
	doc_tests_passed="$(jq -r '.checks[]? | select(.id == "go_tests") | (.passed | tostring)' "$out" 2>/dev/null)"
	doc_tests_summary="$(jq -r '.checks[]? | select(.id == "go_tests") | ((.summary // "") | gsub("\n"; " "))' "$out" 2>/dev/null)"
	say "    guard exit=$guard_rc lanes=[$doc_lanes] go_tests.passed=$doc_tests_passed"
	[ -n "$doc_tests_summary" ] && say "    go_tests summary: $(printf '%s' "$doc_tests_summary" | cut -c1-160)"
	return 0
}

# ── mode: red ───────────────────────────────────────────────────────────────
do_red() {
	say "=================================================================="
	say "RED — the three shapes of the lie, with the REAL guard (bounded)"
	say "=================================================================="

	# --- S1: a wall budget exhaustion reported as a code failure ------------
	fx="$work/s1"
	mkgate_fixture "$fx"
	fixture_config "$fx" 3 1800
	stage_probe "$fx"
	git -C "$fx" add -A
	say ""
	say "-- S1 timeout: test_timeout: 3, the tests lane burns ~60s of work"
	guard_doc "$fx" "$work/s1.json" BFS017_WORK=60000000000
	log_new="$(ls -t "$fx"/.gitreins/logs/guard-*.log 2>/dev/null | head -1)"
	human_s1="$(cd "$fx" && BFS017_WORK=60000000000 gitreins guard 2>&1 | grep -m1 'Tier 1 Guards\|DEGRADED')"
	say "    guard header: $human_s1"
	say "    diagnostics line: $(grep -m1 'first_failing_test' "$log_new" 2>/dev/null)  [NOTE: engine/types.py:parse_first_failing_test is pytest-shaped — it cannot see Go's '--- FAIL:' lines, so 'none detected' is a blind spot, not proof. The proof is the next line.]"
	defect_lines="$(jq -r '[.checks[]? | select((.summary // "") | test("--- FAIL:"))] | length' "$work/s1.json" 2>/dev/null)"
	say "    evidence checks containing a '--- FAIL:' line: $defect_lines"
	if [ "$guard_rc" = "1" ] && printf '%s' "$doc_tests_summary" | grep -q 'Tests timed out after'; then
		declare_ok "S1 reproduced: exit 1 (FAIL) on a budget exhaustion, with no test verdict"
	else
		declare_bad "S1 did not reproduce (exit $guard_rc)"
	fi
	if [ "$defect_lines" = "0" ]; then
		declare_ok "S1 attribution: NO gate in the evidence carries a failing test — the FAIL has no test behind it"
	else
		declare_bad "S1 attribution: the evidence carries $defect_lines failing-test line(s)"
	fi

	# --- S2: the overall budget swallows a REAL failure ---------------------
	# build/lint disabled so the pre-tests lanes cannot themselves cross the
	# overall budget: the tests lane's own runtime is what crosses it.
	fx="$work/s2"
	mkgate_fixture "$fx"
	fixture_config "$fx" 20 2 false false
	stage_probe "$fx"
	say ""
	say "-- S2 swallowed: hook_timeout: 2, the tests lane runs ~2.5s and a package GENUINELY fails"
	guard_doc "$fx" "$work/s2.json" BFS017_WORK=3000000000
	human_s2="$(cd "$fx" && BFS017_WORK=3000000000 gitreins guard 2>&1 | grep -m1 'Tier 1 Guards\|DEGRADED')"
	say "    guard header: $human_s2"
	if [ "$guard_rc" = "0" ] && printf '%s' "$human_s2" | grep -q 'Tier 1 Guards: PASS'; then
		if [ "$doc_tests_passed" = "false" ]; then
			declare_ok "S2 reproduced: exit 0 / PASS while the evidence says go_tests FAILED (the failure was discarded)"
		elif [ -z "$doc_tests_passed" ] || [ "$doc_tests_passed" = "null" ]; then
			declare_ok "S2 reproduced as the S3 shape (exit 0 / PASS with the tests lane absent: $(jq -r '[.checks[]?.id]|join(\",\")' "$work/s2.json"))"
		else
			declare_bad "S2: exit 0 but go_tests.passed=$doc_tests_passed (expected the failure to be visible)"
		fi
	else
		declare_bad "S2 did not reproduce (exit $guard_rc, header '$human_s2')"
	fi

	# --- S3: the overall budget drops the tests lane entirely ---------------
	# OPPORTUNISTIC BY DESIGN, and said so. Whether the early return lands
	# before the tests lane depends on how long the pre-tests lanes take, which
	# is not a quantity this repository controls (the big-package trick was
	# tried and rejected: a warm `go build` of a 120k-statement package is
	# 0.02s, so the crossing is not deterministic). Three runs are attempted;
	# if the shape occurs, it is asserted; if it does not, the arm RECORDS that
	# and the shape is pinned deterministically at the DOCUMENT level instead
	# (docs/evidence/BFS-017-probes/classifier-cells.sh: cells `laneabsent`
	# and `empty-scope-skip`). The mechanism itself is proven by S2 above.
	fx="$work/s3"
	mkgate_fixture "$fx"
	mkbig_package "$fx"
	fixture_config "$fx" 20 1
	stage_probe "$fx"
	say ""
	say "-- S3 dropped: hook_timeout: 1, three attempts (opportunistic shape)"
	s3_seen=0
	attempt=1
	while [ "$attempt" -le 3 ]; do
		guard_doc "$fx" "$work/s3-$attempt.json" BFS017_WORK=1000000
		if [ "$guard_rc" = "0" ] && ! printf '%s' "$doc_lanes" | grep -q 'go_tests'; then
			s3_seen=1
			say "    attempt $attempt TRIGGERED: exit 0 / PASS with the tests lane ABSENT (lanes: $doc_lanes)"
			human_s3="$(cd "$fx" && BFS017_WORK=1000000 gitreins guard 2>&1)"
			say "    guard warning: $(printf '%s' "$human_s3" | grep -m1 'Remaining checks skipped')"
			say "    (the --json machine surface drops that warning: $(grep -c 'Remaining checks skipped' "$work/s3-$attempt.json.err" || true) mention(s) on stderr)"
			break
		fi
		say "    attempt $attempt did not trigger (lanes: $doc_lanes)"
		attempt=$((attempt + 1))
	done
	if [ "$s3_seen" = "1" ]; then
		declare_ok "S3 reproduced: exit 0 / PASS with the tests lane ABSENT — a gate that never ran reported as green"
	else
		say "NOTE S3: the dropped-lane shape did not trigger on this run; it is pinned by the document-level cell"
		say "          (classifier-cells.sh laneabsent) and by the direct measurement recorded in the evidence doc."
	fi
}

# ── mode: green ─────────────────────────────────────────────────────────────
do_green() {
	say "=================================================================="
	say "GREEN — the same trees, same configs, through the repo's gate"
	say "=================================================================="

	# --- G1: the S1 tree ---------------------------------------------------
	fx="$work/s1"
	[ -d "$fx" ] || {
		mkgate_fixture "$fx"
		fixture_config "$fx" 3 1800
		stage_probe "$fx"
	}
	git -C "$fx" add -A
	say ""
	say "-- G1: the identical tree S1 failed on"
	(cd "$fx" && BFS017_WORK=60000000000 bash "$gate") >"$work/g1.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/g1.out")"
	say "    wrapper exit=$rc verdict=$v"
	grep -m1 '^  budgets:' "$work/g1.out" | sed 's/^/    /'
	grep -m1 'exhausted its budget' "$work/g1.out" | cut -c1-160 | sed 's/^/    /'
	if [ "$rc" = "3" ] && [ "$v" = "NOT-FINISHED" ]; then
		declare_ok "G1: the same budget exhaustion is NOT-FINISHED (exit 3), never a code failure"
	else
		declare_bad "G1: got exit=$rc verdict=$v (want 3 / NOT-FINISHED)"
	fi

	# --- G2: the S2 tree — a swallowed failure must come back --------------
	fx2="$work/s2"
	[ -d "$fx2" ] || {
		mkgate_fixture "$fx2"
		fixture_config "$fx2" 20 2 false false
		stage_probe "$fx2"
	}
	git -C "$fx2" add -A
	say ""
	say "-- G2: the identical tree S2 passed on"
	guard_doc "$fx2" "$work/g2-guard.json" BFS017_WORK=3000000000
	(cd "$fx2" && BFS017_WORK=3000000000 bash "$gate") >"$work/g2.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/g2.out")"
	say "    wrapper exit=$rc verdict=$v"
	grep -m1 'first failing test' "$work/g2.out" | sed 's/^/    /'
	# The truth depends on which shape the guard produced: a lane that FAILED in
	# the evidence is a TEST-FAILURE; a lane that is absent is NOT-FINISHED.
	# Either way the wrapper must NOT pass — and it must name the right lie.
	if [ "$doc_tests_passed" = "false" ]; then
		if [ "$rc" = "1" ] && [ "$v" = "TEST-FAILURE" ]; then
			declare_ok "G2: the failure the guard swallowed is restored as TEST-FAILURE (exit 1)"
		else
			declare_bad "G2: got exit=$rc verdict=$v (want 1 / TEST-FAILURE — a failing lane is in the evidence)"
		fi
	else
		if [ "$rc" = "3" ] && [ "$v" = "NOT-FINISHED" ]; then
			declare_ok "G2: the dropped tests lane is NOT-FINISHED (exit 3), not a pass"
		else
			declare_bad "G2: got exit=$rc verdict=$v (want 3 / NOT-FINISHED — the tests lane never ran)"
		fi
	fi

	# --- G3: the S3 tree — whatever the guard's lie was, the gate must refuse
	fx3="$work/s3"
	[ -d "$fx3" ] || {
		mkgate_fixture "$fx3"
		mkbig_package "$fx3"
		fixture_config "$fx3" 20 1
		stage_probe "$fx3"
	}
	git -C "$fx3" add -A
	say ""
	say "-- G3: the identical tree S3 attempted"
	guard_doc "$fx3" "$work/g3-guard.json" BFS017_WORK=1000000
	(cd "$fx3" && BFS017_WORK=1000000 bash "$gate") >"$work/g3.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/g3.out")"
	say "    wrapper exit=$rc verdict=$v"
	grep -m1 'never ran\|reported a defect' "$work/g3.out" | cut -c1-140 | sed 's/^/    /'
	if printf '%s' "$doc_lanes" | grep -q 'go_tests'; then
		# the tests lane ran; with hook_timeout 1 the guard's own verdict may
		# still be a false green (passed=True discarding it) — either way the
		# wrapper must not pass, and must call a failing lane a failure.
		if [ "$rc" = "1" ] && [ "$v" = "TEST-FAILURE" ]; then
			declare_ok "G3: the guard's green over a ran-and-failed tests lane is TEST-FAILURE (exit 1) through the gate"
		elif [ "$rc" = "3" ] && [ "$v" = "NOT-FINISHED" ]; then
			declare_ok "G3: the gate refuses (NOT-FINISHED, exit 3) where the guard reported green"
		else
			declare_bad "G3: got exit=$rc verdict=$v (want a refusal: 1/TEST-FAILURE or 3/NOT-FINISHED)"
		fi
	else
		if [ "$rc" = "3" ] && [ "$v" = "NOT-FINISHED" ]; then
			declare_ok "G3: a gate that never ran is NOT-FINISHED (exit 3) — the false green is closed"
		else
			declare_bad "G3: got exit=$rc verdict=$v (want 3 / NOT-FINISHED — the tests lane never ran)"
		fi
	fi
}

# ── mode: control ───────────────────────────────────────────────────────────
do_control() {
	say "=================================================================="
	say "CONTROL — a genuine failure must still fail, loudly, in every shape"
	say "=================================================================="

	fx="$work/control"
	mkgate_fixture "$fx"
	fixture_config "$fx" 60 1800
	stage_probe "$fx"

	guard_out="$(cd "$fx" && BFS017_WORK=1000000 gitreins guard 2>&1)"
	say "-- C1a bare guard: $(printf '%s' "$guard_out" | grep -m1 'Tier 1 Guards\|DEGRADED')"
	printf '%s' "$guard_out" | grep -q 'Tier 1 Guards: FAIL' && declare_ok "C1a: the guard fails on the genuine failure" || declare_bad "C1a: the guard did not fail"

	(cd "$fx" && BFS017_WORK=1000000 bash "$gate") >"$work/c1b.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/c1b.out")"
	say "-- C1b wrapper: exit=$rc verdict=$v"
	grep -m1 'first failing test' "$work/c1b.out" | sed 's/^/    /'
	if [ "$rc" = "1" ] && [ "$v" = "TEST-FAILURE" ]; then
		declare_ok "C1b: a genuine failure is TEST-FAILURE (exit 1) through the wrapper"
	else
		declare_bad "C1b: got exit=$rc verdict=$v (want 1 / TEST-FAILURE)"
	fi

	# C2: the same run ALSO hits a budget — the failure must still dominate.
	say ""
	say "-- C2 both signals in ONE run: a fast genuine failure + a per-binary budget hit"
	(cd "$fx" && BFS017_WORK=60000000000 bash "$gate" --run 'go test -timeout 3s ./...') >"$work/c2.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/c2.out")"
	say "    wrapper exit=$rc verdict=$v"
	grep -m1 'reported a defect' "$work/c2.out" | sed 's/^/    /'
	say "    (the raw command's exit was 1 for BOTH the timeout and the genuine failure — let us show that:)"
	(cd "$fx" && BFS017_WORK=1000000 go test -timeout 60s ./... >/dev/null 2>&1)
	say "      quiet genuine failure  -> exit $?"
	(cd "$fx" && BFS017_WORK=60000000000 go test -timeout 3s ./failonly >/dev/null 2>&1)
	say "      budget exhaustion only -> exit $?"
	if [ "$rc" = "1" ] && [ "$v" = "TEST-FAILURE" ]; then
		declare_ok "C2: with a budget hit in the same run, the real failure is STILL TEST-FAILURE (exit 1)"
	else
		declare_bad "C2: got exit=$rc verdict=$v (want 1 / TEST-FAILURE) — the fix must not blunt the guard"
	fi
}

# ── mode: contention ────────────────────────────────────────────────────────
do_contention() {
	say "=================================================================="
	say "CONTENTION — the same tree, no edit: PASS quiet, did-not-finish busy"
	say "=================================================================="
	fx="$work/contention"
	mkgate_fixture "$fx"
	fixture_config "$fx" 60 1800

	# Calibrate: measure the fixture's WORK in wall ms on THIS host, then set the
	# per-run budget from it. Two corrections learned the hard way (the first
	# version of this arm reported "quiet PASS / contended PASS" and proved
	# nothing):
	#   * WARM UP FIRST. The first `go test` run compiles the test binary, so its
	#     wall is mostly compile time; on a loaded host that made 1e6 iterations
	#     look slower than 3.9e6 (measured: 310 ms then 237 ms).
	#   * SUBTRACT the fixed overhead. The budget scales the WORK (1.5x), not the
	#     compile+startup the arm also pays — otherwise a small work amount gets
	#     a budget rounded up to 1s and a 2x contention swing cannot cross it.
	measure_wall_ms() { # <fixture> <BFS017_WORK> <cmd...>
		local fx_m="$1" work_m="$2"
		shift 2
		local t0 t1
		t0=$(now_ms)
		(cd "$fx_m" && BFS017_WORK="$work_m" "$@" >/dev/null 2>&1)
		t1=$(now_ms)
		printf '%s' "$((t1 - t0))"
	}

	unit=1000000
	(cd "$fx" && BFS017_WORK=1000 go test -count=1 -run TestWork ./slow >/dev/null 2>&1) # warm the binary
	base_ms=$(measure_wall_ms "$fx" 1000 go test -count=1 -run TestWork ./slow)
	unit_ms=$(measure_wall_ms "$fx" "$unit" go test -count=1 -run TestWork ./slow)
	unit_work=$((unit_ms - base_ms))
	[ "$unit_work" -lt 1 ] && unit_work=1
	target_ms="${BFS017_TARGET_MS:-1200}"
	iterations=$((unit * target_ms / unit_work))
	say "calibration: overhead ${base_ms} ms, $unit iterations = ${unit_ms} ms (work ${unit_work} ms)"
	say "             target ${target_ms} ms of work -> $iterations iterations"

	quiet_ms=$(measure_wall_ms "$fx" "$iterations" go test -count=1 -run TestWork ./slow)
	budget_ms=$(( (quiet_ms - base_ms) * 3 / 2 + base_ms ))
	[ "$budget_ms" -lt 500 ] && budget_ms=500
	say "quiet: BFS017_WORK=$iterations -> ${quiet_ms} ms wall; derived budget = ${budget_ms}ms (1.5x the WORK, plus the same fixed overhead)"

	say "-- arm A: quiet, through the gate"
	(cd "$fx" && BFS017_WORK=$iterations bash "$gate" --run "go test -count=1 -run TestWork -timeout ${budget_ms}ms ./slow") >"$work/cont-a.out" 2>&1
	rc_a=$?
	v_a="$(verdict_of "$work/cont-a.out")"
	say "    exit=$rc_a verdict=$v_a (loadavg $(cut -d' ' -f1-3 /proc/loadavg))"

	say "-- arm B: the SAME tree, no edit, under $HOGS bounded CPU hogs"
	start_hogs "$HOGS"
	t0=$(now_ms)
	(cd "$fx" && BFS017_WORK=$iterations bash "$gate" --run "go test -count=1 -run TestWork -timeout ${budget_ms}ms ./slow") >"$work/cont-b.out" 2>&1
	rc_b=$?
	t1=$(now_ms)
	stop_hogs
	cont_ms=$((t1 - t0))
	v_b="$(verdict_of "$work/cont-b.out")"
	say "    exit=$rc_b verdict=$v_b (loadavg $(cut -d' ' -f1-3 /proc/loadavg))"
	say "    wall: quiet ${quiet_ms} ms -> contended ${cont_ms} ms; budget ${budget_ms}ms"
	grep -m1 'panicked on its own per-binary ceiling' "$work/cont-b.out" | sed 's/^/    /'

	if [ "$rc_a" = "0" ] && [ "$v_a" = "PASS" ] && [ "$rc_b" = "3" ] && [ "$v_b" = "NOT-FINISHED" ]; then
		declare_ok "CONTENTION: same tree, no edit — PASS quiet, NOT-FINISHED under bounded contention"
		declare_ok "the contention is DELIBERATE and bounded ($HOGS hogs, capped ${HOG_SECONDS}s, killed by PID)"
	else
		declare_bad "CONTENTION: got quiet=$rc_a/$v_a contended=$rc_b/$v_b (want 0/PASS then 3/NOT-FINISHED)"
	fi

	say "-- arm C: the same contention, same command, with a genuine failure in the run"
	start_hogs "$HOGS"
	(cd "$fx" && BFS017_WORK=$iterations bash "$gate" --run "go test -count=1 -timeout ${budget_ms}ms ./...") >"$work/cont-c.out" 2>&1
	rc_c=$?
	stop_hogs
	v_c="$(verdict_of "$work/cont-c.out")"
	say "    exit=$rc_c verdict=$v_c"
	if [ "$rc_c" = "1" ] && [ "$v_c" = "TEST-FAILURE" ]; then
		declare_ok "CONTROL under the SAME contention: a genuine failure is still TEST-FAILURE (exit 1)"
	else
		declare_bad "CONTROL under contention: got exit=$rc_c verdict=$v_c (want 1 / TEST-FAILURE)"
	fi
}

# ── mode: mutations ─────────────────────────────────────────────────────────
do_mutations() {
	say "=================================================================="
	say "MUTATIONS — neuter the fix, the cell must go RED (sha256-verified restore)"
	say "=================================================================="
	bak="$(mktemp)"
	cp "$gate" "$bak"
	hash_before="$(sha256sum "$gate" | cut -d' ' -f1)"
	say "pre-mutation: scripts/gitreins-guard.sh sha256=$hash_before"

	restore_gate() {
		cp "$bak" "$gate"
		now="$(sha256sum "$gate" | cut -d' ' -f1)"
		if [ "$now" != "$hash_before" ]; then
			say "RESTORE FAILED: $now != $hash_before"
			exit 3
		fi
		say "restore: scripts/gitreins-guard.sh sha256=$now (verified byte-identical)"
	}

	fx="$work/s1"
	[ -d "$fx" ] || {
		mkgate_fixture "$fx"
		fixture_config "$fx" 3 1800
		stage_probe "$fx"
	}
	fx3="$work/s3"
	[ -d "$fx3" ] || {
		mkgate_fixture "$fx3"
		mkbig_package "$fx3"
		fixture_config "$fx3" 20 1 true false
		stage_probe "$fx3"
	}
	git -C "$fx" add -A
	git -C "$fx3" add -A

	# M1: remove the budget-signature table -> a wall budget hit is reported as
	# a code failure: exactly the defect this row exists to remove.
	say ""
	say "-- M1: neuter the budget-signature table (the timeout branch)"
	perl -0pi -e 's/is_budget_signature\(\) \{\n.*?\n\}/is_budget_signature() {\n\treturn 1\n}/s' "$gate"
	say "   mutated sha256=$(sha256sum "$gate" | cut -d' ' -f1)"
	(cd "$fx" && BFS017_WORK=60000000000 bash "$gate") >"$work/m1.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/m1.out")"
	restore_gate
	say "   with the mutation: exit=$rc verdict=$v"
	if [ "$rc" = "1" ] && [ "$v" = "TEST-FAILURE" ]; then
		declare_ok "M1 RED: without the budget branch the gate reports a clock as a code failure (the BFS-017 defect)"
	else
		declare_bad "M1: the mutation did not change the verdict (got $rc/$v)"
	fi

	# M2: neuter the completeness check -> a gate that never ran becomes a PASS.
	# This one is pinned on a DOCUMENT, not on a live run: whether the guard
	# drops a lane depends on how fast its pre-tests lanes are, which is not a
	# quantity this repo controls (see the S3 arm). The document is the exact
	# shape `gitreins guard --json` emits when it does drop one (captured from
	# the real guard, see the evidence doc §3.3).
	say ""
	say "-- M2: neuter the completeness check (a gate absent from the evidence)"
	cat >"$work/laneabsent.json" <<'JSON'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: ok"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 2}
}
JSON
	(cd "$root" && bash "$gate" --classify "$work/laneabsent.json") >"$work/m2-clean.out" 2>&1
	rc_clean=$?
	v_clean="$(verdict_of "$work/m2-clean.out")"
	perl -0pi -e 's/for e in \$expected_lanes; do/for e in ; do/' "$gate"
	say "   mutated sha256=$(sha256sum "$gate" | cut -d' ' -f1)"
	(cd "$root" && bash "$gate" --classify "$work/laneabsent.json") >"$work/m2.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/m2.out")"
	restore_gate
	say "   clean: exit=$rc_clean verdict=$v_clean  |  mutated: exit=$rc verdict=$v"
	if [ "$rc_clean" = "3" ] && [ "$v_clean" = "NOT-FINISHED" ] && [ "$rc" = "0" ] && [ "$v" = "PASS" ]; then
		declare_ok "M2 RED: without the completeness check the wrapper calls a never-ran gate a PASS (exit 0)"
	else
		declare_bad "M2: the mutation did not change the verdict (clean=$rc_clean/$v_clean mutated=$rc/$v)"
	fi

	# M3: invert the precedence -> a real failure is swallowed by the budget.
	# Also pinned on a document: the `both` shape (a genuine `--- FAIL:` and a
	# per-binary budget hit in the SAME summary) is what the fixture produces
	# under contention, and the classifier cells assert it too.
	say ""
	say "-- M3: invert the precedence (budget checked before the defect)"
	cat >"$work/both.json" <<'JSON'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "fail",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: ok"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "go vet: clean"},
    {"id": "go_tests", "outcome": "fail", "passed": false, "summary": "--- FAIL: TestGenuineFailure (0.00s)     genuine_test.go:9: BFS-017 control: this failure is real panic: test timed out after 3s FAIL bfs017fixture/fail 3.0s"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
JSON
	(cd "$root" && bash "$gate" --classify "$work/both.json") >"$work/m3-clean.out" 2>&1
	rc_clean=$?
	v_clean="$(verdict_of "$work/m3-clean.out")"
	perl -0pi -e 's/if is_defect_signature "\$summary" \|\| \[ -z "\$budget_hit" \]; then/if [ -z "\$budget_hit" ]; then/' "$gate"
	say "   mutated sha256=$(sha256sum "$gate" | cut -d' ' -f1)"
	(cd "$root" && bash "$gate" --classify "$work/both.json") >"$work/m3.out" 2>&1
	rc=$?
	v="$(verdict_of "$work/m3.out")"
	restore_gate
	say "   clean: exit=$rc_clean verdict=$v_clean  |  mutated: exit=$rc verdict=$v"
	if [ "$rc_clean" = "1" ] && [ "$v_clean" = "TEST-FAILURE" ] && [ "$rc" = "3" ] && [ "$v" = "NOT-FINISHED" ]; then
		declare_ok "M3 RED: inverted precedence buries a genuine failure behind the budget"
	else
		declare_bad "M3: the mutation did not change the verdict (clean=$rc_clean/$v_clean mutated=$rc/$v)"
	fi

	# M4: neuter the empty-scope exemption -> a gate that graded nothing is
	# treated as a gate that never ran.
	# WHICH CELL THIS IS, precisely: with the installed gitreins 0.15.0 a
	# docs-only run reports its Go lanes as `outcome: pass` with the message
	# "No Go files staged", so the live docs-only run is a PASS with or without
	# the exemption — the exemption's only observable effect there is the NOTE.
	# The version that reports an empty scope as a SKIP (`outcome: unknown` +
	# that skip reason) is where the exemption changes the VERDICT, and that
	# document shape is what this cell drives (the `empty-scope-skip` cell in
	# classifier-cells.sh asserts the same shape).
	say ""
	say "-- M4: remove the empty-scope exemption (a gate that graded nothing)"
	cat >"$work/empty-scope-skip.json" <<'JSON'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "unknown", "passed": null, "summary": "skipped — No Go files staged"},
    {"id": "go_lint", "outcome": "unknown", "passed": null, "summary": "skipped — No Go files staged"},
    {"id": "go_tests", "outcome": "unknown", "passed": null, "summary": "skipped — No Go files staged"}
  ],
  "metadata": {"degraded": true, "skippedSteps": ["go_build", "go_lint", "go_tests"], "checkCount": 4}
}
JSON
	(cd "$root" && bash "$gate" --classify "$work/empty-scope-skip.json") >"$work/m4-clean.out" 2>&1
	rc_clean=$?
	v_clean="$(verdict_of "$work/m4-clean.out")"
	perl -0pi -e 's/is_empty_scope_summary\(\) \{\n.*?\n\}/is_empty_scope_summary() {\n	return 1\n}/s' "$gate"
	say "   mutated sha256=$(sha256sum "$gate" | cut -d' ' -f1)"
	(cd "$root" && bash "$gate" --classify "$work/empty-scope-skip.json") >"$work/m4-mutated.out" 2>&1
	rc_mut=$?
	v_mut="$(verdict_of "$work/m4-mutated.out")"
	restore_gate
	docs_only="$work/docs-only"
	mkgate_fixture "$docs_only"
	fixture_config "$docs_only" 20 1800
	printf 'docs\n' >"$docs_only/NOTES.md"
	git -C "$docs_only" add NOTES.md
	(cd "$docs_only" && bash "$gate") >"$work/m4-live.out" 2>&1
	rc_live=$?
	v_live="$(verdict_of "$work/m4-live.out")"
	say "   document: clean=$rc_clean/$v_clean  mutated=$rc_mut/$v_mut"
	say "   live docs-only run (installed 0.15.0, lanes report 'No Go files staged'): exit=$rc_live verdict=$v_live"
	if [ "$rc_clean" = "0" ] && [ "$v_clean" = "PASS" ] && [ "$rc_mut" = "3" ] && [ "$v_mut" = "NOT-FINISHED" ]; then
		declare_ok "M4 RED: without the empty-scope exemption a gate that graded nothing is refused (NOT-FINISHED)"
	else
		declare_bad "M4: mutated=$rc_mut/$v_mut clean=$rc_clean/$v_clean (want 3/NOT-FINISHED then 0/PASS)"
	fi

	cp "$bak" "$gate"
	say "final: scripts/gitreins-guard.sh sha256=$(sha256sum "$gate" | cut -d' ' -f1)"
}

# ── mode: hook ──────────────────────────────────────────────────────────────
do_hook() {
	say "=================================================================="
	say "HOOK — the local commit path, in a SCRATCH CLONE (main tree untouched)"
	say "=================================================================="
	clone="$work/clone"
	branch="$(git -C "$root" rev-parse --abbrev-ref HEAD)"
	git clone --shared --quiet --branch "$branch" "$root" "$clone" 2>&1 | tail -2
	[ -d "$clone/.git" ] || {
		declare_bad "HOOK: could not clone $root ($branch)"
		return
	}
	say "clone: $clone (branch $branch, shared objects — nothing is written back)"
	(cd "$clone" && bash scripts/install-git-hooks.sh) | sed 's/^/    /'
	say "    hook file: $(git -C "$clone" rev-parse --git-path hooks)/pre-commit"

	# Starve the tests budget in the CLONE (an uncommitted config edit, so the
	# guard reads it): the full bunker suite cannot finish in 8s on any host.
	perl -0pi -e 's/^  test_timeout: \d+/  test_timeout: 8/m' "$clone/.gitreins/config.yaml"
	say "    clone budget: $(grep -m1 'test_timeout' "$clone/.gitreins/config.yaml")"
	hook_probe_file="$(ls "$clone"/internal/registry/*.go 2>/dev/null | grep -v '_test\.go$' | head -1)"
	if [ -n "$hook_probe_file" ]; then
		printf '\n// bfs017 hook probe\n' >>"$hook_probe_file"
	else
		declare_bad "HOOK: no non-test .go file to probe in $clone/internal/registry"
		return
	fi
	git -C "$clone" add -A
	say "    staged: $(git -C "$clone" diff --cached --name-only | head -3 | tr '\n' ' ')"

	t0=$(now_ms)
	commit_out="$(cd "$clone" && git -c user.email=bfs017@example.invalid -c user.name=bfs017 commit -m "BFS-017 hook probe" 2>&1)"
	rc=$?
	t1=$(now_ms)
	say "    commit exit: $rc (wall $((t1 - t0)) ms)"
	say "    verdict in the hook output: $(printf '%s' "$commit_out" | sed -n 's/^GUARD VERDICT: \([A-Z-]*\).*/\1/p' | head -1)"
	printf '%s' "$commit_out" | grep -m1 'budgets:' | sed 's/^/    /'
	printf '%s' "$commit_out" | grep -m1 'exhausted its budget' | cut -c1-150 | sed 's/^/    /'
	if [ "$rc" != "0" ] && printf '%s' "$commit_out" | grep -q 'GUARD VERDICT: NOT-FINISHED'; then
		declare_ok "HOOK: the commit was REFUSED with a NOT-FINISHED verdict (not a code failure) — the guard still blocks"
	else
		declare_bad "HOOK: commit exit=$rc and no NOT-FINISHED verdict in the hook output"
		printf '%s\n' "$commit_out" | head -20 | sed 's/^/    | /'
	fi
	if printf '%s' "$commit_out" | grep -q 'Tier 1 Guards: FAIL'; then
		declare_ok "HOOK: the bare guard's FAIL is still visible in the hook output (the wrapper preserves it)"
	fi
	say "    main tree status (must be unchanged by this arm): $(git -C "$root" status --short | wc -l | tr -d ' ') path(s)"
}

# ── mode: once ──────────────────────────────────────────────────────────────
# "Must not add a retry that hides intermittent failures." The gate makes exactly
# ONE guard invocation per run. That is proved, not asserted: a stub `gitreins`
# is put FIRST on PATH, it appends its argv to a counter file and execs the real
# binary, so the count is of real invocations. The third cell is the control that
# makes the count non-vacuous: a shim that DOES retry must be caught.
do_once() {
	say "=================================================================="
	say "ONCE — one run of the gate makes exactly one guard invocation"
	say "=================================================================="

	stub="$work/bin"
	mkdir -p "$stub"
	real="$(command -v gitreins)"
	cite="$work/invocations.txt"
	# the fixture's `fail`/`failonly` packages are genuine failures; `_`-prefixed
	# dirs are ignored by the go tool, which is how the green cell gets a fixture
	# whose suite really passes (never by editing a test to pass).
	greenify_fixture() {
		mv "$1/fail" "$1/_fail" 2>/dev/null || true
		mv "$1/failonly" "$1/_failonly" 2>/dev/null || true
	}
	for who in green exhausted; do
		fx="$work/once-$who"
		mkgate_fixture "$fx"
		if [ "$who" = exhausted ]; then
			fixture_config "$fx" 3 1800 # per-binary ceiling 3s, overall 1800s
		else
			fixture_config "$fx" 60 1800
			greenify_fixture "$fx"
		fi
		stage_probe "$fx"
		git -C "$fx" add -A
		cat >"$stub/gitreins" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$cite"
exec "$real" "\$@"
STUB
		chmod +x "$stub/gitreins"
		: >"$cite"
		if [ "$who" = exhausted ]; then
			# the fixture's own 3s ceiling is the budget: no host load required
			(cd "$fx" && PATH="$stub:$PATH" BFS017_WORK=60000000000 bash "$gate") >"$work/once-$who.out" 2>&1
		else
			(cd "$fx" && PATH="$stub:$PATH" BFS017_WORK=1000000 bash "$gate") >"$work/once-$who.out" 2>&1
		fi
		rc=$?
		n="$(wc -l <"$cite" | tr -d ' ')"
		v="$(verdict_of "$work/once-$who.out")"
		say ""
		say "-- $who run: exit=$rc verdict=$v guard invocations=$n"
		say "   one invocation per run is the whole rule: a NOT-FINISHED verdict must"
		say "   send the operator to a quieter box, not to an automatic second attempt."
		want=PASS
		[ "$who" = exhausted ] && want=NOT-FINISHED
		if [ "$n" = "1" ] && [ "$v" = "$want" ]; then
			declare_ok "ONCE/$who: exactly ONE guard invocation, verdict=$v (exit=$rc)"
		else
			declare_bad "ONCE/$who: expected 1 invocation and verdict $want; measured $n invocation(s) and $v"
		fi
	done

	# the control: a shim that retries until green. The cell must see it.
	retry_dir="$work/retrybin"
	mkdir -p "$retry_dir"
	cat >"$retry_dir/gitreins" <<SHIM
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$cite"
"$real" "\$@" >"$work/shim-first.out" 2>&1
rc=\$?
if [ "\$rc" != "0" ]; then
	printf '%s\n' "\$* (retry)" >>"$cite"
	"$real" "\$@" >"$work/shim-second.out" 2>&1
	rc=\$?
fi
exit "\$rc"
SHIM
	chmod +x "$retry_dir/gitreins"
	: >"$cite"
	fx="$work/once-exhausted"
	(cd "$fx" && PATH="$retry_dir:$PATH" BFS017_WORK=60000000000 bash "$gate") >"$work/once-retryshim.out" 2>&1
	n="$(wc -l <"$cite" | tr -d ' ')"
	say ""
	say "-- control: the same cell against a shim that retries a failed run once"
	say "   (this is the retry-hides-a-failure shape; the counter must see 2, and"
	say "    the cell above must be able to fail — otherwise the count is vacuous)"
	if [ "$n" -ge 2 ]; then
		declare_ok "ONCE/control: the counter sees the retry ($n invocations) — the count is not vacuous"
	else
		declare_bad "ONCE/control: a retrying shim produced only $n invocation(s); the counter cannot detect a retry"
	fi
}

# ── mode: times ─────────────────────────────────────────────────────────────
do_times() {
	say "=================================================================="
	say "TIMES — the green path before and after (wall clock, ms)"
	say "=================================================================="
	fx="$work/times"
	mkgate_fixture "$fx"
	fixture_config "$fx" 60 1800
	stage_probe "$fx"
	git -C "$fx" add -A
	(cd "$fx" && BFS017_WORK=1000000 go test ./... >/dev/null 2>&1) # warm cache

	# the fixture's suite is ~2s, so the wrapper's own work is a visible fraction
	# of the run instead of being lost in a 60s one; BFS017_TIME_RUNS=5 tightens
	# the estimate.
	runs="${BFS017_TIME_RUNS:-3}"

	t0=$(now_ms)
	for i in $(seq 1 "$runs"); do (cd "$fx" && BFS017_WORK=1000000 gitreins guard --json) >/dev/null 2>&1; done
	t1=$(now_ms)
	before=$((t1 - t0))

	t0=$(now_ms)
	for i in $(seq 1 "$runs"); do (cd "$fx" && BFS017_WORK=1000000 bash "$gate") >/dev/null 2>&1; done
	t1=$(now_ms)
	after=$((t1 - t0))

	say "-- fixture repo, $runs GREEN runs each way (the fixture's suite is ~2s):"
	say "   bare    gitreins guard --json     : ${before} ms ($((before / runs)) ms/run)"
	say "   wrapper scripts/gitreins-guard.sh : ${after} ms ($((after / runs)) ms/run)"
	say "   overhead: $((after - before)) ms over $runs runs ($(((after - before) / runs)) ms/run)"

	say ""
	say "-- THIS repo (bunker): interleaved A/B/A/B, same staged change"
	say "   (the box is shared: an A-vs-B pair run minutes apart measures the HOST as much"
	say "    as the gate, so the order is interleaved and the spread is reported)"
	probe="$root/internal/registry/bfs017_timing_probe.go"
	printf '%s\n' 'package registry' '' '// bfs017 timing probe (removed by the arms script; never committed).' >"$probe"
	git -C "$root" add "$probe"
	say "   staged: $(git -C "$root" diff --cached --name-only | tr '\n' ' ')"
	bare_walls=""
	wrap_walls=""
	round=1
	while [ "$round" -le 2 ]; do
		t0=$(now_ms)
		(cd "$root" && gitreins guard --json) >"$work/times-bare-$round.json" 2>&1
		rcb=$?
		t1=$(now_ms)
		bare=$((t1 - t0))
		t0=$(now_ms)
		(cd "$root" && bash "$gate") >"$work/times-wrapper-$round.out" 2>&1
		rcw=$?
		t1=$(now_ms)
		wrapped=$((t1 - t0))
		say "   round $round: bare=${bare} ms (exit $rcb)  wrapper=${wrapped} ms (exit $rcw, $(verdict_of "$work/times-wrapper-$round.out"))"
		bare_walls="$bare_walls $bare"
		wrap_walls="$wrap_walls $wrapped"
		round=$((round + 1))
	done
	git -C "$root" reset -q -- "$probe"
	rm -f "$probe"
	probe=""
	say "   bare walls   :$bare_walls"
	say "   wrapper walls:$wrap_walls"
	# The wrapper's OWN cost, isolated: the classifier alone, on a real evidence
	# document from exactly this run. Everything else the gate does is one
	# process spawn plus a few hundred bytes of shell.
	c0=$(now_ms)
	for i in 1 2 3 4 5; do (cd "$root" && bash "$gate" --classify "$work/times-bare-1.json") >/dev/null 2>&1; done
	c1=$(now_ms)
	say "   classifier alone, 5 runs on the real document: $((c1 - c0)) ms ($(((c1 - c0) / 5)) ms per run)"
	say "   git status after cleanup: $(git -C "$root" status --short | tr '\n' ' ' | cut -c1-160)"
}

# ── dispatch ────────────────────────────────────────────────────────────────
case "$mode" in
red) do_red ;;
green) do_green ;;
control) do_control ;;
contention) do_contention ;;
mutations) do_mutations ;;
hook) do_hook ;;
once) do_once ;;
times) do_times ;;
all)
	do_red
	do_green
	do_control
	do_contention
	do_mutations
	do_hook
	do_once
	do_times
	;;
esac

say ""
say "cells passed: $pass  failed: $fail_count"
[ "$fail_count" -eq 0 ] || exit 1
exit 0
