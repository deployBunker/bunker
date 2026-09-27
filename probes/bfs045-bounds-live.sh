#!/usr/bin/env bash
# bfs045-bounds-live.sh — BFS-045 on the LIVE route: a real FUSE mount, the real
# read path, and the reported figures checked against an independent measurement.
#
# What only a live run can show, and what this probe therefore asserts:
#
#   1. THE OVER-CAP READ MOVES THE COUNTER FROM THE LIVE PATH. BFS-032's defect
#      was a counter the live read path could never reach (a pre-filter sat above
#      Cache.Insert). Here a file bigger than --cache-max-entry-bytes is read
#      THROUGH THE MOUNT and the counter must move, by name.
#   2. THE REPORTED FIGURE AGREES WITH `du`. BFS-031's defect was a reported
#      figure that described something other than the thing it claimed to bound,
#      so the cache directory is measured twice: once by the client (reported in
#      the status document, with its own sample age) and once by `du` here.
#   3. EVERY DECLARED BOUND IS VISIBLE with its value, and the figures this build
#      cannot source are ABSENT WITH A REASON rather than zero.
#   4. THE NULL-RULE, on the live document: every reason field the client authors
#      opens with one of the four classes.
#
# Nothing here is written to the real user cache (XDG_CACHE_HOME is a temp dir),
# no live daemon is touched (davserve serves the same handler with no agent
# lifecycle), and teardown is bounded: `timeout` around fusermount, explicit PIDs
# for every process this probe starts, and no pkill -f anywhere.
#
# Usage: bash bfs045-bounds-live.sh [--keep]
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

TMP=$(mktemp -d /tmp/bfs045-live-XXXXXX)
export XDG_CACHE_HOME="$TMP/cache"
TREE="$TMP/tree"
MNT="$TMP/mnt"
mkdir -p "$TREE" "$MNT" "$XDG_CACHE_HOME"

DAV_PID=""
MOUNT_PID=""

