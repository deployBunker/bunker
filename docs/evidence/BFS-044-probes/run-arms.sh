#!/usr/bin/env bash
# run-arms.sh — the BFS-044 arm battery.
#
# Six arms, each in its own fresh run directory and its own freshly generated
# served tree, each with the expectation encoded so the EXIT CODE is the verdict
# (BFS-030's convention: not a human reading a transcript).
#
#   defaults        the stock mount. Correctness must pass.
#   all-off         EVERY invalidation and hot-file feature at its most degraded
#                   legal value (push -> poll, snapshot OFF, hot OFF, and every
#                   hot bound at the smallest value that validates). Correctness
#                   must pass UNCHANGED — this is the brief's hardest test.
#   cadence-control the same as `defaults` but with the poll cadence pushed to an
#                   hour. The propagation cell must go RED here: it is the arm
#                   that proves that cell can fail, so its green elsewhere means
#                   something.
#   entries-4       byte bound at its default, ENTRY bound 4. 40 files read.
#   entries-max     byte bound at its default, ENTRY bound at the default. Same
#                   40 files: the pair isolates the entry bound.
#   byte-1k         a 1 KiB BYTE bound and the default entry bound. This is
#                   BFS-031's premise measured at this head: the directory is
#                   larger than the byte bound while the client's own figures
#                   stay inside it.
#
# usage: run-arms.sh --bin BUNKER [--davserve DS] [--outdir DIR] [--work DIR]
#                    [--arms "defaults all-off"]
set -uo pipefail

BIN=""; DS=""; OUTDIR=""; WORK=/tmp/bfs044; ARMS="defaults all-off all-off-nosnap cadence-control entries-4 entries-max byte-1k misconfigured"
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --outdir) OUTDIR="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --arms) ARMS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] || { echo "--bin required" >&2; exit 2; }
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
[ -n "$DS" ] || DS="$REPO/bin/davserve"
[ -n "$OUTDIR" ] || OUTDIR="$REPO/docs/evidence/BFS-044-probes"
mkdir -p "$OUTDIR" "$WORK"

# EVERY invalidation and hot-file FEATURE at its most degraded LEGAL value.
# Values are legal on purpose: the point is not to break the configuration but to
# switch every optional behaviour off inside it. The one-call snapshot stays ON
# here (it is not an invalidate/hot knob, and with it on the cache is actually
# exercised); `--no-snapshot` is the extra arm below.
ALL_OFF="--invalidation poll --poll-interval 2000ms \
--cache-max-entries 4 --cache-max-entry-bytes 1024 \
--hot.enabled=false --hot.read-weight 1 --hot.edit-weight 8 --hot.decay 1 \
--hot.decay-step 1s --hot.score-ceiling 1 --hot.read-touch-window 1s \
--hot.flush-interval 1s --hot.max-entries 1 --hot.max-tracker-bytes 1 \
--hot.max-file-bytes 1024 --hot.queue-depth 1 --hot.queue-max-wait 1s \
--hot.max-concurrent-refresh 1 --hot.pool-share 1/64 --hot.backoff-base-ms 1 \
--hot.backoff-max-ms 1 --hot.backoff-factor 1 --hot.backoff-jitter none \
--hot.refresh-deadline 1s --hot.reacquire-window 1s --hot.yield-after 1ms \
--hot.stop-deadline 150ms --hot.tick-interval 100ms --hot.pool-pressure-ticks 1"

# The same, with the one-call node-tree snapshot disabled as well: the strongest
# legal degradation, where every directory read falls back to the standard
# PROPFIND path. Correctness still may not move.
ALL_OFF_NOSNAP="$ALL_OFF --no-snapshot"

