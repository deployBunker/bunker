#!/usr/bin/env bash
# BFS-019 arms: a directory listing through the mount can come back empty (or
# short) while the directory has entries.
#
# Modes:
#   bash docs/evidence/BFS-019-arms.sh red        # the filed defect, BASE binaries
#   bash docs/evidence/BFS-019-arms.sh green      # the acceptance, fixed tree
#   bash docs/evidence/BFS-019-arms.sh general    # repeated / nested / server-writer
#   bash docs/evidence/BFS-019-arms.sh mutations  # negative control per cell + restore
#   bash docs/evidence/BFS-019-arms.sh all
#
# Every mount and every fusermount call is bounded by `timeout`; every process is
# killed by explicit PID (never `pkill -f <pattern>`, which matches the invoking
# shell line). Scratch trees are fresh `mktemp -d` per arm. Nothing is ever
# `rm -rf`'d.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=${REPO:-$(cd "$HERE/../.." && pwd)}
BINBASE=${BINBASE:-/tmp/bfs019/bin}
FAILED=0
PASSES=0

say()  { printf '%s\n' "$*"; }
pass() { printf 'ARM PASS: %s\n' "$*"; PASSES=$((PASSES+1)); }
fail() { printf 'ARM FAIL: %s\n' "$*"; FAILED=1; }

sha() { sha256sum "$1" | cut -d' ' -f1; }

# build <tree> <prefix>
build() {
  local tree=$1 pre=$2
  mkdir -p "$BINBASE"
  ( cd "$tree" && go build -o "$BINBASE/$pre-bunker" ./cmd/bunker && go build -o "$BINBASE/$pre-davserve" ./probes/davserve ) \
    || { fail "build of $tree ($pre)"; return 1; }
}

# ---------------------------------------------------------------------------
# The serving tree: the row's shape — six root entries, one of them a
# collection with many children, one a nested collection, and a `.git/refs/heads`
# so a SHORT answer (not only an empty one) is visible.
# ---------------------------------------------------------------------------
make_tree() { # make_tree <dir>
  local SRC=$1
  mkdir -p "$SRC"
  printf 'x\n' > "$SRC/CHANGELOG.md"
  printf 'x\n' > "$SRC/README.md"
  printf 'x\n' > "$SRC/go.mod"
  mkdir -p "$SRC/pkg" "$SRC/scratch" "$SRC/src" "$SRC/.git/refs/heads"
  printf 'x\n' > "$SRC/pkg/one.txt"
  printf 'x\n' > "$SRC/scratch/two.txt"
  printf 'ref: refs/heads/main\n' > "$SRC/.git/HEAD"
  printf 'a\n' > "$SRC/.git/refs/heads/main"
  printf 'a\n' > "$SRC/.git/refs/heads/dev"
  printf 'a\n' > "$SRC/.git/refs/heads/release"
  local i=0
  while [ $i -lt 120 ]; do printf '%s\n' "$i" > "$SRC/src/f$(printf '%03d' "$i").txt"; i=$((i+1)); done
}

# ---------------------------------------------------------------------------
# start_serve <tree> <stagedir>  -> sets DPID, URL
# ---------------------------------------------------------------------------
start_serve() {
  local SRC=$1 ST=$2 SBIN=$3
  "$SBIN" --root "$SRC" --addr 127.0.0.1:0 > "$ST/davserve.log" 2>&1 &
  DPID=$!
  URL=""
  local _
  for _ in $(seq 1 100); do grep -q '^URL=' "$ST/davserve.log" 2>/dev/null && break; sleep 0.1; done
  URL=$(grep '^URL=' "$ST/davserve.log" | head -1 | cut -d= -f2-)
  [ -n "$URL" ] || { fail "davserve produced no URL"; return 1; }
  say "[arm] DAVSERVE_PID=$DPID URL=$URL"
}

