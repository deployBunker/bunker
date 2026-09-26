# BFS-002 — Can `bunkerd` serve HTTP/1.1 + HTTP/2 + HTTP/3 from ONE listener?

**Row:** BFS-002 · **Status:** investigation complete · **Date:** 2026-09-26
**Design authority:** `docs/prd/PRD-bunker-fs.md` (this row settles the transport question that
PRD calls `AC-13` / slice `C6`, *before* any protocol code is written).
**Deliverable:** this document. No product code was changed by this row.

---

## 0. Verdict in one paragraph

**Yes — with one correction to the wording.** There is no single *listener* that speaks all three
versions, because HTTP/3 is QUIC and QUIC is UDP while HTTP/1.1 and HTTP/2 are TCP. What is
achievable, and what was **measured working end-to-end inside this repo's own daemon**, is
**one port number, two transports, one process**: a TCP socket on `:PORT` whose TLS ALPN selects
HTTP/1.1 or HTTP/2 (stdlib `net/http`, *zero* new dependencies), plus a UDP socket on the *same*
`:PORT` served by `quic-go`'s HTTP/3 server, with the TCP responses advertising
`Alt-Svc: h3=":PORT"` so a client discovers HTTP/3 without being configured. HTTP/1.1 keeps working
unchanged in every configuration measured. HTTP/3 costs exactly **two new modules**
(`github.com/quic-go/quic-go@v0.63.0` + its `qpack@v0.6.0`), **+2.07 MiB on the daemon binary**,
**no cgo**, and **no toolchain change**. `h2c` (cleartext HTTP/2) is available **from the stdlib at
zero dependency cost** but is *prior-knowledge only* — the RFC-7540 `Upgrade: h2c` dance is not
implemented by `net/http`, measured below — so it is worth offering only to LAN/self-owned clients.
One premise in the task row is **falsified by measurement** and is corrected in §2.3: on the live
deployment the gRPC listener `:19090` does **not** serve HTTP/2 today; it is cleartext HTTP/1.1,
because `tls.enabled: false` there and h2c is not configured.

---

## 1. Instrument and environment (so the numbers can be reproduced)

| Item | Value |
|---|---|
| Go toolchain | `go1.26.5 linux/amd64` (`GOROOT=/home/kara/sdk/go1.26.5`), `CGO_ENABLED=1` default |
| Worktree under test | `/home/kara/worktrees/bunker-BFS-002`, commit `6a28444` |
| Daemon binary probed | built from that tree, `LDFLAGS` identical to `make build` (`VERSION=0.1.4`) |
| curl | `8.18.0 (x86_64-pc-linux-gnu) libcurl/8.18.0 OpenSSL/3.5.5 nghttp2/1.68.0` |
| curl HTTP/3 support | **NONE** — `curl --version` → `Protocols: dict file ftp … tftp ws wss`. No `http3`. |
| openssl | `3.5.5` (used to read the negotiated ALPN directly) |
| Live deployment probed | `bunker-mvp` (`78.46.173.180`), `bunkerd 0.1.4`, commit `a6a52a1` |
| quic-go candidate | `github.com/quic-go/quic-go v0.63.0` (released `2026-09-22`), `go 1.26.0` in its `go.mod` |

**Instrument limitation, stated up front:** this box's `curl` cannot speak HTTP/3, so every
HTTP/3 assertion in this document was made with a **Go `http3` client** built against quic-go
`v0.63.0` (`/tmp/bfs002/h3lab/client`). ALPN was read with `openssl s_client`, not with curl —
a bare `curl -k` is not proof of the negotiated protocol, exactly as the PRD notes.

---

## 2. Current state of `bunkerd`'s listeners (acceptance criterion 1)

### 2.1 Where the listeners are

| Listener | Source | HTTP versions it can serve **today** |
|---|---|---|
| gRPC / connect listener | `internal/server/server.go:342-352` | **HTTP/1.1 only in production**, HTTP/2 when TLS is on |
| REST listener (optional) | `internal/server/server.go:358-368` | **HTTP/1.1 only in production**, HTTP/2 when TLS is on |
| TLS material | `internal/server/server.go:328-335` (chosen here), `buildTLSConfig` at `:407-487` | file certs → `&tls.Config{Certificates, MinVersion: TLS12}` (`:483-486`); self-signed `:444-461`; mTLS `:464-469`; certmagic `:412-441` |

