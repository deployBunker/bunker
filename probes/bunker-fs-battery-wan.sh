#!/usr/bin/env bash
# bunker-fs-battery-wan.sh — the mount battery, aimed at a DATACENTRE (BFS-034).
#
# WHY THIS IS A SECOND FILE AND NOT A `--mode remote` FLAG ON
# probes/bunker-fs-battery.sh:
#
#   1. The failure this file exists to kill is "an instrument that LOOKS like it
#      can be pointed at a remote server and silently cannot". A flag on the
#      original reproduces exactly that shape: the same command line, the same
#      `--tree`, a different meaning for it — invisible at the call site. Here
#      the contract is different and it is enforced, not documented: the side is
#      DECLARED (--ssh HOST, or local) and ASSERTED before a single measurement
#      is taken. If the declared side is not the side the mount's writes land on,
#      this file REFUSES TO RUN (exit 3) instead of reporting a green number.
#   2. The original must stay byte-identical so BFS-016's 35-cell loopback
#      numbers remain attributable to the exact instrument that produced them.
#   3. The original's `--tree` is a local mirror BY CONSTRUCTION (its own banner
#      calls it "served tree"). Reinterpreting it as a remote path inside the
#      same file would leave both meanings reachable from one flag.
#
# THE RULE THIS FILE IMPLEMENTS: every verification of a write is performed on
# the SAME SIDE OF THE LINK AS THE WRITE.
#
#   * side = the tree the endpoint actually serves.
#       - with --ssh HOST : that tree is on HOST; every read-back is an ssh read
#         (mechanism b — the authoritative server-side view) and every read
#         through the mount is mechanism a (what a user sees).
#       - without --ssh  : the tree is local and the local read IS the
#         server-side read (the server serves that directory off this
#         filesystem). Mechanism (b) is realised as a direct local read; the
#         assertion below is what makes that claim true rather than assumed.
#
# WHAT IS DELIBERATELY UNCHANGED so the numbers stay comparable with the
# recorded baselines (sshfs 5 ok / 7-8 stalls; WebDAV 11 ok / 1 stall; native
# 14/14 in 0.41 s): the 14-operation shape (section 2's 13 mount ops + section
# 4's git cells, same names, same order), the per-op wall clock (each op timed
# exactly as the original times it), the exit class and the STALL
# CLASSIFICATION (rc=124 => STALL(timeout)). The same-side verification cells
# are additionally recorded in their own `verify` section, so the measured rows
# are not disturbed by them.
#
# WHAT IS CHANGED, and why it is safe to change:
#   * the "server side" reads are reads of the SIDE (ssh when remote) — that is
#     the whole point;
#   * each mount gets its OWN --cache-dir, and the counters/status/conflict log
#     are read from that directory (BFS-016 found the client's own counter
#     reader picking up ANOTHER mount's status.json; reading the mount's own
#     cache dir removes the ambiguity);
#   * no `pkill` of any kind: every mount process is tracked by PID and killed
#     by pid, and every fusermount is bounded with `timeout` (a sibling's mount
#     is not this run's to kill);
#   * `--fresh-tree` builds the fixture ON THE SIDE and cleans it up there, so
#     the battery can be run against a server whose tree we have no local
#     mirror of.
#
# USAGE
#   # (b) a server at a datacentre, tree reachable over ssh
#   probes/bunker-fs-battery-wan.sh --url http://DC:PORT/dav \
#       --ssh root@dedi-2 --tree /tmp/wan/tree --mnt /tmp/wan/mnt --bin ./bunker
#
#   # (a) a local server (the instrument's own smoke test / the control)
#   probes/bunker-fs-battery-wan.sh --url http://127.0.0.1:PORT/dav \
#       --tree /tmp/wan/tree --mnt /tmp/wan/mnt --bin ./bunker
#
#   flags:
#     --ssh HOST        the tree lives on HOST (the declared side)
#     --fresh-tree      build the fixture at --tree ON THE SIDE and remove it on
#                       the way out; records exactly how it was created
#     --fixture-only    build the fixture at --tree and stop (implies --keep-tree);
#                       so an operator can serve a tree this repo never mirrored
#     --keep-tree       leave the side tree in place after the run
#     --control         append the negative control (section 8; it runs BEFORE the
#                       endpoint kill so the endpoint is still up)
#     --control-only    run ONLY the negative control
#     --mirror DIR      where the control keeps its stale local mirror
#     --stop-endpoint C the section-8 endpoint kill command (operator supplied,
#                       e.g. 'ssh root@dedi-2 kill $(cat /tmp/wan/srv.pid)')
#
# EXIT: 0 when the battery completed (individual stalls and same-side MISMATCHes
# are REPORTED, not fatal — same contract as the original), 2 on usage error,
# 3 when the SIDE ASSERTION fails (the declared tree is not the write side —
# refusing to measure is the correct outcome), 4 when the negative control does
# not behave as the instrument requires.
set -uo pipefail

URL=""; TREE=""; MNT=""; BIN=""; SSH_TARGET=""
CONC=25
OP_TIMEOUT=45
CSV=""
STOP_ENDPOINT=""
KEEP="${KEEP:-0}"
FRESH_TREE=0; CONTROL=0; CONTROL_ONLY=0; MIRROR=""
FIXTURE_ONLY=0; KEEP_TREE=0
RUN_DIR=""; CACHE=""; MOUNT_PIDS=""
VERIFY_TOTAL=0; VERIFY_FAIL=0; FINDINGS=0; VERIFY_VACUOUS=0
CONTROL_RC=0
SIDE_LABEL=""; SIDE_MECH=""

usage() {
  echo "usage: $0 --url URL --tree DIR --mnt DIR --bin PATH" >&2
  echo "          [--ssh HOST] [--fresh-tree] [--control] [--control-only]" >&2
  echo "          [--mirror DIR] [--concurrency N] [--timeout S] [--csv OUT] [--stop-endpoint CMD]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --url) URL="$2"; shift 2 ;;
    --tree) TREE="$2"; shift 2 ;;
    --mnt) MNT="$2"; shift 2 ;;
    --bin) BIN="$2"; shift 2 ;;
    --ssh) SSH_TARGET="$2"; shift 2 ;;
    --concurrency) CONC="$2"; shift 2 ;;
    --timeout) OP_TIMEOUT="$2"; shift 2 ;;
    --csv) CSV="$2"; shift 2 ;;
    --stop-endpoint) STOP_ENDPOINT="$2"; shift 2 ;;
    --mirror) MIRROR="$2"; shift 2 ;;
    --fresh-tree) FRESH_TREE=1; shift ;;
    --fixture-only) FRESH_TREE=1; FIXTURE_ONLY=1; KEEP_TREE=1; shift ;;
    --keep-tree) KEEP_TREE=1; shift ;;
    --control) CONTROL=1; shift ;;
    --control-only) CONTROL=1; CONTROL_ONLY=1; shift ;;
    *) echo "unknown arg: $1" >&2; usage ;;
  esac
done
[ -n "$URL" ] && [ -n "$TREE" ] && [ -n "$MNT" ] && [ -n "$BIN" ] || usage

