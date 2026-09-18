# DF-BUNKER-22 — streaming RPCs over REST: envelope proof + live recipe (2026-09-18)

**Row:** DF-BUNKER-22 (P1) — "`ExecAgent` streaming over REST has no recipe in
`docs/integration.md`; the §2-documented call returns a bare `415` with no
`{code,message}` envelope". Parent run:
[dogfood/2026-09-18-integration.md](2026-09-18-integration.md) §4.1–4.3.

**What this file is:** the evidence trail for the two deliverables — a JSON error
envelope for the streaming-media-type rejection (server) and the missing
streaming recipe plus a runnable client (docs). Every command below was run; the
raw output is pasted verbatim.

**Verdict:** both deliverables verified live. One **new, separate** defect was
measured on the way (§4): a command whose output reaches *both* stdout and
stderr corrupts the streamed response — pre-existing, not introduced or fixed
here, filed below with its reproduction.

---

## 1. Stack used

| Item | Value |
|---|---|
| Repo / branch | `/home/kara/bunker` @ `dbdfcbe` + the two commits of this row, branch `main` |
| Scratch daemon | locally built `bunkerd` (Go 1.26.5, non-root), `server.rest_addr=127.0.0.1:18099`, `server.grpc_addr=127.0.0.1:19099`, auth disabled, config `/tmp/dfbunker22/config.yaml` |
| Pre-existing daemon (untouched) | `bunkerd` on `:10001`/`:18080` (this host) — not used |
| Live daemon for the doc example | `bunker-las-02` = `http://100.116.99.35:10001`, `version 0.1.4`, build `cef10fc` (2026-09-13, i.e. **older than the fix in this row**) |
| Agent used | `kara-lair` (already running on `bunker-las-02`) |
| Token handling | read from `~/.bunker/config.yaml` (server `bunker-las-02`) into `$BUNKER_TOKEN`; never printed, never hardcoded (48 chars) |
| Live footprint | `ExecAgent` only — no spawn, no destroy, no restart, no config change on any shared host |

---

## 2. Server: the 415 now carries an envelope (scratch daemon, post-fix build)

### 2.1 Before the fix — the failure the row describes

The pre-fix behavior is reproduced by disabling the wrapper in
`internal/server/streaming_envelope.go` (`h.next.ServeHTTP(w, r)` instead of
`ew`) and running the new acceptance test against the **real** connect handler:

```
$ go test ./internal/server/ -count=1 -run 'TestStreamingEnvelope_UnaryJSONOnStreamingRPC' -v
=== RUN   TestStreamingEnvelope_UnaryJSONOnStreamingRPC
    streaming_envelope_test.go:99: Content-Type = "", want a JSON media type
    streaming_envelope_test.go:102: body is empty: the pre-fix behavior this task removes
--- FAIL: TestStreamingEnvelope_UnaryJSONOnStreamingRPC (0.00s)
FAIL
```

(Status was still `415`; the body and `Content-Type` were absent — exactly the
live finding in the parent report §4.1. The wrapper was restored immediately
after and the same test passes; the file was re-diffed against the copy taken
before the RED run.)

### 2.2 After the fix — verbatim wire transcript

```bash
$ curl -s -i -X POST http://127.0.0.1:18099/bunker.v1.Bunkerd/ExecAgent \
    -H 'Content-Type: application/json' -d '{}'
HTTP/1.1 415 Unsupported Media Type
Accept-Post: application/connect+json, application/connect+json; charset=utf-8, application/connect+proto, application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto
Content-Type: application/json
Date: Fri, 18 Sep 2026 16:38:07 GMT
Content-Length: 789

{"code":"invalid_argument","message":"/bunker.v1.Bunkerd/ExecAgent is a server-streaming RPC, so Content-Type \"application/json\" is not accepted: application/json is the unary REST shape (one JSON object in, one JSON object out), while a streaming RPC needs Connect envelope framing. Retry with Content-Type: application/connect+json and one 5-byte-prefixed envelope per message; the response is an envelope stream (see docs/integration.md §5). Accepted content types: application/connect+json, application/connect+json; charset=utf-8, application/connect+proto, application/grpc, application/grpc+json, application/grpc+json; charset=utf-8, application/grpc+proto, application/grpc-web, application/grpc-web+json, application/grpc-web+json; charset=utf-8, application/grpc-web+proto."}
```

