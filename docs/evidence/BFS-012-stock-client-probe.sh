#!/usr/bin/env bash
# STOCK-CLIENT COMPAT: the release's hard constraint is that HTTP/1.1 keeps working for clients
# that have never heard of h2/h3 or our extensions. curl IS such a client. Bounded everywhere.
set -uo pipefail
T=/tmp/stock-client
rm -rf "$T" 2>/dev/null; mkdir -p "$T/src/sub"
printf 'stock client reads this\n' > "$T/src/plain.txt"
printf 'nested\n' > "$T/src/sub/inner.txt"
cd /home/kara/bunker || exit 1
go build -o /tmp/davserve ./probes/davserve 2>&1 | head -2
/tmp/davserve --root "$T/src" --addr 127.0.0.1:0 > "$T/srv.log" 2>&1 &
SRV=$!
for i in $(seq 1 40); do ROOT=$(grep -oE 'URL=[^ ]+' "$T/srv.log" 2>/dev/null | head -1 | cut -d= -f2-); [ -n "$ROOT" ] && break; sleep 0.25; done
echo "surface root: ${ROOT:-NOT-UP}   (server pid $SRV)"
[ -z "${ROOT:-}" ] && { echo "server did not start"; exit 1; }
D="${ROOT%/}/plain.txt"; D2="${ROOT%/}/sub/inner.txt"
echo

echo "=== 1. is curl even speaking HTTP/1.1? (prove the client, not just the claim) ==="
timeout 20 curl -sS -o /dev/null -w '  http_version=%{http_version}  code=%{http_code}\n' "$D" 2>&1 | sed 's/^/  /'
echo

echo "=== 2. the stock verbs a plain client uses ==="
printf '  GET      : '; timeout 20 curl -sS -o /dev/null -w 'code=%{http_code} bytes=%{size_download}\n' "$D"
printf '  GET body : '; timeout 20 curl -sS "$D" | tr -d '\n' | sed 's/^/"/;s/$/"/'
printf '\n  HEAD     : '; timeout 20 curl -sS -I -o /dev/null -w 'code=%{http_code}\n' "$D"
printf '  OPTIONS  : '; timeout 20 curl -sS -X OPTIONS -o /dev/null -w 'code=%{http_code}\n' "$D"
printf '  PROPFIND : '; timeout 20 curl -sS -X PROPFIND -H 'Depth: 1' -o /dev/null -w 'code=%{http_code}\n' "${ROOT%/}/"
printf '  nestedGET: '; timeout 20 curl -sS -o /dev/null -w 'code=%{http_code}\n' "$D2"
echo

echo "=== 3. a stock client WRITING (no extensions, plain PUT) ==="
printf '  PUT new  : '; timeout 30 curl -sS -X PUT --data-binary 'written by curl, not by our client' -o /dev/null -w 'code=%{http_code}\n' "${ROOT%/}/curl-made.txt"
printf '  GET back : '; timeout 20 curl -sS "${ROOT%/}/curl-made.txt"; echo
echo "  on the server's disk: $(cat "$T/src/curl-made.txt" 2>/dev/null || echo 'ABSENT')"
printf '  DELETE   : '; timeout 30 curl -sS -X DELETE -o /dev/null -w 'code=%{http_code}\n' "${ROOT%/}/curl-made.txt"
echo "  after DELETE on disk: $(cat "$T/src/curl-made.txt" 2>/dev/null || echo 'GONE (correct)')"
echo

echo "=== 4. CONFLICT REFUSAL with a stale base (BFS-004 6.1 - the refuse-loudly rule) ==="
ET=$(timeout 20 curl -sS -I "$D" 2>/dev/null | grep -i '^etag:' | tr -d '\r' | cut -d' ' -f2-)
echo "  current ETag : ${ET:-none}"
printf '  PUT with a STALE If-Match: '
timeout 30 curl -sS -X PUT -H 'If-Match: "sha256:0000000000000000000000000000000000000000000000000000000000000000"' \
  --data-binary 'should be REFUSED' -o "$T/conflict.out" -w 'code=%{http_code}\n' "$D"
echo "  response body (first 200 chars):"; head -c 200 "$T/conflict.out" | sed 's/^/    /'; echo
echo "  did the file survive unmodified? -> $(head -c 40 "$T/src/plain.txt" | tr -d '\n')"
printf '  PUT with the CORRECT If-Match: '
timeout 30 curl -sS -X PUT -H "If-Match: $ET" --data-binary 'legitimately updated' -o /dev/null -w 'code=%{http_code}\n' "$D" 2>/dev/null || echo "(needs the etag header)"
echo "  on disk now: $(cat "$T/src/plain.txt" 2>/dev/null | head -c 40)"
echo

echo "=== 5. older/no-extension compatibility: does it still answer plainly? ==="
printf '  GET with a stock User-Agent and no extension headers: '
timeout 20 curl -sS -A 'Mozilla/4.0 (a very old client)' -o /dev/null -w 'code=%{http_code}\n' "$D"
printf '  GET with no Accept header at all: '
timeout 20 curl -sS -H 'Accept:' -o /dev/null -w 'code=%{http_code}\n' "$D"
echo
echo "=== cleanup ==="
kill -TERM $SRV 2>/dev/null; sleep 1; kill -0 $SRV 2>/dev/null && kill -KILL $SRV 2>/dev/null
echo "  server stopped: $(kill -0 $SRV 2>/dev/null && echo no || echo yes)"
echo "  leftover mounts: $(grep -c "$T" /proc/mounts 2>/dev/null)"