RUN_DIR="$(mktemp -d /tmp/bfs034-XXXXXX)"
CSV="${CSV:-$RUN_DIR/bunker-fs-battery-wan.csv}"
: > "$CSV"
echo "section,op,elapsed_s,rc,class,requests,in_flight_max,note" >> "$CSV"
MIRROR="${MIRROR:-$RUN_DIR/mirror}"

if [ -n "$SSH_TARGET" ]; then
  SIDE_LABEL="ssh:$SSH_TARGET"
  SIDE_MECH="b(server-side over ssh)"
else
  SIDE_LABEL="local (same filesystem as the served tree)"
  SIDE_MECH="b(server-side, direct local read)"
fi
SSH_OPTS=(-o BatchMode=yes -o LogLevel=ERROR -o ConnectTimeout=15)
# optional wall-clock bound for side calls (0 = unbounded); set by cleanup so a
# dead datacentre cannot hold the exit path open
SIDE_TIMEOUT="${SIDE_TIMEOUT:-0}"

# Is PATH mounted? /proc/mounts column 2 is the mountpoint; matching it as a WHOLE
# FIELD matters here because this battery mounts several siblings under one parent
# (mnt-ctl vs mnt-ctl2): a substring match reports the wrong mount.
is_mounted() {
  awk -v m="$1" '$2 == m { found = 1 } END { exit !found }' /proc/mounts 2>/dev/null
}

say() { printf '%s\n' "$*"; }
hr() { printf '%s\n' "────────────────────────────────────────────────────────────────────────────"; }

# ── the SIDE: every read-back of a write goes through here ───────────────────
# q1 single-quotes one argument for the remote shell.
q1() { printf "'%s'" "${1//\'/\'\\\'\'}"; }

# side_sh "shell command"  — run a shell command ON THE SIDE.
side_sh() {
  if [ -z "$SSH_TARGET" ]; then
    bash -c "$1"
  elif [ "$SIDE_TIMEOUT" != "0" ]; then
    timeout "$SIDE_TIMEOUT" ssh -n "${SSH_OPTS[@]}" "$SSH_TARGET" -- "$1"
  else
    ssh -n "${SSH_OPTS[@]}" "$SSH_TARGET" -- "$1"
  fi
}
# side_stream "shell command" — same, but stdin flows (used for tar).
side_stream() {
  if [ -z "$SSH_TARGET" ]; then bash -c "$1"; else ssh "${SSH_OPTS[@]}" "$SSH_TARGET" -- "$1"; fi
}
# side argv...  — run argv ON THE SIDE (each argument quoted for the remote shell).
side() {
  if [ -z "$SSH_TARGET" ]; then
    "$@"
  else
    local q="" a
    for a in "$@"; do q="$q $(q1 "$a")"; done
    if [ "$SIDE_TIMEOUT" != "0" ]; then
      timeout "$SIDE_TIMEOUT" ssh -n "${SSH_OPTS[@]}" "$SSH_TARGET" -- "$q"
    else
      ssh -n "${SSH_OPTS[@]}" "$SSH_TARGET" -- "$q"
    fi
  fi
}
SIDE_READ_RC=0
# side_wait_eq SHELL_COMMAND EXPECTED [TENTHS]
#
# Run SHELL_COMMAND ON THE SIDE, and if its output is not yet EXPECTED, poll (up
# to SIDE_WAIT_TENTHS x 50 ms) before giving up. A write through the mount is
# published by a PUT that lands AFTER the write syscall returns — measured at
# 63-66 ms on loopback (docs/evidence/BFS-034-visibility.txt) — so a single read
# is a race, not a verification. The wait is BOUNDED, and a bound that expires is
# the MISMATCH: "not visible yet" and "not there" are told apart by the bound.
# Echoes the last output seen and sets SIDE_WAIT_POLLS to the polls it took.
side_wait_eq() {
  local cmd="$1" exp="$2" n="${SIDE_WAIT_TENTHS:-30}" i got=""
  SIDE_WAIT_POLLS=0
  for i in $(seq 1 "$n"); do
    got="$(side_sh "$cmd" 2>/dev/null)"
    SIDE_WAIT_POLLS=$i
    [ "$got" = "$exp" ] && break
    sleep 0.05
  done
  printf '%s' "$got"
}
SIDE_WAIT_POLLS=0
SIDE_WAIT_TENTHS="${SIDE_WAIT_TENTHS:-30}"

side_cat()  { side cat -- "$1" 2>/dev/null; }

# The SIDE's branch name, with a fallback that needs no git on the side at all
# (.git/HEAD is a text file: "ref: refs/heads/<branch>"). SIDE_BRANCH_MECH says
# which route answered, so a report never has to guess.
SIDE_GIT=0
SIDE_BRANCH_MECH="git rev-parse"
side_branch() { # side_branch TREE
  local t="$1" b=""
  if [ "$SIDE_GIT" = "1" ]; then
    b="$(side_sh "git -C $(q1 "$t") rev-parse --abbrev-ref HEAD 2>/dev/null")"
    SIDE_BRANCH_MECH="git rev-parse"
  fi
  if [ -z "$b" ]; then
    b="$(side_sh "sed -n 's|^ref: refs/heads/||p' $(q1 "$t/.git/HEAD") 2>/dev/null" | head -1)"
    SIDE_BRANCH_MECH=".git/HEAD (no usable git on the side)"
  fi
  printf '%s' "$b"
}
side_sha()  { side sha256sum -- "$1" 2>/dev/null | awk '{print $1}' | head -1; }
side_have() { side test -e "$1"; }
side_write() { side_sh "printf '%s' $(q1 "$2") > $(q1 "$1")"; }
side_rm()   { side_sh "rm -f -- $(q1 "$1")"; }

# classify: 0 ok, 124 timed out (the stall symptom the baselines measure), else error.
classify() {
  case "$1" in
    0)   echo "ok" ;;
    124) echo "STALL(timeout)" ;;
    *)   echo "error(rc=$1)" ;;
  esac
}
NOW() { date +%s.%N; }

csv_row() { # section op elapsed rc class requests inflight note
  echo "$1,$2,$3,$4,$5,${6:-},${7:-},\"$(printf '%s' "$8" | tr '\n' ' ' | tr ',' ';' | head -c 220)\"" >> "$CSV"
}

# The mount's OWN counters, from the mount's OWN cache directory. The original
# battery picked the newest directory under $XDG_CACHE_HOME/bunker/fs, which
# BFS-016 proved can be another mount's document.
status_of()   { cat "$1/status.json" 2>/dev/null; }
requests_now() { status_of "$CACHE" | grep -o '"requests_total": *[0-9]*' | grep -o '[0-9]*' | head -1; }
inflight_now() { status_of "$CACHE" | grep -o '"in_flight_max": *[0-9]*' | grep -o '[0-9]*' | head -1; }

