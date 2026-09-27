#!/usr/bin/env bash
# BFS-024-probe.sh — the row's own scenario, run against the CURRENT tree.
#
# THE ROW, as filed: "a path already read through the mount is NOT invalidated
# when the agent replaces it" (docs/evidence/BFS-009-visibility-nongit.txt, arm
# P2b: mount served the PRE-EDIT 22 bytes where NATIVE read 45, VERDICT: STALE,
# with evictions=0 bypass=0 hits=0 and resyncs_total=0 — nothing moved).
#
# WHAT THIS PROBE DOES NOT DO: it does not decide the row. It runs the scenario
# and prints, per OUT-OF-BAND writer class, (a) what the mount serves at +0 ms,
# +400 ms (the control reads), and (b) WHEN it first serves the new bytes, on ONE
# clock, until it does or the hold expires. Coverage is reported per class
# because the landed mechanisms have different reach, and a named escape is
# worth more than a general fix.
#
# ARM LADDER. Every writer below is a SHELL WRITE ON THE SERVED TARGET, never
# WebDAV — a WebDAV write would be the surface talking to itself. Every path is
# created BEFORE the mount binds, so "already read through the mount" is true by
# construction and not by a race with the snapshot.
#
#   W1  replace, DIFFERENT length                the row's own shape
#   W2  replace, SAME length, SAME mtime         the adversarial metadata-equal
#                                                shape (ctime still moves)
#   W3  replace, SAME length, NEW mtime          the ordinary same-size edit
#   W4  unlink + recreate                        new inode, same name
#   W5  mv a new file over it                    atomic-rename replace
#   N1  write THROUGH the mount                  in-band control (new file)
#   N2  NEVER-READ path, replaced                BFS-025's shape, for contrast
#   NEG a FRESH mount, its own cache dir         fixture control: its first read
#                                                of every arm path must be the
#                                                NEW bytes, so a stale answer on
#                                                mount 1 is OUR cache
#
# usage: BFS-024-probe.sh --tree DIR --bin PATH --davserve PATH [--hold S]
set -uo pipefail

TREE=""; BIN=""; DS=""; HOLD=14; POLL_MS=100
while [ $# -gt 0 ]; do
  case "$1" in
    --tree) TREE="$2"; shift 2;;
    --bin) BIN="$2"; shift 2;;
    --davserve) DS="$2"; shift 2;;
    --hold) HOLD="$2"; shift 2;;
    --poll-ms) POLL_MS="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$TREE" ] && [ -n "$BIN" ] && [ -n "$DS" ] || { echo "missing arg" >&2; exit 2; }

OUT="$(mktemp -d -t bfs024-XXXXXX)"
export XDG_CACHE_HOME="$OUT/xdg"
MNT="$OUT/mnt";  mkdir -p "$MNT"
MNT2="$OUT/mnt2"; mkdir -p "$MNT2"
echo "out       = $OUT"
echo "tree      = $TREE"
echo "binaries  = $BIN / $DS"
echo "date      = $(date -u +%Y-%m-%dT%H:%M:%SZ)"

cleanup() {
  for v in MNT_PID MNT2_PID; do
    eval "p=\${$v:-}"
    if [ -n "$p" ]; then kill -TERM "$p" 2>/dev/null; sleep 0.4; kill -KILL "$p" 2>/dev/null; fi
  done
  timeout 20 fusermount3 -u "$MNT2" >/dev/null 2>&1 || timeout 20 fusermount -u "$MNT2" >/dev/null 2>&1
  timeout 20 fusermount3 -u "$MNT"  >/dev/null 2>&1 || timeout 20 fusermount -u "$MNT"  >/dev/null 2>&1
  [ -n "${SRV_PID:-}" ] && kill -TERM "$SRV_PID" 2>/dev/null
}
trap cleanup EXIT

# ---------- helpers ----------------------------------------------------------
sha() { sha256sum "$1" 2>/dev/null | awk '{print $1}'; }
# now_ms is MILLISECONDS since the epoch, from one clock for the whole run.
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
sha_of_str() { printf %s "$1" | sha256sum | awk '{print $1}'; }

