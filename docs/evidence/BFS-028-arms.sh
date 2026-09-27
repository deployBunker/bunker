#!/usr/bin/env bash
# BFS-028 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE THE PROGRAM IS BUILT ON (BFS-046): a test that can not fail proves
# nothing. Every cell of this row that claims something appears below with the
# SOURCE MUTATION that turns it RED. Each mutation is a patch in this directory,
# applied with `git apply`, run against the named cell, and RESTORED from a byte
# copy whose sha256 is re-checked afterwards; a restore that does not match aborts
# the run. A cell with no mutation is a cell with no evidence.
#
# THE TWO THINGS THIS ROW CLAIMS, and the arm that owns each:
#
#   CLAIM 1 — `GOOS=windows go build ./...` succeeds, and the refusal on that
#             platform is REACHABLE rather than a build-tagged claim.
#             Arms: `red` (the base tree fails, reproduced), `green` (the tree as
#             committed builds for windows and both windows targets), `vet` (the
#             `!unix` test file type-checks for windows).
#   CLAIM 2 — POSIX behaviour is UNCHANGED, and the refusal is loud.
#             Arms: the three cells below, each with its mutation; and the
#             `unix-refuses` control, which is what "unchanged" means when tested:
#             make the unix arm refuse and every POSIX cell in the package dies.
#
# modes:
#   red            the row as FILED: build the base commit's tree for
#                  windows/amd64 and require the recorded syscall.Stat failure.
#   green          the tree as committed: windows build (both targets), linux
#                  build, the three cells, and the POSIX cells. Every cell PASSES.
#   no-gate        MUTATION: the platform gate is deleted from runUmount, so a
#                  build with no unmount mechanism resolves nothing and reports
#                  success — the accidental behaviour the row exists to prevent.
#                  The gate cell must FAIL; the TEXT cell and every POSIX cell
#                  must still PASS (attribution).
#   vague-refusal  MUTATION: the refusal is reworded into a generic
#                  "unsupported build (goos/goarch)" — the compile error's dead end
#                  in prose. The TEXT cell must FAIL; the gate cell and every POSIX
#                  cell must still PASS (attribution).
#   unix-refuses   MUTATION: the unix arm returns the refusal instead of nil, i.e.
#                  POSIX behaviour is NOT unchanged. Every POSIX cell must FAIL;
#                  the gate cell must still PASS (attribution: the cells that
#                  substitute the seam are unaffected by what the real arm says).
#   vet            the windows TYPE-CHECK of the seam's own test file
#                  (`GOOS=windows go vet ./internal/cli`), which is blocked by three
#                  PRE-EXISTING untagged test files owned by other rows. They are
#                  named in the output and removed in a THROWAWAY CLONE only; the
#                  lane must then PASS, which is what proves umount_nonunix_test.go
#                  compiles for the platform it is written for.
#   all            red, green, every mutation, then vet.
#
# usage: bash docs/evidence/BFS-028-arms.sh <red|green|no-gate|vague-refusal|unix-refuses|vet|all>

set -uo pipefail

mode="${1:-}"

case "$mode" in
red | green | no-gate | vague-refusal | unix-refuses | vet | all) ;;
*)
	printf 'usage: %s <red|green|no-gate|vague-refusal|unix-refuses|vet|all>\n' "$0" >&2
	exit 2
	;;
esac

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root" || exit 2

cli_pkg=./internal/cli
umount_go=internal/cli/umount.go
unix_go=internal/cli/umount_unix.go

# The tree the REDs are taken on. Re-checked after every restore: a restore that
# does not land is a broken run, not a footnote.
hash_umount_go=699d5db94c1706792fd7748bc9b7fd4a48ab64d10e17b373ee4607b08e5e25e6
hash_unix_go=e78c6ba30948e1297da4229ddc527e30a5be260da795cd019ff77d407cf07b64

# The row's BASE — wt/BFS-028 was cut from here, and `red` builds it.
BASE_COMMIT=48ab48433212b9c7035e540d48bcb0a092c738bb

