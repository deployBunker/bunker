# Bunker Integration Guide

How clients talk to a `bunkerd` daemon: transport, authentication, the agent
lifecycle, and the RPC surface. Written for integrators who want to drive Bunker
programmatically (CLI, curl, gRPC, or a custom client) instead of reading the
proto files and server source.

Companion docs: [../README.md](../README.md) (quick start), the dogfood
integration report ([dogfood/2026-08-03-integration.md](dogfood/2026-08-03-integration.md)),
and the 2026-08-06 live verification ([dogfood/2026-08-06-gap-009-verify.md](dogfood/2026-08-06-gap-009-verify.md)).

---

## 1. Architecture in one paragraph

`bunkerd` is a long-running daemon (must run as **root**) that hosts isolated
agents. Each agent is a dedicated Linux user with its own home directory, SSH
keypair, and **rootless Docker daemon**, wrapped in resource limits enforced via
systemd user slices (CPU quota, memory, disk, process/file limits, Docker
container count). `bunker` is the CLI; it talks to the daemon over **connect-go**,
which serves the same RPCs over **gRPC and REST** from a single listener pair.

```
┌──────────────┐   gRPC  :19090 / REST :18080   ┌──────────────┐
│  bunker CLI  │ ─────────────────────────────▶ │   bunkerd    │
│  (any user)  │   Authorization: Bearer <tok>  │  (root host) │
└──────────────┘                                └──────┬───────┘
                                                       │ useradd / systemd-run
                                          ┌────────────▼────────────┐
                                          │ agent-a  (user + rootless │
                                          │          dockerd + cgroup)│
                                          └──────────────────────────┘
```

## 2. Transport and ports

| Listener | Default (`config.yaml`) | Live demo instance |
|----------|------------------------|--------------------|
| gRPC     | `:9090`  (`server.grpc_addr`) | `78.46.173.180:19090` |
| REST     | `:8080`  (`server.rest_addr`) | `78.46.173.180:18080` |

- **connect-go dual protocol**: the *same* service is exposed on both listeners.
  gRPC clients use the standard `bunkerv1.Bunkerd` service. REST clients POST
  JSON to the connect-go path convention: `POST /bunker.v1.Bunkerd/<Method>`
  (e.g. `POST /bunker.v1.Bunkerd/SpawnAgent`), `Content-Type: application/json`.
  The handler is mounted **without `WithHTTPGet`**, so REST is POST-only: any
  other HTTP method returns `405`, and the auth interceptor runs on the POST path
  only (a `GET` returns `405`, not `401`). Request fields use proto snake_case
  names — e.g. `{"agent_id": "abc123"}`, not `{"id": ...}`.
