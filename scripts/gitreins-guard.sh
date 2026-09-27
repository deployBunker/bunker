#!/usr/bin/env bash
# gitreins-guard.sh — the repo's Tier-1 gate, with three verdicts instead of two.
#
# WHY THIS EXISTS (BFS-017)
# -------------------------
# `gitreins guard` has exactly two verdicts: exit 0 (PASS) and exit 1 (FAIL).
# A run that ran out of time is not a test failure, but the guard has no word
# for it, so on a loaded box a budget exhaustion is reported — and acted on —
# as a code failure. Measured on this repository (2026-09-27, loadavg 75-76
# with 19 concurrent Go suites): `internal/registry` was reported FAIL and the
# immediate re-run of the SAME tree reported `ok 21.8s`, no edit in between.
#
# The three outcomes this wrapper distinguishes, from the guard's own
# `--json` evidence document (schemas/evidence-v1), never from a guess:
#
#   PASS           every gate the config enables RAN and passed.
#   TEST-FAILURE   a gate ran and reported a defect (a failing test, a build
#                  error, a lint finding) — the tree is wrong.
#   NOT-FINISHED   a gate did not complete: it was killed by a budget
#                  (go_tests "Tests timed out after Ns"), or it never ran at
#                  all (the guard's own `hook_timeout` early-return drops the
#                  remaining lanes and still reports a pass).
#
# NOT-FINISHED is a REFUSAL, not a failure: it exits 3 (non-zero — an
# unverified tree must never land) and says in words that no test verdict was
# reached. It is never a pass, and it is never dressed up as a test failure.
#
# NO SILENT RETRY. This wrapper runs the guard exactly once. It was tempting to
# retry a NOT-FINISHED run "to see if it was load"; that is how a real
# intermittent regression gets buried (this repo has already shipped a
# false-green QA result). A contended run is reported as a contended run; a
# second run is a human's decision, not the instrument's.
#
# A gate that never ran is not a passing gate. `gitreins guard --json` exits 0
# for a DEGRADED document ("a DEGRADED pass is a pass here" — cli.cmd_guard_run)
# and the `hook_timeout` early-return produces a document with the tests lane
# simply ABSENT, `passed: true`, `degraded: false`, exit 0. So this wrapper
# reads the DOCUMENT, not the exit code, and enforces completeness: if an
# expected lane is missing, the verdict is NOT-FINISHED even when the guard
# printed a green header.
#
# USAGE
#   scripts/gitreins-guard.sh [gitreins guard args...]  # guard mode (default)
#   scripts/gitreins-guard.sh --run '<command>'         # suite mode
#   scripts/gitreins-guard.sh --classify <evidence.json>
#   scripts/gitreins-guard.sh --help
#
# EXIT CODES
#   0  PASS
#   1  TEST-FAILURE          (a gate ran and failed)
#   3  NOT-FINISHED          (a budget was exhausted, or a gate never ran)
#   4  GUARD-ERROR           (the guard could not produce evidence / misuse)
#   2  is deliberately unused: `gitreins guard` itself uses it for a DEGRADED
#      pass, and a wrapper that re-used it could not be told apart from a
#      degraded guard run.
#
# BUDGETS (read from .gitreins/config.yaml, printed with every verdict)
#   guards.test_timeout  the wall budget of the go_tests lane
#   guards.hook_timeout  the guard's OVERALL budget; past it the remaining
#                        lanes are SKIPPED (the fail-open path above)
#
# Per-package timings are recorded for every run: the guard's console evidence
# is bounded to the last 2000 chars of go test output, so the full record is
# read from the guard's own run log (.gitreins/logs/guard-*.log, untruncated
# by DF-018). The record is printed and written as JSONL; its path is printed
# even when the run passed, so a contended-but-green run is diagnosable.

set -u

