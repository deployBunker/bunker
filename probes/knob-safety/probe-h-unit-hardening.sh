#!/usr/bin/env bash
# GAP-119 probe: classify every candidate systemd sandboxing property for the
# per-agent rootless-dockerd spawn unit as SAFE / NEEDS-EXCEPTION / BREAKS.
#
# This is the evidence the shipped set in internal/agent/unit_hardening.go is
# built from. It is RE-RUNNABLE on any host: every line is a command and every
# observation is printed verbatim. Two legs:
#
#   leg A (no privileges): what does THIS systemd accept? A property the local
#     manager refuses is a spawn failure (systemd-run exits non-zero), not a
#     silently weaker sandbox.
#   leg B (sudo): the real rootless start path in the spawn unit's own posture
#     (`systemd-run --system --uid=<agent uid>`), one property per arm, using a
#     uid that has /etc/subuid entries — the shape configureSubIDs gives every
#     bunker agent. rootlesskit + the setuid newuidmap helper is what
#     internal/agent/rootless_prereqs.go requires, so this is the path a
#     property must not break.
#
# Interpretation of leg B (measured on systemd 259 + rootlesskit 3.1.0):
#   control, RestrictSUIDSGID, ProtectKernelTunables, ProtectKernelModules,
#   ProtectClock, ProtectHostname                        -> MAP-OK  (SAFE)
#   NoNewPrivileges, CapabilityBoundingSet=              -> newuidmap cannot
#       write uid_map (setuid gain neutralized / CAP_SETUID dropped) -> BREAKS
#   RestrictNamespaces                                   -> child cannot start
#   SystemCallFilter=@system-service                     -> child SIGSYS
#   ProtectSystem=strict                                 -> state dir / EROFS
#   ProtectHome=yes                                      -> the unit cannot exec
#       its own $HOME/bin/dockerd-rootless.sh -> BREAKS
#   MemoryDenyWriteExecute                               -> MAP-OK, but breaks
#       JIT/plugin runtimes -> NEEDS-EXCEPTION
#
# Usage:  bash probes/knob-safety/probe-h-unit-hardening.sh
#         AGENT_USER=kara bash ...        # uid to drive the --system units as
#         SKIP_ROOT=1 bash ...            # leg A only
set -uo pipefail

AGENT_USER="${AGENT_USER:-}"
SKIP_ROOT="${SKIP_ROOT:-}"
WORK="${GAP119_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/gap119-probe.XXXXXX")}"
RUNROOT="${GAP119_RUNROOT:-/run/gap119-probe}"
SUDO=""
[ "$(id -u)" != 0 ] && command -v sudo >/dev/null 2>&1 && SUDO="sudo"

log() { printf '\n=== %s ===\n' "$*"; }

# ── leg A: acceptance by the local manager ─────────────────────────────────
systemd_accepts() { # $1 property
  local prop="$1" out rc
  out=$(systemd-run --user --wait --pipe --collect -u "gap119acc$RANDOM" -p MemoryMax=50M -p "$prop" true 2>&1)
  rc=$?
  if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qi 'Unknown assignment\|Invalid\|not supported'; then
    printf 'OBSERVED %-36s REFUSED  (%s)\n' "$prop" "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-120)"
  else
    printf 'OBSERVED %-36s ACCEPTED\n' "$prop"
  fi
}

log "host baseline"
printf 'OBSERVED systemd=%s\n' "$(systemctl --version | head -1)"
printf 'OBSERVED apparmor_restrict_unprivileged_userns=%s\n' \
  "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns 2>/dev/null || echo '(absent)')"
printf 'OBSERVED newuidmap=%s rootlesskit=%s slirp4netns=%s\n' \
  "$(command -v newuidmap || echo MISSING)" "$(command -v rootlesskit || echo MISSING)" \
  "$(command -v slirp4netns || echo MISSING)"

log "leg A: property acceptance by this systemd"
# The shipped set first, then the excluded candidates.
for p in \
  RestrictSUIDSGID=yes \
  ProtectKernelTunables=yes \
  ProtectKernelModules=yes \
  ProtectClock=yes \
  ProtectHostname=yes \
  NoNewPrivileges=yes \
  CapabilityBoundingSet= \
  RestrictNamespaces=yes \
  SystemCallFilter=@system-service \
  ProtectSystem=strict \
  ProtectHome=yes \
  ProtectControlGroups=yes \
  MemoryDenyWriteExecute=yes ; do
  systemd_accepts "$p"
done

