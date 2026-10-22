#!/usr/bin/env bash
# MOUNT-017 path probe — is the agent-host path direct or DERP-relayed, and what
# does ONE UNCACHED sshfs metadata operation actually cost?
#
# Read-only: reads tailscale state, sends pings, stats/reads existing paths on the
# mount. Never writes to the mount, never touches tailscale config, never mounts
# or unmounts anything.
#
# WHY THIS SCRIPT EXISTS (the trap that hid the finding for a while):
#   * re-stat'ing the SAME file is served from cache and looks ~16x faster;
#   * stat'ing DISTINCT files on an already-walked tree is ALSO cached — sshfs's
#     userspace stat cache outlived its documented 20s timeout on the 3.7.6 build
#     tested here (still warm after 100s), so a distinct-file loop read 2.95
#     ms/op on a 168 ms link.
# So the honest per-op number comes only from operations that CANNOT be cached:
# statfs, a never-before-seen path name, and a first content read. Those are
# reported as `cold.*`; the distinct-file loop is reported as `warm.*` and is a
# control, not the per-op cost.
#
# Usage: probes/mount017-path-probe.sh [mountpoint] [peer] [samples] [extra-peers...]
#   defaults: /tmp/mount017-mnt  bunker-mvp  30
set -u

MP=${1:-/tmp/mount017-mnt}
PEER=${2:-bunker-mvp}
N=${3:-30}
shift 3 2>/dev/null || true
EXTRA=${*:-cube-las-00 master001}

say() { printf '%-34s %s\n' "$1" "$2"; }
ms() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.2f", (b-a)*1000}'; }

say probe.date "$(date -Is)"
say probe.mountpoint "$MP"
say probe.peer "$PEER"

# ---- A. netcheck: do we even have a port mapping? ---------------------------
NC=$(timeout 40 tailscale netcheck 2>/dev/null)
say netcheck.udp "$(printf '%s\n' "$NC" | awk -F': *' '/^[[:space:]]*\* UDP:/{print $2}')"
say netcheck.ipv4 "$(printf '%s\n' "$NC" | awk -F': *' '/^[[:space:]]*\* IPv4:/{print $2}')"
say netcheck.mapping_varies_by_destip "$(printf '%s\n' "$NC" | awk -F': *' '/MappingVariesByDestIP/{print $2}')"
PM=$(printf '%s\n' "$NC" | awk -F': *' '/^[[:space:]]*\* PortMapping:/{print $2}' | tr -d ' ')
say netcheck.portmapping "${PM:-EMPTY}"
say netcheck.nearest_derp "$(printf '%s\n' "$NC" | awk -F': *' '/Nearest DERP/{print $2}')"
for d in mia lax nue; do
  v=$(printf '%s\n' "$NC" | sed -n "s/.*- $d: *\([0-9.]*\)ms.*/\1/p")
  say "netcheck.derp.$d" "${v:-n/a}"
done

# ---- B/C. peer path: DERP relay or direct endpoint pair ---------------------
# CurAddr non-empty == a direct WireGuard endpoint pair; Relay is what it falls
# back to. Both are reported, because the tunnel flips between them when idle.
peer_field() { # $1 = hostname, $2 = json field -> value or "none"
  if command -v jq >/dev/null 2>&1; then
    tailscale status --json 2>/dev/null |
      jq -r --arg h "$1" --arg f "$2" '.Peer[]|select(.HostName==$h)|(.[$f] // "none")'
  else
    tailscale status 2>/dev/null | grep -w "$1" |
      sed -n "s/.*\(direct [0-9.]*:[0-9]*\).*/\1/p;s/.*\(relay \"[a-z]*\"\).*/\1/p" | head -1
  fi
}
path_of() { # $1 = hostname -> "direct <addr>" | "relay <region>" | "unknown"
  ca=$(peer_field "$1" CurAddr)
  rl=$(peer_field "$1" Relay)
  if [ "$ca" != "none" ] && [ -n "$ca" ]; then printf 'direct %s' "$ca"
  elif [ "$rl" != "none" ] && [ -n "$rl" ]; then printf 'relay %s' "$rl"
  else printf 'unknown'; fi
}
say peer.relay "$(peer_field "$PEER" Relay)"
say peer.cur_addr "$(peer_field "$PEER" CurAddr)"
say peer.path "$(path_of "$PEER")"
TP=$(timeout 45 tailscale ping -c 20 "$PEER" 2>&1 | head -1)
say peer.tailscale_ping "$TP"
if printf '%s' "$TP" | grep -qi 'DERP'; then
  say peer.verdict RELAY
elif printf '%s' "$TP" | grep -qi 'via '; then
  say peer.verdict DIRECT
else
  say peer.verdict "UNREACHABLE ($TP)"
fi