# ── never run under a shell that cannot read the evidence ───────────────────
# Measured (2026-09-27): /bin/sh is dash on this host, and dash does not have
# `$'\t'` — `IFS=$'\t'` becomes the literal 3-character string `$\t`, the tab
# separated evidence rows stop splitting, and the classifier then reports a
# budget exhaustion as a PASS. A gate that can be made to LIE by the shell it
# is invoked with is the same defect this row is about, so the wrapper re-execs
# itself under bash rather than trusting its caller. (`sh scripts/gitreins-guard.sh`
# is a habit in this repo's shell suites; the cell sh-invocation in
# docs/evidence/BFS-017-probes/classifier-cells.sh pins this.)
if [ -z "${BASH_VERSION:-}" ]; then
	if command -v bash >/dev/null 2>&1; then
		exec bash "$0" "$@"
	fi
	echo "gitreins-guard.sh: bash is required (the classifier relies on bash semantics); not on PATH" >&2
	exit 4
fi

usage() {
	cat <<'EOF'
usage: gitreins-guard.sh [gitreins guard args...]
       gitreins-guard.sh --run '<command>'
       gitreins-guard.sh --classify <evidence.json>
       gitreins-guard.sh --help

Guard mode (default) runs `gitreins guard --json` once and prints a three-valued
verdict: PASS (0), TEST-FAILURE (1), NOT-FINISHED (3), GUARD-ERROR (4).
--run runs <command> (a go test invocation) and classifies its raw output with
the same rules. --classify applies the classifier to an evidence document
(deterministic, no run). See the header of this file for the why.
EOF
}

mode="guard"
mode_arg=""
while [ $# -gt 0 ]; do
	case "$1" in
	--help | -h)
		usage
		exit 0
		;;
	--run)
		mode="run"
		mode_arg="${2:-}"
		[ -n "$mode_arg" ] || {
			echo "gitreins-guard.sh: --run needs a command string" >&2
			exit 4
		}
		shift 2
		;;
	--classify)
		mode="classify"
		mode_arg="${2:-}"
		[ -n "$mode_arg" ] || {
			echo "gitreins-guard.sh: --classify needs a file" >&2
			exit 4
		}
		shift 2
		;;
	*) break ;;
	esac
done

root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$root" || exit 4

config="$root/.gitreins/config.yaml"
run_started_utc="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

# ── the config's guard knobs, read the way the guard reads them ──────────────
# Mini YAML reader: 2-space-indented `key: value` lines, dotted paths. Comment
# lines are skipped, list items are skipped (they carry no key), and a value is
# only returned when a line actually assigns one. It exists because the wrapper
# must print the budget that was in force — a wrapper that reported someone
# else's numbers would be another liar.
cfg_get() { # <file> <dotted.key>
	[ -f "$1" ] || return 0
	awk -v want="$2" '
		/^[ \t]*#/ { next }
		/^[ \t]*$/ { next }
		{
			line = $0
			match(line, /^[ \t]*/); indent = RLENGTH
			body = substr(line, indent + 1)
			if (body ~ /^-/) next
			if (body !~ /^[A-Za-z0-9_.-]+:/) next
			key = body; sub(/:.*/, "", key)
			val = body; sub(/^[^:]*:[ \t]*/, "", val); sub(/[ \t]+$/, "", val)
			depth = int(indent / 2)
			stack[depth] = key
			for (d = depth + 1; d < 24; d++) stack[d] = ""
			if (val == "") next
			path = ""
			for (d = 0; d <= depth; d++) if (stack[d] != "") path = (path == "" ? stack[d] : path "." stack[d])
			if (path == want) {
				gsub(/^["'"'"']|["'"'"']$/, "", val)
				print val
				exit
			}
		}' "$1"
}

cfg_bool() { # <file> <key> <default>
	v="$(cfg_get "$1" "$2")"
	case "$v" in
	true | True | TRUE | yes | on) echo true ;;
	false | False | FALSE | no | off) echo false ;;
	"") echo "$3" ;;
	*) echo "$v" ;;
	esac
}