The failure is now self-describing: `415` (unchanged), a JSON envelope with
`code`/`message`, the expected media type named in the message, and connect's own
`Accept-Post` list preserved as the machine-readable half.

### 2.3 The same RPC with the streaming media type — no 415

```bash
$ curl -s -i -X POST http://127.0.0.1:18099/bunker.v1.Bunkerd/ExecAgent \
    -H 'Content-Type: application/connect+json' -d '{}'
HTTP/1.1 200 OK
Connect-Accept-Encoding: gzip
Content-Type: application/connect+json
Transfer-Encoding: chunked

<flags=0x02>{"error":{"code":"invalid_argument","message":"protocol error: incomplete envelope: unexpected EOF"}}
```

`200`, not `415` — the pass signal. (The in-stream error is the documented
"do not send an end-of-stream envelope / empty body" behavior of §4.2 in the
parent report; the point here is that the media type itself is accepted.)

### 2.4 Control: the unary path is untouched

```bash
$ curl -s -i -X POST http://127.0.0.1:18099/bunker.v1.Bunkerd/ServerInfo \
    -H 'Content-Type: application/json' -d '{}'
HTTP/1.1 200 OK
Accept-Encoding: gzip
Content-Type: application/json
Content-Length: 280

{"hostname":"karaHermes-mde-7840hs","version":"0.1.4","uptimeSeconds":"11","maxAgents":100,"tmpIsolation":"unknown","tmpIsolationDetail":"daemon is not running as root — /tmp isolation state cannot be verified","residue":{"orphanHomes":1,"staleLingerEntries":100,"status":"ok"}}
```

`go test ./internal/server/ -count=1` additionally diffs the wrapped handler
against the raw connect handler byte-for-byte for unary REST, streaming
connect+json, a bogus unary media type, an auth failure and an unknown path
(`TestStreamingEnvelope_LeavesOtherPathsAlone`,
`TestStreamingEnvelope_DoesNotTouchConnectErrors`).

---

## 3. Docs: the recipe, run live, exactly as written

The Python client in `docs/integration.md` §5 was **extracted from the document
by regex and executed unmodified** (`/tmp/dfbunker22/doc_example.py`), against
`bunker-las-02` and the pre-existing agent `kara-lair`. Raw transcript:

```
## Setup
Daemon : bunker-las-02 (http://100.116.99.35:10001), version from ServerInfo:
     bunker-las-02 version 0.1.4
Agent  : kara-lair (pre-existing; no spawns/destroys in this run)
Token  : read from ~/.bunker/config.yaml for server bunker-las-02 (48 chars, value never printed)
Client : /tmp/dfbunker22/doc_example.py — extracted verbatim from the 'python' block of
         docs/integration.md §5 by the regex in this script (no edits), then run unchanged.

## A. stdout only
### stdout
$ export BUNKER_TOKEN=...   # from ~/.bunker/config.yaml; the value is never printed
$ python3 doc_example.py http://100.116.99.35:10001 kara-lair -- sh -c 'echo OUT'
OUT
exit code: 0
(exit status: 0)

## B. stderr only, non-zero exit
### stderr+exit3
$ python3 doc_example.py http://100.116.99.35:10001 kara-lair -- sh -c 'echo ERR 1>&2; exit 3'
ERR
exit code: 3
(exit status: 0)

## C. the command named in the task (writes to BOTH stdout and stderr)
### both-streams
$ python3 doc_example.py http://100.116.99.35:10001 kara-lair -- sh -c 'echo OUT; echo ERR 1>&2; exit 0'
streaming response was not decodable (<0x00><0x00><0x00><0x00>{"stdout":"T1VUCg=="}<0x00><0x00><0x00><0x00>{"stderr":"RVJSCg=="}HTTP/1.1 200 OK
)
(exit status: 1)

## D. same work, merged into one stream inside the agent (documented workaround)
### merged-stream
$ python3 doc_example.py http://100.116.99.35:10001 kara-lair -- sh -c '{ echo OUT; echo ERR 1>&2; } 2>&1'
OUT
ERR
exit code: 0
(exit status: 0)

## E. rates over 10 runs each, same client, same agent
both-streams : intact=0/10  corrupt-response failures=10/10
stdout-only  : intact=10/10  failures=0/10
```

