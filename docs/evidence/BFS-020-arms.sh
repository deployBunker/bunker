#!/usr/bin/env bash
# BFS-020 — the arms, and the controls that prove each claiming cell can FAIL.
#
# THE RULE (the same one BFS-021's arms script states): a test that cannot fail
# proves nothing. Every cell of this row appears below with the SOURCE MUTATION
# that turns it RED, applied with an exact-match replacement that REFUSES unless the
# anchor appears exactly once, run BY NAME, and RESTORED from `git checkout` with the
# sha256 of every touched file re-checked afterwards — a restore that is not
# byte-identical aborts the run (so a green after an arm can never be read as a
# claim about a tree that is not the committed one). Each arm also names
# ATTRIBUTION cells that must stay GREEN under the mutation, so a red is caused by
# the requirement under test rather than by a broken tree.
#
# The cells (internal/fsmount/bfs020_publication_test.go):
#   C1  a file created and CLOSED through the mount can be renamed IMMEDIATELY,
#       and the served tree holds it after the FLUSH alone (the protocol fact)
#   C2  a create with NO chunk (an EMPTY file) still publishes the name
#   C3  a rename of a name this mount is still holding publishes it first
#   C4  the handles follow the rename: later bytes land on the NEW name and the old
#       name is not resurrected
#   C5  a write AFTER a publication point is still published
#   C6  a create after an UNLINK of the same name resolves a fresh base
#   C7  a create after a RENAME of the same name resolves a fresh base
#   C8  a read after this mount replaced a file by rename serves the MOVED content
#   C9  a refused create does not leave the mount claiming the name
#   C10 a refusal at close(2) reaches the caller
#   C11 rmdir of a NON-EMPTY collection is refused (the surface's DELETE is recursive)
#   C12 unlink of a collection is refused
# plus the LANDMARK cells of the rows this one may not change, which must stay green
# under EVERY mutation: BFS-030's write-shape refusal, BFS-021's append cells, and
# BFS-033's refusal hold.
#
# usage: bash docs/evidence/BFS-020-arms.sh <mode>
#   green                                  the tree as committed: every cell passes.
#   all                                    green, then every mutation.
#   <mutation>                             one mutation; see the list below.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
FILES=(internal/fsmount/fs_linux.go internal/fsmount/append.go internal/fsclient/write.go)
WORK="$(mktemp -d /tmp/bfs020-arms-XXXXXX)"
SRC=internal/fsmount/fs_linux.go
WPSRC=internal/fsclient/write.go

C1=TestACreatedAndClosedFileCanBeRenamedImmediately
C2=TestACreateWithNoChunkStillPublishesTheName
C3=TestARenameOfAFileThisMountIsStillHoldingPublishesItFirst
C4=TestAHandleFollowsARenameAndDoesNotResurrectTheOldName
C5=TestAWriteAfterAPublicationPointIsStillPublished
C6=TestACreateAfterAnUnlinkOfTheSameNameResolvesAFreshBase
C7=TestACreateAfterARenameOfTheSameNameResolvesAFreshBase
C8=TestAReadAfterARenameOverTheTargetServesTheMovedContent
C9=TestARefusedCreateDoesNotLeaveTheMountClaimingTheName
C10=TestAPublicationRefusalAtCloseIsReportedToTheCaller
C11=TestRmdirOfANonEmptyCollectionIsRefused
C12=TestUnlinkOfACollectionIsRefused
L030=TestResizeThroughAWriteHandleIsRefusedBeforeItPublishes
L021=TestAppendThroughAnAppendHandleLandsTheBytes
L033=TestAStaleBaseRefusesTheAppendAndNothingLands
ALL_CELLS="^(${C1}|${C2}|${C3}|${C4}|${C5}|${C6}|${C7}|${C8}|${C9}|${C10}|${C11}|${C12})$"
LMALT="${L030}|${L021}|${L033}"
LANDMARKS="^(${LMALT})$"

MODES="green flush-does-not-publish empty-create-not-published no-rename-barrier no-retarget write-does-not-reopen no-base-advance path-record-survives-unlink rename-keeps-the-path-record rename-keeps-the-destination-cache refusal-keeps-the-created-name flush-swallows-the-refusal rmdir-passes-a-non-empty-collection unlink-passes-a-collection all"