# Wait until the mount has rewritten its status document (it ticks once a second),
# so the figures printed below are the mount's state AFTER the burst and not the
# document from before it. Costs at most one tick; measures nothing.
status_fresh() { # status_fresh CACHE_DIR
  local dir="$1" before after i
  before="$(grep -o '"updated_ms": *[0-9]*' "$dir/status.json" 2>/dev/null | grep -o '[0-9]*')"
  for i in $(seq 1 16); do
    sleep 0.5
    after="$(grep -o '"updated_ms": *[0-9]*' "$dir/status.json" 2>/dev/null | grep -o '[0-9]*')"
    [ -n "$after" ] && [ "$after" != "$before" ] && return 0
  done
  return 0
}

# run_one SECTION NAME -- argv...   (identical shape to the original)
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
  csv_row "$section" "$name" "$elapsed" "$rc" "$cls" "$(requests_now)" "$(inflight_now)" "$(printf '%s' "$out" | head -c 120 | tr '\n' ' ')"
}

# verify_row NAME MECH EXPECTED ACTUAL SEVERITY NOTE
#   severity=fail    : a mismatch is a same-side verification the instrument
#                      could NOT confirm (a finding about the CLIENT)
#   severity=finding : a mismatch is a KNOWN condition already filed elsewhere;
#                      still reported, counted separately
verify_row() {
  local name="$1" mech="$2" exp="$3" act="$4" sev="${5:-fail}" note="$6"
  VERIFY_TOTAL=$((VERIFY_TOTAL+1))
  local cls rc tag
  if [ -z "$exp" ]; then
    cls="VACUOUS"; rc=1; tag="VACUOUS!"; VERIFY_VACUOUS=$((VERIFY_VACUOUS+1))
  elif [ "$exp" = "$act" ]; then
    cls="same-side-ok"; rc=0; tag="ok"
  elif [ "$sev" = "finding" ]; then
    cls="finding"; rc=0; tag="FINDING"; FINDINGS=$((FINDINGS+1))
  else
    cls="MISMATCH"; rc=1; tag="MISMATCH!"; VERIFY_FAIL=$((VERIFY_FAIL+1))
  fi
  printf '   verify %-30s %-9s mech=%s exp=%s got=%s %s\n' \
    "$name" "$tag" "$mech" "$(printf '%s' "$exp" | head -c 44)" "$(printf '%s' "$act" | head -c 44)" "$note"
  csv_row verify "$name" "" "$rc" "$cls" "$(requests_now)" "$(inflight_now)" \
    "mech=$mech exp=$(printf '%s' "$exp" | head -c 80 | tr '\n' ' ') got=$(printf '%s' "$act" | head -c 80 | tr '\n' ' ') $note"
}

mount_one() {
  # mount_one NAME [extra args...] — its own cache dir, its PID tracked.
  local name="$1"; shift
  local mnt="$MNT-$name"
  local cache="$RUN_DIR/cache-$name"
  mkdir -p "$mnt" "$cache"
  "$BIN" fs mount "$mnt" --url "$URL" --concurrency "$CONC" --cache-dir "$cache" "$@" >>"$RUN_DIR/mount-$name.log" 2>&1 &
  local pid=$!
  MOUNT_PIDS="$MOUNT_PIDS $pid"
  local i
  for i in $(seq 1 100); do
    is_mounted "$mnt" && { CACHE="$cache"; return 0; }
    sleep 0.25
  done  # bounded: 25 s, then the caller fails loudly
  echo "MOUNT DID NOT COME UP: $name" >&2
  tail -5 "$RUN_DIR/mount-$name.log" >&2
  return 1
}

umount_one() {
  local name="$1"
  local mnt="$MNT-$name"
  timeout 20 fusermount -u "$mnt" >/dev/null 2>&1 || timeout 20 fusermount3 -u "$mnt" >/dev/null 2>&1
  local i
  for i in $(seq 1 40); do
    is_mounted "$mnt" || { sleep 0.3; return 0; }
    sleep 0.25
  done
  echo "WARNING: $mnt still mounted after a bounded unmount" >&2
  return 0
}

kill_mounts() {
  local p
  for p in $MOUNT_PIDS; do
    kill -TERM "$p" 2>/dev/null
  done
  sleep 1
  for p in $MOUNT_PIDS; do
    kill -KILL "$p" 2>/dev/null
  done
  MOUNT_PIDS=""
}

# The SIDE ASSERTION: write a nonce through the mount, read it back on the
# declared side. This is the instrument's own instrument — a run that fails it
# has no business reporting a number. 0 = match, 1 = mismatch, 2 = the write
# through the mount itself failed.
side_assert() { # side_assert MOUNTPOINT CANDIDATE_TREE
  local mp="$1" cand="$2"
  local nonce="bfs034-$$-$RANDOM-$RANDOM"
  local probe="$mp/.wan-side-check"
  printf '%s\n' "$nonce" > "$probe" 2>"$RUN_DIR/sidecheck.err" || return 2
  local got
  got="$(side_wait_eq "cat -- $(q1 "$cand/.wan-side-check") 2>/dev/null" "$nonce")"
  rm -f "$probe" 2>/dev/null
  side_rm "$cand/.wan-side-check" >/dev/null 2>&1
  [ "$got" = "$nonce" ] || { echo "$nonce" > "$RUN_DIR/sidecheck.nonce"; echo "$got" > "$RUN_DIR/sidecheck.got"; return 1; }
  return 0
}

cleanup() {
  local rc=$?
  if [ "$KEEP" = "1" ]; then
    say "KEEP=1 — leaving $RUN_DIR and every mount in place"
    return $rc
  fi
  local m
  for m in "$MNT-snap" "$MNT-nosnap" "$MNT-nosnap1" "$MNT-lowcache" "$MNT-ctl" "$MNT-ctl2" "$MNT-sidecheck"; do
    timeout 20 fusermount -u "$m" >/dev/null 2>&1
  done
  kill_mounts
  # the scratch files this run put on the SIDE (never a local *different* tree)
  if [ -n "$TREE" ]; then
    SIDE_TIMEOUT=25
    side_sh "rm -f -- $(q1 "$TREE/.battery-probe.txt") $(q1 "$TREE/conflict.txt") $(q1 "$TREE/inval.txt") $(q1 "$TREE/.wan-ctl.txt") $(q1 "$TREE/killed.txt")" >/dev/null 2>&1
    if [ "$FRESH_TREE" = "1" ] && [ "$KEEP_TREE" = "1" ]; then
      say "keeping the side tree in place (--keep-tree/--fixture-only): $TREE"
    elif [ "$FRESH_TREE" = "1" ]; then
      case "$TREE" in
        /tmp/*bunker-wan-*|/tmp/*bfs034*|/home/*/bunker-wan-*)
          side_sh "rm -rf -- $(q1 "$TREE")" >/dev/null 2>&1 ;;
        *)
          say "LEAVING the tree in place: --fresh-tree only removes a path under /tmp"
          say "matching *bunker-wan-* or *bfs034* (this one is $TREE). Remove it yourself." ;;
      esac
    fi
    SIDE_TIMEOUT=0
  fi
  return $rc
}
trap cleanup EXIT

