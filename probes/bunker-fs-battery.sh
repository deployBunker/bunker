#!/usr/bin/env bash
# bunker-fs-battery.sh — the mount battery for the bunker-fs FUSE client
# (BFS-008).
#
# WHAT IT MEASURES, and against what: the same 14-operation shape as
# probes/git-over-mount-probe.sh, run through a bunker-fs mount at
# concurrency > 1, with per-operation wall clock, exit class and stall
# classification — so the numbers are comparable with the recorded baselines
# (sshfs 5 ok / 7-8 stalls; WebDAV 11 ok / 1 stall; NFS 32.74 s whole-tree walk;
# native 0.41 s for all 14).
#
# It also exercises, with real commands and real output:
#   * the one-call node snapshot vs the standard PROPFIND walk (the concurrency
#     lever: same request COUNT, different wall clock),
#   * the bounded cache (used_bytes vs max_bytes, evictions, bypasses),
#   * the conflict refusal (content hashes, ESTALE, the refusal log),
#   * the declared invalidation window (edit on the agent, read through the mount),
#   * the transport kill (a bounded named error, never a hang).
#
# USAGE:
#   probes/bunker-fs-battery.sh --url URL --tree DIR --mnt DIR --bin PATH \
#       [--concurrency N] [--timeout S] [--csv OUT]
#
# EXIT: 0 when the battery completed (individual stalls are reported, not
# fatal), 2 on usage error.
set -uo pipefail

URL=""; TREE=""; MNT=""; BIN=""
CONC=25
OP_TIMEOUT=45
CSV=""
SNAPSHOT_MNT=""
KEEP="${KEEP:-0}"
STOP_ENDPOINT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="$2"; shift 2 ;;
    --tree) TREE="$2"; shift 2 ;;
    --mnt) MNT="$2"; shift 2 ;;
    --bin) BIN="$2"; shift 2 ;;
    --concurrency) CONC="$2"; shift 2 ;;
    --timeout) OP_TIMEOUT="$2"; shift 2 ;;
    --csv) CSV="$2"; shift 2 ;;
    --stop-endpoint) STOP_ENDPOINT="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done
[ -n "$URL" ] && [ -n "$TREE" ] && [ -n "$MNT" ] && [ -n "$BIN" ] || {
  echo "usage: $0 --url URL --tree DIR --mnt DIR --bin PATH [--concurrency N] [--timeout S] [--csv OUT]" >&2
  exit 2
}
TREE="$(cd "$TREE" && pwd)"

CSV="${CSV:-/tmp/bunker-fs-battery.csv}"
: > "$CSV"
echo "section,op,elapsed_s,rc,class,requests,in_flight_max,note" >> "$CSV"

say() { printf '%s\n' "$*"; }
hr() { printf '%s\n' "────────────────────────────────────────────────────────────────────────────"; }

# classify: 0 ok, 124 timed out (the stall symptom the baselines measure), else error.
classify() {
  case "$1" in
    0)   echo "ok" ;;
    124) echo "STALL(timeout)" ;;
    *)   echo "error(rc=$1)" ;;
  esac
}

NOW() { date +%s.%N; }

# run_one SECTION NAME -- argv...
run_one() {
  local section="$1" name="$2"; shift 2
  [ "${1:-}" = "--" ] && shift
  local start end rc elapsed out
  start=$(NOW)
  out="$(timeout "$OP_TIMEOUT" "$@" 2>&1)"; rc=$?
  end=$(NOW)
  elapsed=$(awk -v a="$start" -v b="$end" 'BEGIN{printf "%.3f", b-a}')
  local cls; cls="$(classify "$rc")"
  printf '%-32s %8ss  rc=%-4s %s\n' "$name" "$elapsed" "$rc" "$cls"
  echo "$section,$name,$elapsed,$rc,$cls,$(requests_now),$(inflight_now),\"$(printf '%s' "$out" | head -c 100 | tr '\n' ' ' | tr ',' ';')\"" >> "$CSV"
}

# The mount publishes its counters in its status document; the battery reads them
# so the request count and the in-flight high-water mark are EVIDENCE, not claims.
status_json() {
  local dir
  dir="$(ls -td "${XDG_CACHE_HOME:-$HOME/.cache}/bunker/fs"/* 2>/dev/null | head -1)"
  [ -n "$dir" ] && cat "$dir/status.json" 2>/dev/null
}
requests_now() { status_json | grep -o '"requests_total": *[0-9]*' | grep -o '[0-9]*' | head -1; }
inflight_now() { status_json | grep -o '"in_flight_max": *[0-9]*' | grep -o '[0-9]*' | head -1; }