- **Server-streaming RPCs are the exception to `application/json`.** They cannot
  be reached with the unary media type — the daemon answers `415` with a
  `{"code","message"}` envelope that names the media type to use — because a
  streaming RPC needs Connect's streaming protocol (`Content-Type:
  application/connect+json`, one envelope per message in *and* out). §5 has a
  copy-pasteable `ExecAgent` client.
- TLS: optional per listener (`tls.*` config: cert/key files, certmagic
  auto-TLS/Let's Encrypt, self-signed, or mTLS). Plaintext is the default.
- The CLI resolves the endpoint from the server registry in `~/.bunker/config.yaml`
  (see §3) or an explicit `--server` flag — one CLI can drive many daemons.

### Request and response shape

**Requests use proto field names; responses use protojson JSON names.** A REST
request body is decoded with the field names declared in
`proto/bunker/v1/bunker.proto` — `{"agent_id": "abc123"}`, not
`{"agentId": ...}`. The response is encoded with **protojson**, which emits the
JSON (camelCase) name of every field, so a field goes in as `agent_id` and comes
back as `agentId`.

Every example below is a verbatim transcript from a daemon built at repo HEAD
(version `0.1.4`). The captured daemon listened on a scratch port
(`server.rest_addr: 127.0.0.1:18099`) while the commands below use the default
`:8080` — substitute your own `server.rest_addr`. The capture came from a
**non-root** daemon, which is why the `/tmp` fields read `unknown` — a root
daemon with the host provisioning applied reports `private` (see the README).
With `auth.enabled: true`, add `-H 'Authorization: Bearer <token>'` to each
command (§4).

```bash
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerInfo \
    -H 'Content-Type: application/json' -d '{}'
{"hostname":"<host>","version":"0.1.4","uptimeSeconds":"7","maxAgents":10,"tmpIsolation":"unknown","tmpIsolationDetail":"daemon is not running as root — /tmp isolation state cannot be verified"}
```

**64-bit integral fields are JSON strings; 32-bit and float fields are JSON
numbers.** protojson serializes `int64`/`uint64`/`fixed64` as strings, because a
bare JSON number is an IEEE-754 double and would lose precision on large values.
In the response above `uptimeSeconds` is the string `"7"` while `maxAgents` is
the number `10`. The same split in `ServerMetrics` (byte counters are strings,
the CPU percentage is a number):

```bash
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerMetrics \
    -H 'Content-Type: application/json' -d '{}'
{"cpuUsagePercent":179.27896665671176,"memoryUsedBytes":"16751112192","memoryTotalBytes":"64048345088","diskUsedBytes":"1339266465792","diskTotalBytes":"1966736678912","dockerContainersTotal":8}
```

What that means for a client:

- **Parse before you compare.** `jq '.uptimeSeconds > 5'` is a type error — use
  `(.uptimeSeconds | tonumber) > 5`. In JavaScript a 64-bit field arrives as a
  string, so `"7" === 7` is `false` and `"7" < 5` is `false`; convert explicitly
  (`Number(...)`, or `BigInt(...)` where the value can exceed 2^53).
- The encoding is decided per field by its proto type, not per RPC — never infer
  "numeric" from the field name alone.
- The CLI's own output (`bunker status`, `bunker info`) already parses these for
  you; the string encoding is what a protocol-level client sees.

The same default-omission applies to **scalars**: `cpuUsagePercent` is absent
from a `ServerMetrics` body until the daemon has a CPU sample, so the first call
after startup can answer without it — observed live, immediately after start:

```bash
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerMetrics \
    -H 'Content-Type: application/json' -d '{}'
{"memoryUsedBytes":"15791751168","memoryTotalBytes":"64048345088","diskUsedBytes":"1349257228288","diskTotalBytes":"1966736678912","dockerContainersTotal":8}
```

The next call returns it (`"cpuUsagePercent":437.2045973140275`). Treat a
missing numeric key as its zero value, not as an error.

**A repeated field with no entries is omitted, not `[]`.** protojson omits
fields holding their default value, and a repeated field defaults to empty — so
`ListAgents` on a daemon with no agents answers `{}`, and the client-visible
value is `null`, not an empty list:

```bash
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ListAgents \
    -H 'Content-Type: application/json' -d '{}'
{}
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ListAgents \
    -H 'Content-Type: application/json' -d '{}' | jq '.agents'
null
```

Treat a missing key, `null` and `[]` as the same thing (zero entries) — do not
read `response.agents.length` and expect `0`.

**Errors carry a JSON `{"code","message"}` envelope and a real HTTP status.**
The `code` is the connect code as a string; `message` is human-readable. A
`GetAgent` miss is a `404` (not `200`-with-empty), a body that will not decode is
a `400`:

```bash
$ curl -si http://127.0.0.1:8080/bunker.v1.Bunkerd/GetAgent \
    -H 'Content-Type: application/json' -d '{"agent_id":"deadbeef"}' | head -1
HTTP/1.1 404 Not Found
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/GetAgent \
    -H 'Content-Type: application/json' -d '{"agent_id":"deadbeef"}'
{"code":"not_found","message":"agent \"deadbeef\" not found"}
```

```bash
$ curl -si http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerInfo \
    -H 'Content-Type: application/json' -d 'bad' | head -1
HTTP/1.1 400 Bad Request
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerInfo \
    -H 'Content-Type: application/json' -d 'bad'
{"code":"invalid_argument","message":"unmarshal message: unmarshal into *bunkerv1.ServerInfoRequest: proto: syntax error (line 1:1): invalid value bad"}
```

**Not every non-2xx body is JSON, and the method/path checks run before auth.**
An unknown RPC *path* never reaches the connect handler, so it is the HTTP
router's plain-text 404 — no envelope to decode:

```bash
$ curl -si http://127.0.0.1:8080/bunker.v1.Bunkerd/NoSuchMethod \
    -H 'Content-Type: application/json' -d '{}'
HTTP/1.1 404 Not Found
Content-Type: text/plain; charset=utf-8