Both listeners are plain `net/http.Server` values. Neither sets `NextProtos` — a repo-wide grep
for `NextProtos` in non-test Go code returns **0 hits** — so the HTTP/2 offer is whatever
`net/http` derives (`onceSetNextProtoDefaults`, `net/http/server.go:3754-3774`, reached through
`ListenAndServeTLS`). The repo also has **no direct `golang.org/x/net/http2` import** (0 hits);
the h2 implementation rides inside `net/http`'s bundle — **792 `http2` symbols are present in the
built `bunkerd` binary**, i.e. the h2 *code* is already in the process, but nothing negotiates it
unless TLS is enabled.

### 2.2 Measured protocol matrix

Built from the worktree (`go build ./cmd/bunkerd`), one config per regime, probed on loopback.

**A. Cleartext (`tls.enabled: false`), REST `127.0.0.1:38080`, gRPC `127.0.0.1:39090`:**

```
curl default                  -> http_version=1.1 code=200
curl --http1.1                -> http_version=1.1 code=200
curl --http2  (h2c Upgrade)   -> http_version=1.1 code=200   # server ignored the upgrade
curl --http2-prior-knowledge  -> http_version=0   code=000   # rc=16, preface rejected
```

**B. TLS (`tls.enabled: true`, self-signed), REST `127.0.0.1:28080`, gRPC `127.0.0.1:29090`:**

```
curl default (ALPN auto)      -> HTTP/2 200
curl --http1.1                -> http_version=1.1 code=200
curl --http2                  -> http_version=2   code=200
curl --http2 gRPC :29090      -> http_version=2   code=200
openssl -alpn h2              -> TLSv1.3  ALPN protocol: h2
openssl -alpn h2,http/1.1     -> TLSv1.3  ALPN protocol: h2
openssl -alpn http/1.1        -> TLSv1.3  ALPN protocol: http/1.1
openssl (no -alpn)            -> "No ALPN negotiated"   # server then answers HTTP/1.1
ss -ulnp | grep :28080        -> (none)
```

So: **with TLS on, the same code serves HTTP/1.1 and HTTP/2 depending on ALPN** — no code change
was needed for that, and none of it is reachable while the deployment runs cleartext.
**No UDP socket exists on either port in either regime** → HTTP/3 is impossible today.

### 2.3 Live deployment (`bunker-mvp`) — and a correction to a stated premise