cleanup() {
  if grep -q "$MNT" /proc/mounts 2>/dev/null; then
    timeout 20 fusermount -u "$MNT" >/dev/null 2>&1 || true
  fi
  [ -n "$MOUNT_PID" ] && kill "$MOUNT_PID" >/dev/null 2>&1
  [ -n "$DAV_PID" ] && kill "$DAV_PID" >/dev/null 2>&1
  wait "$MOUNT_PID" 2>/dev/null
  sleep 1
  if grep -q "$MNT" /proc/mounts 2>/dev/null; then
    echo "WARNING: $MNT still mounted after teardown" >&2
    timeout 20 fusermount -u -z "$MNT" >/dev/null 2>&1 || true
  fi
  if [ "$KEEP" = 1 ]; then
    echo "artifacts kept in $TMP"
  else
    echo "artifacts left in $TMP (nothing is deleted: a probe that leaves its"
    echo "evidence behind can be re-read; the directory is a mktemp -d)"
  fi
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }
ok()  { printf '   PASS  %s\n' "$*"; }
bad() { printf '   FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); }
FAILED=0

# ── the fixture ──────────────────────────────────────────────────────────────
printf 'a small file that fits the 64-byte per-entry cap\n' > "$TREE/small.txt"
# An over-cap file: 2000 bytes against a 64-byte cap.
for i in $(seq 1 100); do printf 'OVER-CAP-LINE-%04d-xxxxxxxxxxxxxxxxxxxx\n' "$i"; done > "$TREE/big.bin"
SMALL_BYTES=$(stat -c %s "$TREE/small.txt")
BIG_BYTES=$(stat -c %s "$TREE/big.bin")

say "fixture"
echo "   tree        : $TREE (small.txt=$SMALL_BYTES B, big.bin=$BIG_BYTES B)"
echo "   cache home  : $XDG_CACHE_HOME (a temp dir: the real user cache is untouched)"
echo "   tools       : $(go version | head -c 40)"

# ── build and start the surface ──────────────────────────────────────────────
say "1. build and start the landed surface (davserve: the real handler, no agent lifecycle)"
( cd "$ROOT" && go build -o "$TMP/bunker" ./cmd/bunker ) || { bad "build bunker"; exit 1; }
( cd "$ROOT" && go build -o "$TMP/davserve" ./probes/davserve ) || { bad "build davserve"; exit 1; }
"$TMP/davserve" --root "$TREE" --addr 127.0.0.1:0 > "$TMP/dav.log" 2>&1 &
DAV_PID=$!
URL=""
for _ in $(seq 1 50); do
  URL=$(grep -m1 '^URL=' "$TMP/dav.log" 2>/dev/null | cut -d= -f2-)
  [ -n "$URL" ] && break
  sleep 0.2
done
[ -n "$URL" ] || { bad "davserve did not print a URL"; cat "$TMP/dav.log"; exit 1; }
ok "surface serving at $URL (pid $DAV_PID)"

# ── mount with a deliberately tiny per-entry cap ─────────────────────────────
say "2. mount with --cache-max-entry-bytes 64 (so a normal read IS over the cap)"
"$TMP/bunker" fs mount "$MNT" --url "$URL" --concurrency 4 \
  --cache-max-size 8388608 --cache-max-entry-bytes 64 --no-snapshot \
  > "$TMP/mount.log" 2>&1 &
MOUNT_PID=$!
MOUNTED=0
for _ in $(seq 1 100); do
  if grep -q "$MNT" /proc/mounts 2>/dev/null; then MOUNTED=1; break; fi
  sleep 0.2
done
if [ "$MOUNTED" != 1 ]; then
  bad "the mount did not come up"
  cat "$TMP/mount.log"
  exit 1
fi
ok "mounted at $MNT (pid $MOUNT_PID) — /dev/fuse is real, this is the live read path"

# ── the live reads ───────────────────────────────────────────────────────────
say "3. read a file the per-entry cap REFUSES, and one it accepts"
BIG_READ=$(cat "$MNT/big.bin" | wc -c)
SMALL_READ=$(cat "$MNT/small.txt" | wc -c)
if [ "$BIG_READ" = "$BIG_BYTES" ]; then
  ok "the over-cap read SUCCEEDED and served the whole file ($BIG_READ bytes): the cap decides what is CACHED, never what is served"
else
  bad "the over-cap read served $BIG_READ bytes, want $BIG_BYTES"
fi
if [ "$SMALL_READ" = "$SMALL_BYTES" ]; then
  ok "the under-cap read served the whole file ($SMALL_READ bytes)"
else
  bad "the under-cap read served $SMALL_READ bytes, want $SMALL_BYTES"
fi

# ── the reported figures, and the independent measurement ────────────────────
say "4. the status document the mount keeps fresh"
sleep 3   # the mount writes it on a 1 s cadence
"$TMP/bunker" fs status --json > "$TMP/status.json" 2>"$TMP/status.err" || { bad "bunker fs status"; cat "$TMP/status.err"; }
MOUNT_DIR=$(ls -d "$XDG_CACHE_HOME"/bunker/fs/* 2>/dev/null | head -1)
[ -n "$MOUNT_DIR" ] || { bad "no mount directory under $XDG_CACHE_HOME/bunker/fs"; exit 1; }

jqf() { jq -r "$1" < "$TMP/status.json" 2>/dev/null; }

echo "   reported cache block:"
jq -c '.cache' < "$TMP/status.json" | sed 's/^/     /'

OVERSIZE=$(jqf '.cache.oversize_bypasses')
OVERCAP=$(jqf '.cache.bypass_reasons.over_entry_cap')
CAP=$(jqf '.cache.max_entry_bytes')
MAXBYTES=$(jqf '.cache.max_bytes')
USED=$(jqf '.cache.used_bytes')
RESERVED=$(jqf '.cache.reserved_bytes')
DIRBYTES=$(jqf '.cache.dir_bytes')
DIRAGE=$(jqf '.cache.dir_measured_age_ms')

# 1. THE LIVE COUNTER (BFS-032's acceptance, from the reporting side).
if [ "${OVERSIZE:-0}" -ge 1 ]; then
  ok "LIVE: oversize_bypasses=$OVERSIZE (the read path moved it — BFS-032's counter is reachable)"
else
  bad "LIVE: oversize_bypasses=$OVERSIZE after an over-cap read: the counter did not move from the live path"
fi
if [ "${OVERCAP:-0}" -ge 1 ]; then
  ok "LIVE: bypass_reasons.over_entry_cap=$OVERCAP (counted BY REASON, at the site that refused)"
else
  bad "LIVE: bypass_reasons.over_entry_cap=$OVERCAP"
fi
# 2. THE DECLARED BOUND IS VISIBLE.
if [ "${CAP:-0}" = 64 ]; then
  ok "the bound is reported: max_entry_bytes=$CAP (the cap the refusal was counted against)"
else
  bad "max_entry_bytes=$CAP, want the configured 64"
fi
if [ "${MAXBYTES:-0}" = 8388608 ]; then
  ok "max_bytes=$MAXBYTES (the byte bound, reported)"
else
  bad "max_bytes=$MAXBYTES, want 8388608"
fi
# 3. THE REPORTED FIGURE vs du — the independent measurement (BFS-031).
DU_BYTES=$(du -sb "$MOUNT_DIR" 2>/dev/null | awk '{print $1}')
DELTA=$(( ${DIRBYTES:-0} - ${DU_BYTES:-0} ))
if [ "$DELTA" -lt 0 ]; then DELTA=$((-DELTA)); fi
echo "   independent measurement: du -sb $MOUNT_DIR = $DU_BYTES"
echo "   reported                : dir_bytes=$DIRBYTES (sample age ${DIRAGE} ms), used_bytes=$USED, reserved_bytes=$RESERVED"
# The reported figure is a SAMPLE (its age is reported beside it) and the mount
# rewrites index.json/status.json between the sample and the du, so the two are
# compared with a stated tolerance rather than pretending they are simultaneous.
TOLERANCE=$(( 8 * 1024 ))
if [ "${DIRBYTES:-0}" -gt 0 ] && [ "$DELTA" -le "$TOLERANCE" ]; then
  ok "the reported dir_bytes and du agree within $TOLERANCE B (delta=$DELTA)"
else
  bad "reported dir_bytes=$DIRBYTES vs du=$DU_BYTES (delta=$DELTA, tolerance=$TOLERANCE)"
fi
if [ "${USED:-0}" -le "${MAXBYTES:-0}" ] && [ "${RESERVED:-0}" -le "${MAXBYTES:-0}" ]; then
  ok "both accounts are inside the bound: used=$USED reserved=$RESERVED <= max=$MAXBYTES"
else
  bad "an account exceeded the bound: used=$USED reserved=$RESERVED max=$MAXBYTES"
fi

# ── the invalidation record ──────────────────────────────────────────────────
say "5. the invalidation record: every bound visible, a null explained"
jq -c '.invalidation | {mode, mechanism, channel_available, seq, requests_total, failures_total, content_age, server: (.server.state // null), server_reason, liveness, liveness_reason, refresh}' \
  < "$TMP/status.json" | sed 's/^/     /'

MECH=$(jqf '.invalidation.mechanism')
if [ -n "$MECH" ] && [ "$MECH" != "null" ]; then
  ok "the mechanism in force is named: $MECH (never an implied watcher)"
else
  bad "the record does not name a mechanism"
fi
CAGE=$(jqf '.invalidation.content_age.age_ms')
if [ -n "$CAGE" ] && [ "$CAGE" != "null" ]; then
  BOUND=$(jqf '.invalidation.content_age.bound_ms')
  WITHIN=$(jqf '.invalidation.content_age.within_bound')
  SRC=$(jqf '.invalidation.content_age.evidence_from')
  ok "THE CONTENT-AGE BOUND IS REPORTED: age_ms=$CAGE bound_ms=$BOUND ($SRC) within_bound=$WITHIN"
else
  REASON=$(jqf '.invalidation.content_age_reason')
  bad "no content age, and the reason is: ${REASON:-<empty>}"
fi
REQS=$(jqf '.invalidation.requests_total')
if [ "${REQS:-0}" -ge 1 ]; then
  ok "the invalidation path's own requests are counted: requests_total=$REQS failures_total=$(jqf '.invalidation.failures_total')"
else
  bad "requests_total=$REQS: the channel never answered, which the record must be able to show"
fi
# The hot-refresh queue is not in this build: ABSENT WITH A REASON, never 0.
QUEUEDEPTH=$(jq -c '.invalidation.refresh.queue_depth' < "$TMP/status.json")
QREASON=$(jqf '.invalidation.refresh.absent_reason')
if [ "$QUEUEDEPTH" = "null" ] && [ -n "$QREASON" ] && [ "$QREASON" != "null" ]; then
  ok "the absent hot-refresh queue is null with a reason: $QUEUEDEPTH — $QREASON"
else
  bad "queue_depth=$QUEUEDEPTH absent_reason=$QREASON: an uncountable queue must be null WITH a reason"
fi

# ── the null-reason rule, on the live document ───────────────────────────────
say "6. every client-authored reason on the live document opens with a vocabulary class"
REASONS=$(jq -r '[.cache.dir_bytes_reason, .invalidation.server_reason, .invalidation.content_age_reason,
                  .invalidation.content_age.bound_reason, .invalidation.liveness_reason,
                  .invalidation.refresh.absent_reason] | .[] | select(. != null and . != "")' < "$TMP/status.json")
if [ -z "$REASONS" ]; then
  echo "   (no reason field is set on this run: every figure was sourced)"
fi
while IFS= read -r r; do
  [ -z "$r" ] && continue
  case "$r" in
    disabled:*|unknown:*|not_published:*|no_sample:*) ok "reason: $r" ;;
    *) bad "reason without a vocabulary class: $r" ;;
  esac
done <<< "$REASONS"

say "6b. the SAME document as a person reads it (bunker fs status)"
"$TMP/bunker" fs status 2>/dev/null | sed 's/^/     /'

# ── the counters move on a live out-of-band edit ─────────────────────────────
say "7. an out-of-band edit on the served tree moves the channel's figures"
BEFORE_SEQ=$(jqf '.invalidation.seq')
BEFORE_REQ=$(jqf '.invalidation.requests_total')
BEFORE_OBS=$(jqf '.invalidation.content_age.observations_total')
printf 'edited on the agent at %s\n' "$(date +%s)" > "$TREE/small.txt"
sleep 6   # the declared poll interval is 2 s
"$TMP/bunker" fs status --json > "$TMP/status2.json" 2>/dev/null
AFTER_SEQ=$(jq -r '.invalidation.seq' < "$TMP/status2.json")
AFTER_REQ=$(jq -r '.invalidation.requests_total' < "$TMP/status2.json")
AFTER_OBS=$(jq -r '.invalidation.content_age.observations_total' < "$TMP/status2.json")
echo "   seq            : $BEFORE_SEQ -> $AFTER_SEQ"
echo "   requests_total : $BEFORE_REQ -> $AFTER_REQ"
echo "   observations   : $BEFORE_OBS -> $AFTER_OBS"
if [ "${AFTER_REQ:-0}" -gt "${BEFORE_REQ:-0}" ]; then
  ok "requests_total moved ($BEFORE_REQ -> $AFTER_REQ): the channel is asking, and it is counted"
else
  bad "requests_total did not move ($BEFORE_REQ -> $AFTER_REQ)"
fi
if [ "${AFTER_OBS:-0}" -gt "${BEFORE_OBS:-0}" ]; then
  ok "observations_total moved ($BEFORE_OBS -> $AFTER_OBS): the client re-established evidence"
else
  bad "observations_total did not move ($BEFORE_OBS -> $AFTER_OBS)"
fi
# The edited file must read back through the mount (correctness must not depend on
# any of this accounting).
EDITED=$(cat "$MNT/small.txt")
case "$EDITED" in
  edited\ on\ the\ agent*) ok "the edit is visible through the mount: $EDITED" ;;
  *) bad "the edit is NOT visible through the mount: $EDITED" ;;
esac

say "VERDICT"
if [ "$FAILED" = 0 ]; then
  echo "LIVE-PASS: the live read path moves the counters, the reported figures agree"
  echo "with an independent measurement, every bound is visible, and every null"
  echo "carries a reason. $TMP"
  exit 0
fi
echo "LIVE-FAIL: $FAILED check(s) failed"
exit 1
