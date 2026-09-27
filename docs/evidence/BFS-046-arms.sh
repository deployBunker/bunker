#!/bin/sh
# BFS-046 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE THE PROGRAM IS BUILT ON: a test that cannot fail proves nothing.
# Every cell of the BFS-046 program that claims to catch a defect appears below
# with the SOURCE MUTATION that turns it RED. Each mutation is a patch in this
# directory, applied with `git apply`, run against the named cell, and RESTORED
# from a byte copy whose sha256 is re-checked afterwards; a restore that does not
# match aborts the run. A cell with no mutation is a cell with no evidence.
#
# modes:
#   green              the tree as committed: every BFS-046 cell must PASS.
#   nochannel          MUTATION: the channel's poll form (`X-Bunker-Op: events`)
#                      is refused, so the watcher comes up and the channel still
#                      does not answer. Cell 1 must FAIL; cell 2 must still PASS.
#   nowatchpublish     MUTATION: the watcher observes and never publishes to the
#                      SERVED revision. Cell 1/2 must FAIL; the overflow
#                      negative control must still PASS (attribution).
#   norescan           MUTATION: a rescan request is dropped, so the overflow is
#                      counted and never repaired. Cell 3 must FAIL; cell 2 must
#                      still PASS.
#   mutate-in-place    MUTATION: Cache.Stage reuses the blob the path already
#                      names -- the mutate-in-place refresh. Cell 4 must FAIL;
#                      cell 9 must still PASS.
#   leaked-lock        MUTATION: a pin becomes a PERSISTED in-use marker, so a
#                      SIGKILLed reader poisons the path. Cell 8 (killed reader)
#                      must FAIL; cell 8 (killed refresher) must still PASS.
#   no-sweep           MUTATION: the orphan sweep never removes anything, so an
#                      abandoned refresh's bytes survive. Cell 8 (killed
#                      refresher) must FAIL; cell 8 (killed reader) must PASS.
#   exclusive-size     MUTATION: the size-rule comparison at the bound becomes
#                      exclusive (`>=`), the off-by-one at 8 MiB. Cell 9 must
#                      FAIL; the pinned-defaults cell must still PASS.
#   floor              the coverage floor over the invalidation packages, as a
#                      NUMBER (see BFS-046-coverage.sh).
#   all                green, then every mutation, then the floor.
#   pending            the pending-until-landing gate: which cells are LIVE and
#                      which are waiting on 036/037/039, read from the gate cell.
#
# usage: sh docs/evidence/BFS-046-arms.sh <mode>

set -u

mode="${1:-}"

case "$mode" in
green | nochannel | nowatchpublish | norescan | mutate-in-place | leaked-lock | no-sweep | exclusive-size | floor | all | pending) ;;
*)
	printf 'usage: %s <green|nochannel|nowatchpublish|norescan|mutate-in-place|leaked-lock|no-sweep|exclusive-size|floor|all|pending>\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

webdav_pkg=./internal/server/webdav
fsclient_pkg=./internal/fsclient
watch_go=internal/server/webdav/watch.go
ops_go=internal/server/webdav/ops.go
cache_go=internal/fsclient/cache.go
hotpolicy_go=internal/fsclient/hotpolicy.go

# The pre-mutation hashes of the three files every mutation touches. They are
# re-checked after every restore: a restore that does not land is a broken run,
# not a footnote.
hash_watch_go=74211882400483020c2231f9f5197470eb3ee186f50fb793a684fd291396d138
hash_ops_go=5dc7f4fe8367fe260ac3ea6b1f95a27c018f592c77ff5999764339af1afc0687
hash_cache_go=ff928ac73d7d219232ba49525e66e928b8635bb676316496ed33438e79eabd4e
hash_hotpolicy_go=0b7538b0a3fba4fcba05a715061bcf3dc704b297b4e286b2c03c4956e16d1e01

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_file=""
bak_path=""

restore() {
	status=0
	if [ -n "$bak_file" ] && [ -n "$bak_path" ]; then
		cp "$bak_file" "$bak_path"
		now=$(hash "$bak_path")
		want=""
		case "$bak_path" in
		"$watch_go") want="$hash_watch_go" ;;
		"$ops_go") want="$hash_ops_go" ;;
		"$cache_go") want="$hash_cache_go" ;;
		"$hotpolicy_go") want="$hash_hotpolicy_go" ;;
		esac
		if [ -n "$want" ] && [ "$now" != "$want" ]; then
			say "RESTORE FAILED: $bak_path sha256=$now, expected $want"
			status=1
		else
			say "restore: $bak_path sha256=$now (verified)"
		fi
		rm -f "$bak_file"
		bak_file=""
		bak_path=""
	fi
	return $status
}

trap 'restore || exit 3' EXIT

