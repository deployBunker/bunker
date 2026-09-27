#!/bin/sh
# BFS-062 — the arms, the RED, and the control that proves the cell can fail.
#
# Four modes, one script, so every claim in the evidence bundle is produced by
# the same files:
#
#   green      the fixed tree: every BFS-062 arm must PASS — the consumer arms in
#              internal/fsclient and the producer arms in internal/server/webdav.
#   unfixed    the tree AS FILED: the product files this row changed are replaced
#              by the blobs of the base commit (default 064b39f, the commit the
#              row was filed on) and the file this row ADDED is set aside, so the
#              package under test is the filed code. The two behaviour RED arms
#              must FAIL and the non-vacuity control arm must still PASS, which is
#              what attributes the failures to this row's defect rather than to
#              the harness. The GREEN-only files (which assert surface this row
#              ADDS and so cannot compile against the filed tree) are set aside.
#   noclassify negative control: the frame classification is neutered by patch on
#              the FIXED tree — bufio.ErrTooLong falls back to the transport
#              classifier again, which is the filed defect and nothing else. The
#              RED arm must go FAIL; the non-vacuity control must still PASS.
#              The mutation is applied to internal/fsclient/framelimit.go and
#              restored from a byte copy whose sha256 is re-checked after the
#              copy back: a restore that does not match aborts the run.
#   suites     the whole tree's gates on the fixed tree: gofmt -l, go build,
#              go vet, go test ./... — the row's acceptance cannot rest on the two
#              packages it happens to touch.
#
# usage: sh docs/evidence/BFS-062-arms.sh <green|unfixed|noclassify|suites> [base-ref]

set -u

mode="${1:-}"
base="${2:-064b39f}"

case "$mode" in
green | unfixed | noclassify | suites) ;;
*)
	printf 'usage: %s <green|unfixed|noclassify|suites> [base-ref]\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

# The product files this row CHANGED. In `unfixed` mode each is replaced by the
# base blob and the replacement is asserted to differ from the fixed tree (a swap
# that came back byte-identical would not be the filed code at all).
prod_changed="internal/fsclient/invalidate.go internal/fsclient/errors.go internal/fsclient/capabilities.go internal/server/webdav/events.go internal/server/webdav/watch.go internal/server/webdav/handler.go internal/server/webdav/tree.go internal/server/webdav/invalidation_config.go"

# The product file this row ADDED. It has no base blob, so `unfixed` sets it
# aside instead of swapping it.
prod_added="internal/fsclient/framelimit.go"

# GREEN-only test files: they assert fields/types this row adds, so they cannot
# compile — let alone pass — against the filed tree.
green_only="internal/fsclient/invalidate_bfs062_rule_test.go internal/fsclient/status_census_test.go internal/server/webdav/events_bfs062_test.go"

control_patch="$here/BFS-062-negative-control-classify.patch"
control_file="$prod_added"

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }
bak() { printf '/tmp/bfs062-bak-%s\n' "$(echo "$1" | tr '/' '-')"; }

keep=""
aside=""
pins=""
before_control=""