# read_mount MODE MOUNTDIR REL -> "rc=<n> bytes=<len> sha=<sha> err=<e> body=<b>"
read_mount() {
  local mode="$1" m="$2" rel="$3" body rc
  if [ "$mode" = cat ]; then
    body="$(timeout 30 cat "$m/$rel" 2>"$OUT/.err")"; rc=$?
  else
    body="$(timeout 30 python3 "$OUT/read1.py" "$m/$rel" 2>"$OUT/.err")"; rc=$?
  fi
  printf 'rc=%d bytes=%d sha=%s err=%s body=%s\n' \
    "$rc" "$(printf %s "$body" | wc -c)" "$(sha_of_str "$body")" \
    "$(head -c 160 "$OUT/.err" | tr -d '\n' | sed 's/^$/none/')" \
    "$(printf %s "$body" | head -c 70)"
}
inv_of() { # $1 = status file ; prints "events=.. dropped=.. seq=.. mechanism=.. available=.."
  python3 "$OUT/inv.py" "$1"
}
status_line() { # $1 = status file $2 = group
  python3 "$OUT/group.py" "$1" "$2"
}
reqs() { # the mount's own transport request counter
  python3 "$OUT/reqs.py" "$STATUS"
}

wait_mount() { # $1 mountpoint  $2 pidvarname  $3 log
  local i
  for i in $(seq 1 240); do
    grep -q " $1 " /proc/mounts && return 0
    eval "kill -0 \${$2} 2>/dev/null" || { echo "MOUNT EXITED ($3)"; sed 's/^/    /' "$3"; return 1; }
    sleep 0.25
  done
  echo "MOUNT DID NOT COME UP"; sed 's/^/    /' "$3"; return 1
}

# arm NAME RELPATH NEWBYTES HOW
arm() {
  local name="$1" rel="$2" newbody="$3" how="$4"
  local f="$TREE/$rel"
  local logstart; logstart="$(wc -l <"$OUT/mount.log")"
  local before; before="$(inv_of "$STATUS")"

  echo
  echo "=== arm $name  ($rel)  out-of-band write: $how ==="
  echo "  target before    : '$(cat "$f")'  size=$(wc -c <"$f") mtime=$(stat -c %y "$f" | cut -c1-29)"
  local seed_size; seed_size="$(wc -c <"$f")"
  echo "  mount read (cat) : $(read_mount cat "$MNT" "$rel")"
  echo "  mount read (py)  : $(read_mount py  "$MNT" "$rel")"
  # THE PREMISE, PROVEN RATHER THAN ASSUMED: the row is about a path the mount
  # ALREADY SERVES FROM ITS CACHE. Two more reads with the mount's own request
  # counter around them: costing zero requests is what "already read through the
  # mount" means here, and an arm that cannot show it is labelled VACUOUS below
  # rather than counted as stale or fresh.
  local r0 r1 cached
  r0="$(reqs)"; read_mount cat "$MNT" "$rel" >/dev/null; read_mount py "$MNT" "$rel" >/dev/null; r1="$(reqs)"
  cached=$([ "$(( r1 - r0 ))" -eq 0 ] && echo yes || echo no)
  echo "  cached proof     : two more reads cost $(( r1 - r0 )) request(s) → cached=$cached"
  echo "  channel before   : $before"

  # ---- THE OUT-OF-BAND WRITE ------------------------------------------------
  case "$how" in
    diff-length)
      printf '%s' "$newbody" > "$f" ;;
    same-size-same-mtime)
      local ref="$OUT/.refmtime-$name"; touch -r "$f" "$ref"
      printf '%s' "$newbody" > "$f"
      touch -r "$ref" "$f" ;;
    same-size-new-mtime)
      printf '%s' "$newbody" > "$f"; touch -d '+7 seconds' "$f" ;;
    unlink-recreate)
      timeout 20 rm -f "$f"; printf '%s' "$newbody" > "$f" ;;
    rename-over)
      printf '%s' "$newbody" > "$f.new"; timeout 20 mv -f "$f.new" "$f" ;;
    *) echo "  unknown how=$how" >&2; return 2 ;;
  esac
  local t_edit; t_edit="$(now_ms)"
  echo "  target after     : '$(cat "$f")'  size=$(wc -c <"$f") mtime=$(stat -c %y "$f" | cut -c1-29)"
  echo "  target sha       : $(sha "$f")   (new-bytes sha $(sha_of_str "$newbody"))"
  echo "  edit at          : t+0 = ${t_edit} ms"
  local newsize; newsize="$(wc -c <"$f")"

  # ---- THE CONTROL READS ---------------------------------------------------
  sleep 0.4
  echo "  mount +400 ms    : $(read_mount cat "$MNT" "$rel")"
  sleep 0.1
  echo "  mount +500 ms    : $(read_mount py  "$MNT" "$rel")"

  # ---- WHEN DOES IT GO FRESH? (one clock, every POLL_MS) --------------------
  local want; want="$(sha_of_str "$newbody")"
  local served_ms="NEVER" refusal_ms="NEVER" reads=0
  local deadline=$(( t_edit + HOLD * 1000 ))
  local seen_stale=no
  while [ "$(now_ms)" -lt "$deadline" ]; do
    local r rc got
    r="$(read_mount cat "$MNT" "$rel")"; reads=$(( reads + 1 ))
    got="$(printf '%s' "$r" | sed -n 's/.*sha=\([0-9a-f]*\).*/\1/p')"
    rc="$(printf '%s' "$r" | sed -n 's/^rc=\([0-9]*\).*/\1/p')"
    if [ "$got" = "$want" ]; then served_ms=$(( $(now_ms) - t_edit )); break; fi
    if [ "$rc" != "0" ]; then
      [ "$refusal_ms" = "NEVER" ] && refusal_ms=$(( $(now_ms) - t_edit ))
    else
      seen_stale=yes
    fi
    sleep "$(awk -v ms="$POLL_MS" 'BEGIN{printf "%.3f", ms/1000}')"
  done
  local after; after="$(inv_of "$STATUS")"
  echo "  FIRST-FRESH      : ${served_ms} ms after the edit   (${reads} reads, hold ${HOLD}s)"
  echo "  first REFUSAL    : ${refusal_ms} ms after the edit"
  echo "  channel after    : $after"
  echo "  new mount.log lines naming $rel :"
  tail -n +$(( logstart + 1 )) "$OUT/mount.log" | grep -F "$rel" | sed 's/^/    /' || true
  echo "  NEG native read  : sha=$(sha "$f")  '$(cat "$f" | head -c 70)'"
  printf 'ARM-RESULT name=%s how=%s cached=%s seed_size=%s new_size=%s first_fresh_ms=%s first_refusal_ms=%s stale_served=%s%s\n' \
    "$name" "$how" "$cached" "$seed_size" "$newsize" "$served_ms" "$refusal_ms" "$seen_stale" \
    "$([ "$cached" = yes ] && echo "" || echo " PREMISE-VACUOUS(the path was not served from cache)")"
}