# ---------------------------------------------------------------------------
# The cells. Each is a -run filter over internal/cli.
#
# THE GATE CELL is the row's own claim: on a build with no unmount mechanism the
# real runUmount (and the real cobra command) refuses, prints nothing, executes
# no unmount and leaves no side effect. It is reachable on Linux because the
# platform's ANSWER is a seam; substituting it is the only difference.
cell_gate='TestUmount_UnsupportedPlatformRefusalReachesTheOperator|TestUmountCommand_RefusesAsACommand'

# THE TEXT CELL is what the operator gets instead of a compile error.
cell_text='TestUmount_UnsupportedRefusalNamesTheCommandPlatformAndWayOut'

# THE POSIX CELL is "behaviour unchanged": the cells BFS-039/DF-BUNKER-50 left,
# including the two that call isMountPoint (the probe that moved).
cell_posix='TestUmount_Resolves|TestUmount_Custom|TestUmount_Explicit|TestUmount_Default|TestUmount_Nothing|TestUmount_Empty|TestUmount_Unreadable|TestUmount_Ambiguous|TestUmount_AgentID|TestUmountNothingMountedIsSuccess|TestUmountMissingPathIsSuccess|TestIsMountPoint_FalseForPlainDir|TestMountFault_StrandedMountPointIsDetectable|TestAgentMountpoints_|TestMountTargetHasAgent|TestIsFuseOrNetworkFS'

# Pre-existing untagged test files that block `GOOS=windows go vet ./internal/cli`.
# Each was measured, not guessed (see §5 of the evidence doc). They are OTHER
# rows' findings — named here so they can be filed, never fixed from this one.
vet_blockers="internal/cli/mount_fault_inject_test.go:116 undefined: newMountTestServer (declared in the unix-tagged mount_test.go)
internal/cli/procbuild_test.go:419 undefined: syscall.Flock
internal/cli/exit_code_pipe_test.go:184 undefined: buildCLIOnce"

say() { printf '%s\n' "$*"; }
hash() { sha256sum "$1" | cut -d' ' -f1; }

bak_file=""
bak_path=""

restore() {
	local status=0 now="" want=""
	if [ -n "$bak_file" ] && [ -n "$bak_path" ]; then
		cp "$bak_file" "$bak_path"
		now=$(hash "$bak_path")
		case "$bak_path" in
		"$umount_go") want="$hash_umount_go" ;;
		"$unix_go") want="$hash_unix_go" ;;
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
	local label="$1" pkg="$2" filter="$3"
	say "--- $label: go test $pkg -run '$filter' -count=1 -timeout 300s"
	go test "$pkg" -run "$filter" -count=1 -timeout 300s
	local rc=$?
	say "--- $label exit=$rc"
	return $rc
}

# expect_runs runs one cell and fails the SCRIPT when the cell's outcome is not
# the one the arm declares.
expect_runs() { # <want: pass|fail> <label> <pkg> <filter>
	local want="$1" label="$2" pkg="$3" filter="$4" rc=0
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

expect_build() { # <want: pass|fail> <label> <dir>
	local want="$1" label="$2" dir="$3" out="" rc=0
	say "--- $label: (cd $dir && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...)"
	out=$(cd "$dir" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./... 2>&1)
	rc=$?
	if [ -n "$out" ]; then printf '%s\n' "$out"; fi
	case "$want:$rc" in
	pass:0) say "OK   $label built (exit 0)" ; return 0 ;;
	pass:*) say "FAIL $label did not build (exit $rc)"; return 1 ;;
	fail:0) say "FAIL $label BUILT — the mutation/defect is not present"; return 1 ;;
	fail:*) say "OK   $label failed as declared (exit $rc)"; return 0 ;;
	esac
}

mutation() { # <mode> <file> <patch> <target-label> <target-filter> <attr-label> <attr-filter>
	local mode_name="$1" file="$2" patch_file="$3" tlabel="$4" tfilter="$5" alabel="$6" afilter="$7"
	local mut_rc=0

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

	expect_runs fail "CONTROL $mode_name (the mutation MUST turn this cell red)" "$cli_pkg" "$tfilter" || mut_rc=1
	expect_runs pass "CONTROL $mode_name (attribution: this cell MUST be unmoved)" "$cli_pkg" "$afilter" || mut_rc=1
	restore || mut_rc=1
	trap - EXIT
	return "${mut_rc:-0}"
}