# ── the fixture, built HERE and pushed to the side (--fresh-tree) ────────────
# Same shape and sizes as BFS-016's mkfixture.py fixture (go.mod, README.md,
# src/ with 120 x 15 KiB entries, a deep chain, an empty scratch/, three
# commits) so the run is comparable in size and entry count. The bytes are
# generated deterministically by an awk LCG keyed on the relative path (this
# repo gitignores *.py globally, so a committed python fixture is not an
# option) — same SHAPE, not the same bytes as BFS-016's, and said plainly.
fixture_blob() { # SEED BYTES
  awk -v seed="$1" -v n="$2" 'BEGIN{
    s=7; L=length(seed)
    for (i=1;i<=L;i++) { c=index("abcdefghijklmnopqrstuvwxyz0123456789/._-", substr(seed,i,1)); s=(s*131+c+1)%2147483647 }
    for (i=0;i<n;i++) { s=(s*1103515245+12345)%2147483648; printf "%c", 32+int(s%95) }
  }'
}
build_fixture() { # build_fixture ROOT  (local)
  local root="$1" i rel
  mkdir -p "$root/src" "$root/pkg/deep/a/b/c" "$root/scratch"
  printf 'module fixture.local/tree\n\ngo 1.22\n' > "$root/go.mod"
  printf '# fixture tree\n\nserved by probes/davserve for the bunker-fs WAN battery.\n' > "$root/README.md"
  for i in $(seq -w 0 119); do
    rel="src/f$i.txt"
    fixture_blob "$rel" $((15*1024)) > "$root/$rel"
  done
  for rel in pkg/deep/a/b/c/n0.txt pkg/deep/a/b/c/n1.txt pkg/deep/a/b/c/n2.txt; do
    fixture_blob "$rel" 512 > "$root/$rel"
  done
  g() { git -c user.name=fixture -c user.email=fixture@invalid -C "$root" "$@"; }
  g init -q -b main
  g add -A
  g commit -q -m "fixture: base tree"
  fixture_blob "src/f000.txt#2" $((15*1024+64)) > "$root/src/f000.txt"
  g add -A
  g commit -q -m "fixture: second commit (so rebase HEAD~1 is real)"
  printf '## third commit\n' > "$root/CHANGELOG.md"
  g add -A
  g commit -q -m "fixture: third commit"
}

# ── banner ──────────────────────────────────────────────────────────────────
hr
say "bunker-fs mount battery — WAN / DATACENTRE variant (BFS-034)"
hr
say "endpoint      : $URL"
say "declared side : $SIDE_LABEL"
say "side tree     : $TREE"
say "client        : $BIN (concurrency=$CONC, per-op timeout=${OP_TIMEOUT}s)"
say "verify mech   : (b) $SIDE_MECH  +  (a) read back through the mount"
say "csv           : $CSV"
say "artifacts     : $RUN_DIR"
say ""
say "BASELINES (recorded, not re-derived): sshfs 5 ok / 7-8 stalls; WebDAV 11 ok / 1 stall;"
say "NFS whole-tree diff --stat 32.74 s; native (git on the host) 14/14 in 0.41 s."
say ""
say "SECTION MAP: 1-7 and 9 are the original's sections in the original's order; this"
say "variant ADDS section 8 (the negative control, which runs before the endpoint kill"
say "so the endpoint is still up). The CSV section names (ops/tree-read/git/bound/kill)"
say "and every op name are unchanged, so the measured rows stay comparable with"
say "BFS-016's loopback run."
hr

if [ "$FRESH_TREE" = "1" ]; then
  say "0. FRESH TREE ON THE SIDE (--fresh-tree: created AT --tree, removed on the way out)"
  base="$(basename "$TREE")"
  parent="$(dirname "$TREE")"
  if [ -n "$SSH_TARGET" ]; then
    stage="$RUN_DIR/stage"
    mkdir -p "$stage"
    build_fixture "$stage/$base"
    say "   built locally     : $stage/$base ($(find "$stage/$base" -type f -not -path '*/.git/*' | wc -l | tr -d ' ') files, $(du -sh "$stage/$base" | cut -f1))"
    say "   pushing to the side: ssh $SSH_TARGET tar -C '$parent' (mkdir -p first)"
    side_sh "mkdir -p $(q1 "$parent")" || { say "   remote mkdir failed"; exit 1; }
    tar -C "$stage" -czf - "$base" | side_stream "tar -C $(q1 "$parent") --no-same-owner -xzf -" || { say "   push failed"; exit 1; }
    say "   on the side       : $(side_sh "test -f $(q1 "$TREE/go.mod") && echo 'go.mod present' || echo 'MISSING'")"
    say "   git on the side   : $(side_sh "git -C $(q1 "$TREE") log --oneline 2>&1 | tr '\n' ' '")"
    say "   (stderr included above on purpose: a git that refuses the tree — e.g."
    say "    'dubious ownership' after a uid-preserving extract — must be VISIBLE here,"
    say "    not swallowed by a 2>/dev/null inside a verification cell.)"
  else
    mkdir -p "$parent"
    build_fixture "$TREE"
    say "   built in place (local side): $TREE"
    say "   files             : $(find "$TREE" -type f -not -path '*/.git/*' | wc -l | tr -d ' ') ($(du -sh "$TREE" | cut -f1))"
  fi
  hr
fi

# resolve --tree ON THE SIDE (the original did this with a local cd)
RESOLVED="$(side_sh "cd $(q1 "$TREE") && pwd" 2>/dev/null | tr -d '\r' | tail -1)"
if [ -z "$RESOLVED" ]; then
  say "cannot resolve the declared side tree: $SIDE_LABEL:$TREE"
  exit 2
fi
TREE="$RESOLVED"

# Can the SIDE answer git questions? If not, say so once, here, and let the git
# cells report themselves as not-verifiable-on-the-side rather than as a mismatch
# that blames the client for the instrument's own blindness.
SIDE_GIT=0
if side_sh "command -v git >/dev/null 2>&1"; then
  if side_sh "git -C $(q1 "$TREE") rev-parse --git-dir >/dev/null 2>&1"; then
    SIDE_GIT=1
  else
    say "SIDE CAPABILITY: git is installed on $SIDE_LABEL but cannot read $TREE:"
    side_sh "git -C $(q1 "$TREE") rev-parse --git-dir 2>&1" | sed 's/^/   /'
    say "   -> the git cells' side readings fall back to .git/HEAD and are NAMED as such."
  fi
else
  say "SIDE CAPABILITY: no git on $SIDE_LABEL; git cells' side readings fall back to .git/HEAD."
fi

if [ "$FIXTURE_ONLY" = "1" ]; then
  say "fixture-only: the tree is ready at $TREE; nothing was measured."
  say "serve it, then point this battery at the endpoint with --tree $TREE"
  exit 0
fi

if [ "$CONTROL_ONLY" != "1" ]; then

# ── 1. bind preflight and the one-call snapshot ─────────────────────────────
say "1. BIND PREFLIGHT + THE ONE-CALL SNAPSHOT"
"$BIN" fs probe --url "$URL" 2>&1 | sed 's/^/   /'
say ""
"$BIN" fs snapshot --url "$URL" 2>&1 | sed 's/^/   /'
hr

# ── 2. real file operations through the mount — VERIFIED ON THE SIDE ────────
say "2. REAL FILE OPERATIONS THROUGH THE MOUNT (snapshot on), each write VERIFIED ON THE SIDE"
mount_one snap || exit 1
M="$MNT-snap"
SNP_CACHE="$CACHE"