404 page not found
```

A known path sent with a non-POST method is a `405` carrying `Allow: POST`
(remaining response headers omitted):

```bash
$ curl -si http://127.0.0.1:8080/bunker.v1.Bunkerd/ServerInfo
HTTP/1.1 405 Method Not Allowed
Allow: POST
Content-Length: 0
```

Because both checks precede the auth interceptor, an unauthenticated `GET` is a
`405`, **never** a `401`. Check the status and `Content-Type` before assuming a
non-2xx body is a `{"code","message"}` envelope.

**Auth on the wire** (with `auth.enabled: true` — the default; see §4):

| Request | HTTP status | Body |
|---------|-------------|------|
| POST with no `Authorization` header | `401` | `{"code":"unauthenticated","message":"missing Authorization header"}` |
| POST with a wrong bearer value | `401` | `{"code":"unauthenticated","message":"invalid token"}` |
| POST with the valid token | `200` | the RPC's normal JSON body |

A daemon started with `auth.enabled: false` accepts unauthenticated POSTs and
prints a loud warning on stderr — that is a deliberate local-development mode,
not the default:

```
bunkerd: *** WARNING: AUTH DISABLED *** — running WITHOUT authentication; any client that can reach this server can spawn/destroy agents. Set auth.enabled: true and auth.token in the config file to enable authentication.
```

A daemon running as a **non-root** user still answers the read-only RPCs
(`ServerInfo`, `ServerMetrics`, `ListAgents`, `GetAgent`, `AgentMetrics`, ...),
but `SpawnAgent` fails — agent creation runs `useradd`/`systemd-run` and needs
root. The daemon reports it as `internal` (HTTP 500) with a message naming the
stage that denied it:

```bash
$ curl -si http://127.0.0.1:8080/bunker.v1.Bunkerd/SpawnAgent \
    -H 'Content-Type: application/json' -d '{"ttl":"1h"}'
HTTP/1.1 500 Internal Server Error
Content-Type: application/json

{"code":"internal","message":"spawn 07a0625e failed at stage user-create: useradd bunker-07a0625e failed: exit status 1 (output: useradd: Permission denied.\nuseradd: cannot lock /etc/passwd; try again later.\n)"}
```

(The agent id in that message is generated per request; the `output:` block is
the failing command's verbatim stderr.)

## 3. Connect flow

```bash
# A daemon you run yourself. No third party issues the token, and with
# auth.enabled: false the CLI needs none at all (the daemon then logs a loud
# WARNING: AUTH DISABLED line).
bunker connect http://127.0.0.1:8080

# Register the hosted demo in the local CLI config (writes ~/.bunker/config.yaml).
# Request-access only — see §9 for how a demo token is obtained.
bunker connect http://78.46.173.180:18080 --token <token>

# Token can also come from the environment (no --token flag needed)
BUNKER_TOKEN=<token> bunker connect http://78.46.173.180:18080

# Switch between registered servers
bunker use <name>          # select default server
bunker status --server <name>   # or target a server explicitly
```

Every command resolves the server, attaches the bearer token, and issues the RPC.
`bunker connect` accepts `SERVER_URL` in the form `http://host:port` (the same
URL works for gRPC or REST — connect-go negotiates per request).

## 4. Authentication

Auth is **secure-by-default** (`auth.enabled: true` in `config.example.yaml`):

- **Master token**: configured via `auth.token` (or `BUNKERD_AUTH_TOKEN` env).
  Sent on every request as `Authorization: Bearer <token>`. Without a valid
  token the server returns `401`; missing header and wrong token are both
  rejected before any RPC logic runs (`Config.CheckAuth()` startup gate —
  a daemon configured with auth enabled but no token/jwt_secret **refuses to
  start**; an explicit `auth.enabled: false` prints a prominent
  `*** WARNING: AUTH DISABLED ***` on stderr).
- **Agent-scoped sub-keys**: when JWT auth is enabled (`auth.jwt_secret`),
  spawning an agent can mint an opaque per-agent sub-key. The sub-key may call
  only the **Agent** service, scoped to that `agent_id` (`GetInfo`, `Metrics`,
  `Heartbeat`) — CI/CD and agent-side tooling use these instead of the master
  token. The server-side `Bunkerd` service accepts **master tokens only**.

Example (REST):

```bash
curl -s http://78.46.173.180:18080/bunker.v1.Bunkerd/ServerInfo \
  -H 'Authorization: Bearer <token>' \
  -H 'Content-Type: application/json' -d '{}'
```

#### Audit trail

`bunkerd` records every **authenticated** RPC (master token or agent sub-key)
in an append-only JSONL audit log: one JSON record per request, written
atomically to a mode-`0600` file. Records capture caller identity (derived
from the authenticated claims — **token values are never written**), the
procedure, remote address, target agent, duration, and outcome; the record
schema lives in `internal/audit` (`ts`, `caller`, `method`, `remote_addr`,
`agent_id`, `duration_ms`, `outcome`, `summary`, `hash`, `prev_hash`).
Each record also carries `hash` — the SHA-256 digest of the record's
canonical bytes — and `prev_hash`, the previous record's `hash`, so the log
is a tamper-evident hash chain: editing or dropping any record invalidates
every hash after it, and a client that ignores the two hash fields silently
loses that guarantee.