Probed over SSH **loopback** from the host itself (raw-IP egress from this box was refused by the
environment's tooling; the probes below run on `bunker-mvp`, against `127.0.0.1`):

```
:18080  -> http_version=1.1  code=200     ; --http2-prior-knowledge -> code=000 ; https -> 000
:19090  -> --http1.1 -> 1.1 200           ; --http2 -> 1.1 200 (upgrade ignored)
         ; openssl s_client -> "wrong version number" / "No ALPN negotiated"   (NOT TLS)
ss -tlnp:  *:18080, *:19090         ss -ulnp: systemd-resolved, tailscaled, dhcp — no h3
/etc/bunkerd/config.yaml:  tls: { enabled: false, insecure_dev: true }
```

**Correction, with evidence:** the PRD's capability table says *"gRPC listener on `:19090` →
HTTP/2 present"*. Measured against the live process, **`:19090` serves HTTP/1.1 only** — it is a
cleartext `net/http` listener with neither TLS (so no ALPN) nor `Protocols.SetUnencryptedHTTP2`
(so no h2c). The h2 *stack* is compiled into the binary; no listener enables it in the deployed
configuration. This matters for planning: "an h2 substrate already exists in the process" is true
at the code level and **false at the deployed-surface level** — h2 must be switched on, and the
switch is TLS or the h2c opt-in.

The same is true of the local production instance on this workstation
(`:18080`, `:10001`, `:10002` — all `http_version=1.1`, all `--http2-prior-knowledge → 000`).

---

## 3. One port, three versions: what actually decides it (criterion 2)

Three separate mechanisms, none of which substitutes for another:

| Version | Transport | Decided by | State today |
|---|---|---|---|
| HTTP/1.1 | TCP | absence of ALPN, or ALPN `http/1.1` | works everywhere (measured) |
| HTTP/2 | TCP | **ALPN `h2`** over TLS, *or* **h2c** (prior knowledge, cleartext) | works over TLS (measured); h2c available from stdlib, not enabled |
| HTTP/3 | **UDP** (QUIC, TLS 1.3 inside) | **QUIC ALPN `h3`** + a client that learns the UDP endpoint from **`Alt-Svc`** | absent (no QUIC dependency, no UDP socket) |

**Measured, one port number, two transports, one process** (lab server: stdlib h1/h2 on TCP +
`quic-go` v0.63.0 h3 on UDP, both on `127.0.0.1:2443`; then repeated inside a quic-go-linked
build of *this repo's* `bunkerd`):

```
ss -tlnp | grep 28080   ->  LISTEN 127.0.0.1:28080   users:(("bunkerd_h3",pid=3638885,fd=13))
ss -ulnp | grep 28080   ->  UNCONN 127.0.0.1:28080   users:(("bunkerd_h3",pid=3638885,fd=4))

# in-repo daemon (scratch copy, quic-go linked, TLS on, REST :28080):
curl -k --http1.1 https://127.0.0.1:28080/healthz -> http_version=1.1 code=200
curl -k --http2   https://127.0.0.1:28080/healthz -> http_version=2   code=200
curl -k           https://127.0.0.1:28080/healthz -> HTTP/2 200        (no Alt-Svc yet: not wired)
h3 client (Go/http3) https://127.0.0.1:28080/healthz -> HTTP/3.0 200  body="proto=HTTP/3.0 uri=/healthz"

# lab server, alt-svc wired, same shape on :2443:
curl -skD - -o /dev/null https://127.0.0.1:2443/ -> HTTP/2 200 + "alt-svc: h3=":2443"; ma=2592000"
```

**Alt-Svc discovery, demonstrated rather than asserted.** A client that has no h3 knowledge at all
does exactly this: fetch over TCP, read the header, dial the advertised authority:

```
TCP  : proto=HTTP/2.0 status=200 altsvc="h3=\":2443\"; ma=2592000"
H3   : dialing advertised authority 127.0.0.1:2443
H3   : proto=HTTP/3.0 status=200 body=proto=HTTP/3.0
```

The header value comes from `http3.Server.SetQUICHeaders` (`http3/server.go:695`), whose payload is
built by `generateAltSvcHeader` (`http3/server.go:388-399`) as `h3=":<port>"; ma=2592000`
(30-day max-age). **Nothing is emitted today** — the daemon's responses carry no `Alt-Svc` header
at all (measured on the live `:18080` header dump).

**On the "one listener" phrasing:** a UDP socket cannot carry h1/h2 and a TCP socket cannot carry
h3, so the honest statement of the goal is *one port number* (`:18080`) *on two transports inside
one process, one TLS config, one router*. Bind configuration is `cfg.Server.RESTAddr` (TCP) plus a
sibling h3 address that defaults to the same host:port on UDP.

---

## 4. h2c — is cleartext HTTP/2 worth offering? (criterion 2, part b)

**It is free**: since Go 1.24 the stdlib exposes `net/http.Protocols` with
`SetUnencryptedHTTP2` — confirmed in the toolchain's own API file
(`$GOROOT/api/go1.24.txt:155-178`, issues `#67814` / `#67816`) and in the source in use
(`net/http/http.go:44-56`, dispatch at `net/http/server.go:1985` + `:2165-2178`).

**Measured** with a stdlib-only server (`Protocols{HTTP1, UnencryptedHTTP2}`) — no new dependency:

```
h2c prior-knowledge  -> http_version=2   code=200     # works, cleartext
h1 forced            -> http_version=1.1 code=200     # both on one socket
default              -> http_version=1.1 code=200
Upgrade: h2c attempt -> HTTP/1.1 200                  # RFC-7540 upgrade NOT implemented
```

The last line is the operational catch: `net/http`'s h2c support is **prior-knowledge only**. A
generic client that tries `--http2` over cleartext (upgrade-based) gets HTTP/1.1, silently — which
is precisely the "silent downgrade" class of bug. So:

- **Recommendation:** keep TLS as the default and the only path for anything reachable; offer
  **h2c as an explicit opt-in** (a config key next to `tls.insecure_dev`) for trusted LAN/loopback
  and for *our own* client, which can be written to send the preface. Do not present h2c as the
  universal "h2 for old clients" answer — it is not.
- **HTTP/3 cannot ride the cleartext path at all**: QUIC always encrypts, so h3 requires
  `tls.enabled: true`. Consequence for the deployment: today's production config
  (`tls.enabled: false`) can never expose h3, and exposes h2 only if h2c is turned on.

---

## 5. What HTTP/3 costs (criterion 3)

Dependency: **`github.com/quic-go/quic-go v0.63.0`** (+ `github.com/quic-go/qpack v0.6.0`,
indirect). Both **MIT**. Measured on a scratch copy of this worktree (so the deliverable commit
touches no product code):

| Measurement | Before | After | Delta |
|---|---|---|---|
| Modules actually linked into `cmd/bunkerd` (`go list -deps`) | 28 | 30 | **+2** (`quic-go`, `qpack`) |
| `go.mod` require lines | — | — | **+2** (one direct, one `// indirect`) |
| `go.sum` lines | 103 | 105 | **+2** |
| Module download payload (release zips) | — | — | **1,068,116 B ≈ 1.02 MiB** (quic-go 1,019,911 B + qpack 48,205 B) |
| Extracted source in module cache | — | — | 4.6 MB + 320 KB |
| `go list -m all` (superset incl. quic-go's own tool/test deps) | 58 | 63 | +5 (also `go-ossfuzz-seeds`, `gcassert`, `uber/mock`; `testify` v1.11.1→v1.12.1) — **graph only, not built** |
| **`bunkerd` binary, `CGO_ENABLED=1`** | 21,192,735 B | 23,367,795 B | **+2,175,060 B = +2.074 MiB (+10.26%)** |
| **`bunkerd` binary, `CGO_ENABLED=0`** | 21,120,287 B | 23,296,515 B | **+2,176,228 B = +2.075 MiB (+10.31%)** |
| Lab program, stdlib h1/h2 → +h3 | 9,013,410 B | 11,806,372 B | +2,792,962 B = +2.664 MiB (+30.99%) |

- **cgo: not required.** The daemon builds and links with `CGO_ENABLED=0` (the h3 build is
  `not a dynamic executable` under `ldd`).
- **Go-version floor:** quic-go `v0.63.0`'s own `go.mod` declares `go 1.26.0`; this repo is
  `go 1.26.5` → **no toolchain change, no `go` directive bump**. Its `golang.org/x/net` floor
  (`v0.56.0`) is *below* the version already in this repo's graph (`v0.57.0`) → `go mod tidy`
  bumped nothing (`x/crypto`, `x/sys`, `x/text` unchanged too).
- **Honesty note on the binary number:** a probe that merely *references* `http3.Server` measures
  only +159,060 B, because the linker drops unreachable methods. The number quoted above is with
  the **serving path reachable** (`http3.Server.ListenAndServe` appears in `go tool nm` output:
  1,019 `http3.`/`quic-go.` symbols). That is the number a real h3 listener pays.
- **Vulnerability posture, measured:** `govulncheck ./cmd/bunkerd` **before and after** adding
  quic-go produced *identical* vulnerability IDs and counts (5 reachable — **all Go standard
  library**, all fixed in `go1.26.6`; plus 2 imported-package and 5 required-module advisories that
  are not reached). Neither `quic-go` nor `qpack` carries an advisory of its own. The only diffs are
  two extra example traces *into the same pre-existing stdlib advisories*
  (`GO-2026-6090` `crypto/tls` via `tls.QUICConn.HandleData`/`Start`; `GO-2026-5972`
  `encoding/asn1` via the h3 server's cert load) — i.e. **quic-go adds code paths through
  vulnerabilities the daemon is already exposed to**, it does not add a new vulnerable module.

**If quic-go is rejected, the alternatives (ranked):**

1. **Ship h1 + h2 only.** TLS ALPN gives h2 with **zero new dependencies** (already true today when
   TLS is on) and h2c is available from the stdlib for the LAN case. This preserves both stated
   goals — old clients keep working (h1), and the measured lever (25.0× on 100 concurrent requests)
   is carried by h2. Cost: **0 bytes, 0 modules.**
2. **External h3 terminator** (a QUIC-capable proxy/load-balancer in front of `bunkerd`). Keeps the
   Go dependency at zero, but adds a second process to the path and moves TLS/UDP termination out of
   the daemon; **not measured here**, and it conflicts with the row's "the multi-protocol surface
   belongs in `internal/server/server.go`" instruction.
3. **Defer h3**, exactly as `PRD-bunker-fs.md` `AC-13` already says: keep h3 only if it beats h2 on
   wall time on this path. Nothing in this investigation claims h3 is faster — its benefit is
   unmeasured (see Open questions).

---

## 6. Design sketch — concrete files (criterion 4)

The multi-protocol surface extends **`internal/server/server.go`**, per the row's seed-extension
instruction. Nothing here is implemented by this row.

1. **`internal/server/server.go` — listener block (`:327-370`).** Keep both existing
   `http.Server` values exactly as they are (that is what preserves §2's measured behaviour).
   Add, next to them, an h3 server bound to the **same port number** on UDP:
   `h3 := &http3.Server{Addr: h3Addr, TLSConfig: http3.ConfigureTLSConfig(tlsConfig), Handler: r}`
   then `go func(){ errCh <- h3.ListenAndServe() }()`. `errCh` is currently `make(chan error, 2)`
   at `:338` — it must grow to 3. The h3 branch is only entered when `cfg.TLS.Enabled` (QUIC
   requires TLS 1.3); a cleartext config simply has no h3.
   **Never assign `NextProtos` on the shared `*tls.Config`** — hand it to
   `http3.ConfigureTLSConfig`, which clones (`http3/server.go:54-57`) and sets `["h3"]` on the copy.
2. **`internal/server/server.go` — router chain (`:144-152`, beside `middleware.Recoverer`).** An
   `Alt-Svc` middleware that calls `h3.SetQUICHeaders(w.Header())` on h1/h2 responses so clients
   discover h3 with no configuration (measured header: `h3=":PORT"; ma=2592000`). Emit it only when
   the h3 listener is actually up.
3. **`internal/config/config.go`** — new `ServerConfig` fields beside `GRPCAddr`/`RESTAddr`
   (`:195-196`): `H3Enabled bool` (`h3_enabled`), `H3Addr string` (`h3_addr`, empty ⇒ derive the
   REST port on UDP), `H2CEnabled bool` (`h2c_enabled`, LAN opt-in). Defaults beside `:774-775`
   (h3 off, h2c off — additive, nothing changes for existing configs); ENV bindings beside `:903-904`;
   validation in `Validate()` (`:1020`) including "h3 requires `tls.enabled`"; and a look at
   `CheckTLS` (`:1421`) so the cleartext-refusal gate stays coherent with an explicit `tls.insecure_dev`
   + `h2c_enabled` LAN posture.
4. **`internal/server/server.go` — h2c opt-in (optional).** Where the two `http.Server` values are
   built (`:342`, `:358`), set `Protocols: &http.Protocols{}` with `SetHTTP1(true)` +
   `SetUnencryptedHTTP2(true)` when `cfg.Server.H2CEnabled`. Stdlib-only; no dependency.
5. **`config.example.yaml`** — document the three new keys beside the `server:`/`tls:` blocks
   (`:11-17`).
6. **`go.mod` / `go.sum`** — add `github.com/quic-go/quic-go v0.63.0` (+ `qpack` indirect). This is
   the only step that costs supply chain.
7. **`probes/`** — the PRD (`:302`) puts the measurement harness here beside
   `git-over-mount-probe.sh`: a probe that records the **negotiated** protocol per client
   (`openssl s_client -alpn` for TCP, a Go/http3 client for UDP), plus the Alt-Svc header — the
   instrument this investigation had to build.
8. **Tests** — `internal/server/server_protocol_test.go` (table-driven ALPN matrix, h1 fallback,
   h3-over-TCP refusal, `NextProtos` immutability), plus `internal/config` tests for the new keys
   with the "h3 without TLS is refused" row.
9. **`docs/prd/PRD-bunker-fs.md`** — correct the capability-table row that says the `:19090` gRPC
   listener carries HTTP/2 (`§2.3` above has the evidence).

---

## 7. The old-client guarantee (criterion 5)

**A plain HTTP/1.1 client keeps working unchanged, in every configuration measured:**

- Cleartext listener: `curl` default, `--http1.1`, and even the ignored h2c-Upgrade attempt all
  returned `http_version=1.1` / `200`.
- TLS listener: `curl --http1.1` → `1.1 200` while the same server negotiates `h2` for a client
  that offers it.
- A client that sends **no ALPN at all** gets HTTP/1.1 (measured: `No ALPN negotiated`, then a
  normal h1 response) — the graceful default is preserved.
- The h3 listener is additive: it is a *different transport*, so its presence or absence cannot
  change what the TCP socket does, and it is only announced via `Alt-Svc`, which an old client
  ignores.

Design rule that follows: the effective TCP ALPN list must always contain `http/1.1`. Note that
`net/http` enforces exactly this in `adjustNextProtos` (`net/http/server.go:3533-3562`): it keeps
only `http/1.1` and `h2` from a configured list, appends whichever is missing, and **deletes
anything else** — which is also why `h3` can never be negotiated on the TCP port even by accident
(§8, F2). The one way to break the guarantee is to bypass the stdlib path (hand-rolled
`tls.NewListener` with a `NextProtos` that omits `http/1.1`); the sketch in §6 deliberately does not.

---

## 8. Failure modes, and the test that catches each (criterion 6)

**F1 — Silent HTTP/1.1 downgrade (ALPN not negotiated).** A client whose ALPN list does not overlap
(e.g. empty, or h3-only on TCP) negotiates nothing and is answered over HTTP/1.1 without an error.
This is exactly the observation recorded in the row's premises (`curl -k` → `http_version=1.1`
against an h2-capable server). *Measured:* `openssl` with `-alpn http/1.1` → `http/1.1`; with no
`-alpn` → `No ALPN negotiated`.
**Test:** assert **both** surfaces at once — the client's `resp.Proto == "HTTP/2.0"` **and**
`resp.TLS.NegotiatedProtocol == "h2"` (and, for the CLI probe, that `openssl s_client -alpn h2`
prints `ALPN protocol: h2`). A test that asserts only `Proto`, or only that the request succeeded,
passes on a downgraded connection and is therefore vacuous.

**F2 — h3 offered over TCP.** *Measured:* dialing the TCP port with ALPN `["h3"]` is **refused** —
`http: TLS handshake error … tls: client requested unsupported application protocols (["h3"])`, no
certificate presented. Correct per RFC 9114 (h3 is QUIC-only), but it means an h3-only client has
**no fallback** unless it retries over TCP.
**Test (table-driven):** TCP + ALPN `["h3"]` ⇒ handshake **error** (must not be a silent h1
response); TCP + `["h2"]` ⇒ `h2`; TCP + `["http/1.1"]` ⇒ `http/1.1`; TCP + `[]` ⇒ no ALPN, body
served. The negative control must be present, otherwise a broken server that refuses *everything*
would pass.

**F3 — shared `*tls.Config` clobbered by the h3 wiring.** The subtlest trap in this design: if
`http3.ConfigureTLSConfig` mutated the caller's config (`config.NextProtos = ["h3"]`), the TCP
listener would advertise `h3` and lose `h2`/`http/1.1` for every client. *Measured safe:*
`shared config before: NextProtos=[]` and `shared config after h3 wiring: NextProtos=[]` on the
same pointer handed to both servers (quic-go clones — `http3/server.go:54-57`), and with that one
shared config the TCP socket still negotiated `h2`/`http/1.1` while the UDP socket served
`HTTP/3.0`.
**Test:** construct both servers from one `*tls.Config`, assert `NextProtos` is unchanged
afterwards, then assert TCP still negotiates `h2` and `http/1.1` and UDP still answers `HTTP/3.0`.
Add a mutation control (a deliberately clobbering config) to prove the assertion is not vacuous.

**F4 — an ALPN-stripping middlebox.** A proxy/terminator that drops ALPN turns h2 into h1 with no
error (the PRD's own mitigation row names this: "a downgrade is a visible error, never silent" —
this design does not yet make it visible). **Test:** same request through the proxied and direct
paths, compare the negotiated ALPN and fail on mismatch; surface a loud warning when h2 was
expected and h1 was negotiated.

---

## 9. Open questions (criterion 8 — things not determined here)

1. **Which real WebDAV clients actually negotiate h2 or h2c.** Only `curl` and `openssl` were used
   as instruments. rclone/davfs2/Finder/Windows were not exercised against *our* surface. In
   particular, whether any of them will send the h2c preface (prior knowledge) is unverified — which
   is the whole question behind "is h2c worth offering".
2. **HTTP/3's actual benefit on this path is unmeasured.** Nothing here says h3 is faster; the
   PRD's `AC-13` gate (same battery over h2 and h3, keep h3 only if it wins on wall time) still
   requires a curl or client with real h3 support and a lossy-path measurement. This box's curl
   cannot do it.
3. **Whether `Alt-Svc` advertised over a *cleartext* connection is honoured by any client we care
   about.** Discovery was demonstrated over TLS. Browsers require a secure context for h3; not
   measured for CLI clients.
4. **Five reachable Go *standard library* advisories** (`GO-2026-6218` `net/url`, `GO-2026-6090`
   `crypto/tls`, `GO-2026-6089`/`GO-2026-5026` `net/http`, `GO-2026-5972` `encoding/asn1`) are
   reachable in `cmd/bunkerd` on the current `go1.26.5` toolchain (all fixed in `go1.26.6`),
   **before and after** quic-go. That is a pre-existing toolchain-bump item, out of scope for this
   row, and it is why the "adds new attack surface" answer above must be read precisely.
5. **Operational port choice.** Whether h3 should share the REST port (`:18080`) or the gRPC port
   (`:19090`), and what the UDP exposure policy is on the bunker hosts — an owner call, not
   measured here.
6. **`x/net` version skew**, if any: quic-go declares `x/net v0.56.0`, this repo carries `v0.57.0`;
   `go mod tidy` bumped nothing, and no behavioural difference was tested.
7. **Wildcard bind behaviour** with TCP `*:18080` plus UDP `:18080` on the live host: the
   daemon-level probe above bound loopback for both transports. A wildcard-address run was not
   performed.
8. **The remote probe method**: `bunker-mvp` was audited through SSH loopback probes because
   raw-IP egress from this environment was refused by tooling; the port/version results are
   therefore "on the host, from the host", not "from this workstation across the network". The
   live facts (`tls.enabled: false`, h1-only listeners, no UDP) are the same either way.

---

## 10. Evidence index (what to re-run to falsify any claim above)

| Claim | Command / artifact |
|---|---|
| Live deployment h1-only, no TLS, no UDP | `ssh bunker-mvp 'ss -tlnp; ss -ulnp; curl … 127.0.0.1:18080'` (§2.3 transcript) |
| Worktree cleartext = h1 only, h2c-forced 000 | `/tmp/bfs002/probe.sh` section A (worktree-built `bunkerd`, `127.0.0.1:38080`) |
| Worktree TLS = h1 + h2 via ALPN | same script, section B (`:28080`, self-signed) |
| One port, two transports, h3 served | lab `full` server + in-repo `bunkerd_h3` probe copy (`ss` TCP+UDP on one port; Go/http3 GET → `HTTP/3.0 200`) |
| Alt-Svc discovery, header value | lab `client` (TCP → `alt-svc: h3=":2443"; ma=2592000` → h3 GET) |
| ALPN matrix incl. h3-over-TCP refusal | `openssl s_client -alpn {h2,h2,http/1.1,http/1.1,h3}` + daemon log line |
| h2c available with no new dependency | stdlib-only lab with `Protocols.SetUnencryptedHTTP2` + `curl --http2-prior-knowledge`; API provenance `$GOROOT/api/go1.24.txt:155-178` |
| Dependency cost | scratch copy: `go mod tidy` diff, `go list -deps`, module-cache sizes, binary sizes with identical `LDFLAGS`, `govulncheck` before/after |
| Artifacts | `/tmp/bfs002/h3lab/{baseline,full,shared,client}`, `/tmp/bunker-dep-probe` (scratch copy), `/tmp/bfs002/probe.sh` |

*Scratch trees used for measurement are outside the repo and are not part of this commit; the only
change in this row is this document.*