# Every hot knob at a NON-default value with the feature ARMED, for the
# fast-path comparison: the surface must cost nothing until something reads it,
# so the request counts must be identical to the stock mount's.
HOT_ARMED="--hot.enabled --hot.decay 0.85 --hot.decay-step 200ms \
--hot.score-ceiling 5000 --hot.read-touch-window 2s --hot.flush-interval 15s \
--hot.max-entries 2048 --hot.max-tracker-bytes 524288 --hot.max-file-bytes 4194304 \
--hot.queue-depth 128 --hot.queue-max-wait 120s --hot.max-concurrent-refresh 4 \
--hot.pool-share 1/4 --hot.backoff-base-ms 100 --hot.backoff-max-ms 20000 \
--hot.backoff-factor 3 --hot.backoff-jitter none --hot.refresh-deadline 5s \
--hot.reacquire-window 20s --hot.yield-after 100ms --hot.stop-deadline 2s \
--hot.tick-interval 500ms --hot.pool-pressure-ticks 3"

FILES=40
FAILED=0

make_tree() {
  local dir="$1" n="$2"
  mkdir -p "$dir"
  for i in $(seq 1 "$n"); do
    printf 'file-%03d-payload-%s\n' "$i" "$(head -c 32 /dev/zero | tr '\0' x)" > "$dir/f$(printf '%03d' "$i").txt"
  done
}

arm() {
  local label="$1" mode="$2" flags="$3" outfile="$4" n="${5:-$FILES}"
  local tree="$WORK/tree-$label"
  make_tree "$tree" "$n"
  local out="$OUTDIR/$outfile"
  {
    echo "### arm=$label mode=$mode"
    echo "### generated $(date -u +%Y-%m-%dT%H:%M:%SZ) on $(uname -n), loadavg $(cut -d' ' -f1-3 /proc/loadavg)"
    echo
  } > "$out"
  if [ "$mode" = census ]; then
    bash "$HERE/mount-arm.sh" --label "$label" --tree "$tree" --bin "$BIN" --davserve "$DS" \
      --work "$WORK" --flags "$flags" --reader bash \
      --reader-args "$HERE/census.sh --files $n" \
      >> "$out" 2>&1
  elif [ "$mode" = fastpath ]; then
    bash "$HERE/mount-arm.sh" --label "$label" --tree "$tree" --bin "$BIN" --davserve "$DS" \
      --work "$WORK" --flags "$flags" --reader bash \
      --reader-args "$HERE/fastpath.sh --files $n" \
      >> "$out" 2>&1
  else
    local prop=visible
    [ "$mode" = delayed ] && prop=delayed
    bash "$HERE/mount-arm.sh" --label "$label" --tree "$tree" --bin "$BIN" --davserve "$DS" \
      --work "$WORK" --flags "$flags" --reader bash \
      --reader-args "$HERE/cells.sh --files $n --propagation $prop" \
      >> "$out" 2>&1
  fi
  local rc=$?
  if [ "$rc" = 0 ]; then echo "ARM $label: PASS (rc=0)"; else echo "ARM $label: FAIL (rc=$rc) -> $out"; FAILED=1; fi
}

for a in $ARMS; do
  case "$a" in
    defaults)        arm defaults        cells " "                                    BFS-044-arm-defaults.txt;;
    all-off)         arm all-off         cells "$ALL_OFF"                             BFS-044-arm-all-features-off.txt;;
    all-off-nosnap)  arm all-off-nosnap  cells "$ALL_OFF_NOSNAP"                      BFS-044-arm-all-off-no-snapshot.txt;;
    cadence-control) arm cadence-control cells "--invalidation poll --poll-interval 3600s" BFS-044-arm-cadence-control.txt;;
    entries-4)       arm entries-4       census "--cache-max-entries 4"                BFS-044-arm-entry-bound-4.txt;;
    entries-max)     arm entries-max     census "--cache-max-entries 16384"            BFS-044-arm-entry-bound-max.txt;;
    byte-1k)         arm byte-1k         census "--cache-max-size 1024"                BFS-044-arm-byte-bound-1k.txt;;
    misconfigured)   arm misconfigured   census "--cache-max-entry-bytes 1024"         BFS-044-arm-misconfigured.txt;;
    fastpath)        arm fastpath        fastpath " "                                BFS-044-arm-fastpath.txt 200;;
    fastpath-armed)  arm fastpath-armed  fastpath "$HOT_ARMED"                      BFS-044-arm-fastpath-hot-armed.txt 200;;
    *) echo "unknown arm $a" >&2; FAILED=1;;
  esac
done
exit $FAILED