restore() {
	status=0
	for f in $keep; do
		orig=${f%%|*}
		copy=${f#*|}
		cp "$copy" "$orig"
		now=$(hash "$orig")
		want=$(printf '%s' "$pins" | grep " $orig\$" | cut -d' ' -f1)
		if [ -n "$want" ] && [ "$now" != "$want" ]; then
			say "RESTORE FAILED: $orig sha256=$now, expected $want"
			status=1
		else
			say "restore: $orig sha256=$now (verified against its pre-mode hash)"
		fi
	done
	for f in $aside; do
		if [ -f "$f.setaside" ]; then
			mv "$f.setaside" "$f"
			say "restore: $f (moved back from .setaside)"
		fi
	done
	if [ -f "$control_file.mutant" ]; then
		cp "$control_file.mutant" "$control_file"
		now=$(hash "$control_file")
		if [ "$now" != "$before_control" ]; then
			say "RESTORE FAILED: $control_file sha256=$now, expected $before_control"
			status=1
		else
			say "restore: $control_file sha256=$now (verified against the pre-mutation hash)"
		fi
		rm -f "$control_file.mutant"
	fi
	return $status
}

trap 'restore || exit 3' EXIT

stash() {
	for f in $1; do
		now=$(hash "$f")
		pins="$pins$now $f
"
		cp "$f" "$(bak "$f")"
		keep="$keep$f|$(bak "$f")
"
		say "pre-swap: $f sha256=$now"
	done
}

run_fsclient() {
	label="$1"
	filter="$2"
	say "--- $label: go test ./internal/fsclient -run $filter -count=1 -v -timeout 300s"
	go test ./internal/fsclient -run "$filter" -count=1 -v -timeout 300s
	rc=$?
	say "--- $label exit=$rc"
	return $rc
}

say "BFS-062 arms — mode=$mode base=$base"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "$mode" in
green)
	for f in $prod_changed $prod_added; do
		say "tree under test: $f sha256=$(hash "$f")"
	done
	run_fsclient GREEN 'TestBFS062'
	say "--- green (producer): go test ./internal/server/webdav -run TestBFS062 -count=1 -v -timeout 300s"
	go test ./internal/server/webdav -run 'TestBFS062' -count=1 -v -timeout 300s
	say "--- producer arms exit=$?"
	;;

unfixed)
	# The file this row ADDED has no base blob: it is set aside so the package
	# under test is the filed code, and moved back by the restore trap.
	mv "$prod_added" "$prod_added.setaside"
	aside="$aside$prod_added
"
	say "set aside (added by this row, no base blob): $prod_added"
	stash "$prod_changed"
	for f in $prod_changed; do
		git show "$base:$f" >"$f"
		now=$(hash "$f")
		fixed=$(printf '%s' "$pins" | grep " $f\$" | cut -d' ' -f1)
		if [ "$now" = "$fixed" ]; then
			say "SWAP UNVERIFIED: $base:$f is byte-identical to the fixed tree, so this is not the filed code"
			exit 4
		fi
		say "swapped in $base:$f sha256=$now (was $fixed)"
	done
	for f in $green_only; do
		mv "$f" "$f.setaside"
		aside="$aside$f
"
		say "set aside (GREEN-only): $f"
	done
	run_fsclient "RED (tree as filed): the frame-limit arm" 'TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried'
	run_fsclient "RED (tree as filed): the declaration arm" 'TestBFS062TheReaderFollowsTheDeclaration'
	run_fsclient "RED (tree as filed): non-vacuity control" 'TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames'
	;;

noclassify)
	before_control=$(hash "$control_file")
	cp "$control_file" "$control_file.mutant"
	say "pre-mutation: $control_file sha256=$before_control"
	git apply "$control_patch" || {
		say "the control patch did not apply"
		exit 5
	}
	say "mutated: $control_file sha256=$(hash "$control_file") via $(basename "$control_patch")"
	run_fsclient "CONTROL noclassify (this arm MUST fail)" 'TestBFS062ALegalOversizedFrameIsNamedCountedAndNotRetried'
	run_fsclient "CONTROL noclassify (attribution: this arm MUST still pass)" 'TestBFS062TheClassificationDoesNotFireOnOrdinaryFrames'
	;;

suites)
	say "--- gofmt -l internal cmd (must print nothing)"
	gofmt -l internal cmd
	say "--- gofmt exit=$?"
	say "--- go build ./..."
	go build ./...
	say "--- go build exit=$?"
	say "--- go vet ./internal/..."
	go vet ./internal/...
	say "--- go vet exit=$?"
	say "--- go test ./... -count=1"
	go test ./... -count=1
	say "--- go test exit=$?"
	;;
esac

restore
trap - EXIT
say "done: mode=$mode"
