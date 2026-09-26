#!/usr/bin/env bash
# webdav-h1h2-probe.sh — prove, against a LIVE bunkerd, that the BFS-004 WebDAV
# surface is served over HTTP/1.1 AND HTTP/2 (TLS ALPN), that h2c is
# prior-knowledge only, that HTTP/1.1 is a first-class protocol rather than a
# fallback, and (BFS-007) that the SAME surface is served over HTTP/3 (QUIC) on
# the SAME port number, discovered through Alt-Svc and advertised only while the
# UDP socket is live.
#
# WHY: the row's acceptance is "the capability exists and is OBSERVABLE". The
# unit tests assert the surface through httptest; this probe asserts it through
# a real process and real sockets, using clients that know nothing about bunker
# (curl for h1/h2, the Go h3client in probes/h3client for QUIC — curl on this
# box has no HTTP/3 support at all, measured in BFS-002 §1). It also reproduces
# the facts that decide the design:
#   * the SAME request over h1.1, h2 and h3 returns the same status, the same
#     bytes and the same headers, with the version echoed in X-Bunker-Proto;
#   * h2c (cleartext prior knowledge) works with the opt-in, while the RFC 7540
#     `Upgrade: h2c` dance is answered over HTTP/1.1 with no error — net/http
#     does not implement it, so the surface reports that as a degradation
#     instead of implying general h2c support;
#   * Alt-Svc appears on h1/h2 responses ONLY while the h3 listener is up, and
#     an h3 client against an h3-less daemon FAILS — the two arms of "advertised
#     only while listening", measured on the running daemon rather than
#     asserted;
#   * h3 without TLS is refused at startup instead of silently serving a
#     TCP-only daemon (QUIC always encrypts).
#
# SAFETY: everything happens in a scratch directory under $TMPDIR (served tree,
# config, generated self-signed certificate) on loopback ports. No repository
# file is read or written; the daemon runs with auth enabled and a token
# generated at run time, so no credential is committed here.
#
# USAGE:
#   probes/webdav-h1h2-probe.sh [--binary PATH] [--port N] [--keep]
#
#   --binary PATH  use an existing bunkerd binary (default: build one)
#   --port N       first loopback port to bind (default: 18080; N, N+1, N+2)
#   --keep         keep the scratch directory and the daemon log
#
# EXIT: 0 when every cell passed, 1 when any cell failed, 2 on usage error.

set -uo pipefail

BIN=""
PORT=""
KEEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BIN="${2:-}"; shift 2 ;;
    --port)   PORT="${2:-}"; shift 2 ;;
    --keep)   KEEP=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# pick_port returns a loopback port nothing is listening on. A fixed default
# would collide with a development daemon (this workstation runs one on 18080),
# and the probe would then be measuring THAT daemon.
pick_port() {
  local candidate
  for _ in $(seq 1 40); do
    candidate=$(( (RANDOM % 20000) + 20000 ))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

if [ -z "$PORT" ]; then
  PORT="$(pick_port)" || { echo "no free loopback port found" >&2; exit 2; }
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/webdav-h1h2-probe.XXXXXX")"
ROOT="$WORK/tree"
mkdir -p "$ROOT/src" "$ROOT/empty"
printf 'package main\n\nfunc main() {}\n' > "$ROOT/src/main.go"
printf 'package main\n\nfunc util() {}\n' > "$ROOT/src/util.go"
printf '# probe fixture\n' > "$ROOT/README.md"

# A per-run credential: never a committed literal.
TOKEN="$(date +%s)-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PORT_TLS=$((PORT + 1))
PORT_H3=$((PORT + 2))
TREE_URL="http://127.0.0.1:$PORT/dav"
TLS_URL="https://127.0.0.1:$PORT_TLS/dav"
H3_URL="https://127.0.0.1:$PORT_H3/dav"
DAEMON_PID=""
PASS=0
FAIL=0
STATUS=""
HTTPVER=""
HEADERS=""
BODY=""

cleanup() {
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null
    wait "$DAEMON_PID" 2>/dev/null
  fi
  if [ "$KEEP" = "1" ]; then
    echo "scratch kept at $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

ok()  { printf 'PASS  %s\n' "$1"; PASS=$((PASS + 1)); }
no()  { printf 'FAIL  %s\n' "$1"; FAIL=$((FAIL + 1)); }

expect_eq() { # label got want
  if [ "$2" = "$3" ]; then ok "$1 ($2)"; else no "$1: got [$2] want [$3]"; fi
}

expect_contains() { # label haystack needle
  case "$2" in
    *"$3"*) ok "$1" ;;
    *) no "$1: [$2] does not contain [$3]" ;;
  esac
}

