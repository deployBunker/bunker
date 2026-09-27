#!/usr/bin/env bash
# flagtable.sh — the surface, as the binary itself renders it, plus the refusal
# transcript. The table in the evidence document is written from THIS output, so
# a flag whose default or whose refusal drifts changes the evidence.
#
# usage: flagtable.sh --bin BUNKER
set -uo pipefail
BIN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$BIN" ] || { echo "--bin required" >&2; exit 2; }

echo "### the mount surface, as the binary renders it"
echo "### binary = $BIN"
"$BIN" version 2>/dev/null | head -2 || true
echo
echo "--- bunker fs mount --help ---"
"$BIN" fs mount --help 2>&1
echo
echo "--- the BFS-044 flags with their defaults (from --help) ---"
"$BIN" fs mount --help 2>&1 | grep -E '^\s+--(hot\.|cache-max-entries|cache-max-inflight|invalidate-idle-timeout)' | sed 's/^ */  /'
echo
echo "--- REFUSAL TRANSCRIPT: every invalid value this surface names, driven live ---"
MP="$(mktemp -d -t bfs044flags-XXXXXX)/mnt"
run() {
  local label="$1"; shift
  printf '\n### %s\n' "$label"
  printf '$ bunker fs mount <mp> --url http://127.0.0.1:1/dav %s\n' "$*"
  out=$("$BIN" fs mount "$MP" --url http://127.0.0.1:1/dav "$@" 2>&1)
  rc=$?
  printf 'exit=%d\n%s\n' "$rc" "$out"
  # Self-describing: a row that reached the transport was NOT refused by the
  # configuration, which is the whole distinction this transcript exists to make.
  case "$out" in
    *"bind refused"*) printf 'CLASSIFIED: NOT refused at configuration time — the run reached the transport (a relation that is REPORTED, not enforced, on a policy the feature does not run)\n';;
    *) printf 'CLASSIFIED: refused at configuration time, before any request\n';;
  esac
}
run "--hot.decay out of range"      --hot.decay 2.0
run "--hot.decay negative"          --hot.decay -0.5
run "--hot.read-weight zero"        --hot.read-weight 0
run "--hot.enabled + edit weight not above read weight (H-2)" --hot.enabled --hot.edit-weight 1 --hot.read-weight 8
run "--hot.max-entries zero"        --hot.max-entries 0
run "--hot.max-tracker-bytes zero"  --hot.max-tracker-bytes 0
run "--hot.max-file-bytes zero"     --hot.max-file-bytes 0
run "--hot.queue-depth zero"        --hot.queue-depth 0
run "--hot.max-concurrent-refresh 0" --hot.max-concurrent-refresh 0
run "--hot.pool-share above one"    --hot.pool-share 3/2
run "--hot.pool-share zero numerator" --hot.pool-share 0/8
run "--hot.pool-share unparseable"  --hot.pool-share eighth
run "--hot.backoff-base-ms zero"    --hot.backoff-base-ms 0
run "--hot.backoff-max-ms below base" --hot.backoff-max-ms 10
run "--hot.backoff-factor below one" --hot.backoff-factor 0.5
run "--hot.backoff-jitter unknown"  --hot.backoff-jitter sometimes
run "--hot.tick-interval zero"      --hot.tick-interval 0
run "--hot.pool-pressure-ticks zero" --hot.pool-pressure-ticks 0
run "--cache-max-entries negative"  --cache-max-entries -1
run "--cache-max-inflight negative" --cache-max-inflight -1
run "--invalidate-idle-timeout negative" --invalidate-idle-timeout -5s
run "--concurrency negative"        --concurrency -2
run "--poll-interval negative"      --poll-interval -1s
printf '\n### ARMED relations: the same shape of value, on a policy the feature actually runs\n'
run "--hot.enabled + backoff cap above the op deadline (A.11)" --hot.enabled --hot.backoff-max-ms 45000
run "--hot.enabled + refresh deadline above the op deadline (A.8)" --hot.enabled --hot.refresh-deadline 45s
run "--hot.enabled + stop deadline below tick+yield (A.6)" --hot.enabled --hot.stop-deadline 100ms
run "--hot.enabled + backoff cap below its own base (A.11)" --hot.enabled --hot.backoff-max-ms 10
run "--hot.enabled + size rule above the per-entry cap (S-10)" --hot.enabled --hot.max-file-bytes 999999999
run "--hot.enabled + size rule above the cache bound (S-9)" --hot.enabled --cache-max-size 1048576 --cache-max-entry-bytes 4194304 --hot.max-file-bytes 2097152
printf '\n### REPORTED, not refused: a pre-existing flag combination with the hot path OFF (S-11)\n'
printf '$ bunker fs mount <mp> --url http://127.0.0.1:1/dav --cache-max-entry-bytes 1024\n'
out=$("$BIN" fs mount "$MP" --url http://127.0.0.1:1/dav --cache-max-entry-bytes 1024 2>&1); rc=$?
printf 'exit=%d (the TRANSPORT refuses; the configuration did not)\n%s\n' "$rc" "$out"