cfg_int() { # <file> <key> <default>
	v="$(cfg_get "$1" "$2")"
	case "$v" in
	"") echo "$3" ;;
	''|*[!0-9]*) echo "$3" ;;
	*) echo "$v" ;;
	esac
}

budget_test_timeout="$(cfg_int "$config" guards.test_timeout 180)"
budget_hook_timeout="$(cfg_int "$config" guards.hook_timeout 300)"

# The lanes a complete run must contain, straight from the config the guard
# reads — so a lane disabled on purpose is not reported as a missing lane.
expected_lanes=""
[ "$(cfg_bool "$config" guards.secrets true)" = "true" ] && expected_lanes="secrets"
if [ "$(cfg_bool "$config" guards.go.tests true)" = "true" ] ||
	[ "$(cfg_bool "$config" guards.go.lint true)" = "true" ] ||
	[ "$(cfg_bool "$config" guards.go.build true)" = "true" ]; then
	[ "$(cfg_bool "$config" guards.go.build true)" = "true" ] && expected_lanes="$expected_lanes go_build"
	[ "$(cfg_bool "$config" guards.go.lint true)" = "true" ] && expected_lanes="$expected_lanes go_lint"
	[ "$(cfg_bool "$config" guards.go.tests true)" = "true" ] && expected_lanes="$expected_lanes go_tests"
fi
# NOTE: `expected_lanes` is deliberately NOT normalized through `set --` — the
# first version did that and clobbered the script's OWN positional parameters,
# so `"$@"` (the extra arguments handed to `gitreins guard`) became the lane
# list and the guard died with "unrecognized arguments: secrets go_build …"
# (caught by the green cells, which is what cells are for).

lane_is_expected() { # <lane>
	case " $expected_lanes " in
	*" $1 "*) return 0 ;;
	*) return 1 ;;
	esac
}

# ── signature tables ────────────────────────────────────────────────────────
# A budget signature: a message only a clock (or a host) can produce. If any of
# these appears, the run did not reach a verdict on the tree.
is_budget_signature() {
	case "$1" in
	*"Tests timed out after"*) return 0 ;;
	*"test timed out after"*) return 0 ;;
	*"Remaining checks skipped"*) return 0 ;;
	*"fail-open"*) return 0 ;;
	*) return 1 ;;
	esac
}

# A defect signature: a message only a WRONG TREE produces.
is_defect_signature() {
	case "$1" in
	*"--- FAIL:"*) return 0 ;;
	*"[build failed]"*) return 0 ;;
	*"build failed"*) return 0 ;;
	*"cannot find package"*) return 0 ;;
	*"undefined:"*) return 0 ;;
	*) return 1 ;;
	esac
}

# An EMPTY-SCOPE message: the gate ran and had nothing to grade. This is not a
# gap — it is the intended shape of a docs-only change, and treating it as one
# would refuse every such commit. It is reported as a note instead.
is_empty_scope_summary() {
	case "$1" in
	*"No Go files staged"*) return 0 ;;
	*"No Go files in scope"*) return 0 ;;
	*"no files in scope"*) return 0 ;;
	*"No files staged"*) return 0 ;;
	*"no staged files"*) return 0 ;;
	*"No matching test files"*) return 0 ;;
	*) return 1 ;;
	esac
}

