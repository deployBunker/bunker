#!/bin/sh
# BFS-039 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE (BFS-046's, kept): a test that cannot fail proves nothing. Every cell
# this row adds appears below with the SOURCE MUTATION that turns it RED. Each
# mutation is a patch in this directory, applied with `git apply`, run against
# the named cell, and RESTORED from a byte copy whose sha256 is re-checked
# afterwards; a restore that does not land aborts the run. Each mutation also
# names an ATTRIBUTION cell that must stay UNMOVED, so a red cell is attributable
# to the mechanism the mutation removed rather than to a broken build.
#
# modes:
#   green                 the tree as committed: every BFS-039 cell must PASS.
#   cancel-class          MUTATION: the caller's cancellation is not attributed
#                         to the caller, so it is classified as a transport
#                         fault again. Cell: the deliberate cancel must FAIL;
#                         the killed-reader cell must stay unmoved.
#   sticky-flush          MUTATION: a cancelled publication is recorded as a
#                         VERDICT (flushed + a permanent failure), so the retry
#                         EINTR asks for is impossible. Cell: the cancel/retry
#                         cell must FAIL; the retried-write cell must stay up.
#   named-buffer          MUTATION: the write buffer keeps its NAME, so a killed
#                         writer leaves an orphan in the cache directory. Cell:
#                         the killed-writer cell must FAIL; the lock-sensor
#                         control must stay unmoved.
#   closable-body         MUTATION: the PUT body is the buffer FILE again, which
#                         net/http closes on a failed round trip. Cell: the
#                         mid-request cancel arm must FAIL; the lock-sensor
#                         control must stay unmoved.
#   non-positional-write  MUTATION: the buffer write stops being POSITIONAL
#                         (WriteAt -> Write), so a retried write at the same
#                         offset appends twice — the NON-IDEMPOTENT control.
#                         Cell: the retried-write cell must FAIL; the lock-sensor
#                         control must stay unmoved.
#   removed-noop-rule     MUTATION: the surface's identical-content no-op is
#                         removed, so a retry with a correct base re-writes
#                         identical bytes and moves the mtime. Cell: the
#                         retried-write cell must FAIL; the mid-request cancel
#                         arm must stay unmoved.
#   blind-lock-sensor     MUTATION: the lock sensor stops looking for lock files,
#                         so its green reading means nothing. Cell: the sensor's
#                         own control must FAIL; the killed-writer cell must
#                         stay unmoved (it is what the sensor guards).
#   all                   green, then every mutation.
#
# usage: sh docs/evidence/BFS-039-arms.sh <mode>

set -u

mode="${1:-}"

case "$mode" in
green | cancel-class | sticky-flush | named-buffer | closable-body | non-positional-write | removed-noop-rule | blind-lock-sensor | all) ;;
*)
	printf 'usage: %s <green|cancel-class|sticky-flush|named-buffer|closable-body|non-positional-write|removed-noop-rule|blind-lock-sensor|all>\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

fsclient_pkg=./internal/fsclient
fsmount_pkg=./internal/fsmount

errors_go=internal/fsclient/errors.go
fs_linux_go=internal/fsmount/fs_linux.go
handler_go=internal/server/webdav/handler.go
cancel_test=internal/fsmount/bfs039_cancel_test.go

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_file=""
bak_path=""
want_hash=""

restore() {
	status=0
	if [ -n "$bak_file" ] && [ -n "$bak_path" ]; then
		cp "$bak_file" "$bak_path"
		now=$(hash "$bak_path")
		if [ "$now" != "$want_hash" ]; then
			say "RESTORE FAILED: $bak_path sha256=$now, expected $want_hash"
			status=1
		else
			say "restore: $bak_path sha256=$now (verified byte-identical)"
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
	want_hash=$(hash "$bak_path")
	bak_file=$(mktemp)
	cp "$bak_path" "$bak_file"
	say "pre-mutation: $bak_path sha256=$want_hash"
}

run_cell() {
	label="$1"
	pkg="$2"
	filter="$3"
	say "--- $label: go test $pkg -run $filter -count=1 -timeout 300s"
	go test "$pkg" -run "$filter" -count=1 -timeout 300s
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
	fail:0) say "FAIL $label PASSED but the mutation declares it must FAIL"
		return 1 ;;
	pass:*) say "FAIL $label did not pass (exit $rc)"
		return 1 ;;
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

say "BFS-039 arms — mode=$mode"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

rc_all=0

