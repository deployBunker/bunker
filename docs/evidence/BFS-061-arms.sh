#!/bin/sh
# BFS-061 — the arms, and the two controls that prove each claim can fail.
#
# The row makes TWO claims, so it needs two controls:
#
#   green     the fixed tree: every BFS-061 arm must PASS.
#   unfixed   the tree AS FILED. internal/fsclient/invalidate.go is replaced by the
#             blob of the base commit and sha256-checked against the hash recorded
#             below, so "the unfixed tree" is a named artifact rather than a
#             memory. Arm A (5 heartbeats -> one change -> a manufactured gap) and
#             arm C (a heartbeat masking a real gap) must FAIL; arm B (a real gap
#             is a gap) must still PASS, because it is the invariant, not the
#             defect. The census arm — whose poll fixture this row corrects rather
#             than its expectation — must PASS here too, which is what proves the
#             correction is not fix-dependent.
#   neutered  control 1 (claim: "a heartbeat no longer manufactures a gap"): the fix
#             neutered by a single argument — the heartbeat consumes a seq it never
#             records again, exactly as filed — while the gap check stays armed for
#             every other line. Arm A must FAIL again: the cell catches the defect
#             coming back. Arm C fails with it, and that is the same mechanism and no
#             accident — the mutation removes the heartbeat's seq, which is exactly
#             what C probes — so the attribution arm here is B, a real gap on a
#             CHANGE line, which must still PASS.
#   loosegap  control 2 (claim: "a real gap is still a gap"): the OTHER claim, and
#             the one that matters. The gap classification is removed while the
#             heartbeat still advances the cursor — i.e. the cheap wrong fix, which
#             would trade this P1 for a silent-staleness P0. Arm B AND arm C must
#             FAIL. Arm A still PASSES in this mutant, which is the point: the
#             mutant looks like a fix and is not one.
#   race      the whole fsclient package under -race on the fixed tree.
#
# Every mutation is a two-line substitution on text this row ADDED (so it survives
# a rebase of somebody else's hunks), verified after the fact by grep in BOTH
# directions — the mutant text present, the pre-image text gone — and built before
# the arm runs, so a mutation that does not compile is reported as such rather than
# silently measuring the fixed tree. The restore is a byte copy whose sha256 is
# re-checked after the copy back; a restore that does not match aborts the run.
#
# usage: sh docs/evidence/BFS-061-arms.sh <green|unfixed|neutered|loosegap|race> [base-ref]

set -u

mode="${1:-}"
base="${2:-064b39f}"

case "$mode" in
green | unfixed | neutered | loosegap | race) ;;
*)
	printf 'usage: %s <green|unfixed|neutered|loosegap|race> [base-ref]\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

prod=internal/fsclient/invalidate.go
arms_file=internal/fsclient/invalidate_bfs061_test.go
census_file=internal/fsclient/status_census_test.go

# The filed blob of commit 064b39f — the content the RED was measured on.
filed_prod=14617c6518e69c3335a29ae1ac7cc6aff1c41e67bf24f25aac72e8339056db65

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_prod=""
before_prod=""

restore() {
	status=0
	if [ -n "$bak_prod" ]; then
		cp "$bak_prod" "$prod"
		now=$(hash "$prod")
		if [ "$now" != "$before_prod" ]; then
			say "RESTORE FAILED: $prod sha256=$now, expected $before_prod"
			status=1
		else
			say "restore: $prod sha256=$now (verified byte-identical against the pre-mutation hash)"
		fi
	fi
	return $status
}

trap 'restore || exit 3' EXIT

stash_prod() {
	before_prod=$(hash "$prod")
	bak_prod=$(mktemp)
	cp "$prod" "$bak_prod"
	say "pre-mutation: $prod sha256=$before_prod"
}