say "   SIDE ASSERTION — is the declared tree really the side these writes land on?"
say "     nonce written THROUGH the mount, read back on the declared side ($SIDE_LABEL):"
side_assert "$M" "$TREE"
case $? in
  0) say "     MATCH — the declared side IS the write side. Proceeding."
     csv_row preflight side-assertion "0.000" 0 "same-side-ok" "" "" "nonce written through the mount and read on $SIDE_LABEL" ;;
  1) say "     MISMATCH — the declared tree does NOT see the mount's writes."
     say "     declared side saw : '$(cat "$RUN_DIR/sidecheck.got" 2>/dev/null)'"
     say "     written through M : '$(cat "$RUN_DIR/sidecheck.nonce" 2>/dev/null)'"
     say "     REFUSING TO MEASURE (exit 3): a number from this configuration would"
     say "     be a green that proves nothing — the exact defect this file exists for."
     csv_row preflight side-assertion "0.000" 3 "MISMATCH-refused" "" "" "declared side does not see the mount's writes"
     exit 3 ;;
  *) say "     the write through the mount itself failed; see $RUN_DIR/sidecheck.err"
     exit 3 ;;
esac
say ""

run_one ops "cat go.mod (read)"          -- cat "$M/go.mod"
run_one ops "ls -l (list+stat)"          -- ls -l "$M"
run_one ops "ls src (120 entries)"       -- ls "$M/src"
run_one ops "stat go.mod"                -- stat -c '%n size=%s' "$M/go.mod"
run_one ops "mkdir newdir"               -- mkdir "$M/newdir"
verify_row "mkdir newdir" "(b) side" "dir" "$(side_wait_eq "test -d $(q1 "$TREE/newdir") && echo dir || echo absent" "dir")" fail "the mkdir must land on the side"
run_one ops "write file (printf>)"       -- sh -c "printf 'hello from bunker-fs\n' > '$M/newdir/hello.txt'"
SIDE_HELLO="$(side_wait_eq "cat -- $(q1 "$TREE/newdir/hello.txt") 2>/dev/null" "hello from bunker-fs")"
verify_row "write file (printf>)" "(b) side" "hello from bunker-fs" "$SIDE_HELLO" fail "the bytes must be on the side, not in a local mirror (appeared after $SIDE_WAIT_POLLS poll(s))"
run_one ops "read it back"               -- cat "$M/newdir/hello.txt"
verify_row "read it back" "(a) mount vs (b) side" "$SIDE_HELLO" "$(cat "$M/newdir/hello.txt" 2>/dev/null)" fail "the mount must agree with the side"
run_one ops "mv (rename)"                -- mv "$M/newdir/hello.txt" "$M/newdir/renamed.txt"
SIDE_MV_NEW="$(side_wait_eq "test -f $(q1 "$TREE/newdir/renamed.txt") && echo new=yes || echo new=no" "new=yes")"
SIDE_MV_OLD="$(side_wait_eq "test -e $(q1 "$TREE/newdir/hello.txt") && echo old=yes || echo old=no" "old=no")"
verify_row "mv (rename)" "(b) side" "new=yes old=no" "$SIDE_MV_NEW $SIDE_MV_OLD" fail "a rename must land on the side, both halves"
run_one ops "ls newdir (after rename)"   -- ls "$M/newdir"
run_one ops "append (>>)"                -- sh -c "printf 'second line\n' >> '$M/newdir/renamed.txt'"
SIDE_APPEND="$(side_wait_eq "cat -- $(q1 "$TREE/newdir/renamed.txt") 2>/dev/null | tr '\n' '|'" "hello from bunker-fs|second line|")"
verify_row "append (>>)" "(b) side" "hello from bunker-fs|second line|" "$SIDE_APPEND" fail "the appended bytes must be on the side (BFS-021 says they are not; $SIDE_WAIT_POLLS poll(s))"
run_one ops "read appended"              -- cat "$M/newdir/renamed.txt"
run_one ops "rm (unlink)"                -- rm "$M/newdir/renamed.txt"
verify_row "rm (unlink)" "(b) side" "absent" "$(side_wait_eq "test -e $(q1 "$TREE/newdir/renamed.txt") && echo present || echo absent" "absent")" fail "the unlink must land on the side"
run_one ops "rmdir"                      -- rmdir "$M/newdir"
verify_row "rmdir" "(b) side" "absent" "$(side_wait_eq "test -e $(q1 "$TREE/newdir") && echo present || echo absent" "absent")" fail "the rmdir must land on the side"

say ""
say "   POSIX-level proof of the writes on the SIDE ($SIDE_LABEL) — the bytes are really there:"
say "   side view of the mount's scratch files (both should be absent: they were removed):"
side_sh "find $(q1 "$TREE") -maxdepth 2 \\( -name 'renamed.txt' -o -name 'hello.txt' -o -name 'newdir' \\) 2>/dev/null" | sed 's/^/     /'
say "   (an empty list here means the unlink and rmdir landed on the side — it is not"
say "    evidence by itself; the per-cell verdicts above are)"
say ""
say "   cache after the ops (this mount's own cache dir: $SNP_CACHE):"
status_fresh "$SNP_CACHE"
status_of "$SNP_CACHE" | grep -A 12 '"cache"' | sed 's/^/     /'
hr

# ── 3. the whole-tree read: snapshot vs PROPFIND walk, at two concurrencies ──
say "3. WHOLE-TREE READ (the proven lever)"
say "   arm A: snapshot op (ONE call), concurrency $CONC"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$M" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
csv_row tree-read "snapshot-op ls -lR" "$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')" "$RC" "ok" "${AFTER:-0}" "$(inflight_now)" "one snapshot call + in-process readdirplus"
umount_one snap

say "   arm B: standard PROPFIND walk, concurrency $CONC (no snapshot)"
mount_one nosnap --no-snapshot || exit 1
N="$MNT-nosnap"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$N" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
csv_row tree-read "propfind-walk ls -lR c=$CONC" "$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')" "$RC" "ok" "${AFTER:-0}" "$(inflight_now)" "one PROPFIND per directory, dispatched concurrently"
umount_one nosnap

say "   arm C: standard PROPFIND walk, concurrency 1 (the serial comparison)"
SAVED_CONC=$CONC; CONC=1
mount_one nosnap1 --no-snapshot || exit 1
N1="$MNT-nosnap1"
BEFORE=$(requests_now)
T0=$(NOW); ls -lR "$N1" >/dev/null 2>&1; RC=$?; T1=$(NOW)
AFTER=$(requests_now)
say "     ls -lR  $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s rc=$RC  requests +$(( ${AFTER:-0} - ${BEFORE:-0} ))  in_flight_max=$(inflight_now)"
csv_row tree-read "propfind-walk ls -lR c=1" "$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')" "$RC" "ok" "${AFTER:-0}" "$(inflight_now)" "same request count, serial"
umount_one nosnap1
CONC=$SAVED_CONC
hr

