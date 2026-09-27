#!/bin/sh
# BFS-037 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE: a test that cannot fail proves nothing. Every requirement of this row
# appears below with the SOURCE MUTATION that turns its cell RED, applied with
# `git apply`, run by name, and RESTORED from a byte copy whose sha256 is
# re-checked afterwards; a restore that does not match aborts the run. Each arm
# also names an ATTRIBUTION cell that must stay GREEN under the mutation, so a
# red is caused by the requirement under test rather than by a broken tree.
#
# THREE OF THESE ARMS CAUGHT BLIND CELLS ON THE FIRST RUN, which is the whole
# point of writing them: the pool bound was asserted only per-path (a bound that
# cannot bind), the stop was enforced twice so removing one check changed nothing,
# and the atomicity of the publish route is not the mechanism the in-place
# mutation attacks. Each was then FIXED (a real assertion, a two-hunk mutation, a
# mutation of the publish route) rather than declared.
#
# modes:
#   green            the tree as committed: every BFS-037 and BFS-046 cell passes.
#   exclusive-size   MUTATION: the size rule's comparison at the bound becomes
#                    exclusive (`<`), the off-by-one at the ceiling. Cell 1 (and
#                    BFS-046's boundary cell) must FAIL; the no-partial-read cell
#                    must still PASS.
#   unbounded-pool   MUTATION: the client's refresh budget is sized 1024 instead
#                    of the configured share, so the request layer is bounded by
#                    nothing. Cell 2 must FAIL; the boundary cell must still PASS.
#   unbounded-launch-governor
#                    MUTATION: the manager launches as many refreshes as the queue
#                    holds, ignoring the governor. Cell 2 must FAIL; the boundary
#                    cell must still PASS.
#   stop-lets-in-flight-finish
#                    MUTATION: neither the in-flight checkpoint nor the stop's
#                    cancel observes the stop, i.e. STOP IN FULL becomes "let
#                    in-flight finish" — unbounded in time. Cell 3 (and BFS-046's
#                    stop cell) must FAIL; the boundary cell must still PASS.
#   promotion-noop   MUTATION: a read no longer promotes a queued item, so the
#                    queue and the direct call both run the same path. BFS-046's
#                    exactly-one-fetch cell must FAIL; the boundary cell must
#                    still PASS.
#   stream-into-published
#                    MUTATION: the refresh writes the bytes it has so far into the
#                    blob a READER can reach, mid-body — the in-place refresh.
#                    The no-partial-read cell must FAIL; BFS-046's boundary cell
#                    must still PASS.
#   feature-off-ignored
#                    MUTATION: the manager is built even when the policy is off,
#                    so "off" is not off. Cell 6 must FAIL; the boundary cell
#                    must PASS.
#   counter-cannot-move
#                    MUTATION: the refusal that a stop causes is no longer
#                    counted (BFS-032's shape: a displayed counter with a
#                    pre-filter above it). The census arms must FAIL; an
#                    unrelated census arm must still PASS.
#   all              green, then every mutation.
#
# usage: sh docs/evidence/BFS-037-arms.sh <mode>

set -u

mode="${1:-}"

usage() {
	printf 'usage: %s <green|exclusive-size|unbounded-pool|unbounded-launch-governor|stop-lets-in-flight-finish|promotion-noop|stream-into-published|feature-off-ignored|counter-cannot-move|all>\n' "$0" >&2
	exit 2
}

case "$mode" in
green | exclusive-size | unbounded-pool | unbounded-launch-governor | stop-lets-in-flight-finish | promotion-noop | stream-into-published | feature-off-ignored | counter-cannot-move | all) ;;
*) usage ;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

fsclient_pkg=./internal/fsclient
cache_go=internal/fsclient/cache.go
hotcache_go=internal/fsclient/hotcache.go
hotrefresh_go=internal/fsclient/hotrefresh.go

# The pre-mutation hashes of the files every mutation touches, re-checked after
# every restore: a restore that does not land is a broken run, not a footnote.
hash_cache_go=bf3e978d446610c7ddb11866fc3ed1948ffd33137758d7035ceed468b47ab016
hash_hotcache_go=212d0c11082fb42c31a5a9caf6255974097b2a34a4973164499c2aaa780f84db
hash_hotrefresh_go=76151e867db9a7ff2d45c7888db15bd37a91bbeba2f6c5c1e14aa782bd62b88c

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
		"$cache_go") want="$hash_cache_go" ;;
		"$hotcache_go") want="$hash_hotcache_go" ;;
		"$hotrefresh_go") want="$hash_hotrefresh_go" ;;
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
	say "--- $label: go test $pkg -run $filter -count=1 -timeout 600s"
	go test "$pkg" -run "$filter" -count=1 -timeout 600s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

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

