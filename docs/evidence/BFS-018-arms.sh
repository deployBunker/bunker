#!/usr/bin/env bash
# BFS-018 arms: the live acceptance THROUGH the mount, one mutation per cell,
# and the attribution cells that must stay green under each mutation.
#
# Every mount and every fusermount call is bounded by `timeout`, every process
# is killed by explicit PID (never `pkill -f <pattern>`, which matches the
# invoking shell line), and the scratch tree is a fresh `mktemp -d` per arm.
#
# Usage:
#   bash docs/evidence/BFS-018-arms.sh live          # the acceptance, through the mount
#   bash docs/evidence/BFS-018-arms.sh red           # the filed defect, with the BASE binaries
#   bash docs/evidence/BFS-018-arms.sh old-surface   # new client against the BASE surface
#   bash docs/evidence/BFS-018-arms.sh mutations     # one mutation per cell + attribution
#   bash docs/evidence/BFS-018-arms.sh suites        # the neighbouring semantics' counts
#   bash docs/evidence/BFS-018-arms.sh all
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=${REPO:-$(cd "$HERE/../.." && pwd)}
BIN=${BIN:-/tmp/bfs018/bin}
WORK=$(mktemp -d /tmp/bfs018-arms-XXXXXX)
MUTDIR=$HERE/BFS-018-mutations
FAILED=0

say() { printf '%s\n' "$*"; }
pass() { printf 'ARM PASS: %s\n' "$*"; }
fail() { printf 'ARM FAIL: %s\n' "$*"; FAILED=1; }

build() { # build() <tree> <prefix>
  local tree=$1 pre=$2
  ( cd "$tree" && go build -o "$BIN/$pre-bunker" ./cmd/bunker && go build -o "$BIN/$pre-davserve" ./probes/davserve ) || {
    fail "build of $tree ($pre)"; return 1; }
}

sha() { sha256sum "$1" | cut -d' ' -f1; }

