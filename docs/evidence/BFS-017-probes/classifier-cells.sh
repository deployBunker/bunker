#!/usr/bin/env bash
# classifier-cells.sh — the deterministic, CI-safe cells of BFS-017.
#
# WHY THESE EXIST: the row's defect is one of CLASSIFICATION, so the cells that
# prove the classifier are DOCUMENTS, not load. Each cell feeds the gate a
# synthetic evidence-v1 document (the shape `gitreins guard --json` really
# emits — see engine/evidence.py:guard_evidence) and asserts the verdict and
# the exit code. A cell that needs the machine to be busy is not a cell; this
# file needs no contention at all and finishes in well under a second per cell.
#
# The contention itself is proven separately (contention-cells.sh), and the
# real guard is driven in real-guard-cells.sh.
#
# usage: sh docs/evidence/BFS-017-probes/classifier-cells.sh
set -u

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../../.." && pwd)
gate="$root/scripts/gitreins-guard.sh"

[ -f "$gate" ] || {
	echo "classifier-cells: missing $gate" >&2
	exit 2
}

work=$(mktemp -d "${TMPDIR:-/tmp}/bfs017-cells-XXXXXX")
# The scratch is deliberately NOT deleted: the row's own discipline (and this
# fleet's) is to leave a reproducer's scratch behind for inspection. Its path
# is printed so an operator can look at the exact documents a cell used.
trap 'printf "scratch kept: %s\n" "$work"' EXIT

pass=0
fail=0

# doc <name> <body> — writes one synthetic evidence document.
doc() {
	cat >"$work/$1.json"
}

run_cell() { # <label> <file> <want-rc> <want-verdict>
	label="$1"
	file="$2"
	want_rc="$3"
	want_verdict="$4"
	out="$work/$label.out"
	sh "$gate" --classify "$file" >"$out" 2>&1
	got_rc=$?
	got_verdict="$(sed -n 's/^GUARD VERDICT: \([A-Z-]*\).*/\1/p' "$out" | head -1)"
	if [ "$got_rc" = "$want_rc" ] && [ "$got_verdict" = "$want_verdict" ]; then
		printf 'OK   %-14s verdict=%-13s rc=%s\n' "$label" "$got_verdict" "$got_rc"
		pass=$((pass + 1))
	else
		printf 'FAIL %-14s verdict=%s (want %s) rc=%s (want %s)\n' \
			"$label" "${got_verdict:-<none>}" "$want_verdict" "$got_rc" "$want_rc"
		sed -n '1,40p' "$out"
		fail=$((fail + 1))
	fi
	# the budget line must be printed with EVERY verdict: a verdict without the
	# budget that produced it is not attributable.
	if ! grep -q 'budgets: guards.test_timeout=' "$out"; then
		printf 'FAIL %-14s no budget line in the report\n' "$label"
		fail=$((fail + 1))
	fi
}

echo "== BFS-017 classifier cells (deterministic; no contention) =="
echo "gate: $gate"
echo

# ── cell 1: a complete green run ────────────────────────────────────────────
doc green <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "golangci-lint: clean"},
    {"id": "go_tests", "outcome": "pass", "passed": true, "summary": "ok  github.com/deployBunker/bunker/internal/registry\t13.4s\nok  github.com/deployBunker/bunker/internal/agent\t49.2s"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
EOF

# ── cell 2: the guard's wall budget killed the tests lane ───────────────────
# The exact string engine/guards.py:check_go_tests produces, and the exact
# shape that refused a real commit: exit 1, "overall: FAIL", and the guard's
# own diagnostics saying no test failed.
doc budget <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "fail",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "golangci-lint: clean"},
    {"id": "go_tests", "outcome": "fail", "passed": false,
     "summary": "Tests timed out after 600s (guards.test_timeout). Raise it in .gitreins/config.yaml — e.g. test_timeout: 900 for large projects with slow integration suites."}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
EOF

# ── cell 3: a gate ran and reported a defect ────────────────────────────────
doc defect <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "fail",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "golangci-lint: clean"},
    {"id": "go_tests", "outcome": "fail", "passed": false,
     "summary": "--- FAIL: TestRegistryRevoke (0.01s)\n    registry_test.go:88: revoke did not free the port\nFAIL\tgithub.com/deployBunker/bunker/internal/registry\t1.2s"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
EOF

# ── cell 4: the hook_timeout false green ────────────────────────────────────
# The guard's own overall budget fired after the lint lane: engine/guard_manager
# returns a result with the remaining lanes simply ABSENT, passed=true,
# degraded=false — so `gitreins guard --json` exits 0 and prints a PASS header
# while the tests lane never ran. Measured in real-guard-cells.sh, arm
# `falsegreen`.
doc laneabsent <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 2}
}
EOF

