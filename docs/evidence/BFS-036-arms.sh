#!/bin/sh
# BFS-036 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE (BFS-046's, applied to this row): a test that cannot fail proves
# nothing. Each mutation below neuters the mechanism ONE cell claims to catch,
# that cell must then go RED, and the file is restored from a byte copy whose
# sha256 is re-checked afterwards — a restore that does not land aborts the run.
# Every mutation also names a cell that must STAY GREEN, because a mutation that
# reddens everything has not been shown to hit the mechanism under test.
#
# HOW A MUTATION IS APPLIED. By an exact `sed` substitution over the committed
# file, and the script ASSERTS that the substitution landed (the file's hash must
# have changed) before it runs anything. That assertion is the point: a patch
# whose hunk happens to apply while changing nothing would otherwise be evidence
# of nothing, and with eight mutations the assertion is what makes the mechanics
# safe rather than the hunk arithmetic.
#
# modes:
#   green                 the tree as committed: every BFS-036 cell must PASS.
#   probe-not-declared    MUTATION: the client stops reading the declared mode, so
#                         the handshake probes the stream op. C-1 must FAIL; C-2
#                         (a pushed change is applied) must still PASS.
#   no-fanout             MUTATION: the ledger's push funnel stops broadcasting.
#                         P-1 must FAIL; P-6 (the heartbeat bound) must still PASS.
#   no-heartbeat          MUTATION: the tree's clock is replaced by one that never
#                         fires. P-6 must FAIL; P-1 must still PASS.
#   no-gap-marker         MUTATION: the owed gap marker is never declared, so a
#                         drop is silent. P-4 must FAIL; P-1 must still PASS.
#   no-write-deadline     MUTATION: no deadline is set on a line's write, so a
#                         client that never reads cannot be found. P-3 must FAIL;
#                         P-2 (a client that CLOSES its connection) must still PASS
#                         — the context is an independent detector.
#   cap-is-degradation    MUTATION: the subscriber cap answers 501
#                         capability_unavailable instead of a retryable 429.
#                         P-5 must FAIL; P-1 must still PASS.
#   silent-gap-resume     MUTATION: a cursor that falls off the end of the journal
#                         is answered with the retained tail instead of the
#                         overflow marker. P-8 must FAIL; P-1 must still PASS.
#   no-commit-flush       MUTATION: the channel's commitment is not flushed, so
#                         nothing leaves the server until the first line exists.
#                         P-10 must FAIL; P-1 (delivery) must still PASS — the
#                         property is the COMMIT, not the delivery.
#   mechanism-lie         MUTATION: the poll records the WATCH mechanism for an
#                         answer the poll delivered. C-3 (the honesty control) must
#                         FAIL; C-2 must still PASS.
#   all                   green, then every mutation, then the restore census.
#
# usage: sh docs/evidence/BFS-036-arms.sh <mode>

set -u

mode="${1:-}"
case "$mode" in
green | probe-not-declared | no-fanout | no-heartbeat | no-gap-marker | no-write-deadline | cap-is-degradation | silent-gap-resume | no-commit-flush | mechanism-lie | all) ;;
*)
	printf 'usage: %s <green|probe-not-declared|no-fanout|no-heartbeat|no-gap-marker|no-write-deadline|cap-is-degradation|silent-gap-resume|no-commit-flush|mechanism-lie|all>\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

push_go=internal/server/webdav/push.go
events_go=internal/server/webdav/events.go
caps_go=internal/fsclient/capabilities.go
invalidate_go=internal/fsclient/invalidate.go

# The committed hashes of the four files every mutation touches, re-checked after
# every restore. A restore that does not land is a broken run, not a footnote.
hash_push_go=f63d5d875760d470e9bfe793c21151d15ef9bfbd2ee7daf9904a1cc06d3dacf7
hash_events_go=dee8b78910c2a9e063e97fd2d03a2a33e5e5de1b019c5c57bb73884c746639b8
hash_caps_go=d138795e131af7e24195858123d6772c929f065a87488fcf43bf3fc8b36078a4
hash_invalidate_go=e7575efebe770996edbb23f4ade8d363e2e54152faae51e191017efa85bf7621

