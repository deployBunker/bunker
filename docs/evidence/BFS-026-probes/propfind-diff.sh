#!/usr/bin/env bash
# propfind-diff.sh — attribute the ONE-BYTE PROPFIND length difference the
# parity battery reported, rather than waving at it. Prints the two raw bodies
# and a byte-level diff.
set -uo pipefail
NEW=/tmp/bfs026-bin/new/davserve
BASE=/tmp/bfs026-bin/base/davserve
PROBES="$(cd "$(dirname "$0")" && pwd)"
OUT="$(mktemp -d -t bfs026-propfind.XXXXXX)"

run() { # <davserve> <tree> <tag>
  "$1" --root "$2" --addr 127.0.0.1:0 >"$OUT/serve-$3.out" 2>&1 &
  echo $! > "$OUT/pid-$3"
  for _ in $(seq 1 80); do
    u=$(sed -n 's/^URL=//p' "$OUT/serve-$3.out" | head -1)
    [ -n "$u" ] && { echo "$u"; return 0; }
    sleep 0.25
  done
  return 1
}

for tag in base new; do
  TREE="$OUT/tree-$tag"
  python3 "$PROBES/mkfixture.py" "$TREE" >/dev/null
  # identical mtimes, so the only possible difference is the surface's own
  find "$TREE" -exec touch -d '2026-01-01T00:00:00Z' {} + 2>/dev/null
  BIN="$NEW"; [ "$tag" = base ] && BIN="$BASE"
  URL=$(run "$BIN" "$TREE" "$tag")
  curl -sS -X PROPFIND "$URL/" -H 'Depth: 1' -o "$OUT/body-$tag.xml"
  kill -TERM "$(cat "$OUT/pid-$tag")" 2>/dev/null
  echo "$tag: $(wc -c < "$OUT/body-$tag.xml") bytes"
done
sleep 0.3

echo
echo "=== the two bodies, tree token normalised ==="
for tag in base new; do
  sed 's/tree:[0-9a-f]\{16\}/tree:TOKEN/g' "$OUT/body-$tag.xml" > "$OUT/body-$tag.norm"
done
if diff -u "$OUT/body-base.norm" "$OUT/body-new.norm" > "$OUT/body.diff"; then
  echo "  normalised bodies are IDENTICAL — the raw difference is the tree token (and its one-byte length effect, below)"
else
  echo "  a real difference beyond the tree token:"
  sed 's/^/  /' "$OUT/body.diff"
fi
echo
echo "=== the raw bodies side by side, first 2 lines each ==="
echo "  base: $(head -c 400 "$OUT/body-base.xml" | head -2 | tr '\n' ' ')"
echo "  new : $(head -c 400 "$OUT/body-new.xml" | head -2 | tr '\n' ' ')"
echo
echo "=== is the length difference the tree token itself? ==="
python3 - "$OUT/body-base.xml" "$OUT/body-new.xml" <<'PY'
import re, sys
a = open(sys.argv[1], 'rb').read()
b = open(sys.argv[2], 'rb').read()
ta = re.search(rb'tree:[0-9a-f]{16}', a)
tb = re.search(rb'tree:[0-9a-f]{16}', b)
print("  base len=%d token=%s" % (len(a), ta.group().decode() if ta else "-"))
print("  new  len=%d token=%s" % (len(b), tb.group().decode() if tb else "-"))
na = re.sub(rb'tree:[0-9a-f]{16}', b'tree:TOKEN', a)
nb = re.sub(rb'tree:[0-9a-f]{16}', b'tree:TOKEN', b)
print("  equal after normalising the token: %s" % (na == nb))
if na != nb:
    for i, (x, y) in enumerate(zip(na, nb)):
        if x != y:
            print("  first byte difference at %d: %r vs %r" % (i, na[max(0,i-40):i+40], nb[max(0,i-40):i+40]))
            break
    print("  lengths after normalising: %d vs %d" % (len(na), len(nb)))
PY
echo
echo "=== the only remaining candidate: X-Bunker-Tree header vs the b:tree property ==="
echo "transcripts: $OUT"
