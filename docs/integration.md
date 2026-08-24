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
- TLS: optional per listener (`tls.*` config: cert/key files, certmagic
  auto-TLS/Let's Encrypt, self-signed, or mTLS). Plaintext is the default.
- The CLI resolves the endpoint from the server registry in `~/.bunker/config.yaml`
  (see §3) or an explicit `--server` flag — one CLI can drive many daemons.

## 3. Connect flow

```bash
# Register a server in the local CLI config (writes ~/.bunker/config.yaml)
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
| `ServerInfo` | unary | hostname, version, uptime, agent count/capacity, total & available resources |
| `ServerMetrics` | unary | live CPU %, memory used/total, disk used/total |
| `SpawnAgent` | unary | create an agent (name, TTL, resource limits, network mode, env vars) |
| `DestroyAgent` | unary | tear down an agent (idempotent — unknown id → `CodeNotFound`) |
| `ListAgents` | unary | all agents with status, resources, endpoints |
| `GetAgent` | unary | one agent's details |
| `AgentMetrics` | unary | one agent's live resource usage |
| `ExecAgent` | **server-streaming** | run a command, stream stdout/stderr + exit code |
| `RunAgent` | unary | run a command in the agent's environment (`--detach` for background) |
| `HeartbeatAgent` | unary | extend an agent's TTL |

### `bunkerd.Agent` — scoped sub-key access

| RPC | Purpose |
|-----|---------|
| `GetInfo` | agent self-description (id, limits, endpoints) |
| `Metrics` | the agent's own resource usage |
| `Heartbeat` | the agent extends its own TTL |

### Errors

connect-go error codes: `CodeInvalidArgument` (e.g. bad `--ttl` format), `CodeNotFound`
(destroy/exec on an unknown agent), `CodeUnauthenticated` (401 — missing/invalid
token), `CodeFailedPrecondition` (non-root daemon attempting spawn), `CodeInternal`
(surfaces as `exit code N` on the CLI, which exits with the remote code).

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
| TTL | none (required on spawn) | `\d+[hmd]`, heartbeat-extendable |
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

`78.46.173.180` — gRPC `:19090`, REST `:18080`, auth enforced (401 without
token). Resource-limited shared sandbox; demo tokens are provisioned on request.
See the README **Live demo** callout.