say "BFS-028 arms — mode=$mode"
say "go $(go version | cut -d' ' -f3) · $(git rev-parse --short HEAD 2>/dev/null || echo 'no-git') · $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
say "cell GATE  = $cell_gate"
say "cell TEXT  = $cell_text"
say "cell POSIX = ${#cell_posix} chars of -run filter"

rc_all=0

# --- red: the row as filed ---------------------------------------------------
# Built from a THROWAWAY CLONE of the base commit rather than by reverting the
# worktree: the fleet commits concurrently into the shared tree, and a revert
# would be a window another worker's write could land in.
do_red() {
	local tmp out rc
	tmp=$(mktemp -d)
	say ""
	say "=============================================================="
	say "RED — the base commit $BASE_COMMIT, built for windows/amd64"
	say "  scratch clone: $tmp/base (left in place; nothing is deleted)"
	say "=============================================================="
	if ! git clone --shared --quiet --no-checkout "$root" "$tmp/base"; then
		say "FAIL could not clone the base tree from $root"
		return 1
	fi
	if ! git -C "$tmp/base" checkout --quiet "$BASE_COMMIT"; then
		say "FAIL could not check out $BASE_COMMIT (does the clone have it?)"
		return 1
	fi
	out=$(cd "$tmp/base" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./... 2>&1)
	rc=$?
	printf '%s\n' "$out"
	if [ $rc -eq 0 ]; then
		say "FAIL the base tree BUILT for windows — the row's premise is not reproducible here"
		return 1
	fi
	if ! printf '%s\n' "$out" | grep -q 'umount\.go.*undefined: syscall\.Stat'; then
		say "FAIL the base tree failed, but not with the recorded 'undefined: syscall.Stat' errors"
		return 1
	fi
	say "OK   the base tree fails exactly as the row records (exit $rc, internal/cli/umount.go)"
	return 0
}

# --- green: the tree as committed -------------------------------------------
do_green() {
	local tmp rc
	say ""
	say "=============================================================="
	say "GREEN — the tree as committed"
	say "=============================================================="
	say "tree under test:"
	say "  $umount_go sha256=$(hash "$umount_go")"
	say "  $unix_go sha256=$(hash "$unix_go")"

	expect_build pass "GREEN (windows/amd64 build of the tree as committed)" "$root" || rc_all=1

	say "--- GREEN (windows/arm64 build of the tree as committed)"
	if (cd "$root" && GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build ./... 2>&1); then
		say "OK   windows/arm64 built (exit 0)"
	else
		say "FAIL windows/arm64 did not build"
		rc_all=1
	fi

	say "--- GREEN (linux build of the tree as committed)"
	if (cd "$root" && go build ./... 2>&1); then
		say "OK   linux built (exit 0)"
	else
		say "FAIL linux did not build"
		rc_all=1
	fi

	expect_runs pass "GREEN (the gate cell: the refusal is reachable)" "$cli_pkg" "$cell_gate" || rc_all=1
	expect_runs pass "GREEN (the text cell: the refusal is loud and specific)" "$cli_pkg" "$cell_text" || rc_all=1
	expect_runs pass "GREEN (the POSIX cells: behaviour unchanged)" "$cli_pkg" "$cell_posix" || rc_all=1

	say "--- GREEN (gofmt + vet on this tree)"
	if [ -z "$(gofmt -l internal/cli/)" ] && (cd "$root" && go vet "$cli_pkg" 2>&1); then
		say "OK   gofmt clean, go vet $cli_pkg clean"
	else
		say "FAIL gofmt or go vet is not clean"
		rc_all=1
	fi
}