webdav_pkg=./internal/server/webdav
fsclient_pkg=./internal/fsclient
# The two halves of the program, as one regex each.
server_cells='TestPushCell0[1-9]|TestPushCell10'
client_cells='TestPushClient'

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_file=""
bak_path=""

backup() {
	bak_path="$1"
	bak_file=$(mktemp -t bfs036-bak.XXXXXX) || exit 2
	cp "$bak_path" "$bak_file" || exit 2
}

restore() {
	[ -z "$bak_path" ] && return 0
	cp "$bak_file" "$bak_path" || exit 2
	rm -f "$bak_file"
	bak_file=""
	bak_path=""
}

# EVERY exit path restores. An arm that fails its own "did the cell go RED"
# assertion must not leave the tree mutated: the first run of this script taught
# exactly that (a failed arm left the mutation in place, and the next run's GREEN
# then failed against a mutated tree, which reads as a broken row rather than as a
# broken script).
trap restore EXIT INT TERM

# mutate <path> <committed-hash> <sed-expr>: the substitution must LAND, or the
# arm is not evidence about anything.
mutate() {
	path="$1"
	want="$2"
	expr="$3"
	if [ "$(hash "$path")" != "$want" ]; then
		say "REFUSING TO MUTATE: $path is not at its committed hash ($want). A mutation against an unknown tree is not evidence."
		exit 2
	fi
	sed -i "$expr" "$path" || exit 2
	if [ "$(hash "$path")" = "$want" ]; then
		say "MUTATION DID NOT LAND in $path: $expr — an arm that changes nothing proves nothing"
		exit 2
	fi
	if ! go build ./... >/tmp/bfs036-arm-build.log 2>&1; then
		say "THE MUTATION DOES NOT COMPILE, so a red would be a build failure and not evidence:"
		cat /tmp/bfs036-arm-build.log
		exit 2
	fi
	say "mutation landed and compiles: $path"
}

