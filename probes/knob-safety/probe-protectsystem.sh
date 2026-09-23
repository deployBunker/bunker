#!/usr/bin/env bash
# GAP-114 probe: ProtectSystem=strict class — the knob makes the whole file
# hierarchy read-only (EROFS) except /dev,/proc,/sys; writes (and the root-unit
# useradd /etc/passwd lock) fail once it is on.
#
# WHAT THIS PROBE MEASURED (2026-09-22, this host):
#   1. USER-level transient units: ProtectSystem SILENTLY NO-OPS here —
#      /etc write fails EACCES with AND without the knob (the failure is
#      plain uid ownership, not the sandbox), and /tmp stays WRITABLE under
#      strict. No EROFS is ever produced. Conclusion for GAP-119: at a
#      user-level agent the knob buys nothing; it must be applied where the
#      unit actually runs (root manager or the container runtime).
#   2. CONTAINER level (docker --read-only + --tmpfs, the same read-only
#      bind-mount mechanism ProtectSystem uses): /etc write -> "Read-only
#      file system" (EROFS) — the mechanism reproduced live; --tmpfs
#      /tmp:rw is the working ReadWritePaths-style exemption.
#   3. Root-unit useradd reproduction (useradd cannot lock /etc/passwd,
#      misleading "permission denied" style lock-contention error):
#      UNMEASURED here — no root units available; refusal captured.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }
J() { journalctl --user -u "$1.service" --no-pager -o cat --since "60 seconds ago" 2>/dev/null | grep -E "$2" | sed -n 1,3p; }

log "part1: user-level transient units — does the knob do anything here?"
run_unit() {  # $1 label, $2 marker-regex, rest = -p args
  local label="$1" marker="$2"; shift 2
  systemctl --user reset-failed "gap114-ps-$label" 2>/dev/null || true
  set +e
  systemd-run --user --wait -u "gap114-ps-$label" -p MemoryMax=100M "$@" \
    sh -c "$PAYLOAD" >/dev/null 2>&1
  local rc=$?
  set -e
  echo "OBSERVED $label unit-exit=$rc journal-markers:"
  J "gap114-ps-$label" "$marker"
}
PAYLOAD='echo probe > /etc/gap114-ps-write 2>&1 || echo "WRITE-FAILED-etc rc=$?"'
run_unit "etc-strict"  'WRITE-FAILED-etc|cannot create' -p ProtectSystem=strict
PAYLOAD='echo probe > /etc/gap114-ps-write 2>&1 && echo CONTROL-WRITE-OK || echo "WRITE-FAILED-etc rc=$?"'
run_unit "etc-control" 'WRITE-FAILED-etc|CONTROL-WRITE-OK|cannot create' -p ProtectSystem=no
PAYLOAD='echo probe > /tmp/gap114-ps-write && echo "WRITE-OK-tmp-UNDER-STRICT" && rm -f /tmp/gap114-ps-write || echo "TMP-WRITE-FAILED"'
run_unit "tmp-strict"  'WRITE-OK-tmp-UNDER-STRICT|TMP-WRITE-FAILED' -p ProtectSystem=strict

log "part2: container level — the EROFS mechanism reproduced (docker --read-only)"
docker container remove -f gap114-ps-ro >/dev/null 2>&1 || true
set +e
docker run --name gap114-ps-ro --read-only --tmpfs /tmp:rw \
  gap114/toolchain sh -c 'echo probe > /etc/gap114-ro-write 2>&1 || echo "WRITE-FAILED-etc: $?"; touch /tmp/ok && echo "WRITE-OK-tmp-exempt"; ls /etc/gap114-ro-write 2>&1'
RC=$?
set -e
echo "OBSERVED docker --read-only run exit=$RC (payload markers above show EROFS on /etc, /tmp exempt via --tmpfs)"
docker container remove -f gap114-ps-ro >/dev/null 2>&1 || true

log "part3: root-unit useradd reproduction — UNMEASURED (no root units)"
set +e
systemd-run --system -u gap114-ps-root-probe true 2>&1 | tail -1
set -e
echo "PROBE-PROTECTSYSTEM-OK"