# ── the per-package timing record ───────────────────────────────────────────
# go test prints one line per package: `ok  <pkg>\t13.4s` / `FAIL\t<pkg>\t120.0s`.
# The split that matters on a busy box is the RATIO between a quiet run and a
# contended one (internal/registry: 13s quiet, 194s contended) — so the record
# keeps every package's number, and the report leads with the slowest ones.
timings_file="${BUNKER_GUARD_TIMINGS:-${TMPDIR:-/tmp}/bunker-go-tests-timings.jsonl}"
timings_tmp=""
report_timings() { # <file-with-go-test-output> <label> <verdict>
	[ -s "$1" ] || return 0
	awk -v out="$timings_file" -v label="$2" -v verdict="$3" -v at="$run_started_utc" '
		BEGIN { n = 0 }
		/^(ok|FAIL|---)[ 	]+/ {
			raw = $0
			sub(/^(ok|FAIL|---)[ 	]+/, "", raw)
			if (raw !~ /[0-9]+\.[0-9]+s$/) next
			secs = raw; sub(/^.*[ 	]/, "", secs); sub(/s$/, "", secs)
			pkg = raw; sub(/[ 	][0-9]+\.[0-9]+s$/, "", pkg)
			status = (substr($0, 1, 4) == "FAIL") ? "fail" : "ok"
			if (substr($0, 1, 3) == "---") { next }
			n++
			pkgs[n] = pkg; sec[n] = secs; st[n] = status
		}
		END {
			if (n == 0) exit 0
			# NOTE: the record array is `rec`, not `line`: gawk refuses a var
			# and an array sharing a name ("illegal reference to variable
			# line"), which silently killed this whole record in the first
			# version of the file.
			for (i = 1; i <= n; i++)
				rec[i] = sprintf("{\"at\":\"%s\",\"verdict\":\"%s\",\"label\":\"%s\",\"package\":\"%s\",\"seconds\":%s,\"status\":\"%s\"}",
					at, verdict, label, pkgs[i], sec[i], st[i])
			for (i = 1; i <= n; i++) print rec[i] > out
			close(out)
			# insertion sort, descending by seconds (n is small)
			for (i = 2; i <= n; i++) {
				ks = sec[i]; kp = pkgs[i]; kt = st[i]; j = i - 1
				while (j >= 1 && sec[j] + 0 < ks + 0) { sec[j+1] = sec[j]; pkgs[j+1] = pkgs[j]; st[j+1] = st[j]; j-- }
				sec[j+1] = ks; pkgs[j+1] = kp; st[j+1] = kt
			}
			print "  per-package timings (slowest first, " n " package(s) — full record: " out ")"
			top = (n < 8) ? n : 8
			for (i = 1; i <= top; i++) printf "    %-6s %8ss  %s\n", st[i], sec[i], pkgs[i]
		}' "$1"
}

# ── the classifier ──────────────────────────────────────────────────────────
# verdict_* are the three facts the classifier derives; the precedence is the
# contract: a defect found by a gate that DID run outranks an unfinished gate,
# because "a real failure must still fail under the same contention".
v_defect=""      # "1" when a gate ran and reported a defect
v_defect_why=""
v_unfinished=""  # "1" when a gate did not complete
v_unfinished_why=""
v_failing_test=""
v_na=""          # notes: gates that had nothing to grade (not a gap)

classifier_note() { printf '%s\n' "$*"; }