# mount_it <client> <mnt> <stagedir> -> sets MPID
mount_it() {
  local CBIN=$1 MNT=$2 ST=$3
  timeout 120 "$CBIN" fs mount "$MNT" --url "$URL" --invalidation poll > "$ST/mount.log" 2>&1 &
  MPID=$!
  local _
  for _ in $(seq 1 300); do mountpoint -q "$MNT" && break; sleep 0.1; done
  mountpoint -q "$MNT" || { fail "the mount did not come up"; tail -5 "$ST/mount.log"; return 1; }
}

unmount_it() { # unmount_it <mnt> <mpid> <dpid>
  timeout 60 fusermount -u "$1" 2>&1; local rc=$?
  kill "$2" "$3" 2>/dev/null
  wait "$2" 2>/dev/null
  say "[arm] fusermount -u rc=$rc"
}

# ---------------------------------------------------------------------------
# side_by_side <label> <mount> <server-dir>
#
# The mount's count vs the SERVER's count for the same directory, in the same
# output, plus the two rc's — because the defect's signature is that rc is 0
# while the answer is wrong.
# ---------------------------------------------------------------------------
side_by_side() {
  local label=$1 MNT=$2 SDIR=$3
  local mcount scount mrc src lines_m lines_s
  mcount=$(timeout 60 find "$MNT" -mindepth 1 2>/dev/null | wc -l); mrc=$?
  scount=$(timeout 60 find "$SDIR" -mindepth 1 2>/dev/null | wc -l)
  lines_m=$(timeout 60 ls -lR "$MNT" 2>/dev/null | wc -l)
  lines_s=$(timeout 60 ls -lR "$SDIR" 2>/dev/null | wc -l)
  say "--- [$label]"
  say "    find <mount>  -mindepth 1 | wc -l = $mcount   (find rc=$mrc)"
  say "    find <server> -mindepth 1 | wc -l = $scount"
  say "    ls -lR <mount>  | wc -l = $lines_m"
  say "    ls -lR <server> | wc -l = $lines_s"
  say "    mount  root names: $(timeout 30 ls -A "$MNT" 2>/dev/null | sort | tr '\n' ' ')"
  say "    server root names: $(timeout 30 ls -A "$SDIR" 2>/dev/null | sort | tr '\n' ' ')"
}

# names_of <abs-dir> -> sorted names, newline-separated
names_of() { timeout 60 ls -A "$1" 2>/dev/null | sort; }

# expect_equal <label> <mount-dir> <server-dir>
expect_equal() {
  local label=$1 MD=$2 SD=$3 mn sn
  mn=$(names_of "$MD"); sn=$(names_of "$SD")
  if [ "$mn" = "$sn" ]; then
    pass "$label: mount listing == server listing ($(printf '%s' "$sn" | grep -c . ) names)"
  else
    fail "$label: mount=[$(printf '%s' "$mn" | tr '\n' ',')] server=[$(printf '%s' "$sn" | tr '\n' ',')]"
  fi
}

