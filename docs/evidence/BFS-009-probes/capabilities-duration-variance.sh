#!/usr/bin/env bash
# bfs009-capabilities-duration-variance.sh — why the guard's
# TestSameSurfaceOverHTTP1AndHTTP2/op_capabilities cell is load-sensitive.
#
# That cell compares the two version bodies byte-for-byte after normalising ONLY
# the `proto` key (internal/server/webdav/transport_test.go:200-216, blankProto).
# The capability document is returned inside the E-4 envelope, and the envelope
# carries `duration_ms` — a MEASURED server-side elapsed time. So two identical
# requests produce different bodies whenever one of them takes >= 1 ms, and the
# test has no normalisation for it.
#
# This samples the same op repeatedly and reports the distribution, plus whether
# two consecutive responses with identical content differ in that field.
#
# usage: bfs009-capabilities-duration-variance.sh [N] [URL]
set -uo pipefail
N="${1:-60}"
U="${2:-http://127.0.0.1:18491/dav}"
OUT=/tmp/bfs009/cap-durations.txt
: > "$OUT"
for _ in $(seq 1 "$N"); do
  curl -sS -X POST -H 'X-Bunker-Op: capabilities' -H 'Content-Type: application/json' \
    --data '{}' "$U/" >> "$OUT"
  echo >> "$OUT"
done
python3 - "$OUT" <<'PY'
import json, sys, collections
rows = []
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        rows.append(json.loads(line))
    except Exception:
        pass
dur = [r.get("duration_ms") for r in rows]
print("requests sampled          : %d" % len(rows))
print("duration_ms distribution  : %s" % dict(sorted(collections.Counter(dur).items())))
# everything except duration_ms must be identical across the samples
def strip(r):
    r = dict(r)
    r.pop("duration_ms", None)
    return json.dumps(r, sort_keys=True)
variants = collections.Counter(strip(r) for r in rows)
print("distinct bodies ignoring duration_ms : %d" % len(variants))
print("duration_ms values observed          : %d" % len(set(dur)))
print()
if len(set(dur)) > 1:
    print("VERDICT: two identical requests returned DIFFERENT duration_ms values")
    print("         (%s), so the guard cell's byte comparison can fail on that field" % sorted(set(dur)))
else:
    print("VERDICT: every sampled request reported the same duration_ms (%s) - %s" % (dur[0] if dur else "?", sorted(set(dur))))
PY