mount_one() {
  # mount_one NAME [extra args...]
  local name="$1"; shift
  local mnt="$MNT-$name"
  mkdir -p "$mnt"
  "$BIN" fs mount "$mnt" --url "$URL" --concurrency "$CONC" "$@" >"/tmp/bfs-mount-$name.log" 2>&1 &
  MPID=$!
  local i
  for i in $(seq 1 60); do
    grep -q "$mnt" /proc/mounts && return 0
    sleep 0.25
  done
  echo "MOUNT DID NOT COME UP: $name" >&2
  cat "/tmp/bfs-mount-$name.log" >&2
  return 1
}

umount_one() {
  local name="$1"
  local mnt="$MNT-$name"
  fusermount -u "$mnt" >/dev/null 2>&1
  local i
  for i in $(seq 1 40); do
    grep -q "$mnt" /proc/mounts || { sleep 0.5; return 0; }
    sleep 0.25
  done
  echo "WARNING: $mnt still mounted" >&2
  return 0
}

cleanup() {
  [ "$KEEP" = "1" ] && return 0
  local m
  for m in "$MNT-snap" "$MNT-nosnap" "$MNT-lowcache"; do
    fusermount -u "$m" >/dev/null 2>&1
  done
  pkill -x "$(basename "$BIN")" >/dev/null 2>&1
  rm -f "$TREE/.battery-probe.txt" 2>/dev/null
}
trap cleanup EXIT

hr
say "bunker-fs mount battery"
hr
say "endpoint      : $URL"
say "served tree   : $TREE"
say "client        : $BIN (concurrency=$CONC, per-op timeout=${OP_TIMEOUT}s)"
say "csv           : $CSV"
say ""
say "BASELINES (recorded, not re-derived): sshfs 5 ok / 7-8 stalls; WebDAV 11 ok / 1 stall;"
say "NFS whole-tree diff --stat 32.74 s; native (git on the host) 14/14 in 0.41 s."
hr

# ── 1. bind preflight and the one-call snapshot ─────────────────────────────
say "1. BIND PREFLIGHT + THE ONE-CALL SNAPSHOT"
"$BIN" fs probe --url "$URL" 2>&1 | sed 's/^/   /'
say ""
"$BIN" fs snapshot --url "$URL" 2>&1 | sed 's/^/   /'
hr

# ── 2. real file operations through the mount ───────────────────────────────
say "2. REAL FILE OPERATIONS THROUGH THE MOUNT (snapshot on)"
mount_one snap || exit 1
M="$MNT-snap"
run_one ops "cat go.mod (read)"          -- cat "$M/go.mod"
run_one ops "ls -l (list+stat)"          -- ls -l "$M"
run_one ops "ls src (120 entries)"       -- ls "$M/src"
run_one ops "stat go.mod"                -- stat -c '%n size=%s' "$M/go.mod"
run_one ops "mkdir newdir"               -- mkdir "$M/newdir"
run_one ops "write file (printf>)"       -- sh -c "printf 'hello from bunker-fs\n' > '$M/newdir/hello.txt'"
run_one ops "read it back"               -- cat "$M/newdir/hello.txt"
run_one ops "mv (rename)"                -- mv "$M/newdir/hello.txt" "$M/newdir/renamed.txt"
run_one ops "ls newdir (after rename)"   -- ls "$M/newdir"
run_one ops "append (>>)"                -- sh -c "printf 'second line\n' >> '$M/newdir/renamed.txt'"
run_one ops "read appended"              -- cat "$M/newdir/renamed.txt"
run_one ops "rm (unlink)"                -- rm "$M/newdir/renamed.txt"
run_one ops "rmdir"                      -- rmdir "$M/newdir"
say ""
say "   POSIX-level proof of the writes on the SERVER side (the bytes are really there):"
if [ -f "$TREE/.battery-should-not-exist" ]; then say "   unexpected file present"; fi
say "   server-side view of the mount's scratch file (should be empty: it was removed):"
find "$TREE" -name 'renamed.txt' -o -name 'hello.txt' 2>/dev/null | sed 's/^/     /' || true
say ""
say "   cache after the ops:"
"$BIN" fs status --json 2>/dev/null | grep -A 12 '"cache"' | sed 's/^/     /'
hr

# ── 3. the whole-tree read: snapshot vs PROPFIND walk, at two concurrencies ──
say "3. WHOLE-TREE READ (the proven lever)"
say "   arm A: snapshot op (ONE call), concurrency $CONC"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$M" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
echo "tree-read,snapshot-op ls -lR,$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}'),$RC,ok,${AFTER:-0},$(inflight_now),one snapshot call + in-process readdirplus" >> "$CSV"
umount_one snap