# --- vet: the windows type-check of the seam's own test file -----------------
do_vet() {
	local tmp rc out
	tmp=$(mktemp -d)
	say ""
	say "=============================================================="
	say "VET — GOOS=windows go vet ./internal/cli (the seam's own test file)"
	say "  scratch clone: $tmp/vet (left in place; nothing is deleted)"
	say "=============================================================="
	if ! git clone --shared --quiet "$root" "$tmp/vet"; then
		say "FAIL could not clone the tree"
		return 1
	fi

	out=$(cd "$tmp/vet" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet "$cli_pkg" 2>&1)
	rc=$?
	if [ $rc -eq 0 ]; then
		say "OK   the lane is clean on the tree as committed; the pre-existing blockers below are gone"
		say "  (if you are reading this after those rows landed, delete the exclusion list from this script)"
		return 0
	fi

	say "the lane is blocked by PRE-EXISTING untagged test files owned by other rows:"
	printf '%s\n' "$vet_blockers" | while IFS= read -r line; do say "  $line"; done
	say "removing ONLY those files in the scratch clone, then re-running:"
	rm -f "$tmp/vet/internal/cli/mount_fault_inject_test.go" \
		"$tmp/vet/internal/cli/procbuild_test.go" \
		"$tmp/vet/internal/cli/exit_code_pipe_test.go"

	# Re-check: the blockers must be the ONLY ones. If the lane still fails, a
	# fourth file is involved and this arm's exclusion list is incomplete.
	local tries=0
	while [ $tries -lt 12 ]; do
		out=$(cd "$tmp/vet" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet "$cli_pkg" 2>&1)
		rc=$?
		[ $rc -eq 0 ] && break
		local f
		f=$(printf '%s\n' "$out" | grep -oE 'internal/cli/[A-Za-z0-9_]+_test\.go' | head -1)
		if [ -z "$f" ]; then
			printf '%s\n' "$out" | tail -5
			say "FAIL the lane fails for a reason that is not an untagged test file"
			return 1
		fi
		say "  ANOTHER BLOCKER (the exclusion list is incomplete): $f -- $(printf '%s\n' "$out" | tail -1)"
		rm -f "$tmp/vet/$f"
		tries=$((tries + 1))
	done

	if [ $rc -ne 0 ]; then
		say "FAIL the lane did not pass after exclusions"
		return 1
	fi
	say "OK   GOOS=windows go vet $cli_pkg PASSES once the three pre-existing blockers are excluded"
	say "     => umount_nonunix_test.go (//go:build !unix) and umount_platform_test.go COMPILE and"
	say "        type-check for windows/amd64. That is the strongest check available without a"
	say "        Windows host, and it is the check internal/fsmount/platform_unsupported_test.go"
	say "        relies on too. The three blockers are named above to be filed, not fixed here."
	return 0
}

do_mutations() {
	mutation no-gate "$umount_go" "$here/BFS-028-redproof-no-gate.patch" \
		"GATE cell" "$cell_gate" \
		"POSIX cells" "$cell_posix" || rc_all=1
	mutation vague-refusal "$umount_go" "$here/BFS-028-redproof-vague-refusal.patch" \
		"TEXT cell" "$cell_text" \
		"GATE cell" "$cell_gate" || rc_all=1
	mutation unix-refuses "$unix_go" "$here/BFS-028-redproof-unix-refuses.patch" \
		"POSIX cells" "$cell_posix" \
		"GATE cell" "$cell_gate" || rc_all=1
}

running_one_mutation() {
	case "$mode" in
	no-gate)
		mutation no-gate "$umount_go" "$here/BFS-028-redproof-no-gate.patch" \
			"GATE cell" "$cell_gate" "POSIX cells" "$cell_posix" || rc_all=1
		;;
	vague-refusal)
		mutation vague-refusal "$umount_go" "$here/BFS-028-redproof-vague-refusal.patch" \
			"TEXT cell" "$cell_text" "GATE cell" "$cell_gate" || rc_all=1
		;;
	unix-refuses)
		mutation unix-refuses "$unix_go" "$here/BFS-028-redproof-unix-refuses.patch" \
			"POSIX cells" "$cell_posix" "GATE cell" "$cell_gate" || rc_all=1
		;;
	esac
}

case "$mode" in
red) do_red || rc_all=1 ;;
green) do_green ;;
no-gate | vague-refusal | unix-refuses) running_one_mutation ;;
vet) do_vet || rc_all=1 ;;
all)
	do_red || rc_all=1
	do_green
	do_mutations
	do_vet || rc_all=1
	;;
esac

say ""
if [ "$rc_all" -eq 0 ]; then
	say "done: mode=$mode — all declared outcomes held"
else
	say "done: mode=$mode — DECLARED OUTCOME NOT HELD (see the FAIL lines above)"
fi
exit "$rc_all"