classify_evidence() { # <evidence.json> <guard_rc>
	local doc="$1" guard_rc="$2"
	v_defect=""
	v_defect_why=""
	v_unfinished=""
	v_unfinished_why=""
	v_failing_test=""
	v_na=""

	local checks lane outcome passed summary
	# ONE jq pass builds the per-lane table; every later question is answered in
	# bash. The first version spawned jq 12-15 times per run — measured in the
	# `times` arm as ~250 ms of the fixture's 950 ms run, which is overhead a
	# gate that runs on every commit should not pay.
	# NOTE: jq's `//` alternative operator treats `false` as empty, so a failed
	# check would read as "null" and the defect loop would never fire — the
	# `defect`/`both` cells caught exactly that. `tostring` is exact.
	if ! jq -e 'has("checks")' "$doc" >/dev/null 2>&1; then
		classifier_note "GUARD-ERROR: the evidence document is not readable as evidence-v1 JSON"
		return 4
	fi
	checks="$(jq -r '.checks[]? | [.id, (.outcome // ""), (.passed | tostring), ((.summary // "") | gsub("\n"; " "))] | @tsv' "$doc" 2>/dev/null)"
	meta="$(jq -r '[(.metadata.degraded // false), ((.metadata.skippedSteps // []) | join(","))] | @tsv' "$doc" 2>/dev/null)"

	declare -A L_OUT=() L_PASS=() L_SUM=()
	doc_order=""
	while IFS=$'	' read -r lane outcome passed summary; do
		[ -n "$lane" ] || continue
		L_OUT["$lane"]="$outcome"
		L_PASS["$lane"]="$passed"
		L_SUM["$lane"]="$summary"
		doc_order="$doc_order $lane"
	done <<EOF
$checks
EOF

	# 1. gates that ran and reported a defect.
	# PRECEDENCE, and it is the point of the row: a summary that names a real
	# defect outranks a budget signature in the SAME summary. A run can both
	# fail a test and run out of time (the fixture cell `both` pins it); the
	# failure must still be reported as a failure, or the fix has made the gate
	# quieter about real regressions — the trade this row forbids.
	# An unexplained non-pass is a FAILURE, not a timeout: only a summary that
	# actually names a budget may be downgraded to NOT-FINISHED.
	for lane in $doc_order; do
		[ "${L_PASS[$lane]}" = "false" ] || continue
		summary="${L_SUM[$lane]}"
		budget_hit=""
		is_budget_signature "$summary" && budget_hit=1
		if is_defect_signature "$summary" || [ -z "$budget_hit" ]; then
			v_defect="1"
			v_defect_why="${v_defect_why:+$v_defect_why; }$lane reported a defect"
			if [ -n "$budget_hit" ]; then
				v_defect_why="$v_defect_why (and the run ALSO hit a budget — it is incomplete)"
			fi
			case "$summary" in
			*"--- FAIL:"*)
				v_failing_test="${v_failing_test:+$v_failing_test; }$(printf '%s' "$summary" | sed -n 's/.*\(--- FAIL: [^ ]*\).*/\1/p' | head -1)"
				;;
			esac
			continue
		fi
		v_unfinished="1"
		v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }$lane exhausted its budget ($summary)"
	done

	# 2. lanes that never ran. The evidence document is the authority: the
	# guard's `hook_timeout` early-return drops the remaining lanes and still
	# reports passed=true, so a green header is not evidence a lane ran.
	local e
	for e in $expected_lanes; do
		case " $doc_order " in
		*" $e "*)
			# Present — but a skip (TRUST-001) is still "did not run", UNLESS
			# the skip reason is an EMPTY SCOPE. A gate that had nothing to
			# grade did its job: refusing here would block every docs-only
			# commit. The distinction matters because the installed gitreins
			# reports a no-scope lane as `outcome: pass` with the message
			# "No Go files staged", while newer versions mark it
			# `outcome: unknown` with that skip reason; both are handled, one
			# as a note and one as a note instead of a refusal.
			summary_e="${L_SUM[$e]}"
			if is_empty_scope_summary "$summary_e"; then
				v_na="${v_na:+$v_na; }$e graded nothing ($summary_e)"
				continue
			fi
			if [ "${L_OUT[$e]}" = "unknown" ]; then
				v_unfinished="1"
				v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }$e was skipped, not run"
			fi
			;;
		*)
			v_unfinished="1"
			v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }$e never ran (absent from the evidence)"
			;;
		esac
	done

	# 3. the guard's own degradation flags.
	# A substantive gate that did no work is a DEGRADED run, and TRUST-001 says
	# a gate that never ran is not a passing gate — EXCEPT when the reason is an
	# empty scope (nothing to grade), which is the intended shape of a docs-only
	# change. Those are notes. Any other skip, and a degraded run whose skips
	# cannot be attributed at all, is a gap.
	degraded="$(printf '%s' "$meta" | cut -f1)"
	skipped="$(printf '%s' "$meta" | cut -f2)"
	if [ "$degraded" = "true" ] || [ -n "$skipped" ]; then
		unattributed=""
		for e in $(printf '%s' "$skipped" | tr ',' ' '); do
			summary_e="${L_SUM[$e]}"
			if is_empty_scope_summary "$summary_e"; then
				v_na="${v_na:+$v_na; }$e skipped on an empty scope ($summary_e)"
			elif is_budget_signature "$summary_e"; then
				v_unfinished="1"
				v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }$e did not finish ($summary_e)"
			else
				unattributed="$unattributed $e"
			fi
		done
		if [ -n "$unattributed" ]; then
			v_unfinished="1"
			v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }guard metadata marks these gates degraded/skipped:$unattributed"
		elif [ -z "$skipped" ]; then
			v_unfinished="1"
			v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }guard metadata marks the run degraded and names no step"
		fi
	fi
	# 4. fail-closed: a non-zero guard exit that the document does not explain
	# is never a pass. (The document is preferred, but the exit code is a fact.)
	if [ "$guard_rc" -ne 0 ] && [ -z "$v_defect" ] && [ -z "$v_unfinished" ]; then
		v_unfinished="1"
		v_unfinished_why="the guard exited $guard_rc with no failing gate and no named cause"
	fi
	return 0
}

