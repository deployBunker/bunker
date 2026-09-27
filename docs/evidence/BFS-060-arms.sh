#!/bin/sh
# BFS-060 — the arms, and the controls that prove they can fail.
#
# Five modes, one script, so the RED, the GREEN and BOTH negative controls are
# produced by the same three files (one product file + two test files):
#
#   green     the fixed tree: every BFS-060 arm must PASS.
#   unfixed   the tree AS FILED: internal/fsclient/{invalidate,errors}.go are
#             replaced by the blobs of the base commit (default 5baef51, the
#             commit this row was filed on) and each replacement is sha256-checked
#             against the hash recorded below, so "the unfixed tree" is a named
#             artifact rather than a memory. The three BEHAVIOUR arms must FAIL.
#             The GREEN-only rule arm (which asserts the fields this row ADDS and
#             so cannot compile against the filed tree) is set aside for this mode
#             and restored at the end.
#   noeof     negative control 1 (control EOF): the EOF detection disabled by
#             patch. The EOF arm must FAIL; the stall arm must still PASS, which
#             is what attributes each fix to its own defect.
#   noidle    negative control 2 (control IDLE): the idle deadline armed but not
#             acted upon, i.e. the declared rule has no implementation again. The
#             stall arm must FAIL; the EOF arm must still PASS.
#   race      the whole fsclient package under -race on the fixed tree.
#
# Every mutation is applied to internal/fsclient/invalidate.go — the file this row
# owns — and restored from a byte copy whose sha256 is re-checked after the copy
# back. A restore that does not match aborts the run.
#
# usage: sh docs/evidence/BFS-060-arms.sh <green|unfixed|noeof|noidle|race> [base-ref]

set -u

mode="${1:-}"
base="${2:-5baef51}"

case "$mode" in
green | unfixed | noeof | noidle | race) ;;
*)
	printf 'usage: %s <green|unfixed|noeof|noidle|race> [base-ref]\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

prod=internal/fsclient/invalidate.go
prod_err=internal/fsclient/errors.go
rule=internal/fsclient/invalidate_bfs060_rule_test.go
arms_file=internal/fsclient/invalidate_bfs060_test.go
aside="$rule.setaside"

# The filed blobs of commit 5baef51 — the content the RED was measured on.
filed_prod=d54bbde1f2d7407c89c629feaa1c8badc8be6739661a00b5cc4787ef6def07b8
filed_err=e47d970008be14604027727111644c53e341862ed12aab73abc32f70e91866b4

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_prod=""
bak_err=""
before_prod=""
before_err=""
moved_rule=0

restore() {
	status=0
	if [ -n "$bak_prod" ]; then
		cp "$bak_prod" "$prod"
		now=$(hash "$prod")
		if [ "$now" != "$before_prod" ]; then
			say "RESTORE FAILED: $prod sha256=$now, expected $before_prod"
			status=1
		else
			say "restore: $prod sha256=$now (verified against the pre-mutation hash)"
		fi
	fi
	if [ -n "$bak_err" ]; then
		cp "$bak_err" "$prod_err"
		now=$(hash "$prod_err")
		if [ "$now" != "$before_err" ]; then
			say "RESTORE FAILED: $prod_err sha256=$now, expected $before_err"
			status=1
		else
			say "restore: $prod_err sha256=$now (verified against the pre-mutation hash)"
		fi
	fi
	if [ "$moved_rule" = 1 ] && [ -f "$aside" ]; then
		mv "$aside" "$rule"
		moved_rule=0
		say "restore: $rule (moved back)"
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

say "BFS-060 arms — mode=$mode base=$base"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"

case "$mode" in
green)
	say "tree under test: $prod sha256=$(hash "$prod")"
	say "               $prod_err sha256=$(hash "$prod_err")"
	say "               $rule sha256=$(hash "$rule")"
	say "               $arms_file sha256=$(hash "$arms_file")"
	run_arms GREEN 'TestBFS060'
	;;

unfixed)
	stash_prod
	git show "$base:$prod" >"$prod"
	filed=$(hash "$prod")
	say "swapped in $base:$prod sha256=$filed"
	if [ "$filed" != "$filed_prod" ]; then
		say "MISMATCH: expected the filed blob $filed_prod"
		exit 4
	fi
	before_err=$(hash "$prod_err")
	bak_err=$(mktemp)
	cp "$prod_err" "$bak_err"
	git show "$base:$prod_err" >"$prod_err"
	filede=$(hash "$prod_err")
	say "swapped in $base:$prod_err sha256=$filede"
	if [ "$filede" != "$filed_err" ]; then
		say "MISMATCH: expected the filed blob $filed_err"
		exit 4
	fi
	mv "$rule" "$aside"
	moved_rule=1
	say "set aside: $rule (GREEN-only arm: it asserts fields that do not exist on the filed tree)"
	run_arms "RED (tree as filed)" 'TestBFS060'
	;;

noeof | noidle)
	stash_prod
	if [ "$mode" = noeof ]; then
		patch_file="$here/BFS-060-negative-control-eof.patch"
		arm='TestBFS060CleanEOFIsAChannelEndNotASuccess'
		other='TestBFS060StalledChannelIsNotAQuietTree'
	else
		patch_file="$here/BFS-060-negative-control-idle.patch"
		arm='TestBFS060StalledChannelIsNotAQuietTree'
		other='TestBFS060CleanEOFIsAChannelEndNotASuccess'
	fi
	git apply "$patch_file"
	say "mutated: $prod sha256=$(hash "$prod") via $(basename "$patch_file")"
	run_arms "CONTROL $mode (mutant: this arm MUST fail)" "$arm"
	run_arms "CONTROL $mode (attribution: this arm MUST still pass)" "$other"
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