# mutation -> "RED cells|GREEN (attribution) cells"
declare -A RED GREEN
RED[flush-does-not-publish]="^(${C1}|${C2}|${C4}|${C5})$"
GREEN[flush-does-not-publish]="^(${C3}|${C8}|${LMALT})$"
RED[empty-create-not-published]="^(${C2})$"
GREEN[empty-create-not-published]="^(${C1}|${C5}|${LMALT})$"
RED[no-rename-barrier]="^(${C3})$"
GREEN[no-rename-barrier]="^(${C1}|${C2}|${C4}|${LMALT})$"
RED[no-retarget]="^(${C4})$"
GREEN[no-retarget]="^(${C1}|${C3}|${C5}|${LMALT})$"
RED[write-does-not-reopen]="^(${C5}|${C4})$"
GREEN[write-does-not-reopen]="^(${C1}|${C2}|${C3}|${LMALT})$"
RED[no-base-advance]="^(${C5})$"
GREEN[no-base-advance]="^(${C1}|${C2}|${LMALT})$"
RED[path-record-survives-unlink]="^(${C6}|${C7})$"
GREEN[path-record-survives-unlink]="^(${C1}|${C5}|${LMALT})$"
RED[rename-keeps-the-path-record]="^(${C7})$"
GREEN[rename-keeps-the-path-record]="^(${C1}|${C3}|${C6}|${LMALT})$"
RED[rename-keeps-the-destination-cache]="^(${C8})$"
GREEN[rename-keeps-the-destination-cache]="^(${C1}|${C2}|${C5}|${LMALT})$"
RED[refusal-keeps-the-created-name]="^(${C9})$"
GREEN[refusal-keeps-the-created-name]="^(${C1}|${C5}|${C10}|${LMALT})$"
RED[flush-swallows-the-refusal]="^(${C10}|${C9})$"
GREEN[flush-swallows-the-refusal]="^(${C1}|${C2}|${LMALT})$"
RED[rmdir-passes-a-non-empty-collection]="^(${C11})$"
GREEN[rmdir-passes-a-non-empty-collection]="^(${C1}|${C3}|${C5}|${LMALT})$"
RED[unlink-passes-a-collection]="^(${C12})$"
GREEN[unlink-passes-a-collection]="^(${C1}|${C2}|${C5}|${LMALT})$"

usage() {
  printf 'usage: %s <mode>\n  modes: %s\n' "$(basename "$0")" "$MODES" >&2
  exit 2
}

snapshot() { (cd "$REPO" && sha256sum "${FILES[@]}") > "$WORK/before.sha"; }

restore() {
  git -C "$REPO" checkout -- "${FILES[@]}"
  if ! (cd "$REPO" && sha256sum "${FILES[@]}") | diff -q - "$WORK/before.sha" >/dev/null; then
    echo "  RESTORE IS NOT BYTE-IDENTICAL — aborting"
    return 1
  fi
  echo "  restore: sha256 byte-identical for ${#FILES[@]} file(s)"
}

# run_set <label> <regex> — 0 when the run PASSED (every matched test green)
run_set() {
  local label="$1" re="$2" out rc
  out=$(cd "$REPO" && go test ./internal/fsmount/ -count=1 -v -run "$re" 2>&1)
  rc=$?
  echo "  $label rc=$rc"
  printf '%s\n' "$out" | grep -E "^(--- FAIL|--- PASS|ok|FAIL|PASS)" | sed 's/^/      /'
  if printf '%s' "$out" | grep -q "no tests to run"; then
    echo "      NO TEST MATCHED ${re}: this arm is VACUOUS"
    return 1
  fi
  if ! printf '%s' "$out" | grep -qE "^--- (PASS|FAIL)"; then
    echo "      NO TEST RAN (no per-test result line): this arm is VACUOUS"
    return 1
  fi
  return $rc
}

# mutate <file> — exact, exactly-once replacement of the fragment in $WORK/old with
# the one in $WORK/new; refuses otherwise (so a mutation that no longer matches the
# source is an ABORT, not a silent no-op that would make the arm vacuous). The
# replacement is done by anchor-replace.pl, a shipped file rather than a `perl -e`
# one-liner: an arm that cannot be re-run by a reader is not evidence.
mutate() {
  local f="$REPO/$1"
  perl "$HERE/BFS-020-probes/anchor-replace.pl" "$WORK/old" "$WORK/new" "$f"
}

# write_frag FILE — the fragment on stdin.
write_frag() { cat > "$1"; }

ARM_FAILURES=0

arm() {
  local name="$1" rc=0
  echo
  echo "================================================================================"
  echo "MUTATION $name"
  echo "  RED (must FAIL)   = ${RED[$name]}"
  echo "  GREEN (must PASS) = ${GREEN[$name]}"
  echo "================================================================================"
  snapshot
  case "$name" in
    flush-does-not-publish)
      write_frag "$WORK/old" <<'EOF'
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno { return h.publish(ctx) }
EOF
      write_frag "$WORK/new" <<'EOF'
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno { return 0 }
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    empty-create-not-published)
      write_frag "$WORK/old" <<'EOF'
	if h.tmp == nil && !h.created {
EOF
      write_frag "$WORK/new" <<'EOF'
	if h.tmp == nil {
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    no-rename-barrier)
      write_frag "$WORK/old" <<'EOF'
	if errno := n.m.publishPending(ctx, src); errno != 0 {
		return errno
	}
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    no-retarget)
      write_frag "$WORK/old" <<'EOF'
	n.m.retargetHandles(src, dst)
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    write-does-not-reopen)
      write_frag "$WORK/old" <<'EOF'
	if h.flushed {
		h.flushed, h.result = false, nil
	}
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    no-base-advance)
      write_frag "$WORK/old" <<'EOF'
		h.base = fsclient.WriteBase{IfMatch: res.Hash, Source: fsclient.BaseFromServed}