do_green() {
	say "tree under test:"
	say "  $errors_go      sha256=$(hash "$errors_go")"
	say "  $fs_linux_go    sha256=$(hash "$fs_linux_go")"
	say "  $handler_go     sha256=$(hash "$handler_go")"
	say "  $cancel_test    sha256=$(hash "$cancel_test")"
	expect_runs pass "GREEN (client: the cancel taxonomy and the completed BFS-046 cell 8)" "$fsclient_pkg" 'TestBFS039|TestBFS046Cell08|TestBFS046PendingUntilLandingGate' || rc_all=1
	expect_runs pass "GREEN (mount: kill, idempotence, cancel/retry, lock)" "$fsmount_pkg" 'TestBFS039' || rc_all=1
}

do_mutations() {
	mutation cancel-class "$errors_go" "$here/BFS-039-redproof-cancel-class.patch" \
		"$fsclient_pkg" 'TestBFS039DeliberateCancelIsDistinguishableFromFailure' \
		"$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' || rc_all=1
	mutation sticky-flush "$fs_linux_go" "$here/BFS-039-redproof-sticky-flush.patch" \
		"$fsmount_pkg" 'TestBFS039DeliberateCancelReturnsEINTRAndTheRetryLandsOnce' \
		"$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' || rc_all=1
	mutation named-buffer "$fs_linux_go" "$here/BFS-039-redproof-named-buffer.patch" \
		"$fsmount_pkg" 'TestBFS039KilledWriterLeavesNoResidueAndThePathUsable' \
		"$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1
	mutation closable-body "$fs_linux_go" "$here/BFS-039-redproof-closable-body.patch" \
		"$fsmount_pkg" 'TestBFS039CancelledPublicationDoesNotCloseTheBuffer' \
		"$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1
	mutation non-positional-write "$fs_linux_go" "$here/BFS-039-redproof-non-positional-write.patch" \
		"$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' \
		"$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1
	mutation removed-noop-rule "$handler_go" "$here/BFS-039-redproof-removed-noop-rule.patch" \
		"$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' \
		"$fsmount_pkg" 'TestBFS039CancelledPublicationDoesNotCloseTheBuffer' || rc_all=1
	mutation blind-lock-sensor "$cancel_test" "$here/BFS-039-redproof-blind-lock-sensor.patch" \
		"$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' \
		"$fsmount_pkg" 'TestBFS039KilledWriterLeavesNoResidueAndThePathUsable' || rc_all=1
}

case "$mode" in
green) do_green ;;
cancel-class | sticky-flush | named-buffer | closable-body | non-positional-write | removed-noop-rule | blind-lock-sensor)
	case "$mode" in
	cancel-class) mutation cancel-class "$errors_go" "$here/BFS-039-redproof-cancel-class.patch" "$fsclient_pkg" 'TestBFS039DeliberateCancelIsDistinguishableFromFailure' "$fsclient_pkg" 'TestBFS046Cell08KilledReaderLeavesTheBlobAndThePathIntact' || rc_all=1 ;;
	sticky-flush) mutation sticky-flush "$fs_linux_go" "$here/BFS-039-redproof-sticky-flush.patch" "$fsmount_pkg" 'TestBFS039DeliberateCancelReturnsEINTRAndTheRetryLandsOnce' "$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' || rc_all=1 ;;
	named-buffer) mutation named-buffer "$fs_linux_go" "$here/BFS-039-redproof-named-buffer.patch" "$fsmount_pkg" 'TestBFS039KilledWriterLeavesNoResidueAndThePathUsable' "$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1 ;;
	closable-body) mutation closable-body "$fs_linux_go" "$here/BFS-039-redproof-closable-body.patch" "$fsmount_pkg" 'TestBFS039CancelledPublicationDoesNotCloseTheBuffer' "$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1 ;;
	non-positional-write) mutation non-positional-write "$fs_linux_go" "$here/BFS-039-redproof-non-positional-write.patch" "$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' "$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' || rc_all=1 ;;
	removed-noop-rule) mutation removed-noop-rule "$handler_go" "$here/BFS-039-redproof-removed-noop-rule.patch" "$fsmount_pkg" 'TestBFS039RetriedWriteDoesNotApplyTwice' "$fsmount_pkg" 'TestBFS039CancelledPublicationDoesNotCloseTheBuffer' || rc_all=1 ;;
	blind-lock-sensor) mutation blind-lock-sensor "$cancel_test" "$here/BFS-039-redproof-blind-lock-sensor.patch" "$fsmount_pkg" 'TestBFS039LockSensorIsNotBlind' "$fsmount_pkg" 'TestBFS039KilledWriterLeavesNoResidueAndThePathUsable' || rc_all=1 ;;
	esac
	;;
all)
	do_green
	do_mutations
	;;
esac

say ""
if [ "$rc_all" -eq 0 ]; then
	say "BFS-039 arms: OK (every declared outcome matched)"
else
	say "BFS-039 arms: FAILED (an outcome did not match its declaration)"
fi
exit "$rc_all"
