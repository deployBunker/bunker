#!/usr/bin/env bash
# stock-parity.sh — BFS-026's fourth requirement, measured: a client that does
# not use the invalidation channel must behave EXACTLY as before.
#
# The same raw verb battery is driven against two servers over byte-identical
# fixture trees: one built from the tree as BFS-026 was filed, one from the tree
# this row changes. Every answer is reduced to the facts a stock client acts on
# (status, verdict, Allow, DAV, ETag, body digest, length) and the two
# transcripts are diffed. The diff must be EMPTY.
#
# The one intended difference — the capability document, which is the whole
# point of the change — is printed separately and ONLY for a client that already
# speaks X-Bunker-Op. A stock client never sends that header, so it never sees it.
#
# usage: stock-parity.sh --new-bin DIR --base-bin DIR
set -uo pipefail

NEW=""; BASE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --new-bin) NEW="$2"; shift 2;;
    --base-bin) BASE="$2"; shift 2;;
    *) echo "unknown arg $1" >&2; exit 2;;
  esac
done
[ -n "$NEW" ] && [ -n "$BASE" ] || { echo "both --new-bin and --base-bin are required" >&2; exit 2; }
PROBES="$(cd "$(dirname "$0")" && pwd)"

OUT="$(mktemp -d -t bfs026-parity.XXXXXX)"
echo "out = $OUT"

start() { # <davserve path> <tree> <tag>
  "$1" --root "$2" --addr 127.0.0.1:0 >"$OUT/serve-$3.out" 2>&1 &
  echo $! > "$OUT/pid-$3"
  for _ in $(seq 1 80); do
    url=$(sed -n 's/^URL=//p' "$OUT/serve-$3.out" | head -1)
    [ -n "$url" ] && { echo "$url"; return 0; }
    sleep 0.25
  done
  return 1
}

battery() { # <url> <outfile>
  local url="$1" out="$2"
  local h="$OUT/h.$RANDOM" b="$OUT/b.$RANDOM"
  : > "$out"
  req() { # method path [curl args]
    local method="$1" path="$2"; shift 2
    local head=""
    [ "$method" = "HEAD" ] && head="--head"
    local code
    if [ -n "$head" ]; then
      code=$(curl -sS -D "$h" -o /dev/null -w '%{http_code}' --request-target "$path" --head "$url" "$@" 2>/dev/null)
    else
      code=$(curl -sS -D "$h" -o "$b" -w '%{http_code}' --request-target "$path" -X "$method" "$url" "$@" 2>/dev/null)
    fi
    local verdict allow dav etag bhash len
    verdict=$(sed -n 's/^X-Bunker-Verdict: \(.*\)\r*/\1/ip' "$h" | head -1)
    allow=$(sed -n 's/^Allow: \(.*\)\r*/\1/ip' "$h" | head -1)
    dav=$(sed -n 's/^DAV: \(.*\)\r*/\1/ip' "$h" | head -1)
    etag=$(sed -n 's/^ETag: \(.*\)\r*/\1/ip' "$h" | head -1)
    len=$(wc -c < "$b" 2>/dev/null || echo 0)
    # The digest is taken over the body with the two legitimately-per-run values
    # normalised out (the tree token, and the server's own measured duration_ms).
    # A digest over the RAW body would flag those two as differences forever,
    # which is an instrument defect and not a behaviour change.
    bhash=$(sed -e 's/tree:[0-9a-f]\{16\}/tree:TOKEN/g' -e 's/"duration_ms":[0-9]*/"duration_ms":N/g' "$b" 2>/dev/null | sha256sum | awk '{print $1}' | cut -c1-20)
    printf '%-9s %-20s -> %s verdict=%-22s allow=%-10s dav=%-5s etag=%-12s body=%s len=%s\n' \
      "$method" "$path" "$code" "${verdict:--}" "${allow:--}" "${dav:--}" "${etag:--}" "$bhash" "$len" >> "$out"
  }

  req OPTIONS '/dav/'
  req OPTIONS '*'
  req GET '/dav/README.md'
  req HEAD '/dav/README.md'
  req GET '/dav/nope.txt'
  req GET '/dav/src'
  req GET '/dav/README.md' -H 'Range: bytes=0-4'
  req PROPFIND '/dav/' -H 'Depth: 0'
  req PROPFIND '/dav/' -H 'Depth: 1'
  req PROPFIND '/dav/src' -H 'Depth: 1'
  req PROPFIND '/dav/' -H 'Depth: infinity'
  req PROPFIND '/dav/README.md'
  req PUT '/dav/stock.txt' --data-binary 'stock client wrote this'
  req GET '/dav/stock.txt'
  req PUT '/dav/stock.txt' -H 'If-Match: "sha256:0000000000000000000000000000000000000000000000000000000000000000"' --data-binary 'must be refused'
  req PUT '/dav/cond.txt' -H 'If-None-Match: *' --data-binary 'created once'
  req DELETE '/dav/stock.txt'
  req MKCOL '/dav/stockdir'
  req COPY '/dav/README.md' -H 'Destination: /dav/copy.md'
  req MOVE '/dav/copy.md' -H 'Destination: /dav/moved.md'
  req PROPPATCH '/dav/README.md' --data-binary '<D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><x:a xmlns:x="urn:x">1</x:a></D:prop></D:set></D:propertyupdate>'
  req LOCK '/dav/README.md'
  req UNLOCK '/dav/README.md'
  req FROBNICATE '/dav/'
  req REPORT '/dav/'
  req POST '/dav/'
  req POST '/dav/' -H 'X-Bunker-Op: frobnicate'
  req POST '/dav/' -H 'X-Bunker-Op: status'
  rm -f "$h" "$b"
}