# ---------- fixture: every arm path exists BEFORE the mount binds ------------
printf '%s' 'v1-original-content-22' > "$TREE/stale-w1.txt"
printf '%s' 'v1-original-content-22' > "$TREE/stale-w2.txt"
printf '%s' 'v1-original-content-22' > "$TREE/stale-w3.txt"
printf '%s' 'v1-original-content-22' > "$TREE/stale-w4.txt"
printf '%s' 'v1-original-content-22' > "$TREE/stale-w5.txt"
printf '%s' 'n2-original-22-bytes-ok' > "$TREE/never.txt"
printf '%s' 'n1-v1-through-mount\n'  > "$TREE/inband.txt"
echo "fixture   = $(ls "$TREE" | tr '\n' ' ')"

# the two tiny readers, as files (never `python3 -c`)
cat > "$OUT/read1.py" <<'PY'
import sys
try:
    sys.stdout.buffer.write(open(sys.argv[1], "rb").read())
except OSError as exc:
    sys.stderr.write("%s: %s\n" % (exc.__class__.__name__, exc.strerror or exc))
    raise SystemExit(3)
PY
cat > "$OUT/inv.py" <<'PY'
import json, sys
try:
    inv = json.load(open(sys.argv[1]))["invalidation"]
except Exception as exc:
    print("UNREADABLE %s" % exc); raise SystemExit
print("events=%s dropped=%s seq=%s mechanism=%s available=%s resyncs=%s reason=%r"
      % (inv.get("events_total"), inv.get("paths_dropped_total"), inv.get("seq"),
         inv.get("mechanism"), inv.get("channel_available"), inv.get("resyncs_total"),
         inv.get("reason", "")))
PY
cat > "$OUT/group.py" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("UNREADABLE"); raise SystemExit
print(json.dumps(d.get(sys.argv[2], {}), sort_keys=True))
PY
cat > "$OUT/reqs.py" <<'PY'
import json, sys
try:
    print(json.load(open(sys.argv[1]))["transport"]["requests_total"])
except Exception:
    print("?")
PY

# ---------- server + mount ---------------------------------------------------
"$DS" --root "$TREE" --addr 127.0.0.1:0 >"$OUT/davserve.out" 2>&1 &
SRV_PID=$!
URL=""
for _ in $(seq 1 80); do
  URL=$(sed -n 's/^URL=//p' "$OUT/davserve.out" | head -1)
  [ -n "$URL" ] && break
  sleep 0.25
done
[ -n "$URL" ] || { echo "davserve did not start" >&2; cat "$OUT/davserve.out" >&2; exit 1; }
echo "endpoint  = $URL"