# req METHOD URL [curl args...] — fills STATUS, HTTPVER, HEADERS, BODY.
req() {
  local method="$1" url="$2"
  shift 2
  : > "$WORK/headers"
  local out
  out="$(curl -sS -X "$method" -o "$WORK/body" -D "$WORK/headers" \
    -w '%{http_code} %{http_version}' -u "bunker:$TOKEN" "$@" "$url")"
  STATUS="${out%% *}"
  HTTPVER="${out##* }"
  HEADERS="$WORK/headers"
  BODY="$WORK/body"
}

hdr() { # header name (case-insensitive), first value
  local file="${HEADERS:-$WORK/headers}"
  grep -i "^$1:" "$file" 2>/dev/null | head -1 | sed 's/^[^:]*:[[:space:]]*//' | tr -d '\r'
}

body_of() { cat "$BODY"; }

# body_of_h3 is the body the last h3req fetched: the HTTP/3 client writes it to
# a fixed path so the shell can hash it or read it as text.
body_of_h3() { cat "$WORK/h3-body"; }

health_wait() { # url prefix (http/https) with port
  local i
  for i in $(seq 1 60); do
    if curl -sS -k -o /dev/null "$1/healthz" 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

start_daemon() { # config log health-url
  "$BIN" --config "$1" >"$2" 2>&1 &
  DAEMON_PID=$!
  if ! health_wait "$3"; then
    no "daemon did not become healthy; log tail:"
    tail -5 "$2" || true
    return 1
  fi
  return 0
}

stop_daemon() {
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null
    wait "$DAEMON_PID" 2>/dev/null
  fi
  DAEMON_PID=""
}

if [ -z "$BIN" ]; then
  BIN="$WORK/bunkerd"
  echo "building bunkerd from $REPO_ROOT ..."
  if ! (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/bunkerd); then
    echo "go build failed" >&2
    exit 2
  fi
fi
[ -x "$BIN" ] || { echo "not executable: $BIN" >&2; exit 2; }

# The h3 instrument: a real HTTP/3 client. Built from the probe's own directory
# so it always matches the tree under test.
H3CLIENT="$WORK/h3client"
echo "building h3client from $REPO_ROOT ..."
if ! (cd "$REPO_ROOT" && go build -o "$H3CLIENT" ./probes/h3client); then
  echo "go build ./probes/h3client failed" >&2
  exit 2
fi
[ -x "$H3CLIENT" ] || { echo "not executable: $H3CLIENT" >&2; exit 2; }

# h3req URL [args...] — drive the HTTP/3 client and fill H3_OUT / H3_PROTO /
# H3_STATUS / H3_ALPN / H3_SERVER_PROTO / H3_BODY_SHA / H3_BYTES. Returns the
# client's exit status, which is the point of the negative arm: an h3 request
# against a daemon with no QUIC listener must FAIL, not be quietly served.
H3_OUT=""
H3_EXIT=0
h3req() {
  local url="$1"
  shift
  H3_OUT="$("$H3CLIENT" -url "$url" -user bunker -token "$TOKEN" -out "$WORK/h3-body" "$@" 2>&1)"
  H3_EXIT=$?
}

# h3field KEY — read one key=value field out of the client's report line.
h3field() {
  printf '%s\n' "$H3_OUT" | tr ' ' '\n' | sed -n "s/^$1=//p" | head -1
}

cat > "$WORK/cleartext.yaml" <<YAML
server:
  grpc_addr: "127.0.0.1:$PORT"
  rest_addr: ""
  request_timeout: 60s
  h2c_enabled: true
  webdav_enabled: true
  webdav_root: "$ROOT"
tls:
  enabled: false
  insecure_dev: true
auth:
  enabled: true
  token: "$TOKEN"
agent:
  base_data_dir: "$WORK/data"
  registry:
    enabled: false
YAML

cat > "$WORK/tls.yaml" <<YAML
server:
  grpc_addr: "127.0.0.1:$PORT_TLS"
  rest_addr: ""
  request_timeout: 60s
  webdav_enabled: true
  webdav_root: "$ROOT"
tls:
  enabled: true
  self_signed: true
  hosts: ["127.0.0.1", "localhost"]
  cert_file: "$WORK/cert.pem"
  key_file: "$WORK/key.pem"
auth:
  enabled: true
  token: "$TOKEN"
agent:
  base_data_dir: "$WORK/data"
  registry:
    enabled: false
YAML

# BFS-007: HTTP/3 on the SAME port number as the TCP listener (rest_addr is
# empty here, so the derived h3 address IS the gRPC address) — one port number,
# two transports, one process.
cat > "$WORK/h3.yaml" <<YAML
server:
  grpc_addr: "127.0.0.1:$PORT_H3"
  rest_addr: ""
  request_timeout: 60s
  webdav_enabled: true
  webdav_root: "$ROOT"
  h3_enabled: true
tls:
  enabled: true
  self_signed: true
  hosts: ["127.0.0.1", "localhost"]
  cert_file: "$WORK/cert.pem"
  key_file: "$WORK/key.pem"
auth:
  enabled: true
  token: "$TOKEN"
agent:
  base_data_dir: "$WORK/data"
  registry:
    enabled: false
YAML

# BFS-007: the incoherent pair. QUIC always encrypts, so this must be REFUSED at
# startup rather than served as a TCP-only daemon the operator believes has h3.
cat > "$WORK/h3-cleartext.yaml" <<YAML
server:
  grpc_addr: "127.0.0.1:$PORT_H3"
  rest_addr: ""
  request_timeout: 60s
  webdav_enabled: true
  webdav_root: "$ROOT"
  h3_enabled: true
tls:
  enabled: false
  insecure_dev: true
auth:
  enabled: true
  token: "$TOKEN"
agent:
  base_data_dir: "$WORK/data"
  registry:
    enabled: false
YAML

echo "== HTTP/1.1 + h2c (cleartext config, h2c opt-in ON) =="

if ! start_daemon "$WORK/cleartext.yaml" "$WORK/cleartext.log" "http://127.0.0.1:$PORT"; then
  echo "cannot continue without the cleartext daemon" >&2
  exit 1
fi

# ── credentials: the mount rides the daemon's own credential model ──────────
STATUS="$(curl -sS -o "$WORK/body" -D "$WORK/headers" -w '%{http_code}' "http://127.0.0.1:$PORT/dav/README.md")"
HEADERS="$WORK/headers"
BODY="$WORK/body"
expect_eq "unauthenticated WebDAV request is refused" "$STATUS" "401"
expect_contains "401 carries WWW-Authenticate" "$(hdr WWW-Authenticate)" "Basic"
expect_contains "401 body carries the verdict code too" "$(body_of)" "unauthenticated"

# ── HTTP/1.1 is the compatibility floor ─────────────────────────────────────
req GET "$TREE_URL/src/main.go" --http1.1
expect_eq "GET over HTTP/1.1" "$STATUS" "200"
expect_eq "server observed HTTP/1.1" "$(hdr X-Bunker-Proto)" "HTTP/1.1"
expect_contains "ETag is the content hash" "$(hdr ETag)" "sha256:"
expect_eq "HTTP/1.1 body is the file's bytes" \
  "$(sha256sum < "$BODY" | awk '{print $1}')" \
  "$(sha256sum < "$ROOT/src/main.go" | awk '{print $1}')"

req OPTIONS "$TREE_URL/" --http1.1
expect_eq "OPTIONS" "$STATUS" "200"
expect_eq "DAV class advertised" "$(hdr DAV)" "1"
expect_contains "Allow names PROPFIND" "$(hdr Allow)" "PROPFIND"
expect_eq "capability document version advertised" "$(hdr X-Bunker-Capabilities)" "1"

# The asterisk-form request target is sent explicitly; curl has no positional
# syntax for it, and a router could never match it (which is why the daemon
# answers it before routing).
req OPTIONS "http://127.0.0.1:$PORT/" --http1.1 --request-target '*'
expect_eq "OPTIONS * answers 200" "$STATUS" "200"
if [ -n "$(hdr DAV)" ]; then
  no "OPTIONS * must not advertise DAV (RFC 4918 10.1)"
else
  ok "OPTIONS * omits the DAV header"
fi
expect_contains "OPTIONS * still carries Allow" "$(hdr Allow)" "PROPFIND"

req PROPFIND "$TREE_URL/src" --http1.1 -H 'Depth: 1'
expect_eq "PROPFIND Depth: 1" "$STATUS" "207"
expect_contains "multistatus body" "$(body_of)" "D:multistatus"
expect_contains "members listed" "$(body_of)" "main.go"

req PROPFIND "$TREE_URL/src" --http1.1 -H 'Depth: infinity'
expect_eq "PROPFIND Depth: infinity is refused" "$STATUS" "403"
expect_eq "refusal code" "$(hdr X-Bunker-Verdict)" "propfind_finite_depth"
expect_contains "RFC's own precondition element" "$(body_of)" "propfind-finite-depth"

req PROPFIND "$TREE_URL/src" --http1.1
expect_eq "PROPFIND without Depth is refused" "$STATUS" "400"
expect_eq "refusal code" "$(hdr X-Bunker-Verdict)" "depth_required"

req REPORT "$TREE_URL/" --http1.1
expect_eq "REPORT (unimplemented WebDAV extension) is refused" "$STATUS" "405"
expect_contains "405 carries Allow" "$(hdr Allow)" "PROPFIND"

req FROBNICATE "$TREE_URL/" --http1.1
expect_eq "unknown method is refused differently from known-unsupported" "$STATUS" "501"
expect_eq "refusal code" "$(hdr X-Bunker-Verdict)" "method_unknown"

req POST "$TREE_URL/" --http1.1 -H 'X-Bunker-Op: snapshot' -H 'Content-Type: application/json' \
  --data '{"path":"src","depth":"infinity","include_hash":true}'
expect_eq "X-Bunker-Op: snapshot" "$STATUS" "200"
expect_contains "snapshot envelope" "$(body_of)" '"op":"snapshot"'
expect_contains "snapshot entries" "$(body_of)" '"path":"src/main.go"'

req POST "$TREE_URL/" --http1.1 -H 'X-Bunker-Op: watch' -H 'Content-Type: application/json' --data '{}'
expect_eq "X-Bunker-Op: watch degrades loudly" "$STATUS" "501"
expect_eq "degradation code" "$(hdr X-Bunker-Verdict)" "capability_unavailable"
expect_contains "degradation names the mode in force" "$(hdr X-Bunker-Capability)" "mode=poll"

# ── the conflict rule, on the live surface ──────────────────────────────────
printf 'package main\n\nfunc main() { /* writer B */ }\n' > "$ROOT/src/main.go"
printf 'package main\n\nfunc main() { /* writer A */ }\n' > "$WORK/writers-bytes"
BEFORE="$(sha256sum "$ROOT/src/main.go" | awk '{print $1}')"
STALE="sha256:$(head -c 32 /dev/zero | sha256sum | awk '{print $1}')"
req PUT "$TREE_URL/src/main.go" --http1.1 -H "If-Match: \"$STALE\"" --data-binary @"$WORK/writers-bytes"
expect_eq "stale-base write is refused" "$STATUS" "412"
expect_eq "refusal code" "$(hdr X-Bunker-Verdict)" "hash_mismatch"
expect_contains "current hash in the header" "$(hdr X-Bunker-Current-Hash)" "sha256:"
expect_contains "expected hash in the header" "$(hdr X-Bunker-Expected-Hash)" "sha256:"
expect_contains "both hashes in the body" "$(body_of)" "<b:current>"
AFTER="$(sha256sum "$ROOT/src/main.go" | awk '{print $1}')"
expect_eq "the refused write left the file byte-identical" "$AFTER" "$BEFORE"

req PUT "$TREE_URL/src/main.go" --http1.1 -H "If-Match: \"$STALE\"" --data-binary @"$ROOT/src/main.go"
expect_eq "stale base + identical bytes is a reported no-op" "$STATUS" "204"
expect_eq "no-op code" "$(hdr X-Bunker-Verdict)" "identical_content"

# ── h2c: prior knowledge yes, Upgrade dance no ──────────────────────────────
req GET "$TREE_URL/src/main.go" --http2-prior-knowledge
expect_eq "h2c prior knowledge is served" "$STATUS" "200"
expect_eq "h2c negotiated HTTP/2" "$HTTPVER" "2"
expect_eq "server observed HTTP/2 over h2c" "$(hdr X-Bunker-Proto)" "HTTP/2.0"

req GET "$TREE_URL/src/main.go" --http2
expect_eq "RFC 7540 Upgrade: h2c silently downgrades (documented)" "$HTTPVER" "1.1"
expect_eq "and the downgrade is visible in the echo" "$(hdr X-Bunker-Proto)" "HTTP/1.1"

stop_daemon

echo
echo "== HTTP/1.1 + HTTP/2 over TLS (ALPN) =="

if ! start_daemon "$WORK/tls.yaml" "$WORK/tls.log" "https://127.0.0.1:$PORT_TLS"; then
  echo "cannot continue without the TLS daemon" >&2
  exit 1
fi

req GET "$TLS_URL/src/main.go" --http1.1 -k
expect_eq "GET over HTTP/1.1 (TLS)" "$STATUS" "200"
expect_eq "server observed HTTP/1.1" "$(hdr X-Bunker-Proto)" "HTTP/1.1"
cp "$BODY" "$WORK/body-h1"

req GET "$TLS_URL/src/main.go" --http2 -k
expect_eq "GET over HTTP/2 (TLS ALPN)" "$STATUS" "200"
expect_eq "h2 negotiated" "$HTTPVER" "2"
expect_eq "server observed HTTP/2.0" "$(hdr X-Bunker-Proto)" "HTTP/2.0"
cp "$BODY" "$WORK/body-h2"

if cmp -s "$WORK/body-h1" "$WORK/body-h2"; then
  ok "the h1 and h2 responses are byte-identical"
else
  no "the h1 and h2 bodies differ"
fi

req PROPFIND "$TLS_URL/src" --http2 -k -H 'Depth: 1'
expect_eq "PROPFIND over h2" "$STATUS" "207"
expect_contains "same multistatus shape over h2" "$(body_of)" "D:multistatus"
expect_eq "server observed HTTP/2.0 on the multistatus" "$(hdr X-Bunker-Proto)" "HTTP/2.0"

# The h2-only and h1-only arms of the same request must agree header for header
# on everything except the version echo.
req OPTIONS "$TLS_URL/" --http1.1 -k
H1_ALLOW="$(hdr Allow)"
H1_DAV="$(hdr DAV)"
H1_EXT="$(hdr X-Bunker-Extensions)"
req OPTIONS "$TLS_URL/" --http2 -k
expect_eq "Allow is identical over h2" "$(hdr Allow)" "$H1_ALLOW"
expect_eq "DAV is identical over h2" "$(hdr DAV)" "$H1_DAV"
expect_eq "X-Bunker-Extensions is identical over h2" "$(hdr X-Bunker-Extensions)" "$H1_EXT"

# ── BFS-007 negative arm: this daemon has NO h3 listener ────────────────────
# The two-arm test of "advertised only while listening": with server.h3_enabled
# off, the TCP responses must carry NO Alt-Svc, and the h3 client must FAIL.
# Without this arm a server that advertised h3 unconditionally, or a client
# that silently fell back to HTTP/2, would pass every h3 cell below.
req GET "$TLS_URL/src/main.go" --http2 -k
if [ -n "$(hdr Alt-Svc)" ]; then
  no "Alt-Svc [$(hdr Alt-Svc)] was advertised by a daemon with no h3 listener"
else
  ok "no Alt-Svc while server.h3_enabled is false"
fi
req GET "$TLS_URL/src/main.go" --http1.1 -k
if [ -n "$(hdr Alt-Svc)" ]; then
  no "Alt-Svc over HTTP/1.1 without an h3 listener: [$(hdr Alt-Svc)]"
else
  ok "no Alt-Svc over HTTP/1.1 either"
fi
h3req "$TLS_URL/src/main.go"
if [ "$H3_EXIT" -ne 0 ]; then
  ok "an h3 client against a TCP-only daemon fails (exit $H3_EXIT): $H3_OUT"
else
  no "the h3 client succeeded against a daemon with no QUIC listener: $H3_OUT"
fi

stop_daemon

echo
echo "== capability document (transports block) =="
# The cleartext run reported h2 unavailable and h2c available; the TLS run
# reports the reverse for h2 and keeps h2c off. Both are read from the document
# the daemon served, not from this script's assumptions.
if start_daemon "$WORK/cleartext.yaml" "$WORK/cleartext2.log" "http://127.0.0.1:$PORT"; then
  req POST "$TREE_URL/" --http1.1 -H 'X-Bunker-Op: capabilities' -H 'Content-Type: application/json' --data '{}'
  CAPS="$(body_of)"
  expect_contains "cleartext: h2c reported available" "$CAPS" '"h2c":{"available":true'
  expect_contains "cleartext: h2 reported unavailable" "$CAPS" '"h2":{"alpn":"h2","available":false'
  expect_contains "h3 reported unavailable (BFS-007)" "$CAPS" '"h3":{"alpn":"h3"'
  expect_contains "the absent watcher is enumerated" "$CAPS" '"capability":"watch","detail"'
  stop_daemon
fi

echo
echo "== HTTP/3 (QUIC): one port number, two transports, one process (BFS-007) =="
# The same daemon binary, the same config shape as the TLS section, plus
# h3_enabled: true. rest_addr is empty, so the derived UDP address is the SAME
# port number the TCP listener is on — the row's "one port" claim.
if ! start_daemon "$WORK/h3.yaml" "$WORK/h3.log" "https://127.0.0.1:$PORT_H3"; then
  echo "cannot continue without the h3 daemon" >&2
  exit 1
fi

# ── two transports on one port number, proven at the socket layer ───────────
PORT_HEX="$(printf '%04X' "$PORT_H3")"
if awk -v p=":$PORT_HEX" 'NR>1 && $2 ~ p "$" { found=1 } END { exit !found }' /proc/net/udp; then
  ok "a UDP socket is bound on the same port number as the TCP listener (:$PORT_H3)"
else
  no "no UDP socket bound on :$PORT_H3 — the QUIC listener is not up"
fi
if command -v ss >/dev/null 2>&1; then
  if ss -uln 2>/dev/null | grep -q ":$PORT_H3 "; then
    ok "ss confirms the UDP listener on :$PORT_H3"
  else
    no "ss does not show a UDP listener on :$PORT_H3: $(ss -uln 2>/dev/null | grep "$PORT_H3" || echo none)"
  fi
else
  echo "NOTE  ss is not installed; the /proc/net/udp cell above is the socket evidence"
fi

# ── the advertisement: present on h1 and h2, and it names the QUIC socket ───
WANT_ALTSVC="h3=\":$PORT_H3\"; ma=2592000"
# The same value as it appears inside the capability document's JSON string.
WANT_ALTSVC_JSON="h3=\\\":$PORT_H3\\\"; ma=2592000"
req GET "$H3_URL/src/main.go" --http1.1 -k
expect_eq "GET over HTTP/1.1 still works with h3 up" "$STATUS" "200"
expect_eq "server observed HTTP/1.1" "$(hdr X-Bunker-Proto)" "HTTP/1.1"
expect_eq "h1 response advertises the live h3 endpoint" "$(hdr Alt-Svc)" "$WANT_ALTSVC"
cp "$BODY" "$WORK/body-h1-h3"

req GET "$H3_URL/src/main.go" --http2 -k
expect_eq "GET over HTTP/2 still works with h3 up" "$STATUS" "200"
expect_eq "h2 negotiated" "$HTTPVER" "2"
expect_eq "h2 response advertises the live h3 endpoint" "$(hdr Alt-Svc)" "$WANT_ALTSVC"
cp "$BODY" "$WORK/body-h2-h3"

# ── the same surface over HTTP/3, negotiated on the wire ────────────────────
h3req "$H3_URL/src/main.go"
expect_eq "h3 request exits 0" "$H3_EXIT" "0"
expect_eq "client observed HTTP/3.0" "$(h3field proto)" "HTTP/3.0"
expect_eq "the QUIC handshake negotiated ALPN h3" "$(h3field alpn)" "h3"
expect_eq "h3 status" "$(h3field status)" "200"
expect_eq "server observed HTTP/3.0" "$(h3field server-proto)" "HTTP/3.0"
expect_eq "an h3 response carries no Alt-Svc" "$(h3field alt-svc)" '""'
expect_eq "h3 body is the file's bytes" "$(h3field sha256)" "$(sha256sum < "$ROOT/src/main.go" | awk '{print $1}')"
if cmp -s "$WORK/h3-body" "$WORK/body-h1-h3"; then
  ok "the HTTP/1.1 and HTTP/3 responses are byte-identical"
else
  no "the h1 and h3 bodies differ"
fi
if cmp -s "$WORK/h3-body" "$WORK/body-h2-h3"; then
  ok "the HTTP/2 and HTTP/3 responses are byte-identical"
else
  no "the h2 and h3 bodies differ"
fi

# A WebDAV verb that only exists in this surface, over QUIC: PROPFIND's
# multistatus is not version-gated either.
h3req "$H3_URL/src" -method PROPFIND -header 'Depth: 1'
expect_eq "PROPFIND over h3 exits 0" "$H3_EXIT" "0"
expect_eq "PROPFIND over h3" "$(h3field status)" "207"
expect_contains "same multistatus shape over h3" "$(body_of_h3)" "D:multistatus"

# The capability document is served over h3 too, and it reports the LIVE
# listener: available true, the authority of the bound socket, and no h3
# degradation (an entry for a capability that now exists would describe a
# process that is not running).
h3req "$H3_URL/" -method POST -header 'X-Bunker-Op: capabilities' -header 'Content-Type: application/json'
expect_eq "capabilities over h3 exits 0" "$H3_EXIT" "0"
H3_CAPS="$(body_of_h3)"
# One needle covering the whole h3 block: the ALPN, the authority of the LIVE
# socket, and availability — in that order, which is the order Go writes a map's
# keys. A daemon that reported an old authority, or availability without a live
# socket, fails here.
expect_contains "the h3 block reports the live UDP authority as available" "$H3_CAPS" \
  "\"h3\":{\"alpn\":\"h3\",\"alt_svc\":\"$WANT_ALTSVC_JSON\",\"available\":true"
if printf '%s' "$H3_CAPS" | grep -q '"capability":"h3"'; then
  no "h3 is live but still enumerated as a degradation: $H3_CAPS"
else
  ok "the h3 degradation is gone while the listener is live"
fi
expect_contains "transports still lists h1/h2/h2c/h3" "$H3_CAPS" '"h2c"'
expect_contains "the absent watcher is still enumerated" "$H3_CAPS" '"capability":"watch"'

stop_daemon

echo
echo "== the incoherent pair is refused: h3 without TLS (BFS-007) =="
# QUIC always encrypts, so this config must not start at all. A daemon that came
# up here would be a TCP-only listener the operator believes carries HTTP/3.
REFUSAL_LOG="$WORK/h3-cleartext.log"
"$BIN" --config "$WORK/h3-cleartext.yaml" >"$REFUSAL_LOG" 2>&1
REFUSAL_RC=$?
if [ "$REFUSAL_RC" -ne 0 ]; then
  ok "h3_enabled with tls.enabled: false is refused (exit $REFUSAL_RC)"
else
  no "the daemon started with server.h3_enabled over a cleartext listener"
fi
expect_contains "the refusal names the key that is missing" "$(cat "$REFUSAL_LOG")" "server.h3_enabled requires tls.enabled"
if awk -v p=":$PORT_HEX" 'NR>1 && $2 ~ p "$" { found=1 } END { exit !found }' /proc/net/udp; then
  no "a UDP socket was bound on :$PORT_H3 by a refused start"
else
  ok "the refused start left no UDP socket behind"
fi

echo
echo "== summary =="
echo "passed: $PASS  failed: $FAIL"
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
exit 0