norm() { sed -e 's/tree:[0-9a-f]\{16\}/tree:TOKEN/g' -e 's/"duration_ms":[0-9]*/"duration_ms":N/g' "$1"; }

TREE_NEW="$OUT/new/tree"; TREE_BASE="$OUT/base/tree"
mkdir -p "$OUT/new" "$OUT/base"
python3 "$PROBES/mkfixture.py" "$TREE_NEW" >/dev/null
python3 "$PROBES/mkfixture.py" "$TREE_BASE" >/dev/null

# One server at a time, so the two batteries see identical trees and identical
# mutation ordering.
URL_BASE=$(start "$BASE/davserve" "$TREE_BASE" base) || { echo "base davserve failed"; exit 1; }
echo "base server = $URL_BASE"
battery "$URL_BASE" "$OUT/battery-base.txt"
kill -TERM "$(cat "$OUT/pid-base")" 2>/dev/null

URL_NEW=$(start "$NEW/davserve" "$TREE_NEW" new) || { echo "new davserve failed"; exit 1; }
echo "new  server = $URL_NEW"
battery "$URL_NEW" "$OUT/battery-new.txt"
kill -TERM "$(cat "$OUT/pid-new")" 2>/dev/null
sleep 0.3

norm "$OUT/battery-new.txt" > "$OUT/battery-new.norm"
norm "$OUT/battery-base.txt" > "$OUT/battery-base.norm"

echo "=== the stock-client battery, base vs new (normalised: tree token, duration_ms) ==="
cat "$OUT/battery-new.norm" | sed 's/^/  /'
echo
if diff -u "$OUT/battery-base.norm" "$OUT/battery-new.norm" > "$OUT/battery.diff"; then
  echo "DIFF: EMPTY — every answer a client that ignores the channel can see is unchanged"
  PARITY=PASS
else
  echo "DIFF (base -> new):"
  sed 's/^/  /' "$OUT/battery.diff"
  PARITY=FAIL
fi

echo
echo "=== the ONE intended difference, and who can see it ==="
start "$BASE/davserve" "$TREE_BASE" base2 >/dev/null
B2=$(sed -n 's/^URL=//p' "$OUT/serve-base2.out" | head -1)
start "$NEW/davserve" "$TREE_NEW" new2 >/dev/null
N2=$(sed -n 's/^URL=//p' "$OUT/serve-new2.out" | head -1)
curl -sS -X POST "$B2" -H 'X-Bunker-Op: capabilities' > "$OUT/caps-base.json" 2>/dev/null
curl -sS -X POST "$N2" -H 'X-Bunker-Op: capabilities' > "$OUT/caps-new.json" 2>/dev/null
kill -TERM "$(cat "$OUT/pid-base2")" 2>/dev/null
kill -TERM "$(cat "$OUT/pid-new2")" 2>/dev/null
python3 - "$OUT/caps-base.json" "$OUT/caps-new.json" <<'PY'
import json, sys
def doc(p):
    d = json.load(open(p))
    return d["result"]["capabilities"]
b, n = doc(sys.argv[1]), doc(sys.argv[2])
print("  document_version   : %s -> %s   (a client that does not know it must fail closed)" % (b["document_version"], n["document_version"]))
print("  methods            : %s" % ("unchanged" if b["methods"] == n["methods"] else "CHANGED"))
print("  op.ops             : %s" % (n["extensions"]["op"]["ops"]))
print("  watch block        : %s" % json.dumps(n["extensions"]["watch"], sort_keys=True))
def degs(d):
    return sorted(x["capability"] for x in d["degradations"])
print("  degradations base  : %s" % degs(b))
print("  degradations new   : %s" % degs(n))
print("  (only a client that sends X-Bunker-Op sees any of this)")
PY

echo
echo "PARITY VERDICT: $PARITY"
echo "transcripts: $OUT"
[ "$PARITY" = PASS ] || exit 1