stash() {
	bak_path="$1"
	bak_file=$(mktemp)
	cp "$bak_path" "$bak_file"
	say "pre-mutation: $bak_path sha256=$(hash "$bak_path")"
}

run_cell() {
	label="$1"
	pkg="$2"
	filter="$3"
	say "--- $label: go test $pkg -run $filter -count=1 -v -timeout 300s"
	go test "$pkg" -run "$filter" -count=1 -v -timeout 300s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

# expect_runs runs one cell and fails the SCRIPT when the cell's outcome is not
# the one the arm declares.
expect_runs() { # <want: pass|fail> <label> <pkg> <filter>
	want="$1"
	label="$2"
	pkg="$3"
	filter="$4"
	run_cell "$label" "$pkg" "$filter"
	rc=$?
	case "$want:$rc" in
	pass:0) say "OK   $label PASSED as declared" ;;
	fail:0) say "FAIL $label PASSED but the mutation declares it must FAIL"; return 1 ;;
	pass:*) say "FAIL $label did not pass (exit $rc)"; return 1 ;;
	fail:*) say "OK   $label FAILED as declared (exit $rc)" ;;
	esac
	return 0
}

mutation() { # <mode> <file> <patch> <target pkg> <target filter> <attr pkg> <attr filter>
	mode_name="$1"
	file="$2"
	patch_file="$3"
	tpkg="$4"
	tfilter="$5"
	apkg="$6"
	afilter="$7"
	mut_rc=0

	say ""
	say "=============================================================="
	say "CONTROL $mode_name — $(basename "$patch_file")"
	say "=============================================================="
	stash "$file"
	if ! git apply "$patch_file"; then
		say "PATCH FAILED TO APPLY: $patch_file"
		restore
		trap - EXIT
		exit 4
	fi
	say "mutated: $file sha256=$(hash "$file") via $(basename "$patch_file")"

	expect_runs fail "CONTROL $mode_name (the mutation MUST turn this cell red)" "$tpkg" "$tfilter" || mut_rc=1
	expect_runs pass "CONTROL $mode_name (attribution: this cell MUST be unmoved)" "$apkg" "$afilter" || mut_rc=1
	restore || mut_rc=1
	trap - EXIT
	return "${mut_rc:-0}"
}

say "BFS-046 arms — mode=$mode"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

rc_all=0

do_green() {
	say "tree under test:"
	say "  $watch_go     sha256=$(hash "$watch_go")"
	say "  $cache_go     sha256=$(hash "$cache_go")"
	say "  $hotpolicy_go sha256=$(hash "$hotpolicy_go")"
	expect_runs pass "GREEN (server cells 1-3)" "$webdav_pkg" 'TestBFS046' || rc_all=1
	expect_runs pass "GREEN (client cells 4-9 + the pending gate)" "$fsclient_pkg" 'TestBFS046' || rc_all=1
}

do_mutations() {
	mutation nochannel "$ops_go" "$here/BFS-046-redproof-nochannel.patch" \
		"$webdav_pkg" 'TestBFS046Cell01' \
		"$webdav_pkg" 'TestBFS046Cell02' || rc_all=1
	mutation nowatchpublish "$watch_go" "$here/BFS-046-redproof-nowatchpublish.patch" \
		"$webdav_pkg" 'TestBFS046Cell0[12]' \
		"$webdav_pkg" 'TestWatchOverflowNegativeControlDisablesTheDrain' || rc_all=1
	mutation norescan "$watch_go" "$here/BFS-046-redproof-norescan.patch" \
		"$webdav_pkg" 'TestBFS046Cell03' \
		"$webdav_pkg" 'TestBFS046Cell02' || rc_all=1
	mutation mutate-in-place "$cache_go" "$here/BFS-046-redproof-mutate-in-place.patch" \
		"$fsclient_pkg" 'TestBFS046Cell04|TestAtomicRefreshHalfFileCell' \
		"$fsclient_pkg" 'TestBFS046Cell09' || rc_all=1
	mutation leaked-lock "$cache_go" "$here/BFS-046-redproof-leaked-lock.patch" \
		"$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' \
		"$fsclient_pkg" 'TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept' || rc_all=1
	mutation no-sweep "$cache_go" "$here/BFS-046-redproof-no-sweep.patch" \
		"$fsclient_pkg" 'TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept' \
		"$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' || rc_all=1
	mutation exclusive-size "$hotpolicy_go" "$here/BFS-046-redproof-exclusive-size.patch" \
		"$fsclient_pkg" 'TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason' \
		"$fsclient_pkg" 'TestHotPolicyDefaultsAreThePinnedNumbers' || rc_all=1
}

do_floor() {
	say ""
	say "=============================================================="
	say "COVERAGE FLOOR over the invalidation packages"
	say "=============================================================="
	sh "$here/BFS-046-coverage.sh" || rc_all=1
}