verify_all() {
	fail=0
	for pair in "$push_go:$hash_push_go" "$events_go:$hash_events_go" "$caps_go:$hash_caps_go" "$invalidate_go:$hash_invalidate_go"; do
		path=${pair%%:*}
		want=${pair#*:}
		got=$(hash "$path")
		if [ "$got" != "$want" ]; then
			say "RESTORE FAILED: $path is $got, want $want"
			fail=1
		fi
	done
	return $fail
}

# must_fail <cell-regex> <package>: the named cells must FAIL.
#
# A NON-ZERO EXIT IS NOT A RED. A build failure, a run killed by the harness, and
# a run that hit its own timeout all exit non-zero and none of them is evidence
# that the cell catches anything — BFS-017's law ("a timeout is not a test
# failure") applied to this row's own arms. The log must carry a real test
# FAILURE, and the first run of this script is what made that concrete: a
# suppressed-line mutation left the cell BLOCKED on a header that was never
# flushed, so the arm HUNG, the timeout killed it, and "exit != 0" would have
# recorded that hang as a red.
must_fail() {
	rx="$1"
	pkg="$2"
	if go test "$pkg" -count=1 -timeout 600s -run "$rx" >/tmp/bfs036-arm-red.log 2>&1; then
		say "CELL DID NOT GO RED under the mutation: $rx in $pkg — the cell cannot fail, so it proves nothing"
		cat /tmp/bfs036-arm-red.log
		return 1
	fi
	if ! grep -qE '^(---|    ---) FAIL' /tmp/bfs036-arm-red.log; then
		say "NOT A RED: $rx in $pkg exited non-zero WITHOUT a test failure. A build error, a killed run or a timeout is not evidence that the cell catches anything:"
		tail -20 /tmp/bfs036-arm-red.log | sed 's/^/    /'
		return 1
	fi
	say "RED as claimed: $rx"
	grep -E '^(---|    ---) FAIL' /tmp/bfs036-arm-red.log | head -12 | sed 's/^/    /'
	return 0
}

# must_pass <cell-regex> <package>: the attribution control — these cells are not
# about the mutated mechanism and must stay green. A SKIP is not a green: a
# control that skipped because the mutation removed its own precondition is a
# control that passed by not running (the reason every gate in the cells reads the
# DECLARED MODE rather than the function under test).
must_pass() {
	rx="$1"
	pkg="$2"
	if ! go test "$pkg" -count=1 -timeout 600s -v -run "$rx" >/tmp/bfs036-arm-green.log 2>&1; then
		say "ATTRIBUTION CONTROL FAILED: $rx in $pkg went RED, so the mutation is not specific to the mechanism under test"
		cat /tmp/bfs036-arm-green.log
		return 1
	fi
	if grep -qE '^(---|    ---) SKIP' /tmp/bfs036-arm-green.log; then
		say "ATTRIBUTION CONTROL SKIPPED: $rx in $pkg did not run, so it is not evidence that the mutation left the mechanism alone"
		grep -E '^(---|    ---) SKIP' /tmp/bfs036-arm-green.log | sed 's/^/    /'
		return 1
	fi
	if ! grep -qE '^(---|    ---) PASS' /tmp/bfs036-arm-green.log; then
		say "ATTRIBUTION CONTROL DID NOT RUN: no cell matched $rx in $pkg"
		return 1
	fi
	say "attribution control green: $rx"
	return 0
}

green_run() {
	say "=== GREEN: every BFS-036 cell, on the tree as committed ==="
	go test "$webdav_pkg" -count=1 -timeout 900s -v -run "$server_cells" >"$here/BFS-036-green-server.txt" 2>&1 || {
		cat "$here/BFS-036-green-server.txt"
		return 1
	}
	grep -E '^(---|    ---) (PASS|FAIL)' "$here/BFS-036-green-server.txt" | sed 's/^/  /'
	go test "$fsclient_pkg" -count=1 -timeout 900s -v -run "$client_cells" >"$here/BFS-036-green-client.txt" 2>&1 || {
		cat "$here/BFS-036-green-client.txt"
		return 1
	}
	grep -E '^(---|    ---) (PASS|FAIL)' "$here/BFS-036-green-client.txt" | sed 's/^/  /'
	# And the landed arms this row sits on top of: BFS-060/061/062/063's cells are
	# the ones a resume-point change can break, and they are re-run here rather
	# than assumed.
	go test "$fsclient_pkg" -count=1 -timeout 900s -run 'TestBFS06' >"$here/BFS-036-green-bfs06x.txt" 2>&1 || {
		cat "$here/BFS-036-green-bfs06x.txt"
		return 1
	}
	say "  $(tail -1 "$here/BFS-036-green-bfs06x.txt")"
	say "GREEN: both halves passed, and the landed BFS-060..063 cells still pass"
}

case "$mode" in
green) green_run ;;
all)
	if ! green_run; then
		say "GREEN FAILED: refusing to run mutations against a tree whose own cells are red"
		exit 1
	fi
	for m in probe-not-declared no-fanout no-heartbeat no-gap-marker no-write-deadline cap-is-degradation silent-gap-resume no-commit-flush mechanism-lie; do
		say ""
		say "#################### $m ####################"
		sh "$0" "$m" || exit 1
	done
	say ""
	if verify_all; then
		say "ALL ARMS RUN: every claim backed by a RED, every restore sha256-verified"
	else
		say "RESTORE VERIFICATION FAILED"
		exit 1
	fi
	;;