(The `$`-lines above are the script's own echo of the argv; the shell's `$*`
display drops the inner quoting — the argv actually passed to Python is
`["sh", "-c", "echo OUT; echo ERR 1>&2; exit 0"]` in case C and
`["sh", "-c", "{ echo OUT; echo ERR 1>&2; } 2>&1"]` in case D.)

So: the example works **as written** (A, B, D, 10/10 clean single-stream runs),
and case C — the exact command this task's verification names — fails on
`bunker-las-02` for a daemon-side reason that this row does not fix. Measured
next.

---

## 4. NEW finding (not fixed here): a command writing to BOTH streams corrupts the response

### 4.1 The measurement

| Command shape | Framing intact |
|---|---|
| stdout only (`echo OUT`) | **10/10** |
| stderr only (`echo ERR 1>&2`) | **10/10** (raw-socket probe, 5/5) |
| both (`echo OUT; echo ERR 1>&2`) | **0/10** |
| silent (`true`) | 5/5 |

Reproduced identically from this host and from `bunker-las-02` itself
(`127.0.0.1:10001`, root, fresh connection per attempt, distinct source ports
and 2s spacing — so it is not a client-side socket-reuse artifact; a Go
`httptest` server running the **same handler** locally never corrupted a
response).

### 4.2 The wire bytes (raw socket, `bunker-las-02` loopback)

```
HTTP/1.1 200 OK\r\nConnect-Accept-Encoding: gzip\r\nContent-Type: application/connect+json\r\n
Date: Fri, 18 Sep 2026 16:41:18 GMT\r\nTransfer-Encoding: chunked\r\n\r\n
1a\r\n\x00\x00\x00\x00\x15{"stderr":"RVJSCg=="}\r\n
ncoding: chunked\r\n\r\n1a\r\n\x00\x00\x00\x00\x15{"stderr":"RVJSCg=="}\r\n
```
…and, in ~half the attempts, the response body bytes precede the status line
entirely:

```
\x00\x00\x00\x00\x15{"stderr":"RVJSCg=="}\x00\x00\x00\x00\x15{"stdout":"T1VUCg=="}HTTP/1.1 200 OK\r\n
```

Two writers are producing one response: the status line/header block, a chunk
framer, and the same envelope bytes appear interleaved and duplicated.

### 4.3 What the daemon logged at the same moment

```
$ journalctl -u bunkerd --since "-20min" | grep superfluous
Sep 18 09:41:34 bunker-las-02 bunkerd[269333]: http: superfluous response.WriteHeader call from github.com/go-chi/chi/v5/middleware.(*basicWriter).Write (wrap_writer.go:99)
Sep 18 09:41:35 bunker-las-02 bunkerd[269333]: http: superfluous response.WriteHeader call from github.com/go-chi/chi/v5/middleware.(*basicWriter).WriteHeader (wrap_writer.go:91)
```

### 4.4 Strongly-indicated cause (in code, not proven end to end)

`internal/server/service.go` `ExecAgent` starts **two** goroutines — one per pipe
— and each calls `stream.Send(...)` directly:

```go
go func() { ... if err := stream.Send(&v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stdout{Stdout: buf[:n]}}); ... }()
go func() { ... if err := stream.Send(&v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stderr{Stderr: buf[:n]}}); ... }()
```

and Connect's send path is unlocked —
`connect@v1.20.0/protocol_connect.go:809`:

```go
func (hc *connectStreamingHandlerConn) Send(msg any) error {
	defer flushResponseWriter(hc.responseWriter)
	if err := hc.marshaler.Marshal(msg); err != nil { return err }
	return nil
}
```