# ── leg B: the real rootless start path, one property per arm ──────────────
if [ -z "$SKIP_ROOT" ] && [ -n "$SUDO" ]; then
  if [ -z "$AGENT_USER" ]; then
    AGENT_USER="$(id -un)"
  fi
  AGENT_UID="$(id -u "$AGENT_USER")"
  log "leg B: rootless start path as uid $AGENT_UID ($AGENT_USER) in spawn posture"
  printf 'OBSERVED subuid: %s\n' "$(grep -h "^$AGENT_USER:" /etc/subuid /etc/subgid 2>/dev/null | tr '\n' ' ' || echo 'NO SUBUID ENTRY (rootlesskit will use a single-uid mapping)')"

  mkdir -p "$WORK"
  # /run is root-only: the run root has to be created BY root and handed to the
  # agent uid, otherwise the unit's own socket-dir write would fail for the
  # wrong reason (a missing parent) and the witness would be meaningless.
  $SUDO mkdir -p "$RUNROOT"
  $SUDO chown "$AGENT_UID" "$WORK" "$RUNROOT"
  # The setuid-neutralization arm needs a COMPILED binary: the kernel ignores
  # the setuid bit on an interpreted script, so a shell helper measures nothing.
  GO="$(command -v go 2>/dev/null || true)"
  [ -z "$GO" ] && GO="/home/$AGENT_USER/.local/bin/go"

  spawn_unit() { # $1 label, rest: -p ... -- cmd...
    local label="$1" out rc; shift
    out=$($SUDO systemd-run --system --wait --pipe --collect -u "gap119p$RANDOM-$label" \
      --uid="$AGENT_UID" --gid="$AGENT_UID" "$@" 2>&1)
    rc=$?
    printf '%s\n' "$out" |
      grep -vE 'Running as unit|Finished with result|Main processes terminated|Service runtime|CPU time|Memory peak' |
      sed "s/^/    | /"
    printf 'OBSERVED %s unit-exit=%s\n' "$label" "$rc"
  }

  MAP=(/usr/bin/rootlesskit /bin/sh -c 'echo IN-NS-uid=$(id -u) MAP-OK')

  printf '\n--- control ---\n'
  spawn_unit control "${MAP[@]}"
  for p in NoNewPrivileges=yes CapabilityBoundingSet= RestrictNamespaces=yes \
           SystemCallFilter=@system-service ProtectSystem=strict ProtectHome=yes \
           RestrictSUIDSGID=yes ProtectKernelTunables=yes ProtectKernelModules=yes \
           ProtectClock=yes ProtectHostname=yes ProtectControlGroups=yes \
           MemoryDenyWriteExecute=yes ; do
    printf '\n--- %s ---\n' "$p"
    spawn_unit "${p//[^A-Za-z0-9]/_}" -p "$p" "${MAP[@]}"
  done

  SET=(-p RestrictSUIDSGID=yes -p ProtectKernelTunables=yes -p ProtectKernelModules=yes \
       -p ProtectClock=yes -p ProtectHostname=yes)
  printf '\n--- shipped set, composed (mapping + slirp4netns + unit writes) ---\n'
  spawn_unit shipped-map "${SET[@]}" "${MAP[@]}"
  spawn_unit shipped-net "${SET[@]}" /usr/bin/rootlesskit --net=slirp4netns /bin/sh -c 'echo NET-OK'
  spawn_unit shipped-writes "${SET[@]}" /bin/sh -c \
    "mkdir -p $RUNROOT/run && touch $RUNROOT/run/docker.sock && echo RUN-SOCK-OK; mkdir -p /home/$AGENT_USER/.cache/gap119 && echo HOME-WRITE-OK && rmdir /home/$AGENT_USER/.cache/gap119"

  printf '\n--- setuid-file creation vs running a pre-existing setuid binary ---\n'
  spawn_unit shipped-suid "${SET[@]}" /bin/sh -c \
    "cp /bin/true $WORK/suid-probe && chmod 4755 $WORK/suid-probe && echo SUID-CREATE-OK || echo SUID-CREATE-DENIED; newuidmap 2>&1 | head -1"

  printf '\n--- excluded-property witnesses (the reasons they stay out) ---\n'
  spawn_unit phome-exec -p ProtectHome=yes /bin/sh -c \
    "cat /home/$AGENT_USER/.profile >/dev/null && echo HOME-READABLE || echo HOME-ENOENT -- the unit could not read its own home"
  spawn_unit ps-run -p ProtectSystem=strict /bin/sh -c \
    "mkdir -p $RUNROOT/run && touch $RUNROOT/run/sock && echo RUN-OK || echo RUN-DENIED -- no socket dir under /run"
  spawn_unit ps-run-exception -p ProtectSystem=strict -p "ReadWritePaths=$RUNROOT" /bin/sh -c \
    "mkdir -p $RUNROOT/run && touch $RUNROOT/run/sock2 && echo RUN-OK-WITH-READWRITEPATHS || echo RUN-DENIED -- the documented exception mechanism"
  spawn_unit cgroup-baseline /bin/sh -c \
    "mkdir -p /sys/fs/cgroup/gap119 2>&1 | head -1; echo CGROUP-BASELINE-DONE"
  spawn_unit cgroup-protected -p ProtectControlGroups=yes /bin/sh -c \
    "mkdir -p /sys/fs/cgroup/gap119 2>&1 | head -1; echo CGROUP-PROTECTED-DONE"

  printf '\n--- NoNewPrivileges neutralizes the setuid bit (the reason it is excluded) ---\n'
  if [ -n "${GO:-}" ] && [ -x "$GO" ]; then
    printf 'package main\n\nimport (\n\t"fmt"\n\t"os"\n)\n\nfunc main() { fmt.Printf("ruid=%%d euid=%%d\\n", os.Getuid(), os.Geteuid()) }\n' > "$WORK/euid.go"
    ( cd "$WORK" && "$GO" build -o euidbin euid.go ) &&
      { cp "$WORK/euidbin" "$WORK/euid-setuid-bin"; $SUDO chown root:root "$WORK/euid-setuid-bin"; $SUDO chmod 4755 "$WORK/euid-setuid-bin";
        spawn_unit suid-control "$WORK/euid-setuid-bin";
        spawn_unit suid-nnp -p NoNewPrivileges=yes "$WORK/euid-setuid-bin"; }
  else
    echo 'SKIP: no Go toolchain — this arm needs a COMPILED binary (the kernel ignores'
    echo '      the setuid bit on a script, so a shell helper would measure nothing)'
  fi

  log "cleanup"
  $SUDO rm -rf "$RUNROOT" "$WORK"
  echo 'OBSERVED cleanup done'
else
  echo
  echo 'SKIP: leg B needs sudo (or SKIP_ROOT=1 was set) — the spawn-posture measurements'
  echo '      cannot be reproduced without creating --system units.'
fi