# ---------------------------------------------------------------------------
# the filed RED, exactly as filed
# ---------------------------------------------------------------------------
arm_red() { # arm_red <bunker> <davserve> <tag> <expect: filed|fixed>
  local CBIN=$1 SBIN=$2 TAG=$3 EXPECT=$4
  local WORK; WORK=$(mktemp -d "/tmp/bfs019-$TAG-XXXXXX")
  local SRC=$WORK/src MNT=$WORK/mnt
  mkdir -p "$SRC" "$MNT"
  make_tree "$SRC"
  start_serve "$SRC" "$WORK" "$SBIN" || return 1
  mount_it "$CBIN" "$MNT" "$WORK" || { kill "$DPID" 2>/dev/null; return 1; }
  say "[$TAG] SRC=$SRC MNT=$MNT"

  side_by_side "$TAG step 0: fresh mount" "$MNT" "$SRC"
  expect_equal "$TAG step 0" "$MNT" "$SRC"

  say
  say "--- [$TAG step 1] ONE mkdir through the mount"
  timeout 30 mkdir "$MNT/oob-dir" 2>&1; say "    mkdir rc=$?"
  side_by_side "$TAG step 1: after one mkdir" "$MNT" "$SRC"
  expect_equal "$TAG step 1" "$MNT" "$SRC"

  say
  say "--- [$TAG step 2] a through-mount write + unlink"
  printf 'hello\n' | timeout 30 tee "$MNT/written.txt" > /dev/null 2>&1; say "    write rc=$?"
  timeout 30 rm -f "$MNT/written.txt" 2>&1; say "    unlink rc=$?"
  side_by_side "$TAG step 2: after write + unlink" "$MNT" "$SRC"
  expect_equal "$TAG step 2" "$MNT" "$SRC"

  say
  say "--- [$TAG] lookups still work (the mount is not dead)"
  say "    stat go.mod      : $(timeout 30 stat -c 'size=%s' "$MNT/go.mod" 2>&1)"
  say "    stat src/f000.txt: $(timeout 30 stat -c 'size=%s' "$MNT/src/f000.txt" 2>&1)"
  say "    cat src/f000.txt through the mount: $(timeout 30 cat "$MNT/src/f000.txt" 2>/dev/null | wc -c) bytes"
  expect_equal "$TAG src subdir" "$MNT/src" "$SRC/src"
  expect_equal "$TAG pkg subdir" "$MNT/pkg" "$SRC/pkg"

  say
  unmount_it "$MNT" "$MPID" "$DPID"
  say "[$TAG] stage kept at $WORK"
}

# ---------------------------------------------------------------------------
# the general case: repeated mutation, nested directories, server-side writer
# ---------------------------------------------------------------------------
arm_general() { # arm_general <bunker> <davserve> <tag>
  local CBIN=$1 SBIN=$2 TAG=$3
  local WORK; WORK=$(mktemp -d "/tmp/bfs019-$TAG-XXXXXX")
  local SRC=$WORK/src MNT=$WORK/mnt
  mkdir -p "$SRC" "$MNT"
  make_tree "$SRC"
  start_serve "$SRC" "$WORK" "$SBIN" || return 1
  mount_it "$CBIN" "$MNT" "$WORK" || { kill "$DPID" 2>/dev/null; return 1; }
  say "[$TAG] SRC=$SRC MNT=$MNT"
  expect_equal "[$TAG] fresh" "$MNT" "$SRC"

  say
  say "--- [$TAG G1] a directory mutated REPEATEDLY through the mount"
  local i
  for i in 1 2 3; do
    timeout 30 mkdir "$MNT/repeat-$i" 2>&1
    expect_equal "[$TAG G1] after mkdir #$i" "$MNT" "$SRC"
  done

  say
  say "--- [$TAG G2] a NESTED directory mutated through the mount"
  timeout 30 mkdir "$MNT/pkg/inner" 2>&1; say "    mkdir pkg/inner rc=$?"
  expect_equal "[$TAG G2] root after nested mkdir" "$MNT" "$SRC"
  expect_equal "[$TAG G2] pkg after nested mkdir" "$MNT/pkg" "$SRC/pkg"
  printf 'deep\n' | timeout 30 tee "$MNT/pkg/inner/deep.txt" > /dev/null 2>&1
  expect_equal "[$TAG G2] pkg/inner after a write inside it" "$MNT/pkg/inner" "$SRC/pkg/inner"
  expect_equal "[$TAG G2] pkg after a write inside its child" "$MNT/pkg" "$SRC/pkg"

  say
  say "--- [$TAG G3] a name that appears while the directory is NOT re-read (the short answer)"
  say "    refs/heads before: mount=[$(names_of "$MNT/.git/refs/heads" | tr '\n' ',')] server=[$(names_of "$SRC/.git/refs/heads" | tr '\n' ',')]"
  timeout 30 sh -c "printf 'a\n' > '$MNT/.git/refs/heads/feature'" 2>&1; say "    create refs/heads/feature through the mount rc=$?"
  expect_equal "[$TAG G3] .git/refs/heads" "$MNT/.git/refs/heads" "$SRC/.git/refs/heads"

  say
  say "--- [$TAG G4] a directory mutated by the SERVER (another writer), then listed"
  printf 'server-side\n' > "$SRC/server-made.txt"
  mkdir -p "$SRC/server-dir"
  printf 'nested server-side\n' > "$SRC/src/server-nested.txt"
  say "    (written natively in the served tree; the mount's invalidation mode is poll)"
  local w
  for w in 1 2 3 4 5 6 7 8 9 10; do
    if [ "$(names_of "$MNT" | tr '\n' ',')" = "$(names_of "$SRC" | tr '\n' ',')" ] \
       && [ "$(names_of "$MNT/src" | tr '\n' ',')" = "$(names_of "$SRC/src" | tr '\n' ',')" ]; then break; fi
    sleep 1
  done
  say "    settled after ${w}s"
  expect_equal "[$TAG G4] root after a server-side write" "$MNT" "$SRC"
  expect_equal "[$TAG G4] src after a server-side write" "$MNT/src" "$SRC/src"

  say
  say "--- [$TAG] lookups/reads still correct (attribution)"
  say "    stat go.mod=$(timeout 30 stat -c '%s' "$MNT/go.mod" 2>&1) cat src/f000.txt=$(timeout 30 cat "$MNT/src/f000.txt" 2>/dev/null | wc -c) bytes"
  say "    stat src/server-nested.txt=$(timeout 30 stat -c '%s' "$MNT/src/server-nested.txt" 2>&1)"

  say
  unmount_it "$MNT" "$MPID" "$DPID"
  say "[$TAG] stage kept at $WORK"
}

