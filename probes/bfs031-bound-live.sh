#!/usr/bin/env bash
# bfs031-bound-live.sh — BFS-031 on the LIVE route: the bound `--cache-max-size`
# names a DIRECTORY, and on a real FUSE mount that directory is inside it — at a
# deliberately tiny 1 KiB bound AND at the default, with `du` as the outside
# witness.
#
# WHAT THIS PROBE IS WRITTEN AGAINST (the row's own reproducer, at 1 KiB):
#   reported used_bytes=26   du(cache dir)=7965   = 7.78x the bound
#   (and 30,689 B = 29.97x in docs/evidence/BFS-012-smallbound-edge.txt, where
#   the refusal log held 40 lines instead of the one the refusal-hold rule
#   records today). The client's own figure described something other than the
#   thing the bound named.
#
# WHAT IT ASSERTS NOW, per bound:
#   1. du(cache dir) <= max_bytes — the bound bounds the directory, measured on
#      the filesystem, in every phase, with NO tolerance;
#   2. |reported cache.dir_bytes − du(cache dir)| <= the STATED tolerance, taken
#      against a FRESH walk (the probe waits for the reported sample's own age to
#      be under 3 s rather than assuming the walk just happened; the walk is
#      reused for up to 30 s by design, and its age is reported beside it);
#   3. the state and the footprint are inside THEIR bounds, and the state's
#      residents are named (status.json, the refusal log, the spills);
#   4. the read path still serves the whole tree and the cache still holds what
#      fits (both figures printed, plus the request count as the read path's
#      cost, and `du` of the cache directory as the eviction path's outcome).
#
# Nothing here touches the real user cache (XDG_CACHE_HOME is a mktemp dir), no
# live daemon is touched, teardown is bounded (explicit PIDs, `timeout` around
# fusermount, no pkill -f anywhere).
#
# Usage: bash probes/bfs031-bound-live.sh [--keep]
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