# ---------------------------------------------------------------------------
# The live arm: mount a git work tree carrying symlinks and drive the ONE flow
# the row is about — ls -l (the TYPE), readlink, a type change THROUGH the
# mount, a `git checkout -f HEAD` THROUGH the mount, and `git status`.
#
# Arguments: <client binary> <surface binary> <tag> <expect: fixed|filed>
# ---------------------------------------------------------------------------
git_flow() {
  local CBIN=$1 SBIN=$2 TAG=$3 EXPECT=$4
  local ST=$WORK/$TAG
  local SRC=$ST/repo MNT=$ST/mnt
  mkdir -p "$SRC" "$MNT"

  ( cd "$SRC" \
    && git init -q -b main . \
    && git config user.email "arm@bfs018.test" && git config user.name "bfs018 arm" \
    && printf 'hello from the source tree' > a.txt \
    && ln -sf a.txt link-to-a \
    && mkdir -p vendored && ln -sf ../a.txt vendored/sibling-link \
    && printf 'nested\n' > vendored/nested.txt \
    && git add -A && git -c commit.gpgsign=false commit -qm "tree with symlinks" ) || { fail "$TAG: repo setup"; return 1; }

  say "=== [$TAG] the committed tree ==="
  ( cd "$SRC" && git ls-files -s )
  say

  "$SBIN" --root "$SRC" --addr 127.0.0.1:0 > "$ST/davserve.log" 2>&1 &
  local DPID=$!
  local i URL=""
  for i in $(seq 1 100); do grep -q '^URL=' "$ST/davserve.log" 2>/dev/null && break; sleep 0.1; done
  URL=$(grep '^URL=' "$ST/davserve.log" | head -1 | cut -d= -f2-)
  say "[$TAG] DAVSERVE_PID=$DPID URL=$URL"

  timeout 120 "$CBIN" fs mount "$MNT" --url "$URL" --invalidation poll > "$ST/mount.log" 2>&1 &
  local MPID=$!
  for i in $(seq 1 300); do mountpoint -q "$MNT" && break; sleep 0.1; done
  if ! mountpoint -q "$MNT"; then
    fail "$TAG: the mount did not come up"; tail -5 "$ST/mount.log"
    kill "$MPID" "$DPID" 2>/dev/null; return 1
  fi

  say "=== [$TAG] ls -l through the mount (the TYPE line) ==="
  timeout 60 ls -l "$MNT" "$MNT/vendored" > "$ST/ls.txt" 2>&1
  cat "$ST/ls.txt"
  say "=== [$TAG] readlink through the mount ==="
  timeout 30 readlink "$MNT/link-to-a" > "$ST/readlink.txt" 2>&1
  local RRC=$?
  say "readlink link-to-a -> '$(cat "$ST/readlink.txt")' (rc=$RRC)"
  timeout 30 readlink "$MNT/vendored/sibling-link" > "$ST/readlink2.txt" 2>&1
  local RRC2=$?
  say "readlink vendored/sibling-link -> '$(cat "$ST/readlink2.txt")' (rc=$RRC2)"

  say "=== [$TAG] the type change, made THROUGH the mount ==="
  timeout 30 rm -f "$MNT/link-to-a"; say "rm link-to-a rc=$?"
  printf 'a.txt' | timeout 30 tee "$MNT/link-to-a" > /dev/null; say "wrote a regular file over the name rc=$?"
  if [ -L "$SRC/link-to-a" ]; then say "[$TAG] server: still a symlink"; else say "[$TAG] server: now a $(stat -c %F "$SRC/link-to-a" 2>/dev/null)"; fi

  say "=== [$TAG] git status through the mount (the type change a developer never made) ==="
  timeout 120 git -C "$MNT" status --porcelain > "$ST/status-before.txt" 2>&1
  cat "$ST/status-before.txt"; say "[$TAG] status lines before checkout: $(wc -l < "$ST/status-before.txt")"

  say "=== [$TAG] git checkout -f HEAD THROUGH the mount ==="
  timeout 180 git -C "$MNT" checkout -f HEAD > "$ST/checkout.txt" 2>&1
  local CRC=$?
  say "checkout rc=$CRC"; tail -5 "$ST/checkout.txt"

  say "=== [$TAG] git status through the mount, after the checkout ==="
  timeout 120 git -C "$MNT" status --porcelain > "$ST/status-after.txt" 2>&1
  cat "$ST/status-after.txt"; say "[$TAG] status lines after checkout: $(wc -l < "$ST/status-after.txt")"

  say "=== [$TAG] what the SERVER holds now ==="
  ls -l "$SRC" "$SRC/vendored" | sed "s#$SRC#<served>#g"

  say "=== [$TAG] materialisation scan (a regular file holding a link target) ==="
  local MAT=0
  while IFS= read -r f; do
    if [ ! -L "$f" ] && [ -f "$f" ]; then
      case "$(head -c 64 "$f" 2>/dev/null)" in
        "a.txt"|"../a.txt") say "[$TAG] MATERIALISED: $f = $(head -c 32 "$f")"; MAT=1;;
      esac
    fi
  done < <(find "$SRC" -path "$SRC/.git" -prune -o -type f -print)
  say "[$TAG] materialised_regular_files=$MAT"

  say "=== [$TAG] the mount's own report (its kept status document) ==="
  # The document is written on a 1 s cadence, so it is read TWICE: a single read
  # that lands between two writes reports the previous second's figures, and a
  # stale zero is not the same fact as "the mount did nothing".
  local attempt
  for attempt in 1 2; do
    say "--- read $attempt ---"
    local REPORT
    REPORT="$(timeout 30 "$CBIN" fs status 2>&1 | grep -E "^symlink|readlinks_total|refusal    :" | head -6)"
    if [ -n "$REPORT" ]; then
      printf '%s\n' "$REPORT"
    else
      say "(the status document carries no symlink block: this build has no symlink awareness at all — which is the finding)"
    fi
    sleep 2
  done

  # The arm's verdict, from the STATE, not from an exit code.
  local LINKIS="$( [ -L "$SRC/link-to-a" ] && echo symlink || echo not-a-symlink )"
  local AFTER="$(wc -l < "$ST/status-after.txt")"
  local LSTYPE="$(awk '/link-to-a/{printf "%s", substr($1,1,1)}' "$ST/ls.txt")"
  say "[$TAG] SUMMARY ls_type=$LSTYPE server_kind=$LINKIS status_after=$AFTER materialised=$MAT checkout_rc=$CRC"
  if [ "$EXPECT" = fixed ]; then
    if [ "$LSTYPE" = l ] && [ "$LINKIS" = symlink ] && [ "$AFTER" = 0 ] && [ "$MAT" = 0 ] && [ "$CRC" = 0 ]; then
      pass "$TAG: a real symlink through the mount, a clean status after the checkout, nothing materialised"
    else
      fail "$TAG: expected the fixed behaviour"
    fi
  else
    if [ "$LSTYPE" != l ] || [ "$AFTER" != 0 ] || [ "$MAT" = 1 ]; then
      pass "$TAG: the filed defect reproduces (the tree is not a symlink tree through the mount)"
    else
      fail "$TAG: the filed defect did NOT reproduce; this arm proves nothing"
    fi
  fi

  timeout 30 fusermount3 -u "$MNT" > /dev/null 2>&1
  sleep 0.5
  kill "$MPID" "$DPID" 2>/dev/null
  say "[$TAG] unmounted (rc=$?), stage kept at $ST"
  say
}