# ---------------------------------------------------------------------------
# negative control: the fix reverted in place, the SAME cells re-run, then a
# sha256-verified byte-identical restore.
#
# The mutation is the one line the fix is: the child-index insert becomes
# conditional on the NODE being new again (the pre-fix text).
# ---------------------------------------------------------------------------
SNAP=internal/fsclient/snapshot.go

arm_mutations() {
  local TREE=${1:-$REPO}
  local BK="/tmp/bfs019-snapshot-$$.orig"
  cp -p "$TREE/$SNAP" "$BK" || { fail "no snapshot.go to back up"; return 1; }
  local ORIG; ORIG=$(sha "$BK")
  say "MUTATION baseline: $SNAP sha256=$ORIG"

  say "--- mutation: the child-index insert is conditional on the NODE being new"
  python3 - "$TREE/$SNAP" <<'PY'
import sys
p = sys.argv[1]
s = open(p, encoding="utf-8").read()
old = """	parent := path.Dir(n.Path)
	if parent == "." {
		parent = ""
	}
	if s.children[parent] == nil {
		s.children[parent] = map[string]struct{}{}
	}
	s.children[parent][path.Base(n.Path)] = struct{}{}
"""
new = """	parent := path.Dir(n.Path)
	if parent == "." {
		parent = ""
	}
	if !existed || old.IsDir != n.IsDir {
		if s.children[parent] == nil {
			s.children[parent] = map[string]struct{}{}
		}
		s.children[parent][path.Base(n.Path)] = struct{}{}
	}
"""
if old not in s:
    print("MUTATION PATCH DID NOT APPLY (the fixed text is not on disk)")
    sys.exit(3)
if new in s:
    print("MUTATION PATCH: the pre-fix text is already on disk")
    sys.exit(3)
open(p, "w", encoding="utf-8").write(s.replace(old, new, 1))
print("MUTATION APPLIED")
PY
  local mrc=$?
  if [ $mrc -ne 0 ]; then
    fail "mutation patch did not apply (rc=$mrc)"
    cp -p "$BK" "$TREE/$SNAP"
    return 1
  fi
  local MUT; MUT=$(sha "$TREE/$SNAP")
  say "MUTATION applied: sha256=$MUT (baseline $ORIG)"

  say "--- the unit cells under the mutation (must FAIL)"
  ( cd "$TREE" && go test ./internal/fsclient/ -run 'BFS019' -count=1 ) > "/tmp/bfs019-mut-unit.txt" 2>&1
  local urc=$?
  tail -12 /tmp/bfs019-mut-unit.txt
  if [ $urc -ne 0 ]; then pass "mutation reddens the unit cells (go test rc=$urc)"; else fail "the unit cells PASS under the mutation (they are blind)"; fi

  say "--- the ATTRIBUTION cell under the mutation (must stay GREEN)"
  ( cd "$TREE" && go test ./internal/fsclient/ -run 'BFS019Attribution' -count=1 ) > "/tmp/bfs019-mut-attr.txt" 2>&1
  local arc=$?
  tail -5 /tmp/bfs019-mut-attr.txt
  if [ $arc -eq 0 ]; then pass "the attribution cell stays green under the mutation"; else fail "the attribution cell went red under the mutation (not independent)"; fi

  say "--- restore from the byte copy, sha256-verified"
  cp -p "$BK" "$TREE/$SNAP"
  local BACK; BACK=$(sha "$TREE/$SNAP")
  if [ "$BACK" = "$ORIG" ]; then pass "restored byte-identical ($BACK)"; else fail "the restore is NOT byte-identical ($BACK vs $ORIG)"; fi
  say "--- the same unit cells after the restore (must PASS)"
  ( cd "$TREE" && go test ./internal/fsclient/ -run 'BFS019' -count=1 ) > "/tmp/bfs019-rest-unit.txt" 2>&1
  local rrc=$?
  tail -5 /tmp/bfs019-rest-unit.txt
  if [ $rrc -eq 0 ]; then pass "the cells pass again after the byte-identical restore"; else fail "still red after the restore (rc=$rrc)"; fi
}