Configuration is daemon-side, under the `audit` key in `config.yaml`:

```yaml
audit:
  enabled: true                     # log every authenticated RPC (default: true)
  path: /var/log/bunkerd/audit.log  # append-only JSONL audit log
```

| Key | Default | Env override |
|-----|---------|--------------|
| `audit.enabled` | `true` | `BUNKERD_AUDIT_ENABLED` |
| `audit.path` | `/var/log/bunkerd/audit.log` | `BUNKERD_AUDIT_PATH` |

The audit log is a server-side concern — clients need nothing special; when
enabled, every authenticated request is recorded daemon-side. Rotate
`audit.path` like any other daemon log. If the log cannot be opened (missing
or unwritable path), the daemon logs a warning and continues **without**
auditing — audit failure never blocks startup.

## 5. RPC surface (`proto/bunker/v1/bunker.proto`)

### `bunkerd.Bunkerd` — server management + agent lifecycle (master token)

| RPC | Kind | Purpose |
|-----|------|---------|
| `ServerInfo` | unary | hostname, version, uptime, agent count/capacity, total & available resources, residue inventory (orphan users/homes/keys/stale linger entries + probe status) |
| `ServerMetrics` | unary | live CPU %, memory used/total, disk used/total, plus an `agents[]` array of per-agent summaries (see below) |
| `SpawnAgent` | unary | create an agent (`agent_id` handle, TTL, resource limits, network mode, SSH key, labels, image spec) |
| `DestroyAgent` | unary | tear down an agent — idempotent in TWO distinct cases, see the note below the tables |
| `ListAgents` | unary | all agents with status, resources, endpoints |
| `GetAgent` | unary | one agent's details |
| `AgentMetrics` | unary | one agent's live resource usage |
| `ExecAgent` | **server-streaming** | run a command, stream stdout/stderr + exit code |
| `RunAgent` | unary | run a command in the agent's environment (`--detach` for background) |
| `HeartbeatAgent` | unary | extend an agent's TTL |
| `QueryAudit` | unary | read the audit trail (filters: agent/method/since/until/limit; see [audit.md](audit.md)) |

### `bunkerd.Agent` — scoped sub-key access

| RPC | Purpose |
|-----|---------|
| `GetInfo` | agent self-description (id, limits, endpoints) |
| `Metrics` | the agent's own resource usage |
| `Heartbeat` | the agent extends its own TTL |

#### `ServerMetrics.agents[]` — per-agent summaries in one call

The `ServerMetrics` response also carries an `agents: [...]` array of
repeated `AgentSummary` messages — the same per-agent summaries
`ListAgents` returns. Use it to read every agent's status, resource
limits, disk usage, and port range in a single call, with no separate
`ListAgents` round-trip. Each element's fields:

| Field | Type | Meaning |
|-------|------|---------|
| `agent_id` | string | the agent handle |
| `status` | string | `pending`, `starting`, `running`, `stopping`, `stopped`, or `failed` |
| `limits` | `ResourceLimits` | the agent's CPU/memory/disk/container caps |
| `created_at` / `expires_at` | string | RFC3339 timestamps; `expires_at` is when the TTL lapses |
| `sshfs_mount` | string | ready-to-run SSHFS mount command (empty when none) |
| `docker_host_tunnel` | string | ready-to-run `ssh -L` tunnel command for the agent's rootless Docker (empty when none) |
| `public_url` | string | the agent's public HTTPS URL |
| `port_range_start` / `port_range_end` | uint32 | the agent's reserved port range |
| `tailnet_ip` | string | the agent's tailnet address |
| `disk_used_bytes` | uint64 | per-agent disk usage in bytes |

Note on `DestroyAgent` idempotency — there are TWO distinct cases, both
deliberate (`internal/agent/manager_destroy.go`):

- **Never-known id** (never existed, or the daemon has no record of it):
  `CodeNotFound` (`404 not_found` over REST).
- **Re-destroying an id the daemon already destroyed and still knows in its
  durable lifecycle store** (post-restart TTL reap, CLI retry, reconcile
  cleanup): `200` with `{"status":"destroyed"}` — the daemon confirms the
  teardown without error.

So a client's "treat `not_found` as success" branch matches reality, but
only for the never-known case; the already-destroyed case reports success
directly.

### `SpawnAgent` over REST — naming the agent