# ---------------------------------------------------------------------------
# One mutation per cell: the mutation must turn ITS cell RED, must leave the
# attribution cell GREEN, and must restore the file BYTE-IDENTICALLY (sha256).
# ---------------------------------------------------------------------------
mutations() {
  build "$REPO" new || return 1
  # file|mutation|cell regex|package|attribution cell regex
  local TABLE=(
    "internal/server/webdav/props.go|mutate-1-server-type-classifier.py|TestSymlinkTypeIsDeclaredOnTheWire|./internal/server/webdav/|TestSymlinkChangeAttributionCell"
    "internal/server/webdav/ops.go|mutate-2-snapshot-link-target.py|TestSnapshotCarriesTheLinkTarget|./internal/server/webdav/|TestSymlinkChangeAttributionCell"
    "internal/server/webdav/handler.go|mutate-3-get-refusal.py|TestSymlinkGetIsRefusedByNameAndTheTargetStillReads|./internal/server/webdav/|TestSymlinkChangeAttributionCell"
    "internal/server/webdav/handler.go|mutate-4-put-over-link.py|TestUndeclaredSymlinkReplaceIsRefusedAndNothingLands|./internal/server/webdav/|TestSymlinkChangeAttributionCell"
    "internal/fsclient/snapshot.go|mutate-5-client-kind-collapse.py|TestSymlinkThroughTheMountIsALinkNotAFile|./internal/fsmount/|TestSymlinkChangeMountAttributionCell"
    "internal/fsmount/fs_linux.go|mutate-6-modeof-siflnk.py|TestSymlinkThroughTheMountIsALinkNotAFile|./internal/fsmount/|TestSymlinkChangeMountAttributionCell"
    "internal/fsclient/client.go|mutate-7-link-target-as-body.py|TestSymlinkCreatedThroughTheMountIsASymlinkOnTheServer|./internal/fsmount/|TestSymlinkChangeMountAttributionCell"
    "internal/fsmount/fs_linux.go|mutate-8-refusal-not-counted.py|TestHardlinkIsRefusedByNameAndReported|./internal/fsmount/|TestSymlinkChangeMountAttributionCell"
  )
  local row file mut cell pkg attr
  for row in "${TABLE[@]}"; do
    IFS='|' read -r file mut cell pkg attr <<< "$row"
    local orig=$WORK/$(basename "$file").orig
    cp "$REPO/$file" "$orig"
    local before after
    before=$(sha "$REPO/$file")
    say "=== mutation $mut on $file (sha256 before $before) ==="
    python3 "$MUTDIR/$mut" "$REPO" || { fail "$mut: could not apply"; cp "$orig" "$REPO/$file"; continue; }

    # A mutation that does not compile proves nothing.
    if ! ( cd "$REPO" && go build ./... > "$WORK/$mut.build" 2>&1 ); then
      fail "$mut: the mutated tree does not compile"; tail -3 "$WORK/$mut.build"
    else
      if ( cd "$REPO" && go test -count=1 -run "$cell" $pkg > "$WORK/$mut.cell" 2>&1 ); then
        fail "$mut: cell $cell stayed GREEN under the mutation (the cell has no teeth)"
      else
        pass "$mut: cell $cell goes RED"
        grep -m2 -E "^(--- FAIL|    )" "$WORK/$mut.cell" | head -3
      fi
      if ( cd "$REPO" && go test -count=1 -run "$attr" $pkg > "$WORK/$mut.attr" 2>&1 ); then
        pass "$mut: attribution cell $attr stays GREEN"
      else
        fail "$mut: attribution cell $attr went RED (the cells are not independent)"
        tail -5 "$WORK/$mut.attr"
      fi
    fi

    cp "$orig" "$REPO/$file"
    after=$(sha "$REPO/$file")
    if [ "$before" = "$after" ]; then
      pass "$mut: restored byte-identically (sha256 $after)"
    else
      fail "$mut: restore is NOT byte-identical ($before -> $after)"
    fi
    if ( cd "$REPO" && go test -count=1 -run "$cell" $pkg > "$WORK/$mut.recheck" 2>&1 ); then
      pass "$mut: the cell is GREEN again after the restore"
    else
      fail "$mut: the cell is still RED after the restore"
    fi
    say
  done
}