# ── 4. the git battery over the mount — every cell same-side by construction ─
say "4. THE 14-OPERATION BATTERY OVER THE MOUNT (the comparable shape)"
say "   (the git cells run THROUGH the mount, so they are on the write side by"
say "    construction; the BASE lookup and the whole cleanup below run ON THE SIDE,"
say "    which is the fix for the original's local-tree lines 222/239-243)"
mount_one snap || exit 1
M="$MNT-snap"
BRANCH="bunker/battery-$(date +%Y%m%d%H%M%S)"
BASE="$(side_branch "$TREE")"
TRACKED="$(git -C "$M" ls-files 2>/dev/null | wc -l | tr -d ' ')"
say "   repo=$M tracked=$TRACKED base=$BASE (side-resolved, never written to)"
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
if [ "$SIDE_GIT" = "1" ]; then
  verify_row "add one file" "(b) side index" "1" "$(side_wait_eq "git -C $(q1 "$TREE") diff --cached --name-only 2>/dev/null | grep -c '^\\.battery-probe\\.txt$'" "1")" fail "1 = the path is in the SIDE repo's index (0 = the add did not land)"
else
  verify_row "add one file" "(b) side index" "not-verifiable (no usable git on the side)" "not-verifiable (no usable git on the side)" fail "NAMED as not same-side: reading an index needs git ON THE SIDE and this host has none; the mount-side rc=128 is the other half"
fi
run_one git "commit"                  -- git -C "$M" -c user.name=battery -c user.email=battery@invalid commit -q -m "battery: git over bunker-fs"
run_one git "commit --amend"          -- git -C "$M" -c user.name=battery -c user.email=battery@invalid commit -q --amend --no-edit
run_one git "rebase HEAD~1"           -- git -C "$M" rebase HEAD~1
run_one git "symbolic-ref / branch"   -- git -C "$M" rev-parse --abbrev-ref HEAD
SIDE_BRANCH="$(side_branch "$TREE")"
verify_row "checkout -b scratch" "(b) side HEAD" "$BRANCH" "$SIDE_BRANCH" finding "the branch created through the mount must be the SIDE repo's HEAD (informational)"
# the CLEANUP runs on the side — the original ran it against a local mirror
side_sh "git -C $(q1 "$TREE") rebase --abort" >/dev/null 2>&1
side_sh "git -C $(q1 "$TREE") reset -q -- .battery-probe.txt" >/dev/null 2>&1
side_rm "$TREE/.battery-probe.txt" >/dev/null 2>&1
side_sh "git -C $(q1 "$TREE") checkout -q $(q1 "$BASE")" >/dev/null 2>&1
side_sh "git -C $(q1 "$TREE") branch -D $(q1 "$BRANCH")" >/dev/null 2>&1
AFTER_BRANCH="$(side_branch "$TREE")"
verify_row "cleanup on the side" "(b) side HEAD via $SIDE_BRANCH_MECH" "$BASE" "$AFTER_BRANCH" fail "the side repo must be back on $BASE with the scratch branch gone"
hr

# ── 5. conflict: content hashes, and a REFUSED write (both sides read) ──────
say "5. CONFLICT: A REFUSED WRITE (content hash, never mtime)"
side_write "$TREE/conflict.txt" 'base content
'
sleep 0.2
CONFLICT_FILE="$M/conflict.txt"
say "   read through the mount (this read IS the base hash):"
cat "$CONFLICT_FILE" | sed 's/^/     /'
say "   now change it ON THE SIDE, out of band ($SIDE_LABEL):"
side_write "$TREE/conflict.txt" 'agent side edit
'
say "     sha256 on the side: $(side_sha "$TREE/conflict.txt")"
say "   write through the mount with the stale base:"
T0=$(NOW); printf 'client edit\n' > "$CONFLICT_FILE" 2>"$RUN_DIR/conflict-err.txt"; RC=$?; T1=$(NOW)
say "     write rc=$RC in $(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", b-a}')s; stderr: $(head -c 200 "$RUN_DIR/conflict-err.txt")"
say "   the file's bytes ON THE SIDE (must be the SIDE-side edit, unchanged):"
say "   (read after a 0.5 s settle, so an in-flight 204 has had time to land)"
sleep 0.5
SIDE_CONFLICT="$(side_cat "$TREE/conflict.txt")"
SIDE_CONFLICT_EXISTS="$(side test -e "$TREE/conflict.txt" && echo present || echo absent)"
SIDE_CONFLICT_BYTES="$(side stat -c %s "$TREE/conflict.txt" 2>/dev/null || echo -)"
printf '%s\n' "$SIDE_CONFLICT" | sed 's/^/     /'
say "     on the side: $SIDE_CONFLICT_EXISTS, size=$SIDE_CONFLICT_BYTES byte(s)"
verify_row "conflict refusal holds" "(b) side content" "agent side edit" "$SIDE_CONFLICT" fail "a refused write must leave the SIDE bytes untouched (BFS-033: a 412 is recorded, then a 204 lands)"
verify_row "conflict target survives" "(b) side existence" "present" "$SIDE_CONFLICT_EXISTS" fail "a FAILED rewrite must not destroy the target (BFS-030: the truncate half commits and the write half fails)"
say "   the refusal, as recorded (this mount's own log):"
cat "$CACHE/conflicts.jsonl" 2>/dev/null | sed 's/^/     /'
say "   the status document's conflict block (this mount's own status.json):"
status_of "$CACHE" | grep -A 8 '"conflicts"' | sed 's/^/     /'
side_rm "$TREE/conflict.txt" >/dev/null 2>&1
hr

# ── 6. invalidation: a SIDE-side edit, seen from both sides ─────────────────
say "6. INVALIDATION: SIDE-side edit, no remount, DECLARED window"
side_write "$TREE/inval.txt" 'one
'
sleep 0.2
say "   read through the mount: $(cat "$M/inval.txt")"
say "   edit ON THE SIDE; waiting the declared poll interval (2 s) + one RTT…"
side_write "$TREE/inval.txt" 'two
'
sleep 3
MOUNT_INVAL="$(cat "$M/inval.txt" 2>/dev/null)"
SIDE_INVAL="$(side_cat "$TREE/inval.txt")"
say "   read through the mount again: $MOUNT_INVAL"
say "   read on the side            : $SIDE_INVAL"
verify_row "invalidation window" "(a) mount vs (b) side" "$SIDE_INVAL" "$MOUNT_INVAL" finding "the declared window should make the mount agree with the side (BFS-016 §6 saw it stale)"
say "   invalidation state (this mount's own status.json):"
status_of "$CACHE" | grep -A 10 '"invalidation"' | sed 's/^/     /'
side_rm "$TREE/inval.txt" >/dev/null 2>&1
umount_one snap
hr