`SpawnAgent` is the call that decides how you address the agent afterwards, and
the only field that does that is **`agent_id`**. Every field of
`SpawnAgentRequest` is optional (the REST conventions are §2's: proto snake_case
field names in, protojson camelCase out, `Content-Type: application/json`):

| Field | Type | Meaning |
|-------|------|---------|
| `agent_id` | string | the handle every later call uses (`GetAgent`, `ExecAgent`, `HeartbeatAgent`, `DestroyAgent`). Empty → the daemon generates one. Must match `[a-z0-9-]{1,63}`; the CLI validates that locally, and a bad id sent straight to the daemon fails the spawn's `validate` stage. |
| `limits` | `ResourceLimits` | `cpu_quota` (cores), `memory_max_bytes`, `disk_max_bytes`, `max_docker_containers`; server defaults when empty |
| `network` | `NetworkConfig` | `mode` (`MODE_CLOUDFLARE_TUNNEL`, `MODE_TAILSCALE` or `MODE_DIRECT`), plus `domain`, `trycloudflare`, `port_range_start`, `port_range_end` |
| `ttl` | string | lifetime as `\d+[hmd]` (`6h`, `90m`, `7d`); the server default (`agent.default_ttl`) when empty |
| `ssh_public_key` | bytes | push an existing key instead of letting the daemon generate one — base64 in JSON, because protojson renders `bytes` that way |
| `labels` | map<string,string> | free-form metadata carried on the agent |
| `image_spec` | `ImageSpec` | declarative image customization (allowlisted base + apt/go/npm package adds, GAP-064), validated before any side effect |

**There is no `name` field, and no environment field.** Request bodies are
decoded with unknown fields *discarded*, not rejected, so naming a field the
proto does not have still answers `200` — the mistake is silent:

| Request body | What actually happens |
|--------------|-----------------------|
| `{"agent_id":"build-1","ttl":"1h"}` | ✅ `200`, the agent is `build-1` |
| `{"name":"build-1","ttl":"1h"}` | ⚠️ `200` with an auto-generated `agentId` — `name` does not exist and is dropped, so the follow-up `GetAgent {"agent_id":"build-1"}` is a `404` |
| `{"agent_id":"build-1","env":{"FOO":"bar"}}` | ⚠️ `200`, `env` dropped — the variable is not set |

Read `agentId` back out of the response and compare it with what you asked for:
for an ignored field that check is the only signal there is.

**Environment is configured after the spawn, not during it.** The supported
interface is the CLI — `bunker env set <agent-id> KEY=VALUE` writes
`/run/bunker/<agent-id>/env` on the agent host through `ExecAgent`, and that file
is sourced at the start of every `bunker exec` / `bunker exec --script` and every
detached `bunker run --detach`, so the variables persist until `bunker env unset`
or the agent is destroyed. A REST-only client gets the same effect by writing
that file with `ExecAgent` (`--raw` mode does **not** source it). There is no
per-spawn env field in the proto.

The round trip, copy-pasteable (drop the `Authorization` header on a daemon with
`auth.enabled: false`):

```bash
# 1) Spawn with the handle you want to use from here on.
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/SpawnAgent \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer <token>' \
    -d '{"agent_id":"build-1","ttl":"1h"}'
{"agentId":"build-1", ...}                        # abbreviated — see below

# 2) The same id addresses it afterwards.
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/GetAgent \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer <token>' \
    -d '{"agent_id":"build-1"}'
{"agentId":"build-1","status":"running", ...}

# 3) A handle that was never created answers 404 with the usual envelope.
$ curl -s http://127.0.0.1:8080/bunker.v1.Bunkerd/GetAgent \
    -H 'Content-Type: application/json' -d '{"agent_id":"build-9"}'
{"code":"not_found","message":"agent \"build-9\" not found"}
```

The bodies are abbreviated to the fields that matter here; `SpawnAgentResponse`
also carries the connection bundle — `dockerHostSsh`, `dockerHostTunnel`,
`sshfsMount`, `publicUrl`, `portRangeStart`/`portRangeEnd`, `sshPrivateKey`,
`limits`, `expiresAt`, `tailnetIp`, `apiKey`, `image` — and the live capture of
this round trip (named spawn → `agentId` echoed → `404` on the dropped name) is
in [dogfood/2026-09-18-integration.md](dogfood/2026-09-18-integration.md) §4.4.

### `ExecAgent` over REST — the streaming recipe

`ExecAgent` is the only **server-streaming** RPC in the service, and it is the
one RPC that `application/json` cannot carry: the daemon refuses the unary media
type with `415` (see §2 and *Errors* below). This section is the missing
recipe — a complete, runnable client that needs nothing but the Python standard
library (3.8+), verified end to end against a live daemon (see
[dogfood/2026-09-18-integration-rest-streaming.md](dogfood/2026-09-18-integration-rest-streaming.md)
for the captured transcript).

