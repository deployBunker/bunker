#!/usr/bin/env bash
# cross-GOOS-build.sh — the guard for the platform seam (BFS-010).
#
# WHY THIS EXISTS. internal/fsmount/fs_unsupported.go describes itself as "the
# platform seam (BFS-010)". It did not compile: it declared `func Mount` and
# `type Mount` in one scope, and exported a constructor (`Mount`) that no caller
# uses, while the function its callers do use (MountAt) was missing from the
# non-Linux build entirely. Nothing caught it, because every build and every test
# in this repo runs on Linux, where that file is not compiled at all. A seam that
# is never built for the platform it is written for is a claim, not a mechanism.
# This script builds the whole module for each platform the seam must hold on, so
# the next drift is a red run instead of a discovery during a Windows port.
#
# WHAT IT ASSERTS
#   1. every REQUIRED target builds `./...` clean, except for packages listed in
#      KNOWN_GAPS (below, printed loudly on every run);
#   2. the seam's own test file type-checks for every target
#      (`go vet ./internal/fsmount`). A !linux test can only ever be COMPILED, never
#      executed — there is no non-Linux host in this environment — so vet is the
#      strongest check available, and it is still enough to catch a redeclaration;
#   3. a required target that fails in any package NOT in KNOWN_GAPS is a hard
#      failure (exit 1).
#
# NOTE FOR THE NEXT PERSON WHO ADDS A SEAM INSIDE internal/cli (BFS-028). Item 2's
# vet lane is per-package and only covers internal/fsmount here, so a new seam in
# another package is NOT type-checked for windows by this script. Extending item 2 to
# `go vet ./internal/cli` is the right thing to do and is currently BLOCKED by three
# PRE-EXISTING untagged test files, which reference unix-only symbols (measured, not
# guessed):
#
#   internal/cli/mount_fault_inject_test.go:116  undefined: newMountTestServer  (declared in the //go:build unix mount_test.go)
#   internal/cli/procbuild_test.go:419           undefined: syscall.Flock
#   internal/cli/exit_code_pipe_test.go:184      undefined: buildCLIOnce
#
# Those are other rows' findings and are NOT fixed from this script. The lane passes
# the moment they are excluded, which is how BFS-028 verified that its own
# `//go:build !unix` test file compiles: see docs/evidence/BFS-028-arms.sh mode `vet`
# (it clones the tree, names the blockers, removes them in the CLONE only, and
# requires the lane to pass). Land those three tags, then point item 2 at internal/cli.
#
# AND ITEM 2 HAS BEEN RED FOR EVERY TARGET BUT ITS OWN — known, measured, NOT yet
# fixed (BFS-028 §9c, with the base-commit proof in
# docs/evidence/BFS-028-crossgos-guard-baseline.txt):
#
#   GOOS=windows go vet ./internal/fsmount
#   -> vet: internal/fsmount/platform_unsupported_test.go:110:5: invalid operation:
#      st != (fsclient.Status{}) (struct containing fsclient.CacheStats cannot be compared)
#
# fsclient.Status carries a map (BypassReasons, internal/fsclient/cache.go:256), so it
# is not comparable and the `!=` in that `//go:build !linux` file has never compiled.
# THE PRACTICAL CONSEQUENCE, and the reason this note is here: the lane that exists to
# catch exactly this class of defect has not been running, and this script therefore
# EXITS 1 ON EVERY RUN regardless of KNOWN_GAPS. Read the BUILD column for the ratchet
# and the VET column for item 2 — do not read the exit status as being about your
# change without checking which column moved.
#
# KNOWN_GAPS IS A RATCHET, NOT AN EXCUSE. A listed gap is printed every run with
# the site that owns it; removing a line means the gap is fixed, and adding one
# needs the same evidence a defect report does (file:line + the exact failing
# command). Silently ignoring a target is how this seam broke in the first place.
#
# Usage: probes/cross-GOOS-build.sh [--survey]
#   --survey  also build every GOOS/GOARCH in SURVEY and print the result as
#             INFORMATION for a port's cost estimate. Never affects the exit status.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

# The targets this repo must build for: the two released Linux platforms
# (RELEASE_PLATFORMS in the Makefile) plus the two Windows platforms the BFS-010
# decision names (the x64/ARM64 Windows that WinFsp supports).
REQUIRED="linux/amd64 linux/arm64 windows/amd64 windows/arm64"

# Targets where a required build is known NOT to reach the end, and the package
# that owns the fix. One entry per line: "<goos>/<goarch>|<import path>".
#
# EMPTY SINCE BFS-028, which is the second time this list has been the point of the
# ratchet rather than an excuse. Every run since BFS-010 carried:
#
#   windows/amd64|github.com/deployBunker/bunker/internal/cli
#   windows/arm64|github.com/deployBunker/bunker/internal/cli
#
# with the site named as internal/cli/umount.go:272 — `undefined: syscall.Stat_t`,
# the mountpoint probe's device-id compare, written inline and unchanged since. The
# row that owned it (BFS-028) moved the probe to internal/cli/umount_unix.go,
# decided what a platform WITHOUT it does (a named refusal that says the command,
# the platform and the way out — internal/cli/umount_nonunix.go), and recorded the
# evidence in docs/evidence/BFS-028-windows-umount-seam.md. Adding a line back needs
# the same evidence a defect report does.
KNOWN_GAPS=""