probe-not-declared)
	backup "$caps_go"
	mutate "$caps_go" "$hash_caps_go" 's|return caps.WatchMode() == ModePush|return false /* MUTATION: stop reading the declared mode */|'
	must_fail 'TestPushClientSwitchReadsTheDeclarationNotAStreamProbe' "$fsclient_pkg" || exit 1
	# Attribution: the DELIVERY path must be untouched. This cell is not gated on
	# the handshake's verdict (Run consults the mode, not the availability), so a
	# mutation to the availability carrier must leave it green — which is what makes
	# the arm specific to the SWITCH rather than to the client as a whole.
	must_pass 'TestPushClientAppliesAPushedChangeAndReportsTheWatchMechanism' "$fsclient_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
no-fanout)
	backup "$events_go"
	mutate "$events_go" "$hash_events_go" 's|^	t.broadcast(ev)$|	_ = ev /* MUTATION: the funnel stops broadcasting */|'
	must_fail 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	# The attribution control is the cell that never attaches a subscriber: the
	# mutation removes DELIVERY, and the ledger's own decisions must be untouched.
	must_pass 'TestPushCell08ResumeNeverAnswersASilentGap' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
no-heartbeat)
	backup "$push_go"
	mutate "$push_go" "$hash_push_go" 's|t := time.NewTicker(h.cfg.heartbeat)|t := time.NewTicker(24 * time.Hour) /* MUTATION: the clock never speaks */|'
	must_fail 'TestPushCell06TheDeclaredHeartbeatBoundIsHonoured' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
no-gap-marker)
	backup "$push_go"
	mutate "$push_go" "$hash_push_go" 's|^	if s.pending {$|	if false { /* MUTATION: the owed gap marker is never declared */|'
	must_fail 'TestPushCell04BackpressureDropsOldestAndCountsOneGap' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
no-write-deadline)
	backup "$push_go"
	mutate "$push_go" "$hash_push_go" 's|^	deadline := hub.cfg.writeDeadline$|	deadline := 100 * time.Hour /* MUTATION: the deadline is armed but can never fire, so the declared bound is not honoured */|'
	must_fail 'TestPushCell03StalledReaderIsReleasedByTheWriteDeadline' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell02DeadClientIsReleasedAndItsSlotComesBack' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
cap-is-degradation)
	backup "$push_go"
	mutate "$push_go" "$hash_push_go" 's|^\t\tw.Header().Set("Retry-After", "1")$|\t\th.writeEnvelope(w, r, start, "watch", 501, VerdictCapabilityUnavailable, false, nil, \&envelopeError{Capability: "watch", Scope: "target", Mode: "poll", Detail: "MUTATION: the cap dressed as a capability degradation"}); return|'
	must_fail 'TestPushCell05SubscriberCapRefusesRetryably' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
silent-gap-resume)
	backup "$events_go"
	mutate "$events_go" "$hash_events_go" 's|^\t\treturn \[\]eventLine{l.overflow(t, p)}$|\t\tif !p.Baseline { return []eventLine{l.overflow(t, p)} } /* MUTATION: a cursor of 0 on a rotated journal falls through to the tail */|'
	must_fail 'TestPushCell08ResumeNeverAnswersASilentGap' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
no-commit-flush)
	backup "$push_go"
	mutate "$push_go" "$hash_push_go" 's|^\tif err := rc.Flush(); err != nil {$|\tif err := error(nil); err != nil { /* MUTATION: the commitment is not flushed */|'
	must_fail 'TestPushCell10TheChannelIsCommittedBeforeAnyLineIsWaited' "$webdav_pkg" || exit 1
	must_pass 'TestPushCell01AChangeIsPushedWithTheMechanismOnTheWire' "$webdav_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
mechanism-lie)
	backup "$invalidate_go"
	mutate "$invalidate_go" "$hash_invalidate_go" 's|^\ti.mechanism = MechanismEvents$|\ti.mechanism = MechanismWatch /* MUTATION: the record claims the watcher for a poll answer */|'
	must_fail 'TestPushClientNeverReportsWatchWhenThePollDelivered' "$fsclient_pkg" || exit 1
	must_pass 'TestPushClientAppliesAPushedChangeAndReportsTheWatchMechanism' "$fsclient_pkg" || exit 1
	restore
	verify_all || exit 1
	;;
esac