```
POST /bunker.v1.Bunkerd/ExecAgent
Content-Type: application/connect+json      # NOT application/json
Authorization: Bearer <token>
body:  [flags:1 byte][length:4 bytes big-endian][protojson payload]
```

The response is `HTTP 200` with `Transfer-Encoding: chunked` (any HTTP library
de-chunks for you) whose body is the same envelope sequence, one envelope per
write of the remote process. The mapping from the proto to that stream:

| Frame | protojson payload | Meaning |
|-------|-------------------|---------|
| `flags=0x00` | `{"stdout":"PGJhc2U2ND4="}` | a chunk of the command's stdout — **base64**, not text (protojson `bytes`) |
| `flags=0x00` | `{"stderr":"PGJhc2U2ND4="}` | a chunk of its stderr, also base64 |
| `flags=0x00` | `{}` | the exit code is **0** — protojson omits a default value, so a zero `exitCode` is simply absent |
| `flags=0x00` | `{"exitCode":3}` | a **non-zero** exit code (the only time the field appears) |
| `flags=0x02` | `{}` | end-of-stream trailer: the command has finished |

An RPC error is not an HTTP error: it arrives **inside** the `200` stream as
`{"error":{"code":"...","message":"..."}}`, so checking the status code alone
reports success on a failure. `Connect-Protocol-Version: 1` is **not** required.

**Do not append an end-of-stream envelope to the request.** Both spec-shaped
endings are rejected, and the rejection arrives inside the `200`:

| Request body tail | What the daemon answers |
|-------------------|-------------------------|
| nothing — let `Content-Length` end the body | ✅ the full frame sequence above |
| a bare `0x02` byte | `{"error":{"code":"invalid_argument","message":"protocol error: incomplete envelope: unexpected EOF"}}` |
| `[0x02][0x00000000]` (5 bytes) | `{"error":{"code":"internal","message":"unmarshal end stream message: unexpected end of JSON input"}}` |

A copy-pasteable client — save as `exec_agent_rest.py`, `chmod +x`, then:

```bash
export BUNKER_TOKEN=...                     # from ~/.bunker/config.yaml; never hardcode it
python3 exec_agent_rest.py http://127.0.0.1:8080 kara-lair -- sh -c 'echo hello'
```

```python
#!/usr/bin/env python3
"""Run a command in a Bunker agent over the REST (Connect streaming) surface.

Stdlib only (Python 3.8+). Usage:

    export BUNKER_TOKEN=...                     # never hardcode a token
    python3 exec_agent_rest.py http://127.0.0.1:8080 kara-lair -- sh -c 'echo hello'

Prints the command's stdout to stdout, its stderr to stderr, and the remote
exit code to stderr. Exits 0 when the command ran (whatever its own exit code)
and 1 on a transport/protocol/RPC failure.
"""
import base64
import http.client
import json
import os
import struct
import sys
import urllib.error
import urllib.request

ENVELOPE_MESSAGE = 0x00
ENVELOPE_TRAILER = 0x02


def envelope(payload: bytes, flags: int = ENVELOPE_MESSAGE) -> bytes:
    """[flags:1][len:4 big-endian][payload] — one Connect streaming message."""
    return bytes([flags]) + struct.pack(">I", len(payload)) + payload


def iter_envelopes(raw: bytes):
    """Yield (flags, payload) for each envelope in an already de-chunked body."""
    pos = 0
    while pos + 5 <= len(raw):
        flags = raw[pos]
        (length,) = struct.unpack(">I", raw[pos + 1:pos + 5])
        pos += 5
        if pos + length > len(raw):
            raise ValueError(
                "truncated envelope: flags=0x%02x needs %d bytes, %d left"
                % (flags, length, len(raw) - pos)
            )
        yield flags, raw[pos:pos + length]
        pos += length


def main(argv) -> int:
    if len(argv) < 5 or argv[3] != "--":
        print(__doc__, file=sys.stderr)
        return 1
    base_url, agent_id = argv[1].rstrip("/"), argv[2]
    command = argv[4:]

    token = os.environ.get("BUNKER_TOKEN", "")
    if not token:
        print("BUNKER_TOKEN is not set — read it from ~/.bunker/config.yaml, never hardcode it.", file=sys.stderr)
        return 1

    body = envelope(json.dumps({
        "agentId": agent_id,          # requests accept the proto field names too
        "command": command[0],
        "args": command[1:],
    }).encode())
    # No end-of-stream envelope: Content-Length ends the request body.

    req = urllib.request.Request(
        base_url + "/bunker.v1.Bunkerd/ExecAgent",
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/connect+json",   # not application/json
            "Authorization": "Bearer " + token,
            "Accept": "*/*",
        },
    )

    try:
        # urlopen de-chunks the Transfer-Encoding: chunked response for us.
        with urllib.request.urlopen(req, timeout=300) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as err:
        print("HTTP %d %s: %s" % (err.code, err.reason, err.read().decode("utf-8", "replace")), file=sys.stderr)
        return 1
    except http.client.HTTPException as err:
        print("streaming response was not decodable (%s)" % err, file=sys.stderr)
        return 1

    exit_code = 0
    try:
        for flags, payload in iter_envelopes(raw):
            if flags & ENVELOPE_TRAILER:
                continue                          # end-of-stream trailer: no data
            if not payload:
                continue                          # e.g. {} for a zero exit code
            msg = json.loads(payload)
            if "error" in msg:                    # delivered inside an HTTP 200
                print("stream error: %s" % json.dumps(msg["error"]), file=sys.stderr)
                return 1
            if "stdout" in msg:
                sys.stdout.write(base64.b64decode(msg["stdout"]).decode("utf-8", "replace"))
            if "stderr" in msg:
                sys.stderr.write(base64.b64decode(msg["stderr"]).decode("utf-8", "replace"))
            if "exitCode" in msg:
                exit_code = msg["exitCode"]
    except (ValueError, json.JSONDecodeError) as err:
        print("stream was truncated or not valid JSON (%s)" % err, file=sys.stderr)
        return 1

    print("exit code: %d" % exit_code, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
```