do_pending() {
	say ""
	say "=============================================================="
	say "CELLS LIVE vs PENDING — read from the program itself"
	say "=============================================================="
	say "LIVE (landed code under test today):"
	say "  cell 1  SMOKE          $webdav_pkg/TestBFS046Cell01SmokeWatcherAndChannelComeUp"
	say "  cell 2  INTEGRATION    $webdav_pkg/TestBFS046Cell02OutOfBandEditOverRealFilesystem (shell write + real git checkout)"
	say "  cell 3  OVERFLOW       $webdav_pkg/TestBFS046Cell03OverflowIsACountedFullRescanAndNeverQuiet (+ BFS-035's real-kernel reproducer)"
	say "  cell 4  HALF-FILE      $fsclient_pkg/TestBFS046Cell04HalfFileCellIsLoadBearing (+ TestAtomicRefreshHalfFileCell, both mutate-in-place controls)"
	say "  cell 8  CANCEL         $fsclient_pkg/TestBFS046Cell08* (killed reader; killed refresher)"
	say "  cell 9  SIZE RULE      $fsclient_pkg/TestBFS046Cell09* (pinned number, inclusive rule, both refusals AT THE EDGE)"
	say "  cell 10 COVERAGE FLOOR $here/BFS-046-coverage.sh"
	say "PENDING (gated, never weakened — the gate FAILS the moment the dependency lands):"
	say "  cell 5  STAMPEDE       pending-until-BFS-037 (foreground latency under a burst)"
	say "  cell 6  STOP           pending-until-BFS-037 (STOP IN FULL, and the two refused readings)"
	say "  cell 7  PROMOTION      pending-until-BFS-037 (EXACTLY 1 fetch per path)"
	say "  cell 8b DELIBERATE     pending-until-BFS-039 (FUSE interrupt; EINTR vs EIO)"
	say "  cell 9b SKIP CENSUS    pending-until-BFS-037 (one cell per reason, driving the LIVE path)"
	say ""
	expect_runs pass "the pending gate is still TRUE (no dependency has landed)" "$fsclient_pkg" 'TestBFS046PendingUntilLandingGate' || rc_all=1
}

case "$mode" in
green) do_green ;;
all)
	do_green
	do_mutations
	do_floor
	do_pending
	;;
floor) do_floor ;;
pending) do_pending ;;
esac

# A single-mutation mode runs the SAME table entry `all` runs, so the mode the
# operator names and the mode `all` exercises cannot drift apart.
case "$mode" in
nowatchpublish | nochannel | norescan | mutate-in-place | leaked-lock | no-sweep | exclusive-size)
	rc_all=0
	case "$mode" in
	nochannel)
		mutation "$mode" "$ops_go" "$here/BFS-046-redproof-$mode.patch" \
			"$webdav_pkg" 'TestBFS046Cell01' \
			"$webdav_pkg" 'TestBFS046Cell02' || rc_all=1
		;;
	nowatchpublish)
		mutation "$mode" "$watch_go" "$here/BFS-046-redproof-$mode.patch" \
			"$webdav_pkg" 'TestBFS046Cell0[12]' \
			"$webdav_pkg" 'TestWatchOverflowNegativeControlDisablesTheDrain' || rc_all=1
		;;
	norescan)
		mutation "$mode" "$watch_go" "$here/BFS-046-redproof-$mode.patch" \
			"$webdav_pkg" 'TestBFS046Cell03' \
			"$webdav_pkg" 'TestBFS046Cell02' || rc_all=1
		;;
	mutate-in-place)
		mutation "$mode" "$cache_go" "$here/BFS-046-redproof-$mode.patch" \
			"$fsclient_pkg" 'TestBFS046Cell04|TestAtomicRefreshHalfFileCell' \
			"$fsclient_pkg" 'TestBFS046Cell09' || rc_all=1
		;;
	leaked-lock)
		mutation "$mode" "$cache_go" "$here/BFS-046-redproof-$mode.patch" \
			"$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' \
			"$fsclient_pkg" 'TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept' || rc_all=1
		;;
	no-sweep)
		mutation "$mode" "$cache_go" "$here/BFS-046-redproof-$mode.patch" \
			"$fsclient_pkg" 'TestBFS046Cell08KilledRefresherNeverPublishesAndIsSwept' \
			"$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' || rc_all=1
		;;
	exclusive-size)
		mutation "$mode" "$hotpolicy_go" "$here/BFS-046-redproof-$mode.patch" \
			"$fsclient_pkg" 'TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason' \
			"$fsclient_pkg" 'TestHotPolicyDefaultsAreThePinnedNumbers' || rc_all=1
		;;
	esac
	;;
esac

say ""
if [ "$rc_all" -eq 0 ]; then
	say "done: mode=$mode — all declared outcomes held"
else
	say "done: mode=$mode — DECLARED OUTCOME NOT HELD (see the FAIL lines above)"
fi
exit "$rc_all"
