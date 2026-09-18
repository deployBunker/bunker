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
`agent_id`, `duration_ms`, `outcome`, `summary`).

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
| `ServerMetrics` | unary | live CPU %, memory used/total, disk used/total |
| `SpawnAgent` | unary | create an agent (name, TTL, resource limits, network mode, env vars) |
| `DestroyAgent` | unary | tear down an agent (idempotent — unknown id → `CodeNotFound`) |
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

## 6. Agent lifecycle walkthrough

```
spawn ──▶ exec/run ──▶ cp/deploy ──▶ mount/tunnel ──▶ metrics ──▶ heartbeat ──▶ destroy
(create)   (command)   (files)        (fs/socket)     (observe)   (extend TTL)   (cleanup)
```

1. **Spawn** — `bunker spawn --name build-1 --ttl 2h --cpu 2 --mem 4g`
   Validates TTL (`\d+[hmd]`, e.g. `6h`, `90m`, `7d`) before any side effects;
   allocates a port range from the configured pool (default 10000–19999, 100 per
   agent); creates the Linux user, SSH keypair, and rootless dockerd.
2. **Exec / Run** — `bunker exec build-1 -- env FOO=bar make test`
   (compound shell snippets work: `bunker exec build-1 -- 'if [ -f x ]; then cat x; fi'`).
   `bunker run build-1 -- --detach sleep 600` backgrounds a long job.
3. **Files** — `bunker cp file.txt build-1:/tmp/`, `bunker deploy build-1 dist/`
   (scp-based; client resolves the SSH host by `--ssh-host` > server URL host >
   server hostname, and always uses `-o IdentitiesOnly=yes`).
4. **Mount / Tunnel** — `bunker mount build-1 /mnt/agent` (SSHFS),
   `bunker tunnel build-1` → `docker -H localhost:2376 ps` (forwards the agent's
   rootless Docker socket).
5. **Observe** — `bunker metrics build-1`, `bunker status` (agent + server level).
6. **Heartbeat** — `bunker heartbeat build-1` extends the TTL; agents expire and
   auto-destroy when the TTL elapses.
7. **Destroy** — `bunker destroy build-1`; idempotent, cleans user + dockerd +
   run dirs. Always destroy scratch agents when done — the demo server is shared.

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