TMP=$(mktemp -d /tmp/bfs031-live-XXXXXX)
FAILED=0
say() { printf '\n== %s\n' "$*"; }
ok()  { printf '   PASS  %s\n' "$*"; }
bad() { printf '   FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); }

export XDG_CACHE_HOME="$TMP/cache"
mkdir -p "$XDG_CACHE_HOME"

DAV_PID=""
MOUNT_PID=""
TREE=""
MNT=""

cleanup() {
  if [ -n "$MNT" ] && grep -q "$MNT" /proc/mounts 2>/dev/null; then
    timeout 20 fusermount -u "$MNT" >/dev/null 2>&1 || true
  fi
  [ -n "$MOUNT_PID" ] && kill "$MOUNT_PID" >/dev/null 2>&1
  [ -n "$DAV_PID" ] && kill "$DAV_PID" >/dev/null 2>&1
  [ -n "$MOUNT_PID" ] && wait "$MOUNT_PID" 2>/dev/null
  sleep 0.5
  if [ -n "$MNT" ] && grep -q "$MNT" /proc/mounts 2>/dev/null; then
    echo "WARNING: $MNT still mounted after teardown" >&2
    timeout 20 fusermount -u -z "$MNT" >/dev/null 2>&1 || true
  fi
  echo "artifacts left in $TMP (a mktemp -d: a probe that leaves its evidence behind can be re-read)"
}
trap cleanup EXIT

say "the fixture and the surface"
echo "   worktree    : $ROOT"
echo "   head        : $(cd "$ROOT" && git rev-parse --short HEAD 2>/dev/null) $(cd "$ROOT" && git log -1 --format=%s 2>/dev/null | head -c 60)"
echo "   host        : $(uname -n) loadavg $(cat /proc/loadavg)"
echo "   tools       : $(go version | head -c 30)"

( cd "$ROOT" && go build -o "$TMP/bunker" ./cmd/bunker ) || { bad "build bunker"; exit 1; }
( cd "$ROOT" && go build -o "$TMP/davserve" ./probes/davserve ) || { bad "build davserve"; exit 1; }

jqf() { jq -r "$1" < "$2" 2>/dev/null; }
du_of() { du -sb "$1" 2>/dev/null | awk '{print $1}'; }

# fresh_status waits until the mount has taken a FRESH directory measurement, and
# writes the owner-facing document to $1. The reported dir_bytes is a WALK, reused
# for up to DirMeasureTTL (30 s) by design and carrying its own age; comparing a
# 30 s old sample against `du` taken now would be comparing two different
# directories, so the probe waits instead of widening a tolerance.
fresh_status() {
  local out="$1" i age
  for i in $(seq 1 45); do
    "$TMP/bunker" fs status --json > "$out" 2>/dev/null
    age=$(jqf '.cache.dir_measured_age_ms' "$out")
    if [ -n "$age" ] && [ "$age" != "null" ] && [ "$age" -lt 3000 ]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

run_arm() {
  local label="$1"; shift
  local args=("$@")
  TREE="$TMP/tree-$label"; MNT="$TMP/mnt-$label"
  mkdir -p "$TREE/src" "$MNT"
  printf 'a file the arm reads first\n' > "$TREE/src/a.txt"
  printf 'a second file the arm reads\n' > "$TREE/src/b.txt"
  printf 'THE TARGET: sixty-four bytes long for the truncate arm xxxxxxxxxxxxxxx\n' > "$TREE/src/target.txt"
  local treebytes
  treebytes=$(find "$TREE" -type f -printf '%s\n' | awk '{s+=$1} END{print s+0}')

  "$TMP/davserve" --root "$TREE" --addr 127.0.0.1:0 > "$TMP/dav-$label.log" 2>&1 &
  DAV_PID=$!
  local url=""
  for _ in $(seq 1 60); do
    url=$(grep -m1 '^URL=' "$TMP/dav-$label.log" 2>/dev/null | cut -d= -f2-)
    [ -n "$url" ] && break
    sleep 0.2
  done
  [ -n "$url" ] || { bad "[$label] davserve did not start"; cat "$TMP/dav-$label.log"; return 1; }

  # shellcheck disable=SC2086
  "$TMP/bunker" fs mount "$MNT" --url "$url" --concurrency 4 --no-snapshot ${args[@]:-} \
    > "$TMP/mount-$label.log" 2>&1 &
  MOUNT_PID=$!
  local mounted=0
  for _ in $(seq 1 150); do
    if grep -q "$MNT" /proc/mounts 2>/dev/null; then mounted=1; break; fi
    kill -0 "$MOUNT_PID" 2>/dev/null || break
    sleep 0.2
  done
  if [ "$mounted" != 1 ]; then
    bad "[$label] the mount did not come up"
    sed 's/^/     | /' "$TMP/mount-$label.log" | head -20
    return 1
  fi

  local mdir cdir
  mdir=$(ls -td "$XDG_CACHE_HOME"/bunker/fs/* 2>/dev/null | head -1)
  cdir="$mdir/cache"
  local maxb
  maxb=$(jqf '.cache.max_bytes' "$mdir/status.json")
  echo "   [$label] mount dir=$mdir"
  echo "   [$label] cache dir=$cdir bound=${maxb}B tree=$treebytes B"
  if [ -d "$cdir" ]; then
    echo "   [$label] the cache directory holds the cache (mount dir): $(ls -a "$mdir" | tr '\n' ' ')"
    echo "   [$label] ...and the cache directory itself: $(ls -a "$cdir" | tr '\n' ' ')"
  fi

  measure() {
    local name="$1" f="$TMP/status-$label-$2.json"
    du_cache=$(du_of "$cdir"); du_mount=$(du_of "$mdir")
    dirb=$(jqf '.cache.dir_bytes' "$f"); used=$(jqf '.cache.used_bytes' "$f")
    peak=$(jqf '.cache.dir_peak_bytes' "$f"); maxb2=$(jqf '.cache.max_bytes' "$f")
    age=$(jqf '.cache.dir_measured_age_ms' "$f")
    printf '   %-28s du(cache)=%-8s reported dir_bytes=%-8s (age %sms) used=%-8s peak=%-8s du(mount %s)=%s\n' \
      "$name" "$du_cache" "$dirb" "${age:-?}" "$used" "$peak" "${maxb2:-$maxb}" "$du_mount"
  }

  # ── phase 0: baseline ───────────────────────────────────────────────────────
  sleep 1.2
  fresh_status "$TMP/status-$label-0.json" || bad "[$label] no fresh directory measurement at baseline"
  measure "0. baseline" 0
  if [ "${du_cache:-0}" -gt "${maxb:-0}" ]; then
    bad "[$label] the cache directory is over its bound at baseline: du=$du_cache max=$maxb"
  fi

  # ── phase 1: read the whole tree through the mount ──────────────────────────
  local served=0
  for f in "$MNT"/src/*; do
    n=$(wc -c < "$f"); served=$((served + n))
  done
  # Sample the cache while it holds what it just read (the invalidation path may
  # drop it later; what the read path cost is what these figures say).
  local best_entries=0 best_blobs=0 reqs=0
  for i in 1 2 3 4 5; do
    "$TMP/bunker" fs status --json > "$TMP/status-$label-p1.json" 2>/dev/null
    e=$(jqf '.cache.entries' "$TMP/status-$label-p1.json"); b=$(jqf '.cache.blobs_bytes' "$TMP/status-$label-p1.json")
    r=$(jqf '.invalidation.requests_total' "$TMP/status-$label-p1.json")
    [ -n "$e" ] && [ "$e" != "null" ] && [ "$e" -gt "$best_entries" ] && best_entries=$e
    [ -n "$b" ] && [ "$b" != "null" ] && [ "$b" -gt "$best_blobs" ] && best_blobs=$b
    [ -n "$r" ] && [ "$r" != "null" ] && reqs=$r
    sleep 0.3
  done
  if [ "$served" -ne "$treebytes" ]; then
    bad "[$label] the read path served $served bytes, want $treebytes"
  else
    ok "[$label] the read path served the whole tree ($served B) in $reqs requests"
  fi
  if [ "$best_entries" -ge 1 ] && [ "$best_blobs" -gt 0 ]; then
    ok "[$label] the read path cached what it served: entries=$best_entries blobs_bytes=$best_blobs (peak inside the bound below)"
  else
    bad "[$label] the read path cached nothing (entries=$best_entries blobs_bytes=$best_blobs): the successful path is unmeasured"
  fi

  # ── phase 2: refused writes — the ROW'S OWN reproducer drives them ──────────
  # It edits the served file out of band and truncates it through the mount, so
  # every write carries a stale base and is refused as ESTALE, appending a line to
  # the refusal log (the state's edit-volume growth). Its own PASS/FAIL lines are
  # captured here; the numbers below are re-measured by this probe.
  echo "   [$label] running the row's reproducer (docs/evidence/BFS-012-probes/small_bound_edge.py) against this mount"
  MNT="$MNT" CDIR="$cdir" MDIR="$mdir" TREE="$TREE" \
    timeout 900 python3 "$ROOT/docs/evidence/BFS-012-probes/small_bound_edge.py" \
      --mount "$MNT" --cache-dir "$cdir" --mount-dir "$mdir" --tree "$TREE" --refusals 20 \
      > "$TMP/reproducer-$label.out" 2>&1
  REPRO_RC=$?
  sed 's/^/     | /' "$TMP/reproducer-$label.out"
  if [ "$REPRO_RC" = 0 ]; then
    ok "[$label] the row's reproducer returns PASS on this tree (its own checks: the directory is inside the bound, the figures agree, the state is bounded)"
  else
    bad "[$label] the row's reproducer returned rc=$REPRO_RC"
  fi
  refused=$(grep -oE 'refused=[0-9]+' "$TMP/reproducer-$label.out" | head -1 | cut -d= -f2)
  landed=$(grep -oE 'LANDED=[0-9]+' "$TMP/reproducer-$label.out" | head -1 | cut -d= -f2)
  sleep 1.5
  fresh_status "$TMP/status-$label-2.json" || bad "[$label] no fresh directory measurement after the writes"
  measure "2. after the writes" 2

  # 1. THE ROW'S CLAIM: the bound bounds the directory, with no tolerance.
  if [ "${du_cache:-0}" -le "${maxb:-0}" ]; then
    ok "[$label] THE BOUND BOUNDS THE DIRECTORY: du(cache dir)=$du_cache <= max_bytes=$maxb"
  else
    bad "[$label] the bound does not bound the directory: du=$du_cache > max_bytes=$maxb"
  fi
  if [ "${peak:-0}" -le "${maxb:-0}" ]; then
    ok "[$label] the peak the bound is enforced against is inside it: dir_peak_bytes=$peak"
  else
    bad "[$label] dir_peak_bytes=$peak exceeds the bound $maxb"
  fi
  # 2. THE AGREEMENT, against a fresh sample, with the tolerance STATED.
  local tol=4096 delta
  delta=$(( ${dirb:-0} - ${du_cache:-0} )); [ "$delta" -lt 0 ] && delta=$((-delta))
  if [ "${dirb:-0}" -gt 0 ] && [ "$delta" -le "$tol" ]; then
    ok "[$label] the reported dir_bytes=${dirb} agrees with du=${du_cache} (delta=$delta B, tolerance=$tol B, sample age ${age:-?} ms)"
  else
    bad "[$label] reported dir_bytes=${dirb} vs du=${du_cache} (delta=$delta, tolerance=$tol)"
  fi
  # 3. The state and the footprint are inside THEIR bounds, and the state's
  #    residents are the ones the record names.
  local sbytes smax foot fmax dropped statuser cfbytes
  sbytes=$(jqf '.state.bytes' "$TMP/status-$label-2.json"); smax=$(jqf '.state.max_bytes' "$TMP/status-$label-2.json")
  foot=$(jqf '.state.footprint_bytes' "$TMP/status-$label-2.json"); fmax=$(jqf '.state.footprint_max_bytes' "$TMP/status-$label-2.json")
  dropped=$(jqf '.state.conflicts_dropped_total' "$TMP/status-$label-2.json")
  statuser=$(jqf '.state.status_bytes' "$TMP/status-$label-2.json")
  cfbytes=$(jqf '.state.conflicts_bytes' "$TMP/status-$label-2.json")
  local st_size cf_size
  st_size=$(stat -c %s "$mdir/status.json" 2>/dev/null || echo 0)
  cf_size=$(stat -c %s "$mdir/conflicts.jsonl" 2>/dev/null || echo 0)
  echo "   [$label] writes: refused=$refused landed=$landed; state: status=$statuser/$st_size conflicts=$cfbytes/$cf_size dropped=$dropped footprint=$foot/$fmax"
  # The edit-volume grower is BOTH bounded and visible: it recorded the refusals
  # and it is inside its own cap.
  if [ "${cfbytes:-0}" -gt 0 ]; then
    ok "[$label] the refusal log recorded the refusals ($cfbytes B on disk) and is inside its cap ($(jqf '.state.conflicts_max_bytes' "$TMP/status-$label-2.json") B)"
  else
    bad "[$label] the refusal log is empty after $refused refused writes: the state's edit-volume growth is not being recorded"
  fi
  if [ "${sbytes:-0}" -le "${smax:-0}" ]; then
    ok "[$label] the state is inside its own bound: $sbytes <= $smax"
  else
    bad "[$label] the state is over its bound: $sbytes > $smax"
  fi
  if [ "${foot:-0}" -le "${fmax:-0}" ]; then
    ok "[$label] the footprint (cache + state + write-buffer budget) is inside its bound: $foot <= $fmax"
  else
    bad "[$label] the footprint is over its bound: $foot > $fmax"
  fi
  # The status figure describes the document that was on disk when the mount
  # measured it; the mount rewrites it once a second, so a small positive delta is
  # the document's own growth between the two reads. 4096 B is one block, and the
  # observed delta is printed.
  local sdelta=$(( ${statuser:-0} - st_size )); [ "$sdelta" -lt 0 ] && sdelta=$((-sdelta))
  if [ "$sdelta" -le 4096 ]; then
    ok "[$label] the reported status_bytes=$statuser describes status.json ($st_size B, delta=$sdelta: the mount rewrites it once a second)"
  else
    bad "[$label] the reported status_bytes=$statuser does not describe status.json ($st_size B)"
  fi

  # The owner-facing surface, as a person reads it.
  "$TMP/bunker" fs status 2>/dev/null | sed 's/^/     | /' | head -32

  if grep -q "$MNT" /proc/mounts 2>/dev/null; then timeout 20 fusermount -u "$MNT" >/dev/null 2>&1 || true; fi
  kill "$MOUNT_PID" >/dev/null 2>&1; kill "$DAV_PID" >/dev/null 2>&1
  wait "$MOUNT_PID" 2>/dev/null
  sleep 0.5
  MOUNT_PID=""; DAV_PID=""
  return 0
}

say "ARM A: a 1 KiB bound (the row's bound)"
run_arm tiny --cache-max-size 1024

say "ARM B: the DEFAULT bound (nothing passed: the real default path)"
run_arm default

say "ARM C: the 40-distinct-path refusal burst — the workload that produced the 29.97x"
# One refusal line per refused PATH (BFS-033's refusal-hold rule), so the log's width
# is the number of DISTINCT refused paths: the row's 29.97x came from a burst like
# this one (27,836 B of log on that tree). This arm runs it against the fixed build
# and asserts the same property at the same 1 KiB bound.
run_burst_arm() {
  local label=burst
  TREE="$TMP/tree-$label"; MNT="$TMP/mnt-$label"
  mkdir -p "$TREE/src" "$MNT"
  local i
  for i in $(seq -w 0 39); do
    printf 'BURST-FILE-%s: sixty-four bytes of content for the burst arm xxxxxxxxxxxxx\n' "$i" > "$TREE/src/f$i.txt"
  done
  "$TMP/davserve" --root "$TREE" --addr 127.0.0.1:0 > "$TMP/dav-$label.log" 2>&1 &
  DAV_PID=$!
  local url=""
  for _ in $(seq 1 60); do
    url=$(grep -m1 '^URL=' "$TMP/dav-$label.log" 2>/dev/null | cut -d= -f2-)
    [ -n "$url" ] && break
    sleep 0.2
  done
  [ -n "$url" ] || { bad "[$label] davserve did not start"; return 1; }
  "$TMP/bunker" fs mount "$MNT" --url "$url" --concurrency 4 --no-snapshot --cache-max-size 1024 \
    > "$TMP/mount-$label.log" 2>&1 &
  MOUNT_PID=$!
  local mounted=0
  for _ in $(seq 1 150); do
    if grep -q "$MNT" /proc/mounts 2>/dev/null; then mounted=1; break; fi
    sleep 0.2
  done
  if [ "$mounted" != 1 ]; then bad "[$label] the mount did not come up"; return 1; fi
  local mdir cdir
  mdir=$(ls -td "$XDG_CACHE_HOME"/bunker/fs/* 2>/dev/null | head -1)
  cdir="$mdir/cache"
  local maxb; maxb=$(jqf '.cache.max_bytes' "$mdir/status.json")
  # 40 distinct paths. READ each one through the mount FIRST: that is what gives the
  # client its base for the path. Then edit the served file out of band, and only
  # then truncate through the mount — so the write carries a stale base and is
  # refused on THIS path (one log line per path).
  for i in $(seq -w 0 39); do
    head -c 64 "$MNT/src/f$i.txt" > /dev/null 2>&1 || true
  done
  sleep 2
  for i in $(seq -w 0 39); do
    printf 'SERVER-EDIT-%s: the out-of-band content for this path xxxxxxxxxxxxxxxxxxxx\n' "$i" > "$TREE/src/f$i.txt"
    timeout 20 python3 -c 'import os,sys; os.truncate(sys.argv[1], 32)' "$MNT/src/f$i.txt" 2>/dev/null || true
  done
  sleep 3
  fresh_status "$TMP/status-$label.json" || bad "[$label] no fresh directory measurement"
  local du_cache du_mount dirb st_size cf_size cfbytes sbytes smax foot fmax used
  du_cache=$(du_of "$cdir"); du_mount=$(du_of "$mdir")
  dirb=$(jqf '.cache.dir_bytes' "$TMP/status-$label.json"); used=$(jqf '.cache.used_bytes' "$TMP/status-$label.json")
  st_size=$(stat -c %s "$mdir/status.json" 2>/dev/null || echo 0)
  cf_size=$(stat -c %s "$mdir/conflicts.jsonl" 2>/dev/null || echo 0)
  cfbytes=$(jqf '.state.conflicts_bytes' "$TMP/status-$label.json")
  sbytes=$(jqf '.state.bytes' "$TMP/status-$label.json"); smax=$(jqf '.state.max_bytes' "$TMP/status-$label.json")
  foot=$(jqf '.state.footprint_bytes' "$TMP/status-$label.json"); fmax=$(jqf '.state.footprint_max_bytes' "$TMP/status-$label.json")
  echo "   [$label] 40 distinct refused paths; the refusal log on disk: $cf_size B (reported $cfbytes B)"
  echo "   [$label] the cache directory : du=$du_cache reported dir_bytes=$dirb used=$used (bound $maxb)"
  echo "   [$label] the state + footprint: status.json=$st_size du(mount dir)=$du_mount state=$sbytes/$smax footprint=$foot/$fmax"
  if [ "${du_cache:-0}" -le "${maxb:-0}" ]; then
    ok "[$label] THE BOUND BOUNDS THE DIRECTORY under the burst: du(cache dir)=$du_cache <= $maxb (the ~$cf_size B of refusals is in the STATE, not in it)"
  else
    bad "[$label] the bound does not bound the directory under the burst: du=$du_cache > $maxb"
  fi
  local delta=$(( ${dirb:-0} - ${du_cache:-0 } )); [ "$delta" -lt 0 ] && delta=$((-delta))
  if [ "${dirb:-0}" -gt 0 ] && [ "$delta" -le 4096 ]; then
    ok "[$label] the reported dir_bytes=${dirb} agrees with du=${du_cache} (delta=$delta B)"
  else
    bad "[$label] reported dir_bytes=${dirb} vs du=${du_cache} (delta=$delta)"
  fi
  if [ "${sbytes:-0}" -le "${smax:-0}" ] && [ "${foot:-0}" -le "${fmax:-0}" ]; then
    ok "[$label] the state ($sbytes <= $smax) and the footprint ($foot <= $fmax) are inside their bounds: the edit-volume growth is bounded AND visible"
  else
    bad "[$label] the state or the footprint is over its bound: state=$sbytes/$smax footprint=$foot/$fmax"
  fi
  if [ "$cf_size" -gt 20000 ]; then
    ok "[$label] the burst grew the refusal log to $cf_size B — the width the row's own fixture reached was 27,836 B — and it is in the STATE, inside its own bound"
  else
    echo "   [$label] NOTE the refusal log is $cf_size B: this run's burst did not reach the width the row's fixture did (27,836 B), so the multiple it would have produced is smaller"
  fi
  if grep -q "$MNT" /proc/mounts 2>/dev/null; then timeout 20 fusermount -u "$MNT" >/dev/null 2>&1 || true; fi
  kill "$MOUNT_PID" >/dev/null 2>&1; kill "$DAV_PID" >/dev/null 2>&1
  wait "$MOUNT_PID" 2>/dev/null; sleep 0.5
  MOUNT_PID=""; DAV_PID=""
  return 0
}
run_burst_arm

say "VERDICT"
if [ "$FAILED" = 0 ]; then
  echo "LIVE-PASS: at 1 KiB and at the default the cache directory is INSIDE the bound"
  echo "it names, the reported figure agrees with du within 4096 B, and the state whose"
  echo "bytes are not the cache is bounded and named in its own right. $TMP"
  exit 0
fi
echo "LIVE-FAIL: $FAILED check(s) failed"
exit 1