# classify_text applies the same rules to a raw command's output (suite mode and
# the text fallback when jq is missing). Exit status: 0/1/3.
classify_text() { # <output-file> <rc>
	local f="$1" rc="$2"
	local defect="" unfinished="" why_u="" why_d=""
	if grep -q '^--- FAIL:' "$f" || grep -q '\[build failed\]' "$f" || grep -q 'cannot find package' "$f"; then
		defect=1
		why_d="a test or build reported a defect"
	fi
	if grep -q 'test timed out after' "$f"; then
		unfinished=1
		why_u="go test panicked on its own per-binary ceiling (test timed out)"
	fi
	if [ "$rc" -ne 0 ] && [ -z "$defect" ] && [ -z "$unfinished" ]; then
		defect=1
		why_d="the command exited $rc with no recognisable cause"
	fi
	if [ "$rc" -ne 0 ] && [ -n "$defect" ] && [ -n "$unfinished" ]; then
		why_d="$why_d; the run ALSO hit a budget ($why_u)"
	fi
	if [ -z "$defect" ] && [ -n "$unfinished" ]; then
		v_defect=""
		v_unfinished="$unfinished"
		v_unfinished_why="$why_u"
		return 3
	fi
	if [ -n "$defect" ]; then
		v_defect="$defect"
		v_defect_why="$why_d"
		return 1
	fi
	return 0
}

# ── reporting ───────────────────────────────────────────────────────────────
print_budget_line() { # <elapsed-seconds> <label>
	local where="${config#"$root"/}"
	[ -f "$config" ] || where="defaults — $where not found"
	printf '  budgets: guards.test_timeout=%ss guards.hook_timeout=%ss (from %s)\n' \
		"$budget_test_timeout" "$budget_hook_timeout" "$where"
	printf '  elapsed: %ss — expected gates: %s\n' "$1" "${expected_lanes:-<none: every gate disabled>}"
	printf '  host loadavg at exit: %s\n' "$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || echo n/a)"
}

emit_verdict() { # <PASS|TEST-FAILURE|NOT-FINISHED|GUARD-ERROR> <why> <elapsed> <rc>
	local verdict="$1" why="$2" elapsed="$3" rc="$4"
	echo
	echo "================================================================"
	case "$verdict" in
	PASS) echo "GUARD VERDICT: PASS — every expected gate ran and passed" ;;
	TEST-FAILURE)
		echo "GUARD VERDICT: TEST-FAILURE — the tree is wrong"
		echo "  $why"
		[ -n "$v_failing_test" ] && echo "  first failing test: $v_failing_test"
		;;
	NOT-FINISHED)
		echo "GUARD VERDICT: NOT-FINISHED — no test verdict was reached"
		echo "  $why"
		echo "  This is NOT a code failure: no gate reported a defect in the tree."
		echo "  Nothing was retried. Raise the budget in .gitreins/config.yaml and"
		echo "  re-run when the host allows it — or land on a quieter box."
		;;
	GUARD-ERROR)
		echo "GUARD VERDICT: GUARD-ERROR — the gate itself could not decide"
		echo "  $why"
		;;
	esac
	[ -n "$v_na" ] && echo "  note: $v_na"
	echo "================================================================"
	print_budget_line "$elapsed"
	exit "$rc"
}