# ---------------------------------------------------------------------------
# The neighbouring semantics the row must not regress: their own cells, counted.
# ---------------------------------------------------------------------------
suites() {
  local line
  # pkg@regex@label — the regexes are the NEIGHBOURING rows' own cells, named by
  # their test-function prefix (the repo's naming is descriptive, so the prefix
  # IS the row). '@' is the field separator BECAUSE the regexes are alternations:
  # a '|' separator silently truncates each regex to its first alternative, which
  # would make a four-cell row report one passing cell.
  local TABLE=(
    "./internal/fsmount/@TestAppend|AppendThrough@BFS-021 append (whole-file publication)"
    "./internal/fsmount/@TestACreate|TestACreatedAndClosed|TestARenameOfAFile|TestAHandleFollows|TestAWriteAfterAPublication@BFS-020 created-name publication + rename/handle rules"
    "./internal/fsmount/@TestAResize|TestFtruncateWith|TestDeliberateResize|TestResizeIsAllowedAgain|TestWriteIntentFollows@BFS-030/033 in-place rewrite + refusal hold"
    "./internal/fsmount/@TestUnlinkOfACollection|TestRmdirOfANonEmpty@BFS-020 recursive-delete refusals"
    "./internal/fsmount/@TestBound|TestReadBound|TestAReader@BFS-025 published-size bound"
    "./internal/server/webdav/@TestExpectedHashIsRevalidated|TestCreateOnlyRuleIsRevalidated|TestCommitSectionIsExclusive|TestConcurrentConditionalWrites@BFS-020/038 commit-time revalidation + exclusive commit"
    "./internal/server/webdav/@TestStaleHashWrite|TestCreateOnlyPrecondition|TestIfMatchAbsent|TestPreconditionAppliesToDelete|TestWeakTagNeverSatisfies@BFS-004 §6 conditional PUT + refusals"
    "./internal/server/webdav/@TestAtomicWriteIsVisibleWhole|TestMethodMatrixNoWritesOnRefusal@BFS-038 atomic publish + refused-write-does-not-land"
  )
  local row pkg re label
  for row in "${TABLE[@]}"; do
    IFS='@' read -r pkg re label <<< "$row"
    # How many cells the selector CAN name (no run): the pass count must equal it,
    # or a cell was silently skipped and the count would prove nothing.
    local named
    named=$( cd "$REPO" && go test -list "$re" $pkg 2>/dev/null | grep -c '^Test' )
    line=$( cd "$REPO" && go test -count=1 -run "$re" $pkg -v 2>&1 )
    local p f
    p=$(printf '%s' "$line" | grep -c '^--- PASS')
    f=$(printf '%s' "$line" | grep -c '^--- FAIL')
    if [ "$f" = 0 ] && [ "$p" != 0 ] && [ "$p" = "$named" ]; then
      pass "$label: cells passed=$p failed=$f (selector names $named)"
    elif [ "$p" = 0 ] && [ "$f" = 0 ]; then
      fail "$label: NO cell matched the selector — a count of zero proves nothing"
    elif [ "$p" != "$named" ]; then
      fail "$label: cells passed=$p failed=$f but the selector names $named — a cell did not run"
    else
      fail "$label: cells passed=$p failed=$f"
      printf '%s\n' "$line" | grep '^--- FAIL' | head -5
    fi
  done
}