run_arms() {
	label="$1"
	filter="$2"
	say "--- $label: go test ./internal/fsclient -run $filter -count=1 -v -timeout 120s"
	go test ./internal/fsclient -run "$filter" -count=1 -v -timeout 120s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

# must_grep <file> <fixed-string> <present|absent> <what it means>
must_grep() {
	if [ "$3" = present ]; then
		if ! grep -qF -- "$2" "$1"; then
			say "MUTATION AUDIT FAILED: $1 does not contain the mutant text [$4]"
			exit 5
		fi
	elif grep -qF -- "$2" "$1"; then
		say "MUTATION AUDIT FAILED: $1 still contains the pre-image text [$4]"
		exit 5
	fi
}

# mutate <label> <perl-expression> <post-string> <pre-string>
mutate() {
	label="$1"
	expr="$2"
	post="$3"
	pre="$4"
	perl -0777 -pi -e "$expr" "$prod" || exit 5
	say "mutated ($label): $prod sha256=$(hash "$prod")"
	must_grep "$prod" "$post" present "$label mutant text"
	must_grep "$prod" "$pre" absent "$label pre-image text"
	if ! gofmt -l "$prod" | grep -q .; then :; else say "MUTATION AUDIT FAILED: $prod is not gofmt-clean after $label"; exit 5; fi
	if ! go build ./internal/fsclient; then say "MUTATION AUDIT FAILED: $prod does not build after $label"; exit 5; fi
	say "mutation audit ($label): mutant text present, pre-image gone, gofmt clean, package builds"
}

say "BFS-061 arms — mode=$mode base=$base"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "$mode" in
green)
	say "tree under test: $prod sha256=$(hash "$prod")"
	say "               $arms_file sha256=$(hash "$arms_file")"
	say "               $census_file sha256=$(hash "$census_file")"
	run_arms GREEN 'TestBFS061'
	;;

unfixed)
	stash_prod
	git show "$base:$prod" >"$prod"
	filed=$(hash "$prod")
	say "swapped in $base:$prod sha256=$filed"
	if [ "$filed" != "$filed_prod" ]; then
		say "MISMATCH: expected the filed blob $filed_prod — this is not the tree the row was filed on, so nothing here would be measured against a named artifact"
		exit 4
	fi
	say "the filed tree, sha256-verified: the RED below is measured on a named content, not on a memory"
	run_arms "RED (tree as filed)" 'TestBFS061'
	run_arms "RED (tree as filed) — the census arm, whose FIXTURE this row corrects and whose expectation it does not" 'TestEveryFigureInTheStatusRecordMovesOrIsExplained'
	;;

neutered)
	stash_prod
	# One argument: the heartbeat consumes a seq it never records (the filed
	# defect), while advanceCursorLocked keeps guarding every other line.
	mutate neutered \
		's/\t\t_, gap := i\.advanceCursorLocked\(ev\.Seq\)/\t\t_, gap := i.advanceCursorLocked(0)/' \
		'_, gap := i.advanceCursorLocked(0)' \
		'_, gap := i.advanceCursorLocked(ev.Seq)'
	run_arms "CONTROL neutered (arm A MUST fail: the resync returns)" 'TestBFS061HeartbeatsDoNotManufactureAGap'
	run_arms "CONTROL neutered (attribution: a real gap on a CHANGE line MUST still pass — it does not depend on the heartbeat's seq)" 'TestBFS061ARealGapIsStillDetected'
	run_arms "CONTROL neutered (same mutation, same mechanism: arm C MUST also fail — it probes the heartbeat's own seq)" 'TestBFS061AHeartbeatCannotMaskARealGap'
	;;

loosegap)
	stash_prod
	# The cheap wrong fix: the cursor still advances over heartbeats (arm A would
	# pass), and the gap classification — the counter AND the verdict — is gone.
	mutate loosegap \
		's/\t\ti\.gaps\+\+\n\t\tgap = true/\t\t\/\/ CONTROL (BFS-061 loosegap): the gap check loosened — an unobserved\n\t\t\/\/ interval is recorded as if it had been observed (silent staleness)./' \
		'CONTROL (BFS-061 loosegap)' \
		'gap = true'
	run_arms "CONTROL loosegap (arm B MUST fail: a REAL gap is no longer detected)" 'TestBFS061ARealGapIsStillDetected'
	run_arms "CONTROL loosegap (arm C MUST fail: a heartbeat now masks a REAL gap)" 'TestBFS061AHeartbeatCannotMaskARealGap'
	run_arms "CONTROL loosegap (attribution: arm A is the mutant's apparent success — the point of the control)" 'TestBFS061HeartbeatsDoNotManufactureAGap'
	;;

race)
	say "tree under test: $prod sha256=$(hash "$prod")"
	say "               $arms_file sha256=$(hash "$arms_file")"
	say "--- race: go test ./internal/fsclient -count=1 -race -timeout 300s"
	go test ./internal/fsclient -count=1 -race -timeout 300s
	say "--- race exit=$?"
	;;
esac

restore
trap - EXIT
say "done: mode=$mode"