# ── modes ───────────────────────────────────────────────────────────────────
start_epoch="$(date +%s)"
elapsed() { echo $(($(date +%s) - start_epoch)); }

needs_jq() {
	command -v jq >/dev/null 2>&1 && return 0
	echo "gitreins-guard.sh: jq is required for guard/--classify mode (evidence parsing)" >&2
	return 1
}

# classify_guard_text is the jq-less path: the same precedence applied to the
# guard's human summary. It is deliberately stricter about PASS (the exact
# `Tier 1 Guards: PASS` header AND no budget signature) because the text form
# cannot verify per-lane completeness — and it says so.
classify_guard_text() { # <output-file> <rc>
	local f="$1" rc="$2"
	v_defect=""
	v_defect_why=""
	v_unfinished=""
	v_unfinished_why=""
	v_failing_test=""
	echo "  NOTE: jq absent — classified from the guard's text summary; per-lane completeness is not verifiable here (install jq for the evidence path)."
	if grep -q '^--- FAIL:' "$f" || grep -q '\[build failed\]' "$f" || grep -q 'cannot find package' "$f"; then
		v_defect=1
		v_defect_why="a test or build reported a defect"
		v_failing_test="$(sed -n 's/^\(--- FAIL: [^ ]*\).*/\1/p' "$f" | head -1)"
	fi
	if grep -q 'Tests timed out after\|Remaining checks skipped\|fail-open\|test timed out after' "$f"; then
		v_unfinished=1
		v_unfinished_why="a gate was killed by a budget (the guard's own budget line is above)"
	fi
	if grep -q 'DEGRADED PASS' "$f"; then
		v_unfinished=1
		v_unfinished_why="${v_unfinished_why:+$v_unfinished_why; }the guard reported a DEGRADED pass (a gate did not run)"
	fi
	if [ "$rc" -ne 0 ] && [ -z "$v_defect" ] && [ -z "$v_unfinished" ]; then
		v_defect=1
		v_defect_why="the guard exited $rc with no recognisable cause"
	fi
	return 0
}

case "$mode" in
classify)
	needs_jq || exit 4
	classify_evidence "$mode_arg" 0
	rc_cls=$?
	[ "$rc_cls" -eq 4 ] && emit_verdict GUARD-ERROR "$(classifier_note 'unreadable evidence document')" "$(elapsed)" 4
	if [ -n "$v_defect" ]; then
		emit_verdict TEST-FAILURE "$v_defect_why" "$(elapsed)" 1
	fi
	if [ -n "$v_unfinished" ]; then
		emit_verdict NOT-FINISHED "$v_unfinished_why" "$(elapsed)" 3
	fi
	emit_verdict PASS "" "$(elapsed)" 0
	;;

run)
	out="$(mktemp)"
	sh -c "$mode_arg" >"$out" 2>&1
	rc=$?
	classify_text "$out" "$rc"
	rc_cls=$?
	echo "================================================================"
	echo "GUARD SUITE: $mode_arg"
	echo "  command exit: $rc"
	echo "================================================================"
	# the raw output is the evidence; print it (bounded only by the caller's patience)
	cat "$out"
	report_timings "$out" "run-mode" "$rc_cls"
	if [ "$rc_cls" -eq 1 ]; then
		emit_verdict TEST-FAILURE "$v_defect_why" "$(elapsed)" 1
	fi
	if [ "$rc_cls" -eq 3 ]; then
		emit_verdict NOT-FINISHED "$v_unfinished_why" "$(elapsed)" 3
	fi
	emit_verdict PASS "" "$(elapsed)" 0
	;;