# ---------------------------------------------------------------------------
# The honest residual: the NEW client against the BASE surface (a build that
# declares no symlink extension). The link must still be TYPED correctly (the
# base surface's snapshot carrier always carried `type`), its target must be
# REFUSED BY NAME (the base surface publishes none), a create must be refused
# with nothing sent, and nothing may be materialised.
# ---------------------------------------------------------------------------
old_surface() {
  local ST=$WORK/oldsurface
  local SRC=$ST/repo MNT=$ST/mnt
  mkdir -p "$SRC" "$MNT"
  printf 'hello from the source tree' > "$SRC/a.txt"
  ln -sf a.txt "$SRC/link-to-a"

  "$BIN/base-davserve" --root "$SRC" --addr 127.0.0.1:0 > "$ST/davserve.log" 2>&1 &
  local DPID=$!
  local i URL=""
  for i in $(seq 1 100); do grep -q '^URL=' "$ST/davserve.log" 2>/dev/null && break; sleep 0.1; done
  URL=$(grep '^URL=' "$ST/davserve.log" | head -1 | cut -d= -f2-)
  say "=== [old-surface] BASE davserve (pid $DPID) at $URL, NEW client ==="

  timeout 120 "$BIN/bunker" fs mount "$MNT" --url "$URL" --invalidation poll > "$ST/mount.log" 2>&1 &
  local MPID=$!
  for i in $(seq 1 300); do mountpoint -q "$MNT" && break; sleep 0.1; done
  if ! mountpoint -q "$MNT"; then fail "old-surface: mount"; kill "$MPID" "$DPID" 2>/dev/null; return 1; fi

  say "--- ls -l through the mount ---"
  timeout 60 ls -l "$MNT" 2>&1
  say "--- readlink (must be refused BY NAME, never an invented target) ---"
  timeout 30 readlink "$MNT/link-to-a" > "$ST/readlink.out" 2>&1
  local RRC=$?
  say "readlink rc=$RRC out=$(cat "$ST/readlink.out")"
  say "--- ln -s through the mount (must be refused: the surface declares no extension) ---"
  timeout 30 ln -s a.txt "$MNT/created" > "$ST/ln.out" 2>&1
  local LRC=$?
  say "ln -s rc=$LRC out=$(cat "$ST/ln.out")"
  say "--- the mount's own report (the status document the mount keeps) ---"
  sleep 2
  timeout 30 "$BIN/bunker" fs status 2>&1 | grep -E "^symlink|readlinks_total|refusal    :|mountpoint" | head -8
  say "--- what the server holds (nothing new may be there) ---"
  ls -l "$SRC"

  local KIND="$( [ -L "$SRC/link-to-a" ] && echo symlink || echo not-a-symlink )"
  local MAT=0
  while IFS= read -r f; do
    if [ ! -L "$f" ] && [ -f "$f" ] && [ "$(head -c 64 "$f")" = "a.txt" ]; then say "[old-surface] MATERIALISED: $f"; MAT=1; fi
  done < <(find "$SRC" -type f -print)
  say "[old-surface] SUMMARY server_kind=$KIND readlink_rc=$RRC ln_rc=$LRC materialised=$MAT"
  if [ "$KIND" = symlink ] && [ "$RRC" != 0 ] && [ "$LRC" != 0 ] && [ "$MAT" = 0 ]; then
    pass "old-surface: typed correctly, refusals by name, nothing materialised"
  else
    fail "old-surface: expected refusals and no materialisation"
  fi

  timeout 30 fusermount3 -u "$MNT" > /dev/null 2>&1
  sleep 0.5
  kill "$MPID" "$DPID" 2>/dev/null
  say
}

case "${1:-all}" in
  live)
    build "$REPO" new || exit 1
    git_flow "$BIN/bunker" "$BIN/davserve" live fixed
    ;;
  red)
    build /tmp/bfs018/base-repo base || exit 1
    git_flow "$BIN/base-bunker" "$BIN/base-davserve" red filed
    ;;
  old-surface)
    build "$REPO" new || exit 1
    build /tmp/bfs018/base-repo base || exit 1
    old_surface
    ;;
  mutations) mutations ;;
  suites) suites ;;
  all)
    build "$REPO" new || exit 1
    build /tmp/bfs018/base-repo base || exit 1
    git_flow "$BIN/base-bunker" "$BIN/base-davserve" red filed
    git_flow "$BIN/bunker" "$BIN/davserve" live fixed
    old_surface
    mutations
    suites
    ;;
  *) say "unknown arm: $1"; exit 2 ;;
esac

say "WORK kept at $WORK"
if [ "$FAILED" = 0 ]; then say "ARMS: ALL PASS"; else say "ARMS: FAILURES PRESENT"; fi
exit "$FAILED"