# ---------------------------------------------------------------------------
# restore_base_binaries copies the PRE-FIX binaries into $BINBASE/base-*, built
# once from this tree before the fix (the tree state the row was measured on).
restore_base_binaries() {
  [ -x "$BINBASE/base-bunker" ] && [ -x "$BINBASE/base-davserve" ] && return 0
  fail "no base binaries at $BINBASE/base-* (build them from the pre-fix tree with: cp $BINBASE/bunker $BINBASE/base-bunker; cp $BINBASE/davserve $BINBASE/base-davserve)"
  return 1
}

case "${1:-all}" in
  red)       restore_base_binaries && arm_red  "$BINBASE/base-bunker" "$BINBASE/base-davserve" red filed ;;
  green)     build "$REPO" fixed && arm_red "$BINBASE/fixed-bunker" "$BINBASE/fixed-davserve" green fixed ;;
  general)   build "$REPO" fixed && arm_general "$BINBASE/fixed-bunker" "$BINBASE/fixed-davserve" general ;;
  mutations) arm_mutations "$REPO" ;;
  suites)
    ( cd "$REPO" && go test ./internal/server/webdav/ ./internal/fsclient/ ./internal/fsmount/ -count=1 ) 2>&1 | tail -20
    ;;
  all)       build "$REPO" fixed && arm_red "$BINBASE/fixed-bunker" "$BINBASE/fixed-davserve" green fixed && arm_general "$BINBASE/fixed-bunker" "$BINBASE/fixed-davserve" general ;;
  *)         echo "usage: $0 red|green|general|mutations|suites|all"; exit 2 ;;
esac

say
say "ARMS: PASSES=$PASSES FAILED=$FAILED"
[ "$FAILED" = 0 ]