Captured against a live `bunker-las-02` (`kara-lair`), 2026-09-18:

```bash
$ python3 exec_agent_rest.py http://100.116.99.35:10001 kara-lair -- sh -c 'echo OUT'
OUT
exit code: 0
$ python3 exec_agent_rest.py http://100.116.99.35:10001 kara-lair -- sh -c 'echo ERR 1>&2; exit 3'
ERR
exit code: 3
```

**Known limitation — a command that writes to BOTH stdout and stderr.** Measured
2026-09-18 on a live `0.1.4` daemon (build `cef10fc`): when the command's output
reaches both pipes, the streamed response arrives corrupted at the socket level
(the status line and body are interleaved with duplicates), so no client can
decode it — `0/10` attempts decoded, while the same daemon decoded `10/10`
stdout-only commands in the same run. The strongly-indicated cause is in the
daemon, not the client: `ExecAgent` writes the stdout and the stderr frames from
two goroutines calling `ServerStream.Send` concurrently, and Connect's send path
marshals straight into the HTTP writer without a lock (the daemon log records
`http: superfluous response.WriteHeader call from ...middleware.(*basicWriter).Write`
at the same moment). Until that is fixed, keep a command's output on one stream,
or write the second stream to a file and read it with a second `ExecAgent` call:

```bash
$ python3 exec_agent_rest.py http://127.0.0.1:8080 kara-lair -- \
      sh -c '{ echo OUT; echo ERR 1>&2; } 2>&1'      # both lines, one stream
OUT
ERR
exit code: 0
```

Related: `RunAgent` is *unary* (it returns one response, `--detach` for
background work) — do not send it Connect streaming framing.

### Errors

connect-go error codes: `CodeInvalidArgument` (e.g. bad `--ttl` format, or a body
that will not decode), `CodeNotFound` (get/destroy/exec on an unknown agent),
`CodeUnauthenticated` (401 — missing/invalid token), `CodeUnavailable` (this
server has audit logging disabled), and `CodeInternal` (HTTP 500 — the operation
failed server-side; a non-root daemon reports its spawn failure here, with a
message naming the failing stage, and the CLI exits `1` on it).

On the wire each of these is a JSON `{"code","message"}` envelope with the HTTP
status the code maps to, and the code appears as its string form (`not_found`,
`invalid_argument`, `unauthenticated`, ...) — see **Request and response shape**
in §2 for captured examples. Not every non-2xx response is JSON: an unknown RPC
path is a plain-text `404` from the HTTP router.

**An unsupported media type on a streaming RPC carries an envelope too.** Calling
a server-streaming RPC with the unary media type (`application/json`) answers
`415 Unsupported Media Type` with
`{"code":"invalid_argument","message":"… is a server-streaming RPC … Retry with
Content-Type: application/connect+json …"}`, and the `Accept-Post` response
header lists every media type the daemon accepts for that RPC. A daemon older
than that change answers the same `415` with an empty body — which is why the
streaming recipe in §5 is worth reading before writing a client.

