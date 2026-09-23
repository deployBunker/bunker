#!/usr/bin/env bash
# GAP-114 probe: memlock class — a small RLIMIT_MEMLOCK turns mlock/mlockall
# into EPERM and the workload aborts (why `docker compose up` dies on
# rootless docker: its default ulimit memlock is tiny).
#
# Stages (docker-level; docker --ulimit maps to systemd LimitMEMLOCK=):
#   1. rootless-docker-style tiny cap (64KiB): mlockall -> EPERM, app aborts
#   2. mlock of 8MiB under the same cap -> EPERM, exit 1
#   3. docker default (64MiB on docker 29.x): same 8MiB lock -> OK
#   4. unlimited (memlock=-1, what bunker must set for rootless workloads):
#      mlockall -> OK
# The C helper is compiled static in the toolchain image context so the
# container needs no compiler.
set -euo pipefail

WORK="${GAP114_WORK:-/tmp/gap114}"
TOOLCHAIN_IMG="gap114/toolchain"
mkdir -p "$WORK"
log() { printf '\n=== %s ===\n' "$*"; }

cat > "$WORK/memlock_helper.c" <<'EOF'
#include <stdio.h>
#include <sys/mman.h>
#include <string.h>
#include <stdlib.h>
int main(int argc, char **argv) {
    if (argc > 1 && !strcmp(argv[1], "mlockall")) {
        if (mlockall(MCL_CURRENT | MCL_FUTURE) != 0) {
            perror("memlock probe: mlockall failed");
            return 1;
        }
        printf("memlock probe: mlockall OK\n");
        return 0;
    }
    size_t bytes = 8 * 1024 * 1024;
    void *p = malloc(bytes);
    if (!p) { perror("malloc"); return 2; }
    memset(p, 1, bytes);
    if (mlock(p, bytes) != 0) {
        perror("memlock probe: mlock(8MiB) failed");
        return 1;
    }
    printf("memlock probe: mlock(8MiB) OK\n");
    return 0;
}
EOF

# compile inside the toolchain image (static, no runtime deps)
docker run --rm -v "$WORK:/w" "$TOOLCHAIN_IMG" \
  sh -c 'apt-get install -y --no-install-recommends libc6-dev >/dev/null 2>&1 || true; gcc -O2 -static -o /w/memlock_helper /w/memlock_helper.c' \
  || gcc -O2 -static -o "$WORK/memlock_helper" "$WORK/memlock_helper.c"
[ -x "$WORK/memlock_helper" ] || { echo "PROBE-FAIL: could not build memlock_helper" >&2; exit 1; }

run_case() {  # $1 label, $2 memlimit spec, $3 helper args
  local label="$1" spec="$2" hargs="$3" rc=0
  docker container remove -f gap114-ml-runner >/dev/null 2>&1 || true
  log "run: $label (ulimit memlock=$spec, helper '$hargs')"
  set +e
  docker run --name gap114-ml-runner --ulimit memlock=$spec:$spec \
    -v "$WORK:/w" "$TOOLCHAIN_IMG" /w/memlock_helper $hargs
  rc=$?
  set -e
  echo "OBSERVED $label exit=$rc (1 = aborted on EPERM)"
  [ "$rc" -le 1 ] || { echo "PROBE-FAIL: unexpected rc=$rc for $label" >&2; exit 1; }
}

log "context"
printf 'OBSERVED host RLIMIT_MEMLOCK (ulimit -l, soft|hard): '
bash -c 'ulimit -l; ulimit -Hl' 2>/dev/null | tr '\n' ' '; echo

run_case "ml-tiny-64k-mlockall" 65536 "mlockall"
run_case "ml-tiny-64k-mlock8m"  65536 "mlock"
run_case "ml-default-64m-mlock8m" 67108864 "mlock"
run_case "ml-unlimited-mlockall" -1 "mlockall"

log "reported limits inside the containers"
for spec in 65536 -1; do
  docker container remove -f gap114-ml-runner >/dev/null 2>&1 || true
  printf 'OBSERVED ulimit memlock=%s -> in-container "ulimit -l" reports: ' "$spec"
  docker run --rm --ulimit memlock=$spec:$spec "$TOOLCHAIN_IMG" sh -c 'ulimit -l'
done
docker container remove -f gap114-ml-runner >/dev/null 2>&1 || true
echo "PROBE-MEMLOCK-OK"