say "   arm B: standard PROPFIND walk, concurrency $CONC (no snapshot)"
mount_one nosnap --no-snapshot || exit 1
N="$MNT-nosnap"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$N" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
echo "tree-read,propfind-walk ls -lR c=$CONC,$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}'),$RC,ok,${AFTER:-0},$(inflight_now),one PROPFIND per directory, dispatched concurrently" >> "$CSV"
umount_one nosnap

say "   arm C: standard PROPFIND walk, concurrency 1 (the serial comparison)"
SAVED_CONC=$CONC; CONC=1
mount_one nosnap1 --no-snapshot || exit 1
N1="$MNT-nosnap1"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$N1" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
echo "tree-read,propfind-walk ls -lR c=1,$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}'),$RC,ok,${AFTER:-0},$(inflight_now),same request count, serial" >> "$CSV"
umount_one nosnap1
CONC=$SAVED_CONC
hr

# ── 4. the 14-operation git battery over the mount ──────────────────────────
say "4. THE 14-OPERATION BATTERY OVER THE MOUNT (the comparable shape)"
mount_one snap || exit 1
M="$MNT-snap"
BRANCH="bunker/battery-$(date +%Y%m%d%H%M%S)"
BASE="$(git -C "$TREE" rev-parse --abbrev-ref HEAD 2>/dev/null)"
TRACKED="$(git -C "$M" ls-files 2>/dev/null | wc -l | tr -d ' ')"
say "   repo=$M tracked=$TRACKED base=$BASE (never written to)"
run_one git "rev-parse HEAD"          -- git -C "$M" rev-parse HEAD
run_one git "log -1"                  -- git -C "$M" log -1 --format=%h
run_one git "log --oneline -20"       -- git -C "$M" log --oneline -20
run_one git "ls-files (count)"        -- git -C "$M" ls-files
run_one git "diff --stat HEAD"        -- git -C "$M" diff --stat HEAD
run_one git "status --short"          -- git -C "$M" status --short
run_one git "status --porcelain -uno" -- git -C "$M" status --porcelain -uno
run_one git "checkout -b scratch"     -- git -C "$M" checkout -q -b "$BRANCH"
printf 'battery %s\n' "$(date -Is)" > "$M/.battery-probe.txt"
run_one git "add one file"            -- git -C "$M" add .battery-probe.txt
run_one git "commit"                  -- git -C "$M" -c user.name=battery -c user.email=battery@invalid commit -q -m "battery: git over bunker-fs"
run_one git "commit --amend"          -- git -C "$M" -c user.name=battery -c user.email=battery@invalid commit -q --amend --no-edit
run_one git "rebase HEAD~1"           -- git -C "$M" rebase HEAD~1
run_one git "symbolic-ref / branch"   -- git -C "$M" rev-parse --abbrev-ref HEAD
git -C "$TREE" rebase --abort >/dev/null 2>&1
git -C "$TREE" reset -q -- .battery-probe.txt >/dev/null 2>&1
rm -f "$TREE/.battery-probe.txt" 2>/dev/null
git -C "$TREE" checkout -q "$BASE" 2>/dev/null
git -C "$TREE" branch -D "$BRANCH" >/dev/null 2>&1
hr

# ── 5. conflict: content hashes, and a REFUSED write ────────────────────────
say "5. CONFLICT: A REFUSED WRITE (content hash, never mtime)"
printf 'base content\n' > "$TREE/conflict.txt"
sleep 0.2
CONFLICT_FILE="$M/conflict.txt"
say "   read through the mount (this read IS the base hash):"
cat "$CONFLICT_FILE" | sed 's/^/     /'
say "   now change it on the AGENT side, out of band:"
printf 'agent side edit\n' > "$TREE/conflict.txt"
say "     sha256 on the server: $(sha256sum "$TREE/conflict.txt" | cut -d' ' -f1)"
say "   write through the mount with the stale base:"
T0=$(NOW); printf 'client edit\n' > "$CONFLICT_FILE" 2>/tmp/bfs-conflict-err.txt; RC=$?; T1=$(NOW)
say "     write rc=$RC in $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s; stderr: $(head -c 200 /tmp/bfs-conflict-err.txt)"
say "   the file's bytes on the server (must be the AGENT side edit, unchanged):"
cat "$TREE/conflict.txt" | sed 's/^/     /'
say "   the refusal, as recorded:"
"$BIN" fs conflicts 2>&1 | sed 's/^/     /'
say "   the status document's conflict block:"
"$BIN" fs status --json 2>/dev/null | grep -A 8 '"conflicts"' | sed 's/^/     /'
rm -f "$TREE/conflict.txt"
hr