# Informational survey set: what "no FUSE binding off Linux" actually covers.
SURVEY="darwin/amd64 darwin/arm64 freebsd/amd64 openbsd/amd64 netbsd/amd64 solaris/amd64 illumos/amd64 plan9/amd64 js/wasm"

LOGDIR=$(mktemp -d -t crossgos.XXXXXX)
trap 'rm -rf "$LOGDIR"' EXIT

failing_packages() { grep '^# ' "$1" 2>/dev/null | sed 's/^# //' | sort -u; }

# known_gap_for <target> <package>
#
# NO PIPELINE, deliberately: `printf … | while … done | grep -q yes` looks right and
# is not — grep -q exits at the first match, the upstream stage takes SIGPIPE, and
# with `set -o pipefail` the whole pipeline reports failure. Measured while writing
# this script: the SAME target (windows/amd64) was reported as FAIL on one run and
# GAP on the next, twelve lines apart, because the race landed differently. A guard
# whose answer depends on which side of a SIGPIPE it lost is worse than no guard.
known_gap_for() {
  local t="$1" p="$2" line tt pp
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    tt="${line%%|*}"
    pp="${line#*|}"
    if [ "$tt" = "$t" ] && [ "$pp" = "$p" ]; then return 0; fi
  done <<<"$KNOWN_GAPS"
  return 1
}

# build_target <target> -> prints one status token; full log kept at $LOGDIR/build-<t>
build_target() {
  local target="$1" log="$LOGDIR/build-${1//\//-}"
  GOOS="${target%%/*}" GOARCH="${target##*/}" CGO_ENABLED=0 go build ./... >"$log" 2>&1
  local rc=$?
  [ $rc -eq 0 ] && { echo PASS; return 0; }
  local out="" unexpected=0 pkg
  while IFS= read -r pkg; do
    [ -z "$pkg" ] && continue
    if known_gap_for "$target" "$pkg"; then
      out="${out}GAP(${pkg##*/}) "
    else
      out="${out}FAIL(${pkg##*/}) "
      unexpected=1
    fi
  done < <(failing_packages "$log")
  [ -z "$out" ] && out="FAIL(unknown) "
  echo "$out"
  return $unexpected
}

# vet_seam <target> -> prints PASS, or REFUSED and keeps the log beside it
vet_seam() {
  local target="$1" log="$LOGDIR/vet-${1//\//-}"
  GOOS="${target%%/*}" GOARCH="${target##*/}" CGO_ENABLED=0 go vet ./internal/fsmount >"$log" 2>&1
  if [ $? -eq 0 ]; then rm -f "$log"; echo PASS; return 0; fi
  echo REFUSED; return 1
}

echo "cross-GOOS platform-seam guard (BFS-010)"
echo "go               : $(go version)"
echo "required targets : $REQUIRED"
echo "known gaps (fixed by a named row, printed every run):"
if [ -z "${KNOWN_GAPS//[[:space:]]/}" ]; then
  echo "  (none — every REQUIRED target builds ./... to the end)"
else
  while IFS= read -r gap; do
    [ -z "$gap" ] && continue
    echo "  ${gap%%|*} -> ${gap#*|}"
  done <<<"$KNOWN_GAPS"
fi
echo ""
printf '%-16s %-10s %-24s %s\n' TARGET BUILD "VET internal/fsmount" RESULT
printf '%-16s %-10s %-24s %s\n' ---------------- ---------- ------------------------ ------
fail=0
for target in $REQUIRED; do
  build=$(build_target "$target"); brc=$?
  vet=$(vet_seam "$target"); vrc=$?
  result="ok"
  if [ $brc -ne 0 ] || [ $vrc -ne 0 ]; then fail=1; result="UNEXPECTED FAILURE"; fi
  printf '%-16s %-10s %-24s %s\n' "$target" "$build" "$vet" "$result"
done

for f in "$LOGDIR"/build-* "$LOGDIR"/vet-*; do
  [ -s "$f" ] || continue          # empty logs (a clean build) print nothing
  echo ""
  echo "--- $(basename "$f") (verbatim) ---"
  cat "$f"
done

if [ "${1:-}" = "--survey" ]; then
  echo ""
  echo "== survey (informational; does not affect the exit status) =="
  for target in $SURVEY; do
    log="$LOGDIR/survey-${target//\//-}"
    GOOS="${target%%/*}" GOARCH="${target##*/}" CGO_ENABLED=0 go build ./... >"$log" 2>&1
    rc=$?
    if [ $rc -eq 0 ]; then
      printf '  %-16s ok\n' "$target"
    else
      printf '  %-16s rc=%d %s\n' "$target" "$rc" "$(failing_packages "$log" | head -3 | tr '\n' ' ')"
      rm -f "$log"
    fi
  done
  echo "  (a platform outside REQUIRED that fails is a finding to file, not a red guard)"
fi

echo ""
if [ $fail -ne 0 ]; then
  echo "RESULT: FAIL — a required target broke in a package that is not a listed gap"
  exit 1
fi
echo "RESULT: PASS — every required target builds; the seam type-checks on all of them"