guard)
	logs_before_guard="$(ls "$root"/.gitreins/logs/guard-*.log 2>/dev/null | wc -l | tr -d ' ')"
	evidence="$(mktemp)"
	guard_err="$(mktemp)"
	if command -v jq >/dev/null 2>&1; then
		# --json is what makes the DOCUMENT (not the exit code) the authority,
		# and it is the only stable machine surface the guard offers.
		gitreins guard --json "$@" >"$evidence" 2>"$guard_err"
		guard_rc=$?
		if [ ! -s "$evidence" ]; then
			cat "$guard_err" >&2
			emit_verdict GUARD-ERROR "the guard produced no evidence document (exit $guard_rc)" "$(elapsed)" 4
		fi
		classify_evidence "$evidence" "$guard_rc"
		cls_rc=$?
		[ "$cls_rc" -eq 4 ] && emit_verdict GUARD-ERROR "the evidence document did not parse" "$(elapsed)" 4
	else
		# No jq: the guard's own text summary, classified with the same
		# precedence and a strictly narrower PASS.
		gitreins guard "$@" >"$evidence" 2>&1
		guard_rc=$?
		guard_err="/dev/null"
		classify_guard_text "$evidence" "$guard_rc"
	fi

	# The guard narrates a non-pass run on stderr; keep it as human context.
	[ -s "$guard_err" ] && cat "$guard_err" >&2

	# The full, untruncated run log is where the complete per-package record
	# lives (the evidence document's check summary is bounded to the tail).
	log_new="$(ls -t "$root"/.gitreins/logs/guard-*.log 2>/dev/null | head -1)"
	timings_src="$evidence"
	timings_label="guard-evidence(tail-bounded)"
	if [ -n "$log_new" ] && [ "$(ls "$root"/.gitreins/logs/guard-*.log 2>/dev/null | wc -l | tr -d ' ')" -gt "$logs_before_guard" ]; then
		timings_src="$log_new"
		timings_label="guard-log(${log_new##*/})"
	fi
	echo
	echo "================================================================"
	echo "BFS-017 gate report — $run_started_utc"
	echo "  run: gitreins guard --json $*"
	echo "  guard exit: $guard_rc"
	echo "  evidence: $evidence"
	[ -n "$log_new" ] && echo "  guard log: $log_new"
	if command -v jq >/dev/null 2>&1; then
		report="$(jq -r '
			([.checks[]?.id] | join(", ")),
			([.checks[]? | select(.passed == false) | .id] | join(", ")),
			([.checks[]? | select(.passed == false) | .summary] | join(" | "))
		' "$evidence" 2>/dev/null)"
		echo "  lanes in the evidence: $(printf '%s\n' "$report" | sed -n 1p)"
		echo "  failing gates: $(printf '%s\n' "$report" | sed -n 2p)"
		echo "  diagnostics: $(printf '%s\n' "$report" | sed -n 3p | cut -c1-300)"
	else
		echo "  lanes: (text mode — see the run output below)"
		grep -E '^  [~✓✗] ' "$evidence" 2>/dev/null | head -8
		grep -E 'first_failing_test|Tests timed out|Remaining checks skipped' "$evidence" 2>/dev/null | head -4
	fi
	echo "================================================================"
	report_timings "$timings_src" "$timings_label" "$guard_rc"

	if [ -n "$v_defect" ]; then
		emit_verdict TEST-FAILURE "$v_defect_why" "$(elapsed)" 1
	fi
	if [ -n "$v_unfinished" ]; then
		emit_verdict NOT-FINISHED "$v_unfinished_why" "$(elapsed)" 3
	fi
	emit_verdict PASS "" "$(elapsed)" 0
	;;
esac

echo "gitreins-guard.sh: unreachable mode $mode" >&2
exit 4