## 6. Agent lifecycle walkthrough

```
spawn ──▶ exec/run ──▶ cp/deploy ──▶ mount/tunnel ──▶ metrics ──▶ heartbeat ──▶ destroy
(create)   (command)   (files)        (fs/socket)     (observe)   (extend TTL)   (cleanup)
```

1. **Spawn** — `bunker spawn build-1 --ttl 2h --cpu 2 --memory 4294967296`
   (`--memory`/`--disk` take a byte count — 4294967296 is 4 GiB; the
   positional argument is an alias for `--agent-id`).
   Validates TTL (`\d+[hmd]`, e.g. `6h`, `90m`, `7d`) before any side effects;
   allocates a port range from the configured pool (default 10000–19999, 100 per
   agent); creates the Linux user, SSH keypair, and rootless dockerd.
2. **Exec / Run** — `bunker exec build-1 -- env FOO=bar make test`
   (compound shell snippets work: `bunker exec build-1 -- 'if [ -f x ]; then cat x; fi'`).
   `bunker run build-1 --detach -- sleep 600` backgrounds a long job
   (`--detach` is a `bunker run` flag, so it goes before the `--` separator;
   everything after `--` belongs to the remote command).
3. **Files** — `bunker cp file.txt build-1:/tmp/`,
   `bunker deploy dist/ build-1:/tmp/dist/` (deploy takes the local directory
   first and an `<agent-id>:/path` destination second; scp-based; client
   resolves the SSH host by `--ssh-host` > server URL host >
   server hostname, and always uses `-o IdentitiesOnly=yes`).
4. **Mount / Tunnel** — `bunker mount build-1 /mnt/agent` (SSHFS),
   `bunker tunnel build-1` → `docker -H localhost:2376 ps` (forwards the agent's
   rootless Docker socket).
5. **Observe** — `bunker metrics build-1`, `bunker status` (agent + server level).
6. **Heartbeat** — `bunker heartbeat build-1` extends the TTL; agents expire and
   auto-destroy when the TTL elapses.
7. **Destroy** — `bunker destroy build-1`; idempotent: a never-known id
   returns `not_found` (safe to treat as success), while re-destroying an id
   the daemon already destroyed and still knows returns `destroyed` with no
   error. Cleans user + dockerd + run dirs. Always destroy scratch agents
   when done — the demo server is shared.

## 7. Resource limits & networking knobs (per spawn)

| Knob | Default | Notes |
|------|---------|-------|
| CPU quota | 2.0 cores | cgroup cpu.max |
| Memory | 4 GiB | cgroup memory.max |
| Disk | 20 GiB | quota |
| Max processes / open files | 4096 / 65536 | systemd Limits |
| Max Docker containers | 10 | per agent |
| TTL | 6h (`agent.default_ttl`) when `--ttl` is omitted | `\d+[hmd]`, heartbeat-extendable |
| Network mode | direct port range | `--network cloudflare` (TryCloudflare/named), `--network tailscale`, or direct |

Server capacity defaults: `max_agents: 50`, port range 10000–19999. All
overridable in `config.yaml` (see `config.example.yaml` at the repo root).

## 8. Integration checklist

- [ ] Auth: master token provisioned; never hardcode it in client code — read
      from config/env.
- [ ] Prefer agent-scoped sub-keys for agent-side tooling (JWT mode).
- [ ] Handle `CodeUnauthenticated` (401) with a clear "bad/expired token" message.
- [ ] Handle `CodeNotFound` on destroy/exec as idempotent success where appropriate.
- [ ] Always destroy scratch agents (or rely on TTL) — leaked agents consume
      host resources.
- [ ] Use `ExecAgent` streaming for long-running commands; set your own client
      read deadlines in addition to the server `request_timeout` (default 300s).

## 9. Live demo instance

`78.46.173.180` — gRPC `:19090`, REST `:18080`, auth enforced (an unauth
**POST** returns `401`; a non-POST request returns `405` before auth). The
instance runs max 50 agents. Resource-limited shared sandbox.

**Demo tokens are request-access only: there is no self-serve signup, no signup
form and no token endpoint in this repo.** Ask the
[deployBunker](https://github.com/deployBunker) maintainers — open a GitHub issue
on the repo, or email them — and a human issues the token, so expect latency
between your request and the reply. The only token you can issue yourself is
`auth.token` in a daemon **you** run: the README quick start ("Run it locally")
needs no token from anyone and no access to the demo host.