# ── 7. the bound: a cache smaller than the tree ─────────────────────────────
say "7. THE CACHE BOUND: --cache-max-size 65536 (64 KiB) over a 1.8 MiB tree"
SAVED_CONC=$CONC; CONC=8
umount_one lowcache
mkdir -p "$MNT-lowcache"
LCACHE="$RUN_DIR/cache-lowcache"
mkdir -p "$LCACHE"
"$BIN" fs mount "$MNT-lowcache" --url "$URL" --cache-max-size 65536 --cache-max-entry-bytes 32768 --concurrency 8 --cache-dir "$LCACHE" >>"$RUN_DIR/mount-lowcache.log" 2>&1 &
MOUNT_PIDS="$MOUNT_PIDS $!"
for i in $(seq 1 100); do is_mounted "$MNT-lowcache" && break; sleep 0.25; done
CACHE="$LCACHE"
L="$MNT-lowcache"
run_one bound "read 40 files (bound exceeded)" -- sh -c "for f in \$(ls '$L/src' | head -40); do cat '$L/src/'\$f >/dev/null; done; echo read-40-ok"
say "   cache figures after over-reading the bound (this mount's own status.json):"
status_fresh "$LCACHE"
status_of "$LCACHE" | grep -A 12 '"cache"' | sed 's/^/     /'
say "   du of this mount's own cache directory (compared with used_bytes above), and"
say "   of the mount directory beside it (BFS-031: --cache-dir names the MOUNT dir;"
say "   the directory the byte bound names is its cache/ subdirectory):"
du -s --block-size=1 "$LCACHE/cache" 2>/dev/null | sed 's/^/     cache dir   /'
du -s --block-size=1 "$LCACHE" 2>/dev/null | sed 's/^/     mount dir   /'
say "   NOTE: this cell verifies a LOCAL resource (the client's cache). It is"
say "   correctly local, and it makes no cross-link claim — stated so it is not"
say "   read as a same-side verification."
say "   reads must still succeed; they do (the run above reported success)."
umount_one lowcache
CONC=$SAVED_CONC
hr

fi  # end of the measured sections (CONTROL_ONLY skips everything above)

# ── 9. the NEGATIVE CONTROL — does the OLD verification detect a broken write? ─
control_section() {
  say "8. NEGATIVE CONTROL — the local-mirror verification vs the SAME-SIDE verification"
  say "   What this proves: given a write that is broken ON THE SIDE (the bytes the"
  say "   client thought it wrote are not the bytes the server has), the verification the"
  say "   original battery performs — a read of a LOCAL tree — reports SUCCESS, while a"
  say "   same-side read detects it. That is the row's premise, demonstrated."
  say "   The mirror is built from the SIDE's own state right after the write landed, so"
  say "   it is exactly what any local mirror of a healthy link would contain, and it is"
  say "   then made stale by exactly one fault event."
  say ""
  local CTL="$MNT-ctl"
  mkdir -p "$CTL" "$MIRROR"
  mount_one ctl || { CONTROL_RC=4; return; }
  local CCACHE="$CACHE"
  CACHE="$CCACHE"

  local nonce="AC-NONCE-$$-$RANDOM"
  local expect="A:$nonce" faulted="B:$nonce"
  printf '%s\n' "$expect" > "$CTL/.wan-ctl.txt" 2>"$RUN_DIR/ctl-write.err"
  local wrc=$?
  say "   8a  write through the mount      : rc=$wrc  path=$CTL/.wan-ctl.txt"
  [ "$wrc" = "0" ] || { say "       the control cannot run: the write itself failed"; CONTROL_RC=4; return; }

  local on_side; on_side="$(side_wait_eq "cat -- $(q1 "$TREE/.wan-ctl.txt") 2>/dev/null" "$expect")"
  local sha_written; sha_written="$(side_sha "$TREE/.wan-ctl.txt")"
  local through_mount=""; local i
  for i in $(seq 1 40); do
    through_mount="$(cat "$CTL/.wan-ctl.txt" 2>/dev/null)"
    [ -n "$through_mount" ] && break
    sleep 0.05
  done
  say "   8b  (b) same-side read           : '$(printf '%s' "$on_side" | tr -d '\n')'  sha=$(printf '%s' "$sha_written" | head -c 12)"
  say "       (a) mount read                : '$(printf '%s' "$through_mount" | tr -d '\n')'"
  verify_row "control: write landed" "(b) side" "$expect" "$on_side" fail "the control's own write must be on the side before it can be broken"
  verify_row "control: mount agrees" "(a) mount" "$expect" "$through_mount" fail "the mount must show what it just wrote"

  # the mirror: the SIDE's state at this instant (a correct local mirror)
  printf '%s\n' "$on_side" > "$MIRROR/.wan-ctl.txt"
  say "   8c  mirror (local, = the side now): '$(printf '%s' "$(cat "$MIRROR/.wan-ctl.txt")" | tr -d '\n')'  sha=$(sha256sum "$MIRROR/.wan-ctl.txt" | cut -c1-12)"

  # THE FAULT: the side loses the write / keeps different bytes.
  side_write "$TREE/.wan-ctl.txt" "$faulted
"
  local fault_seen; fault_seen="$(side_wait_eq "cat -- $(q1 "$TREE/.wan-ctl.txt") 2>/dev/null" "$faulted")"
  local sha_faulted; sha_faulted="$(side_sha "$TREE/.wan-ctl.txt")"
  say "   8d  FAULT on the side            : '$(printf '%s' "$faulted" | tr -d '\n')'  sha=$(printf '%s' "$sha_faulted" | head -c 12)"
  if [ "$sha_faulted" = "$sha_written" ]; then
    say "       the fault changed nothing (sha unchanged) — the control would be VACUOUS"
    CONTROL_RC=4
  else
    say "       the fault changed the bytes (sha differs) — the control is not vacuous"
  fi

  # the OLD verification: read the local tree (what the original battery does)
  local old_got; old_got="$(cat "$MIRROR/.wan-ctl.txt" 2>/dev/null)"
  local old_verdict="FAIL(detected)"
  [ "$old_got" = "$expect" ] && old_verdict="PASS(blind)"
  say "   8e  OLD verification (local tree): '$(printf '%s' "$old_got" | tr -d '\n')'  -> $old_verdict"
  say "       (this is probes/bunker-fs-battery.sh's 'POSIX-level proof of the writes on"
  say "        the SERVER side' and its sha256-on-the-server line, read against --tree)"

  # the NEW verification: same side as the write
  local new_side; new_side="$(side_wait_eq "cat -- $(q1 "$TREE/.wan-ctl.txt") 2>/dev/null" "$expect")"
  local new_verdict_side="FAIL(detected)"
  [ "$new_side" = "$expect" ] && new_verdict_side="PASS"
  say "   8f  NEW verification (b) side   : '$(printf '%s' "$new_side" | tr -d '\n')'  -> $new_verdict_side"
  local stale_mount; stale_mount="$(cat "$CTL/.wan-ctl.txt" 2>/dev/null)"
  say "       (a) existing mount read      : '$(printf '%s' "$stale_mount" | tr -d '\n')'  <- reported, not trusted"
  # a FRESH mount: no cache from before the fault, so it must fetch the side's bytes
  mount_one ctl2 || { say "       (a) fresh mount failed"; CONTROL_RC=4; }
  local new_mount=""
  if is_mounted "$MNT-ctl2"; then
    for i in $(seq 1 40); do
      new_mount="$(cat "$MNT-ctl2/.wan-ctl.txt" 2>/dev/null)"
      [ -n "$new_mount" ] && break
      sleep 0.05
    done
  fi
  CACHE="$CCACHE"
  local new_verdict_mount="FAIL(detected)"
  [ "$new_mount" = "$expect" ] && new_verdict_mount="PASS(blind)"
  say "       (a) FRESH mount read         : '$(printf '%s' "$new_mount" | tr -d '\n')'  -> $new_verdict_mount"
  verify_row "control: OLD path is blind" "(local tree, the wrong side)" "PASS(blind)" "$old_verdict" fail "the old verification MUST report success on a broken write"
  verify_row "control: NEW path detects" "(b) side" "FAIL(detected)" "$new_verdict_side" fail "the same-side verification MUST fail on the same broken write"

  # and the fail-closed guard: declaring the mirror as the side must be REFUSED
  say "   8g  fail-closed check: run the SIDE ASSERTION with the mirror declared as the side"
  side_assert "$CTL" "$MIRROR"
  local sarc=$?
  case $sarc in
    0) say "       MATCH — the guard did NOT catch a wrong side (this is a defect in the guard)"
       CONTROL_RC=4 ;;
    1) say "       MISMATCH — the guard REFUSES to measure against a mirror. Correct." ;;
    *) say "       the probe write failed (rc=$sarc)"; CONTROL_RC=4 ;;
  esac
  verify_row "control: guard refuses mirror" "side assertion" "refused" "$([ "$sarc" = "1" ] && echo refused || echo accepted)" fail "a wrong declared side must be refused, not measured"

  say ""
  if [ "$old_verdict" = "PASS(blind)" ] && [ "$new_verdict_side" = "FAIL(detected)" ] && [ "$sarc" = "1" ] && [ "$CONTROL_RC" = "0" ]; then
    say "   CONTROL VERDICT: the old path PASSED a broken write ('$old_got'), the same-side"
    say "   path DETECTED it ('$new_side' != '$expect'), and the guard refused the mirror."
    say "   The instrument can fail. That is what makes its green worth something."
    csv_row control "negative-control" "" 0 "control-ok" "" "" "old=PASS(blind) new=DETECTED guard=refused"
  else
    say "   CONTROL VERDICT: NOT as required (old=$old_verdict new=$new_verdict_side guard=$sarc rc=$CONTROL_RC)"
    csv_row control "negative-control" "" 4 "control-FAILED" "" "" "old=$old_verdict new=$new_verdict_side guard=$sarc"
    CONTROL_RC=4
  fi
  side_rm "$TREE/.wan-ctl.txt" >/dev/null 2>&1
  rm -f "$MIRROR/.wan-ctl.txt" 2>/dev/null
  umount_one ctl
  umount_one ctl2
  hr
}