mutation() { # <name> <file> <patch> <target pkg> <target filter> <attr pkg> <attr filter>
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

say "BFS-037 arms — mode=$mode"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

rc_all=0

do_green() {
	say "tree under test:"
	say "  $hotcache_go   sha256=$(hash "$hotcache_go")"
	say "  $hotrefresh_go sha256=$(hash "$hotrefresh_go")"
	say "  $cache_go      sha256=$(hash "$cache_go")"
	expect_runs pass "GREEN (BFS-037: the twelve cells, the census and the probes)" "$fsclient_pkg" 'TestBFS037' || rc_all=1
	expect_runs pass "GREEN (BFS-046: every cell, including the four completed for 037)" "$fsclient_pkg" 'TestBFS046' || rc_all=1
}

arm_exclusive_size() {
	mutation exclusive-size "$hotcache_go" "$here/BFS-037-redproof-exclusive-size.patch" \
		"$fsclient_pkg" 'TestBFS037Cell01|TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason' \
		"$fsclient_pkg" 'TestBFS037Cell05NoReaderEverSeesAPartialFile' || rc_all=1
}

arm_unbounded_pool() {
	mutation unbounded-pool "$hotrefresh_go" "$here/BFS-037-redproof-unbounded-refresh-pool.patch" \
		"$fsclient_pkg" 'TestBFS037Cell02TheRefreshPoolIsBoundedAndYields' \
		"$fsclient_pkg" 'TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary' || rc_all=1
}

arm_unbounded_launch() {
	mutation unbounded-launch-governor "$hotrefresh_go" "$here/BFS-037-redproof-unbounded-launch-governor.patch" \
		"$fsclient_pkg" 'TestBFS037Cell02TheRefreshPoolIsBoundedAndYields' \
		"$fsclient_pkg" 'TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary' || rc_all=1
}

arm_stop() {
	mutation stop-lets-in-flight-finish "$hotrefresh_go" "$here/BFS-037-redproof-stop-never-abandons-in-flight.patch" \
		"$fsclient_pkg" 'TestBFS037Cell03StopInFullWhileItemsAreInFlight|TestBFS046Cell06StopInFullRefusesTheTwoWrongReadings' \
		"$fsclient_pkg" 'TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary' || rc_all=1
}

arm_promotion() {
	mutation promotion-noop "$hotrefresh_go" "$here/BFS-037-redproof-promotion-is-a-noop.patch" \
		"$fsclient_pkg" 'TestBFS046Cell07PromotionIsSingleFlightExactlyOneFetch' \
		"$fsclient_pkg" 'TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary' || rc_all=1
}

arm_partial() {
	mutation stream-into-published "$hotrefresh_go" "$here/BFS-037-redproof-stream-into-published.patch" \
		"$fsclient_pkg" 'TestBFS037Cell05NoReaderEverSeesAPartialFile' \
		"$fsclient_pkg" 'TestBFS046Cell09SizeRuleBoundaryIsInclusiveAndCountedByReason' || rc_all=1
}

arm_feature_off() {
	mutation feature-off-ignored "$hotrefresh_go" "$here/BFS-037-redproof-feature-off-ignored.patch" \
		"$fsclient_pkg" 'TestBFS037Cell06WithTheFeatureOffTheClientIsCorrect' \
		"$fsclient_pkg" 'TestBFS037Cell01TheSizeRuleHoldsAtTheBoundary' || rc_all=1
}

arm_counter() {
	mutation counter-cannot-move "$hotrefresh_go" "$here/BFS-037-redproof-counter-cannot-move.patch" \
		"$fsclient_pkg" 'TestBFS037Cell08EverySkipAndAbandonReasonIsReachable/skip_stopped|TestBFS046Cell09SkipCensusByReasonIsDrivable' \
		"$fsclient_pkg" 'TestBFS037Cell08EverySkipAndAbandonReasonIsReachable/skip_untracked' || rc_all=1
}

do_mutations() {
	arm_exclusive_size
	arm_unbounded_pool
	arm_unbounded_launch
	arm_stop
	arm_promotion
	arm_partial
	arm_feature_off
	arm_counter
}

case "$mode" in
green) do_green ;;
exclusive-size) arm_exclusive_size ;;
unbounded-pool) arm_unbounded_pool ;;
unbounded-launch-governor) arm_unbounded_launch ;;
stop-lets-in-flight-finish) arm_stop ;;
promotion-noop) arm_promotion ;;
stream-into-published) arm_partial ;;
feature-off-ignored) arm_feature_off ;;
counter-cannot-move) arm_counter ;;
all)
	do_green
	do_mutations
	;;
esac

say ""
if [ "$rc_all" = 0 ]; then
	say "ARMS: every declared outcome held (mode=$mode)"
else
	say "ARMS: at least one declared outcome did NOT hold (mode=$mode)"
fi
exit "$rc_all"