# ── cell 5: BOTH — a defect found, and a budget hit ─────────────────────────
# The control that matters: the gate must not become quieter about a real
# failure because a clock also ran out. A defect outranks an unfinished gate.
doc both <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "fail",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "golangci-lint: clean"},
    {"id": "go_tests", "outcome": "fail", "passed": false,
     "summary": "--- FAIL: TestPortAllocNeverReusesALivePort (0.00s)\n    registry_test.go:41: port 33001 handed out twice\npanic: test timed out after 3s\nFAIL\tgithub.com/deployBunker/bunker/internal/registry\t3.0s"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
EOF

# ── cell 6: a lane that did not run, for a reason that is NOT an empty scope ─
doc skip <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "go build: clean"},
    {"id": "go_lint", "outcome": "unknown", "passed": null, "summary": "skipped — linter not on PATH"},
    {"id": "go_tests", "outcome": "pass", "passed": true, "summary": "ok  github.com/deployBunker/bunker/internal/registry\t13.4s"}
  ],
  "metadata": {"degraded": true, "skippedSteps": ["go_lint"], "checkCount": 4}
}
EOF

# ── cell 6b: an EMPTY SCOPE, reported the two ways the guard reports it ─────
# A gate that had nothing to grade did its job: this must stay a PASS (refusing
# here would block every docs-only commit) and must be REPORTED as a note.
# Installed gitreins 0.15.0 reports it as `outcome: pass`; newer versions mark
# the lane `outcome: unknown` with the same skip reason.
doc empty-scope-pass <<'EOF'
{
  "schemaVersion": "1.0.0",
  "subject": {"kind": "guard"},
  "outcome": "pass",
  "checks": [
    {"id": "secrets", "outcome": "pass", "passed": true, "summary": "gitleaks: clean"},
    {"id": "go_build", "outcome": "pass", "passed": true, "summary": "No Go files staged"},
    {"id": "go_lint", "outcome": "pass", "passed": true, "summary": "No Go files staged"},
    {"id": "go_tests", "outcome": "pass", "passed": true, "summary": "No Go files staged"}
  ],
  "metadata": {"degraded": false, "skippedSteps": [], "checkCount": 4}
}
EOF

doc empty-scope-skip <<'EOF'
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
EOF

# ── cell 7: a document that is not evidence ─────────────────────────────────
printf 'not json at all\n' >"$work/malformed.json"

run_cell green "$work/green.json" 0 PASS
run_cell budget "$work/budget.json" 3 NOT-FINISHED
run_cell defect "$work/defect.json" 1 TEST-FAILURE
run_cell laneabsent "$work/laneabsent.json" 3 NOT-FINISHED
run_cell both "$work/both.json" 1 TEST-FAILURE
run_cell skip "$work/skip.json" 3 NOT-FINISHED
run_cell empty-scope-pass "$work/empty-scope-pass.json" 0 PASS
run_cell empty-scope-skip "$work/empty-scope-skip.json" 0 PASS
run_cell malformed "$work/malformed.json" 4 GUARD-ERROR

# the empty-scope cells must SAY they graded nothing — a silent green is the
# defect this row is about.
for c in empty-scope-pass empty-scope-skip; do
	if grep -q '  note: ' "$work/$c.out"; then
		printf 'OK   %-14s reports the note ("graded nothing")\n' "$c-note"
		pass=$((pass + 1))
	else
		printf 'FAIL %-14s no note about the empty scope\n' "$c-note"
		fail=$((fail + 1))
	fi
done

# ── cell 8: the same document, through `sh` ────────────────────────────────
# /bin/sh is dash on this host, and dash has no `$'\t'`: the first version of
# the gate read the tab-separated evidence as one field and reported a budget
# exhaustion as PASS when invoked as `sh scripts/gitreins-guard.sh`. The gate
# now re-execs under bash; this cell is the control for that trap.
out="$work/sh-invocation.out"
sh "$gate" --classify "$work/budget.json" >"$out" 2>&1
rc=$?
verdict="$(sed -n 's/^GUARD VERDICT: \([A-Z-]*\).*/\1/p' "$out" | head -1)"
if [ "$rc" = "3" ] && [ "$verdict" = "NOT-FINISHED" ]; then
	printf 'OK   %-14s verdict=%-13s rc=%s\n' "sh-invocation" "$verdict" "$rc"
	pass=$((pass + 1))
else
	printf 'FAIL %-14s verdict=%s rc=%s — a shell that cannot read the evidence must not be able to lie\n' \
		"sh-invocation" "${verdict:-<none>}" "$rc"
	fail=$((fail + 1))
fi

echo
echo "cells passed: $pass  failed: $fail"
[ "$fail" -eq 0 ] || exit 1
exit 0