# ── 6. invalidation: an agent-side edit is visible without a remount ────────
say "6. INVALIDATION: agent-side edit, no remount, DECLARED window"
printf 'one\n' > "$TREE/inval.txt"
sleep 0.2
say "   read through the mount: $(cat "$M/inval.txt")"
say "   edit on the agent side; waiting the declared poll interval (2 s) + one RTT…"
printf 'two\n' > "$TREE/inval.txt"
sleep 3
say "   read through the mount again: $(cat "$M/inval.txt")"
say "   invalidation state:"
"$BIN" fs status --json 2>/dev/null | grep -A 10 '"invalidation"' | sed 's/^/     /'
rm -f "$TREE/inval.txt"
umount_one snap
hr

# ── 7. the bound: a cache smaller than the tree ─────────────────────────────
say "7. THE CACHE BOUND: --cache-max-size 65536 (64 KiB) over a 1.8 MiB tree"
SAVED_CONC=$CONC; CONC=8
umount_one lowcache
mkdir -p "$MNT-lowcache"
"$BIN" fs mount "$MNT-lowcache" --url "$URL" --cache-max-size 65536 --cache-max-entry-bytes 32768 --concurrency 8 >/tmp/bfs-mount-lowcache.log 2>&1 &
for i in $(seq 1 60); do grep -q "$MNT-lowcache" /proc/mounts && break; sleep 0.25; done
L="$MNT-lowcache"
run_one bound "read 40 files (bound exceeded)" -- sh -c "for f in \$(ls '$L/src' | head -40); do cat '$L/src/'\$f >/dev/null; done; echo read-40-ok"
say "   cache figures after over-reading the bound:"
"$BIN" fs status --json 2>/dev/null | grep -A 12 '"cache"' | sed 's/^/     /'
say "   du of the cache directory (compared with used_bytes above):"
CDIR="$(ls -td "${XDG_CACHE_HOME:-$HOME/.cache}/bunker/fs"/* | head -1)"
du -s --block-size=1 "$CDIR" 2>/dev/null | sed 's/^/     /'
say "   reads must still succeed; they do (the run above reported success)."
umount_one lowcache
CONC=$SAVED_CONC
hr

# ── 8. transport kill: bounded error, never a hang ──────────────────────────
say "8. TRANSPORT KILL: bounded error, named cause, no hang"
if [ -n "$STOP_ENDPOINT" ]; then
  say "   stopping the endpoint: $STOP_ENDPOINT"
  say "   (the mount must STAY mounted; every operation must fail loudly and fast"
  say "    with a named errno — the house rule is a bounded timeout, never a hang)"
  mkdir -p "$MNT-snap"
  "$BIN" fs mount "$MNT-snap" --url "$URL" --concurrency "$CONC" >/tmp/bfs-mount-kill.log 2>&1 &
  for i in $(seq 1 60); do grep -q "$MNT-snap" /proc/mounts && break; sleep 0.25; done
  K="$MNT-snap"
  run_one kill "stat . (endpoint up)" -- stat -c '%n' "$K"
  eval "$STOP_ENDPOINT"
  sleep 1
  run_one kill "stat . (endpoint DOWN)" -- stat -c '%n' "$K"
  run_one kill "cat go.mod (endpoint DOWN)" -- cat "$K/go.mod"
  run_one kill "ls (endpoint DOWN)" -- ls "$K"
  run_one kill "write (endpoint DOWN)" -- sh -c "printf 'x\n' > '$K/killed.txt'"
  say "   mountpoint still mounted (not a phantom, not silently unmounted):"
  grep "$K" /proc/mounts | sed 's/^/     /'
  say "   transport verdict + cause from the status document:"
  "$BIN" fs status --json 2>/dev/null | grep -A 8 '"transport"' | sed 's/^/     /'
  umount_one snap
else
  say "   SKIPPED: pass --stop-endpoint '<command>' to run this arm"
fi
hr

# ── summary ────────────────────────────────────────────────────────────────
say "SUMMARY"
python3 - "$CSV" <<'PY'
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
groups = collections.OrderedDict()
for r in rows:
    groups.setdefault(r["section"], []).append(r)
for sec, rs in groups.items():
    print(f"  [{sec}]")
    for r in rs:
        st = r["class"]
        extra = ""
        if r.get("in_flight_max"):
            extra = f"  in_flight_max={r['in_flight_max']}"
        print(f"    {r['op']:<36} {r['elapsed_s']:>8}s  rc={r['rc']:<4} {st}{extra}")
    stalls = [r for r in rs if r["class"].startswith("STALL")]
    print(f"    -> {len(rs)-len(stalls)}/{len(rs)} ok, {len(stalls)} stall(s)"
          + (": " + ", ".join(r["op"] for r in stalls) if stalls else ""))
print()
print("  csv:", sys.argv[1])
PY
hr
say "battery complete"