`ServerStream.Send` is not documented as safe for concurrent use, and both
goroutines flush through the same `http.ResponseWriter`; the chi wrapper's
`WriteHeader` race (`superfluous response.WriteHeader`, §4.3) is the same
window. This matches the correlation exactly: one writer (any single stream)
→ intact; two writers (both streams) → 0/10.

### 4.5 Why it is not fixed in this row, and what a fix needs

The row is scoped to the 415 envelope and the docs, and a change to exec
streaming is SSH/exec behavior — `AGENTS.md` requires the live E2E battery on
`bunker-mvp` (`VERIFY-PASS`) for such a change, which is outside this tick's
budget. Proposed fix for a follow-up row: serialize the two senders (a mutex
around `stream.Send`, or a single sender goroutine draining both pipes), plus a
regression test that drives `client.ExecAgent` through the real connect handler
with a stub `ssh` on `PATH` writing to both pipes and asserts both streams
arrive intact. The `docs/integration.md` §5 recipe documents the limitation and
the workaround until then.

---

## 5. Gate (from `/home/kara/bunker`)

```
$ gofmt -l internal/server/streaming_envelope.go internal/server/streaming_envelope_test.go internal/server/server.go
(no output — clean)

$ go build ./...
OK

$ go vet ./...
OK

$ go test ./internal/server/ -count=1
ok  	github.com/deployBunker/bunker/internal/server	7.516s

$ go test ./... -count=1
ok  	github.com/deployBunker/bunker/cmd/bunker	0.006s
ok  	github.com/deployBunker/bunker/cmd/bunkerd	0.120s
ok  	github.com/deployBunker/bunker/internal/agent	48.361s
ok  	github.com/deployBunker/bunker/internal/apikey	0.003s
ok  	github.com/deployBunker/bunker/internal/audit	3.253s
ok  	github.com/deployBunker/bunker/internal/auth	0.162s
ok  	github.com/deployBunker/bunker/internal/cli	0.991s
ok  	github.com/deployBunker/bunker/internal/config	0.014s
ok  	github.com/deployBunker/bunker/internal/hermes	24.789s
ok  	github.com/deployBunker/bunker/internal/hilo	0.018s
ok  	github.com/deployBunker/bunker/internal/hostsetup	2.126s
ok  	github.com/deployBunker/bunker/internal/imagespec	0.018s
ok  	github.com/deployBunker/bunker/internal/registry	8.166s
ok  	github.com/deployBunker/bunker/internal/resource	0.013s
ok  	github.com/deployBunker/bunker/internal/server	7.571s
ok  	github.com/deployBunker/bunker/internal/sshsig	0.009s
ok  	github.com/deployBunker/bunker/internal/systemd	0.011s
ok  	github.com/deployBunker/bunker/internal/tailscale	0.078s
ok  	github.com/deployBunker/bunker/internal/tlsutil	0.008s
ok  	github.com/deployBunker/bunker/internal/tunnel	21.001s
ok  	github.com/deployBunker/bunker/internal/version	0.003s
?   	github.com/deployBunker/bunker/proto/bunker/v1	[no test files]
?   	github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect	[no test files]
```

`gitreins guard` verdict and the commit SHAs are recorded in the row's commit
messages (Tier 1: secrets + build).

---

## 6. Not verified / not claimed

- The fix has **not** been exercised on a live host running the new binary: the
  only live daemons available are older builds (`bunker-las-02` = `cef10fc`;
  this host's `:10001` untouched), so §3's transcript is from a daemon that
  predates the change. The envelope behavior itself is proven live on the
  scratch daemon built from this tree (§2).
- §4's cause is stated as *strongly indicated*: the correlation and the code
  path are measured, but no debugger/pprof trace was taken of the racing
  writers.
- The corruption's blast radius on other clients: the provider-side CLI
  (`bunker exec`) drives the same RPC and should be affected identically, but
  this run did not exercise the CLI against a corrupt response, so that
  consequence is stated as expected, not measured.