EOF
      write_frag "$WORK/new" <<'EOF'
		_ = res.Hash // mutation: the base does not advance
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    path-record-survives-unlink)
      write_frag "$WORK/old" <<'EOF'
	delete(w.holds, path)
	delete(w.bases, path)
	delete(w.served, path)
EOF
      write_frag "$WORK/new" <<'EOF'
	delete(w.holds, path)
EOF
      mutate "$WPSRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    rename-keeps-the-path-record)
      write_frag "$WORK/old" <<'EOF'
	n.m.wp.NoteDeleted(src)
	n.m.wp.NoteDeleted(dst)
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    rename-keeps-the-destination-cache)
      write_frag "$WORK/old" <<'EOF'
	n.m.cache.Drop(src, dst)
EOF
      write_frag "$WORK/new" <<'EOF'
	n.m.cache.Drop(src)
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    refusal-keeps-the-created-name)
      write_frag "$WORK/old" <<'EOF'
		h.m.snapshot().Drop(h.p)
		if err.Cause == fsclient.CauseConflict {
EOF
      write_frag "$WORK/new" <<'EOF'
		if err.Cause == fsclient.CauseConflict {
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    flush-swallows-the-refusal)
      write_frag "$WORK/old" <<'EOF'
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno { return h.publish(ctx) }
EOF
      write_frag "$WORK/new" <<'EOF'
func (h *writeHandle) Flush(ctx context.Context) syscall.Errno {
	h.publish(ctx)
	return 0
}
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    rmdir-passes-a-non-empty-collection)
      write_frag "$WORK/old" <<'EOF'
	metas, perr := n.m.client.Propfind(ctx, cp, "1")
	if perr != nil {
		n.m.recordFailure(perr)
		return errnoFor(perr)
	}
	for _, meta := range metas {
		if strings.Trim(meta.Path, "/") != cp {
			return syscall.ENOTEMPTY
		}
	}
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    unlink-passes-a-collection)
      write_frag "$WORK/old" <<'EOF'
	if nd, ok := n.m.snapshot().Lookup(cp); ok && nd.IsDir {
		return syscall.EISDIR
	}
EOF
      write_frag "$WORK/new" <<'EOF'
EOF
      mutate "$SRC" || { restore; ARM_FAILURES=$((ARM_FAILURES+1)); return 1; } ;;
    *) echo "no such mutation: $name" >&2; restore; return 2 ;;
  esac
  echo "  applied. changed files: $(git -C "$REPO" diff --name-only | tr '\n' ' ')"
  if ! (cd "$REPO" && go build ./... >/dev/null 2>&1); then
    echo "  THE MUTATED TREE DOES NOT COMPILE — this mutation proves nothing"
    restore
    ARM_FAILURES=$((ARM_FAILURES + 1))
    return 1
  fi
  echo "  the mutated tree compiles"

  if run_set "RED   -run ${RED[$name]}" "${RED[$name]}"; then
    echo "  ARM FAILED: the claimed cell stayed GREEN under its own mutation"
    rc=1
  else
    echo "  RED confirmed: the mutation reddens the cell it is aimed at"
  fi
  if run_set "GREEN -run ${GREEN[$name]}" "${GREEN[$name]}"; then
    echo "  attribution confirmed: the named cells stay GREEN"
  else
    echo "  ARM FAILED: an attribution cell went RED too (the mutation is not specific)"
    rc=1
  fi
  restore || rc=1
  [ "$rc" = 0 ] || ARM_FAILURES=$((ARM_FAILURES + 1))
  return "$rc"
}

mode() {
  local m="${1:-}" n
  case "$m" in
    green)
      echo "================================================================================"
      echo "GREEN — the tree as committed: every claiming cell must PASS"
      echo "================================================================================"
      snapshot
      run_set "ALL       -run ${ALL_CELLS}" "$ALL_CELLS" || ARM_FAILURES=$((ARM_FAILURES + 1))
      run_set "LANDMARKS -run ${LANDMARKS}" "$LANDMARKS" || ARM_FAILURES=$((ARM_FAILURES + 1))
      ;;
    all)
      mode green
      for n in flush-does-not-publish empty-create-not-published no-rename-barrier no-retarget \
               write-does-not-reopen no-base-advance path-record-survives-unlink \
               rename-keeps-the-path-record rename-keeps-the-destination-cache \
               refusal-keeps-the-created-name flush-swallows-the-refusal \
               rmdir-passes-a-non-empty-collection unlink-passes-a-collection; do
        arm "$n"
      done
      ;;
    "")
      usage
      ;;
    *)
      [ -n "${RED[$m]:-}" ] || { echo "no such mutation: $m" >&2; usage; }
      arm "$m"
      ;;
  esac
  echo
  if [ "$ARM_FAILURES" = 0 ]; then
    echo "TOTAL: every declared outcome met (0 arm failures)"
  else
    echo "TOTAL: $ARM_FAILURES arm failure(s)"
  fi
  echo "work dir: $WORK"
  [ "$ARM_FAILURES" = 0 ]
}

mode "${1:-}"