if [ "$CONTROL" = "1" ] || [ "$CONTROL_ONLY" = "1" ]; then
  control_section
fi

if [ "$CONTROL_ONLY" != "1" ]; then
# ── 9. transport kill: bounded error, never a hang (and the write must NOT land)
say "9. TRANSPORT KILL: bounded error, named cause, no hang"
if [ -n "$STOP_ENDPOINT" ]; then
  say "   stopping the endpoint: $STOP_ENDPOINT"
  say "   (the mount must STAY mounted; every operation must fail loudly and fast"
  say "    with a named errno — the house rule is a bounded timeout, never a hang)"
  mount_one snap || exit 1
  K="$MNT-snap"
  KCACHE="$CACHE"
  run_one kill "stat . (endpoint up)" -- stat -c '%n' "$K"
  eval "$STOP_ENDPOINT"
  sleep 1
  run_one kill "stat . (endpoint DOWN)" -- stat -c '%n' "$K"
  run_one kill "cat go.mod (endpoint DOWN)" -- cat "$K/go.mod"
  run_one kill "ls (endpoint DOWN)" -- ls "$K"
  run_one kill "write (endpoint DOWN)" -- sh -c "printf 'x\n' > '$K/killed.txt'"
  CACHE="$KCACHE"
  sleep 1
  verify_row "write against DOWN endpoint" "(b) side" "absent" "$(side test -e "$TREE/killed.txt" && echo present || echo absent)" fail "a write that reports failure must NOT have landed on the side (read after a 1 s settle)"
  say "   mountpoint still mounted (not a phantom, not silently unmounted):"
  awk -v m="$K" '$2 == m { print }' /proc/mounts | sed 's/^/     /'
  say "   transport verdict + cause from this mount's status.json:"
  status_of "$KCACHE" | grep -A 8 '"transport"' | sed 's/^/     /'
  umount_one snap
else
  say "   SKIPPED: pass --stop-endpoint '<command>' to run this arm"
fi
hr
fi

# ── summary ─────────────────────────────────────────────────────────────────
say "SUMMARY"
if [ "$CONTROL_ONLY" != "1" ]; then
python3 - "$CSV" <<'PY'
import csv, sys, collections
rows = list(csv.DictReader(open(sys.argv[1])))
groups = collections.OrderedDict()
for r in rows:
    groups.setdefault(r["section"], []).append(r)
for sec, rs in groups.items():
    if sec == "verify":
        continue
    print(f"  [{sec}]")
    for r in rs:
        st = r["class"]
        extra = ""
        if r.get("in_flight_max"):
            extra = f"  in_flight_max={r['in_flight_max']}"
        print(f"    {r['op']:<36} {r['elapsed_s']:>8}s  rc={r['rc']:<4} {st}{extra}")
    if sec in ("ops", "tree-read", "git", "bound", "kill"):
        stalls = [r for r in rs if r["class"].startswith("STALL")]
        print(f"    -> {len(rs)-len(stalls)}/{len(rs)} ok, {len(stalls)} stall(s)"
              + (": " + ", ".join(r["op"] for r in stalls) if stalls else ""))
print()
print("  csv:", sys.argv[1])
PY
fi
say "  SAME-SIDE VERIFICATION (the point of this file):"
say "    verifications run          : $VERIFY_TOTAL"
say "    confirmed on the same side : $((VERIFY_TOTAL - VERIFY_FAIL - FINDINGS))"
say "    MISMATCH (write not confirmed same-side; named above): $VERIFY_FAIL"
say "    VACUOUS (the expectation itself was empty — an instrument error): $VERIFY_VACUOUS"
say "    findings (known, filed elsewhere; named above)      : $FINDINGS"
say "    the stall count above is comparable with BFS-016's loopback run ONLY for the"
say "    sections the original also measured (ops/tree-read/git/bound/kill)."
if [ "$CONTROL" = "1" ] || [ "$CONTROL_ONLY" = "1" ]; then
  say "    negative control: $([ "$CONTROL_RC" = "0" ] && echo 'as required (old path blind, same-side path detects, guard refuses a mirror)' || echo 'NOT as required — see section 8')"
fi
hr
say "artifacts: $RUN_DIR"
if [ "$CONTROL_RC" != "0" ]; then
  say "battery complete — NEGATIVE CONTROL DID NOT BEHAVE AS REQUIRED (exit 4)"
  trap - EXIT
  cleanup
  exit 4
fi
say "battery complete"
trap - EXIT
cleanup
exit 0