# ---- D. RTT + jitter: direct vs relayed peers, same sample count ------------
rtt_of() { # $1 = hostname
  ip=$(tailscale ip -4 "$1" 2>/dev/null | head -1)
  [ -n "$ip" ] || { say "rtt.$1" "n/a (no tailnet ip)"; return; }
  out=$(timeout 40 ping -c 20 -i 0.3 -W 3 "$ip" 2>&1 | tail -2 | tr '\n' ' ')
  say "rtt.$1.path" "$(path_of "$1")"
  say "rtt.$1" "$(printf '%s' "$out" | sed -n 's/.*\(min\/avg\/max\/mdev = .*\)/\1/p')"
}
rtt_of "$PEER"
for p in $EXTRA; do rtt_of "$p"; done

# ---- E. COLD operations: the real per-op cost ------------------------------
if [ -d "$MP" ]; then
  # statfs: one round trip, never cacheable
  c0=$EPOCHREALTIME; for i in 1 2 3 4 5; do stat -f "$MP" >/dev/null 2>&1; done; c1=$EPOCHREALTIME
  say cold.statfs_5x_total_s "$(awk -v a="$c0" -v b="$c1" 'BEGIN{printf "%.3f", b-a}')"
  say cold.statfs_ms_per_op "$(awk -v a="$c0" -v b="$c1" 'BEGIN{printf "%.2f", (b-a)*1000/5}')"
  say cold.statfs_samples_ms "$(for i in 1 2 3 4 5; do s=$EPOCHREALTIME; stat -f "$MP" >/dev/null 2>&1; e=$EPOCHREALTIME; printf '%s ' "$(ms "$s" "$e")"; done)"

  # negative lookup on a name that cannot be in any cache (unique per pid)
  n0=$EPOCHREALTIME; for i in 1 2 3 4 5; do stat "$MP/no-such-m17-$$-$i" >/dev/null 2>&1; done; n1=$EPOCHREALTIME
  say cold.neglookup_ms_per_op "$(awk -v a="$n0" -v b="$n1" 'BEGIN{printf "%.2f", (b-a)*1000/5}')"
  say cold.neglookup_samples_ms "$(for i in 1 2 3 4 5; do s=$EPOCHREALTIME; stat "$MP/no-such-m17-$$-$i" >/dev/null 2>&1; e=$EPOCHREALTIME; printf '%s ' "$(ms "$s" "$e")"; done)"

  # paired samples: the path state is recorded NEXT TO each cold op, because the
  # tunnel flips between direct and DERP when it goes idle — a single "is it
  # direct?" check at the top of the run does not describe the whole run.
  for i in 1 2 3 4 5; do
    say "cold.paired.$i.path" "$(path_of "$PEER")"
    s=$EPOCHREALTIME; stat -f "$MP" >/dev/null 2>&1; e=$EPOCHREALTIME
    say "cold.paired.$i.statfs_ms" "$(ms "$s" "$e")"
    sleep 3
  done

  # first content read of files this script has not touched (last entries in the
  # dir): cold on the first run, warm on a re-run — hence the explicit label.
  LAST=$(ls -1 "$MP" 2>/dev/null | tail -5)
  if [ -n "$LAST" ]; then
    say cold.read_files "$(printf '%s' "$LAST" | tr '\n' ' ')"
    say cold.read_samples_ms "$(for f in $LAST; do s=$EPOCHREALTIME; cat "$MP/$f" >/dev/null 2>&1; e=$EPOCHREALTIME; printf '%s ' "$(ms "$s" "$e")"; done)"
  fi

  # ---- F. CONTROL ONLY: distinct files on an already-walked tree = cached ---
  FILES=$(ls -1 "$MP" 2>/dev/null | head -"$N")
  CNT=$(printf '%s\n' "$FILES" | grep -c .)
  w0=$EPOCHREALTIME
  for f in $FILES; do stat -c '%s' "$MP/$f" >/dev/null 2>&1; done
  w1=$EPOCHREALTIME
  say warm.distinct_files "$CNT"
  say warm.distinct_ms_per_op "$(awk -v a="$w0" -v b="$w1" -v n="$CNT" 'BEGIN{if(n>0)printf "%.2f", (b-a)*1000/n; else print "n/a"}')"
  say warm.note "CACHED — control only, never quote as the per-op cost"
else
  say cold.statfs_ms_per_op "n/a (no mount at $MP)"
fi

# ---- G. local filesystem baseline, same N distinct stat calls ---------------
LD=$(mktemp -d)
i=0
while [ "$i" -lt "$N" ]; do : > "$LD/l$i"; i=$((i+1)); done
L0=$EPOCHREALTIME
for f in "$LD"/*; do stat -c '%s' "$f" >/dev/null 2>&1; done
L1=$EPOCHREALTIME
say local.distinct_ms_per_op "$(awk -v a="$L0" -v b="$L1" -v n="$N" 'BEGIN{printf "%.3f", (b-a)*1000/n}')"
rm -rf "$LD"