timeout 900 "$BIN" fs mount "$MNT" --url "$URL" --concurrency 8 --verbose >"$OUT/mount.log" 2>&1 &
MNT_PID=$!
wait_mount "$MNT" MNT_PID "$OUT/mount.log" || exit 1
CDIR="$(ls -td "$OUT/xdg/bunker/fs"/* 2>/dev/null | head -1)"
STATUS="$CDIR/status.json"
echo "cache-dir = $CDIR"
echo "mount.log (banner):"
sed -n '1,12p' "$OUT/mount.log" | sed 's/^/    /'
sleep 3
echo "channel at bind = $(inv_of "$STATUS")"
echo "cache at bind   = $(status_line "$STATUS" cache)"

# ---------- the arms ---------------------------------------------------------
arm W1-diff-length         stale-w1.txt 'v2-REPLACED-with-different-length-content'  diff-length
arm W2-same-size-same-mtime stale-w2.txt 'v2X-REPLACED-same-size'                     same-size-same-mtime
arm W3-same-size-new-mtime  stale-w3.txt 'v2X-REPLACED-same-size'                     same-size-new-mtime
arm W4-unlink-recreate      stale-w4.txt 'v2-REPLACED-after-unlink-and-recreate'      unlink-recreate
arm W5-rename-over          stale-w5.txt 'v2-REPLACED-by-atomic-rename'               rename-over

echo
echo "=== arm N1  in-band write THROUGH the mount (a NEW name; this surface cannot overwrite in place) ==="
printf '%s' 'n1-v1-on-the-target' > "$TREE/inband.txt"
echo "  seed             : '$(cat "$TREE/inband.txt")'"
echo "  mount read       : $(read_mount py "$MNT" inband.txt)"
printf '%s' 'n1-v2-written-through-the-mount-itself' > "$MNT/inband-new.txt"
echo "  create via mount : rc=$? (a NEW name - in-place overwrite of an existing file is refused by design, BFS-030)"
echo "  mount read back  : $(read_mount py "$MNT" inband-new.txt)"
echo "  target read      : '$(cat "$TREE/inband-new.txt" 2>/dev/null)'"
echo "  native sha       : $(sha "$TREE/inband-new.txt")"

echo
echo "=== arm N2  NEVER-READ path, replaced out of band (BFS-025's shape, for contrast) ==="
echo "  stat only, no read: size=$(stat -c %s "$TREE/never.txt")"
printf '%s' 'n2-REPLACED-with-much-longer-content-than-before' > "$TREE/never.txt"
echo "  target now       : '$(cat "$TREE/never.txt")' ($(wc -c <"$TREE/never.txt") bytes)"
echo "  mount read       : $(read_mount cat "$MNT" never.txt)"
echo "  mount read (py)  : $(read_mount py  "$MNT" never.txt)"

# ---------- the NEGATIVE CONTROL: a fresh mount, after all the edits ---------
echo
echo "=== NEG control: a FRESH mount (own cache dir, bound AFTER the edits) ==="
export XDG_CACHE_HOME="$OUT/xdg2"
timeout 900 "$BIN" fs mount "$MNT2" --url "$URL" --concurrency 8 --verbose >"$OUT/mount2.log" 2>&1 &
MNT2_PID=$!
if wait_mount "$MNT2" MNT2_PID "$OUT/mount2.log"; then
  sleep 2
  CDIR2="$(ls -td "$OUT/xdg2/bunker/fs"/* 2>/dev/null | head -1)"
  STATUS2="$CDIR2/status.json"
  echo "  cache-dir 2 = $CDIR2"
  echo "  first read of every arm path through the FRESH mount:"
  for p in stale-w1.txt stale-w2.txt stale-w3.txt stale-w4.txt stale-w5.txt never.txt inband.txt; do
    echo "    $p -> $(read_mount cat "$MNT2" "$p")"
  done
  echo "  (native shas for the same paths:)"
  for p in stale-w1.txt stale-w2.txt stale-w3.txt stale-w4.txt stale-w5.txt never.txt inband.txt; do
    echo "    $p native sha=$(sha "$TREE/$p") size=$(stat -c %s "$TREE/$p")"
  done
else
  echo "  second mount failed to come up; the fresh answers are unavailable"
fi

echo
echo "=== FINAL RECORDS ==="
echo "mount1 channel = $(inv_of "$STATUS")"
echo "mount1 cache   = $(status_line "$STATUS" cache)"
echo "mount1 transport = $(status_line "$STATUS" transport)"
[ -n "${STATUS2:-}" ] && echo "mount2 channel = $(inv_of "$STATUS2")"
echo "transcripts: $OUT"
echo "$OUT" > /tmp/bfs024-last
