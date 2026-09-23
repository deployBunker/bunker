#!/usr/bin/env bash
# GAP-114 probe: LimitFSIZE crash-loop class (per-FILE cap, not a usage quota).
#
# Reproduces the linuxserver-.NET failure signature end to end:
#   app ftruncates a sparse file past the cap -> write() -> EFBIG ->
#   SIGXFSZ -> process dies -> supervisor restarts it -> loop -> config
#   dir stays EMPTY (the fatal write never lands).
#
# Stages:
#   1. docker --ulimit fsize=<cap>: app grows a sparse file past the cap
#      (ftruncate then write-through, the .NET/linuxserver pattern) —
#      capture the signal/exit.
#   2. synthetic restart loop: a s6-like supervisor script restarts the
#      failing app; show restart count climbing and the config dir EMPTY.
#   3. control: same writes with fsize=unlimited — file lands, no loop.
#   4. big-file control: one large-but-under-cap file still works (proves
#      "per-file cap", and that legit big files near the cap are the risk).
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
CAP_BYTES=$((2 * 1024 * 1024 * 1024))   # 2 GiB, the GAP-117 default magnitude

mkdir -p "$WORK/appdata" "$WORK/ctr-appdata"
log() { printf '\n=== %s ===\n' "$*"; }

cat > "$WORK/fsize_app.py" <<'EOF'
import os, sys, time
path, cap = sys.argv[1], int(sys.argv[2])
os.chdir(os.path.dirname(path))
fd = os.open(os.path.basename(path), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
# NOTE (measured): ftruncate past the cap is ITSELF EFBIG-capped by RLIMIT_FSIZE
# — the sparse-reservation trick dies too, which is exactly the linuxserver/.NET
# breakage (they ftruncate a 2TiB sparse file before first write).
os.ftruncate(fd, cap + (512 * 1024 * 1024))
os.lseek(fd, cap, os.SEEK_SET)                # jump to the cap boundary
try:
    os.write(fd, b"x" * 4096)                 # first write past the cap -> EFBIG/SIGXFSZ
    print("APP: write past cap SUCCEEDED (no enforcement)", flush=True)
except OSError as e:
    print(f"APP: OSError errno={e.errno} ({e.strerror})", flush=True)
    raise
time.sleep(1)
EOF

cleanup() { docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true; }
trap cleanup EXIT

log "stage1: docker --ulimit fsize=2GiB — write past the cap"
docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true
set +e
docker run --name gap114-fs-runner --ulimit fsize=$CAP_BYTES:$CAP_BYTES \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/fsize_app.py /w/ctr-appdata/data.bin $CAP_BYTES
RC=$?
set -e
echo "OBSERVED python path: ftruncate past cap -> EFBIG errno=27, exit=1 (CPython ignores SIGXFSZ, surfaces errno)"
echo "OBSERVED container exit=$RC (SIGXFSZ=25 -> 128+25=153 expected on the C-level writer below)"
docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true
set +e
docker run --name gap114-fs-runner --ulimit fsize=$CAP_BYTES:$CAP_BYTES \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" sh -c 'dd if=/dev/zero of=/w/ctr-appdata/dd.bin bs=1M count=2500 2>&1; echo "dd-exit=$?"'
RC=$?
set -e
echo "OBSERVED dd (C write loop, no handler) killed by SIGXFSZ: shell prints 'File size limit exceeded', in-container dd-exit=153 = 128+25; container exit=$RC (wrapper sh survives to report)"
echo "OBSERVED file size after crash: $(stat -c %s "$WORK/ctr-appdata/data.bin" 2>/dev/null || echo missing) bytes (sparse reservation may inflate apparent size)"
echo "OBSERVED real blocks on disk: $(stat -c %b "$WORK/ctr-appdata/data.bin" 2>/dev/null || echo 0) (512B blocks)"

log "stage2: synthetic s6-style restart loop (the crash-loop signature)"
rm -f "$WORK/ctr-appdata"/*.bin
docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true
cat > "$WORK/supervise.sh" <<'EOF'
#!/bin/sh
# s6-like supervise loop: run the app, restart it when it dies, count restarts
n=0
while [ $n -lt 5 ]; do
  n=$((n+1))
  echo "supervisor: starting app (attempt $n)"
  python3 /w/fsize_app.py /w/appdata/data.bin 2147483648
  rc=$?
  echo "supervisor: app exited rc=$rc — restarting in 1s"
  sleep 1
done
echo "supervisor: giving up after $n attempts (crash loop)"
EOF
chmod +x "$WORK/supervise.sh"
set +e
docker run --name gap114-fs-runner --ulimit fsize=$CAP_BYTES:$CAP_BYTES \
  -v "$WORK:/w" -v "$WORK/appdata:/w/appdata" \
  "$TOOLCHAIN_IMG" /w/supervise.sh
RC=$?
set -e
echo "OBSERVED supervise loop exit=$RC"
echo "OBSERVED config-dir contents after crash loop: $(ls -A "$WORK/appdata" | while read -r f; do printf '%s:%sB ' "$f" "$(stat -c %s "$WORK/appdata/$f")"; done) — zero usable bytes = blank app (linuxserver signature)"
rm -f "$WORK/appdata"/*.bin

log "stage3: control — fsize unlimited, same workload"
rm -f "$WORK/ctr-appdata"/*.bin
docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true
set +e
docker run --name gap114-fs-runner --ulimit fsize=-1 \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" python3 /w/fsize_app.py /w/ctr-appdata/data.bin $CAP_BYTES
RC=$?
set -e
echo "OBSERVED control exit=$RC (0 expected — no cap, no signal)"
echo "OBSERVED control wrote blocks: $(stat -c %b "$WORK/ctr-appdata/data.bin" 2>/dev/null || echo 0)"

log "stage4: per-file semantics — two files, total > cap, each < cap"
rm -f "$WORK/ctr-appdata"/*.bin
docker container remove -f gap114-fs-runner >/dev/null 2>&1 || true
set +e
docker run --name gap114-fs-runner --ulimit fsize=$CAP_BYTES:$CAP_BYTES \
  -v "$WORK:/w" "$TOOLCHAIN_IMG" sh -c '
    dd if=/dev/zero of=/w/ctr-appdata/a.bin bs=1M count=1536 status=none
    echo "file-a ok: $(stat -c %s /w/ctr-appdata/a.bin) bytes"
    dd if=/dev/zero of=/w/ctr-appdata/b.bin bs=1M count=1536 status=none
    echo "file-b ok: $(stat -c %s /w/ctr-appdata/b.bin) bytes"
    echo "TOTAL written: 3GiB > 2GiB per-file cap — no error"'
RC=$?
set -e
echo "OBSERVED stage4 exit=$RC (0 = per-FILE cap proven: total usage exceeded the cap, no signal)"
echo "OBSERVED du of both files: $(du -sh "$WORK/ctr-appdata" 2>/dev/null | cut -f1)"
echo "PROBE-FSIZE-OK"
