#!/usr/bin/env bash
# GAP-114 probe (g): unit-sandbox set — classify each knob
#   SAFE / NEEDS-EXCEPTION / BREAKS / UNMEASURED
# for a rootless-docker-in-agent workload.
#
# HOST FACT established at baseline (measured): unprivileged user namespaces
# are BLOCKED on this host (AppArmor restricts unprivileged userns), so the
# synthetic rootlesskit-like unshare/mount workload cannot run at baseline —
# in user units OR in rootless-style containers (unshare: Operation not
# permitted). Therefore RestrictNamespaces has nothing measurable to take
# away HERE; the seccomp differentiation is demonstrated on `mkdir`, a plain
# unprivileged syscall any agent workload makes.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }
J() { journalctl --user -u "$1.service" --no-pager -o cat --since "90 seconds ago" 2>/dev/null | grep -E "^($2)" | sed -n 1,4p; }

run_unit() {  # $1 label, $2 marker-regex (anchored at line start), rest = -p args
  local label="$1" marker="$2"; shift 2
  systemctl --user reset-failed "gap114-sb-$label" 2>/dev/null || true
  set +e
  systemd-run --user --wait -u "gap114-sb-$label" -p MemoryMax=100M "$@" \
    sh -c "$PAYLOAD" >/dev/null 2>&1
  local rc=$?
  set -e
  echo "OBSERVED $label unit-exit=$rc markers:"
  J "gap114-sb-$label" "$marker"
}

log "g0: host baseline — userns restriction + plain-syscall sanity"
printf 'OBSERVED kernel.apparmor_restrict_unprivileged_userns='
cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo "(sysctl absent)"
printf 'OBSERVED baseline unshare --user --map-root-user: '
unshare --user --map-root-user true 2>&1 && echo OK || echo "BLOCKED (rc=$?)"
PAYLOAD='mkdir /tmp/gap114-mk0 && echo MKDIR-OK && rmdir /tmp/gap114-mk0'
run_unit "base" 'MKDIR-OK'

log "g1: NoNewPrivileges=yes (SAFE candidate — verify it engages)"
PAYLOAD="grep -E 'NoNewPrivs|Seccomp' /proc/self/status > $WORK/nnp-status.txt; mkdir /tmp/gap114-mk1 && echo MKDIR-OK >> $WORK/nnp-status.txt; cat $WORK/nnp-status.txt"
run_unit "nnp" 'NoNewPrivs|Seccomp|MKDIR' -p NoNewPrivileges=yes

log "g2: RestrictNamespaces=yes — knob accepted, but host already blocks userns"
set +e
OUT=$(systemd-run --user -u "gap114-sb-rns-$$" -p MemoryMax=50M -p RestrictNamespaces=yes true 2>&1 | tail -1)
set -e
echo "OBSERVED RestrictNamespaces=yes -> $(echo "$OUT" | grep -qi 'invocation\|running as' && echo ACCEPTED || echo "REFUSED: $OUT")"
echo "OBSERVED differentiation: UNMEASURED-here (baseline unshare already blocked by host AppArmor policy — see g0)"

log "g3: SystemCallFilter deny-list (~mkdir) — expect MKDIR to break with EPERM"
PAYLOAD='mkdir /tmp/gap114-mk3 2>&1 && echo MKDIR-OK && rmdir /tmp/gap114-mk3 || echo MKDIR-BROKEN:$?'
run_unit "scf-deny" 'MKDIR-OK|MKDIR-BROKEN' '-pSystemCallFilter=~mkdir'

log "g4: SystemCallFilter allow-list @system-service — CLI-grade workload survives"
PAYLOAD="grep -E 'NoNewPrivs|Seccomp' /proc/self/status; echo ALLOWLIST-EXEC-OK; mkdir /tmp/gap114-mk4 && echo MKDIR-OK && rmdir /tmp/gap114-mk4 || echo MKDIR-BROKEN:\$?"
run_unit "scf-allow" 'NoNewPrivs|Seccomp|ALLOWLIST-EXEC-OK|MKDIR-OK|MKDIR-BROKEN' '-pSystemCallFilter=@system-service'

log "g5: namespaced/privileged knobs — accepted by the user manager at all?"
N=0
for PROP in "ProtectKernelTunables=yes" "ProtectKernelModules=yes" "ProtectControlGroups=yes" "RestrictSUIDSGID=yes" "CapabilityBoundingSet=" ; do
  N=$((N+1)); U="gap114-sb-acc-$N-$$"
  printf 'OBSERVED %-28s -> ' "$PROP"
  set +e
  OUT=$(systemd-run --user -u "$U" -p MemoryMax=50M -p "$PROP" true 2>&1 | tail -1)
  set -e
  if echo "$OUT" | grep -qi 'invocation\|running as'; then
    echo "ACCEPTED (user manager took the property; deep effect NOT verified at this level)"
  else
    echo "REFUSED: $OUT"
  fi
  systemctl --user reset-failed "$U" 2>/dev/null || true
done

log "g6: docker rootless-style posture (no-new-privs + cap-drop ALL + seccomp deny mkdir/unshare/mount)"
docker container remove -f gap114-sb-rl >/dev/null 2>&1 || true
set +e
docker run --rm --name gap114-sb-rl \
  --security-opt no-new-privileges \
  --cap-drop ALL \
  --security-opt seccomp=/tmp/gap114/seccomp-deny-mount.json \
  gap114/toolchain sh -c 'mkdir /tmp/x 2>&1 && echo MKDIR-OK || echo MKDIR-BROKEN:$?; unshare --user --map-root-user true 2>&1 && echo UNSHARE-OK || echo UNSHARE-BROKEN:$?'
RC=$?
set -e
echo "OBSERVED docker seccomp-deny run rc=$RC"
docker container remove -f gap114-sb-rl >/dev/null 2>&1 || true

log "g6-control: identical container WITHOUT the seccomp profile"
set +e
docker run --rm --name gap114-sb-rl2 \
  --security-opt no-new-privileges \
  --cap-drop ALL \
  gap114/toolchain sh -c 'mkdir /tmp/x 2>&1 && echo MKDIR-OK || echo MKDIR-BROKEN:$?; unshare --user --map-root-user true 2>&1 && echo UNSHARE-OK || echo UNSHARE-BROKEN:$?'
RC=$?
set -e
echo "OBSERVED docker control rc=$RC"
echo "PROBE-SANDBOX-OK"
