# Bunker — Multi-Agent Coding Platform

> Spin up isolated, resource-limited Linux environments with rootless Docker — each controlled through a single CLI or API call.

![Bunker](bunker-hero.png)

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![gRPC](https://img.shields.io/badge/gRPC-connect--go-4285F4)](https://connectrpc.com/)
[![Docker](https://img.shields.io/badge/Docker-rootless-2496ED?logo=docker)](https://docs.docker.com/engine/security/rootless/)

---

## What is Bunker?

Bunker is a **multi-agent hosting platform** — a daemon (`bunkerd`) that runs on a Linux host and a CLI (`bunker`) that controls it remotely. Each agent is a fully isolated Linux user with its own rootless Docker daemon, SSH access, resource limits, and optional public networking.

```
┌─────────────────────────────────────────────────────┐
│                    bunkerd                          │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐         │
│  │ agent-a  │  │ agent-b  │  │ agent-c  │  ...50   │
│  │ user     │  │ user     │  │ user     │         │
│  │ dockerd  │  │ dockerd  │  │ dockerd  │         │
│  │ ports    │  │ ports    │  │ ports    │         │
│  └──────────┘  └──────────┘  └──────────┘         │
│         ↑ SSHFS mount / Docker tunnel              │
└─────────────────────────────────────────────────────┘
         │                      │
    ┌────┴────┐            ┌────┴────┐
    │ bunker  │            │ bunker  │
    │ CLI     │            │ CLI     │
    └─────────┘            └─────────┘
```

## Features

- **Isolated agents** — Each agent is a dedicated Linux user with its own home directory, SSH keypair, and rootless Docker daemon
- **Resource limits** — CPU, memory, disk, process count, and open file limits enforced via cgroups (systemd user slice)
- **SSHFS native mount** — Mount any agent's filesystem locally: `bunker mount <id> /mnt/agent`
- **Docker tunnel** — Forward the agent's Docker socket locally: `bunker tunnel <id>` → `docker -H localhost:2376 ps`
- **Multi-server** — One CLI, many `bunkerd` instances. Switch with `--server`
- **Scoped API keys** — Master tokens for admin, agent-scoped sub-keys for CI/CD
- **TTL expiry** — Agents auto-destroy after their time-to-live. Heartbeat to extend
- **Private /tmp per agent (requires host provisioning)** — on a fresh,
  unprovisioned host, agent SSH sessions still share the host `/tmp`.
  After the [required host setup](#provision-host-isolation-before-spawning),
  a matching current daemon provides private temporary directories
  (per-session `pam_namespace` instance; `PrivateTmp=yes` on the rootless
  dockerd and detached-run units), so no agent can read or collide with
  another agent's or root's temporary files. Cross-agent file exchange is
  strictly opt-in through one bounded directory (`/srv/bunker-share`, setgid to
  the agent group and NOT writable by it — mode 2750 — with a kernel-enforced
  per-agent size cap). Install the host half with
  `bunker host-provision --apply`; see
  [specs/agent-tmp-isolation.md](specs/agent-tmp-isolation.md).
  `bunker status` reports the /tmp policy a daemon ACTUALLY enforces
  (`private`, `HOST-SHARED` with a warning, or `not reported` for a daemon
  that predates capability reporting). The feature is VERSION-GATED: a
  daemon built from a tagged release before the isolation work reports
  `HOST-SHARED`/`not reported` even though these docs describe isolation —
  build and run a daemon from the same commit as the CLI.
- **Durable registry** — Agent lifecycle state (spawn/heartbeat/destroy) is an
  append-only JSONL log replayed at startup, so agents survive a `bunkerd`
  restart. Size-capped (5 MiB × 3 rotation), compactable offline with
  `bunker registry compact`, and reconciled against system users on boot
  (orphans destroyed or adopted). See [specs/agent-lifecycle.md](specs/agent-lifecycle.md)
- **Networking** — Cloudflare tunnels (named or TryCloudflare), Tailscale mesh, or direct port ranges
- **gRPC + REST** — Dual protocol via connect-go, single binary
- **TLS/mTLS** — Self-signed, Let's Encrypt (certmagic), or mutual TLS
- **Container mode (planned)** — an opt-in spawn mode that runs the workload
  as a container on the agent's own rootless daemon; design in
  [specs/container-mode.md](specs/container-mode.md) (not implemented yet)

## Quick Start

### Live demo

> **⚠️ Request-access only — no self-serve signup.** Demo tokens are
> provisioned on request by the [deployBunker](https://github.com/deployBunker)
> maintainers (GitHub issue or email; a human replies, so expect latency).
> If you need to try the platform immediately, skip to
> [Prerequisites](#prerequisites) below and run your own instance instead.

A public demo instance runs on **bunker-mvp** (`78.46.173.180`, gRPC :19090 / REST :18080) with auth enforced. Once you have a token, try the platform without standing up your own root daemon:

```bash
# Install the CLI (or: make build && ./bunker)
go install github.com/deployBunker/bunker/cmd/bunker@latest

# 1. Request a demo token from the maintainers (request-access only, see above)
# 2. Connect with your provisioned token:
bunker connect http://78.46.173.180:18080 --token <your-demo-token>
bunker status
bunker spawn --ttl 1h demo-agent
```

The demo is a shared, resource-limited sandbox (max 50 agents; per-agent CPU/memory/disk caps, default 1h TTL) — **do not run production workloads on it**. Auth is enforced: every request needs a bearer token (`bunker connect --token`); an unauthenticated **POST** receives `401` (the REST surface is POST-only — a non-POST request returns `405` before auth runs). See [docs/integration.md](docs/integration.md) for the full client-server protocol.

### Prerequisites

- Linux host (Ubuntu 24.04+ recommended)
- **Root access on the host** — `bunkerd` must run as root. Agent spawn creates Linux users (`useradd`) and systemd user slices (`systemd-run`) for cgroup resource limits; both require root privileges. A non-root daemon starts and serves read-only endpoints (list, version, health), but `bunker spawn` fails with `useradd: Permission denied`. The `bunker` CLI itself can run as any user — it talks to the daemon over gRPC/REST.
- Go 1.26+
- Docker CE (for rootless support)
- `sshfs` (for mount command)
- `cloudflared` (optional, for tunnels)

### Install

```bash
git clone https://github.com/deployBunker/bunker.git
cd bunker
# Builds ./bunkerd and ./bunker with version info baked in (ldflags)
make build
```

> **Build before use.** The repo does not ship prebuilt binaries — `bunker`,
> `bunkerd`, and `bin/` are gitignored (GAP-036). Always run `make build`
> after cloning; a stale or missing `./bunker` is not the CLI this README
> documents. Check the build with `./bunker --version` (cobra auto-flag,
> GAP-035) — `bunker version` prints the full commit/build metadata.

> **Freshness check.** `go install ...@latest` serves the newest release
> tag (v0.1.3), which may lag the repo HEAD. If `bunker --version`'s
> `commit:` field doesn't match the repo's `git rev-parse HEAD`, the
> binary is stale — rebuild from HEAD with `make build`.

### Configure

A ready-made example lives at `config.example.yaml` in the repo root:

```bash
cp config.example.yaml /etc/bunkerd/config.yaml
# edit /etc/bunkerd/config.yaml — set auth.token before first start
```

Or write the file directly:

```bash
cat > /etc/bunkerd/config.yaml << EOF
server:
  grpc_addr: ":9090"
  rest_addr: ":8080"

agent:
  ssh_dir: /etc/bunkerd/ssh
  max_agents: 100
  default_cpu_quota: 2.0           # 2 CPU cores
  default_memory_bytes: 4294967296  # 4 GB
  default_disk_bytes: 21474836480   # 20 GB
  default_max_processes: 4096
  default_max_open_files: 65536
  default_max_docker_containers: 10
  port_range_start: 10000
  port_range_end: 19999
  port_range_per_agent: 100

auth:
  enabled: true
  token: "your-master-token-here"
EOF
```

**Non-default ports** — `bunkerd` listens on `:9090` (gRPC) and `:8080` (REST)
by default. If those are already occupied on the host (a common scratch-host
collision), change `server.grpc_addr` / `server.rest_addr` in
`/etc/bunkerd/config.yaml` before starting the daemon:

```yaml
server:
  grpc_addr: ":19090"
  rest_addr: ":18080"
```

Then point the CLI at the new REST port when connecting — e.g.
`bunker connect http://bunker-host:18080 --token ...` (the public demo above
uses exactly these alternate ports).

**Multiple daemons on one host** — a second `bunkerd` on the same machine
shares the host's `bunker-*` user namespace and the durable registry
(`agent.registry.path`). At startup, reconciliation leaves agents whose
persisted ports fall outside that daemon's own pool untouched (logged as a
loud `skipping foreign orphan agent` warning) instead of destroying them —
but two daemons with OVERLAPPING port pools remain unsupported. Give each
instance its own `port_range_start`/`port_range_end` slice, its own
`agent.registry.path`, and destroy an agent from the daemon that owns it.

**Audit trail** — `bunkerd` writes an append-only JSONL audit log of every
authenticated RPC (one record per request; file mode `0600`; token values are
never written). It is on by default; configure it under `audit` in
`config.yaml` — `audit.enabled` (default `true`) and `audit.path` (default
`/var/log/bunkerd/audit.log`) — or via the `BUNKERD_AUDIT_ENABLED` /
`BUNKERD_AUDIT_PATH` env overrides. The log file is `0600` and **root-owned**,
so `bunker audit list` / `export` / `verify` against the local log must run as
root (e.g. via `sudo`); non-root users can instead query the daemon remotely
with `bunker audit list --server <alias>` / `bunker audit export --server <alias>`.

**Containment disclosure (optional, hidden by default)** — an operator can
make managed agents honestly disclose their sandbox. When
`containment.disclosure: true` (or `BUNKERD_CONTAINMENT_DISCLOSURE=true`,
which overrides the config file):

- every agent session (exec, exec --raw, exec --script, run --detach) gets
  the environment variable `BUNKER_SANDBOX=1`; and
- a strict allowlist of system-info probes (`uname`, `hostname`, `uptime`,
  `free`, `df`, `id`, `whoami`, `lsb_release`, and
  `cat /etc/os-release`) gets one self-describing marker line appended to
  its stdout:

  ```
  [bunker: managed sandbox environment — containment active]
  ```

Command exit codes are preserved exactly (including non-zero). Non-probe
commands are never modified, and with the flag off (the default) daemon
behavior is byte-identical to a build without the feature. Machine parsers
should tolerate the bracketed final marker line on disclosing servers.
See `specs/containment-disclosure.md` for the full semantics, allowlist
rules, and safety boundaries.

### Provision host isolation before spawning

**Required for private `/tmp`:** building or starting `bunkerd` does not install
its host-side SSH/PAM configuration. A fresh unprovisioned host leaves agent SSH
sessions sharing the host `/tmp`. Run these commands on the **daemon host**, as
root, from the checkout where `make build` produced both binaries. This is local
host administration, not an RPC to the server selected by `bunker connect`.

```bash
# Inspect the plan first; no host changes without --apply.
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd"
# After reviewing the plan, install the host-side boundary.
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd" --apply
# Inspect the resulting host configuration (add --json for automation).
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd" --status
```

Use the **same daemon binary** when starting the service below. For an installed
service, substitute its actual executable path in `--daemon-binary` (the default
is `/usr/local/bin/bunkerd`). Upgrade/restart an existing service before applying
new host isolation rules: an older daemon can create agents without the required
`bunker-agents` membership, and the fail-closed PAM rules then deny their SSH
sessions (including exec, cp and mount).

The current installer refuses a detected daemon version below **0.1.4**. This
version floor is not a capability guarantee: the `v0.1.4` tag predates the
isolation implementation, and matching version numbers alone do not prove the
running daemon is current. Build CLI and daemon from the same current checkout.
An absent, unreadable, timed-out or unparseable daemon version produces an
**UNKNOWN warning**, not proof of compatibility. Resolve that warning rather
than bypassing it with `--allow-daemon-skew`.

`host-provision --status` inspects static host configuration; it is not an
end-to-end SSH-session test. After starting/restarting the matching daemon and
connecting, run `bunker status` **before spawning**. Require the `/tmp:` line to
report `private`; `HOST-SHARED`, `unknown`, or `not reported` is not evidence of isolation.
Do not put isolation-sensitive workloads on that host until the setup and
running daemon agree. See [Agent isolation](#agent-isolation-tmp-and-cross-agent-exchange)
for the mechanisms and teardown precautions.

### Run the daemon

> **⚠️ `bunkerd` must run as root** — agent spawn calls `useradd`/`systemd-run` and fails with `useradd: Permission denied` under a non-root user. Run it directly as root (or via `sudo`, or as a systemd service under `User=root`):

```bash
sudo ./bunkerd --config /etc/bunkerd/config.yaml
```

To run `bunkerd` as a managed systemd service instead (auto-start on boot, logrotate, status via systemd), use the built-in helper:

```bash
bunker systemd install --binary /usr/local/bin/bunkerd --config /etc/bunkerd/config.yaml --user root
bunker systemd status
# teardown: bunker systemd uninstall
```

### Use the CLI

```bash
# Connect to a server
bunker connect http://bunker-host:8080 --token your-master-token-here

# Create an agent with 2 CPUs and 4 GB RAM
bunker spawn --cpu 2.0 --memory 4294967296 --ttl 6h
# NOTE: the FIRST spawn on a host is slow. It installs rootless Docker into the
# agent's home (a ~93 MB download) and takes 60-90s+ before dockerd is ready.
# The CLI prints "Creating agent..." and waits under a 300s deadline, so a
# multi-minute first spawn is expected, not a hang. Later spawns reuse the
# server's cached tooling and return in ~10s.

# Create an agent with a customized image (GAP-064): package-add spec
cat > spec.json <<'EOF'
{
  "packages": [
    {"manager": "apt", "packages": ["jq", "curl"]},
    {"manager": "npm", "packages": ["typescript@5.6.3"]}
  ]
}
EOF
bunker spawn --image-spec spec.json --ttl 6h
# Rejected specs (curl|sh, base-image swaps, unknown fields, ...) fail fast
# with invalid_argument and build nothing. Identical specs share one cached
# build per agent.

# List agents
bunker list

# Run a command inside the agent (including Docker)
bunker exec abc12345 -- docker run --rm alpine echo hello

# Mount the agent's filesystem locally
bunker mount abc12345 /mnt/my-agent

# Forward the agent's Docker socket
bunker tunnel abc12345
# In another terminal: DOCKER_HOST=tcp://localhost:2376 docker ps

# See agent details
bunker info abc12345

# Set / read agent environment variables (KEY=VALUE as one argument)
bunker env set abc12345 KEY=VALUE
bunker env get abc12345 KEY

# Extend TTL
bunker heartbeat abc12345

# Tear down (also deletes the client-local SSH key ~/.bunker/keys/abc12345)
bunker destroy abc12345
# Keep the local key for a spawn/destroy/spawn key-reuse cycle:
bunker destroy abc12345 --keep-key
```

> **`bunker heartbeat` extends the TTL, it never shortens it — and there is no
> duration flag.** The heartbeat request carries only the agent ID, so the
> daemon always applies its own default TTL (6h unless the daemon config sets
> `agent.default_ttl`). Every acknowledged heartbeat prints the resulting
> expiry plus the semantics:
>
> ```
> Heartbeat acknowledged for agent abc12345
> Expires at: 2026-09-17T21:00:00-05:00
> TTL: the agent's expiry was extended to the daemon's default TTL (6h unless the daemon config sets agent.default_ttl); an existing longer expiry is never shortened
> ```
>
> A 7d agent heartbeated here stays at its long expiry (a heartbeat must not
> reset it to 6h — a shorter expiry would destroy the agent on TTL expiry).

> **`bunker destroy` removes your local key.** Spawn saves the agent's private
> key to `~/.bunker/keys/<agent-id>`; destroy deletes it after a successful
> teardown (including the `not_found` path) unless `--keep-key` is passed. If
> you reuse keys across spawn/destroy cycles, pass `--keep-key`.

> **Expiry timestamps are daemon-local, TTL math is UTC.** The `Expires:` line
> in the spawn bundle (and `bunker info` / `bunker heartbeat` output) is
> printed in the **daemon host's local timezone** with its offset, e.g.
> `2026-09-14T15:00:00-05:00`, while TTL durations (`--ttl`) are computed in
> UTC. A correct 6h TTL therefore shows as a local-time timestamp exactly 6h
> ahead of the daemon host's current local time — it is not a UTC clock
> reading, even though the wall-clock hour may differ from UTC by the offset.

## Architecture

```
                   gRPC + REST (connect-go)
  ┌──────────┐ ◄──────────────────────────► ┌──────────┐
  │  bunker   │                              │ bunkerd  │
  │   CLI     │    HTTP/2, JSON + Proto      │  daemon  │
  │  (cobra)  │                              │          │
  └──────────┘                              └────┬─────┘
                                                  │
                    ┌─────────────────────────────┼──────────────────┐
                    │                             │                  │
               ┌────▼─────┐              ┌───────▼───────┐   ┌──────▼──────┐
               │  Agent    │              │    Agent      │   │   Agent     │
               │  Manager  │              │   (Linux user) │   │  (Linux     │
               │           │              │   dockerd     │   │   user)     │
               └───────────┘              └───────────────┘   └─────────────┘
                    │
          ┌─────────┼─────────┐
          │         │         │
     user.slice   TTL       Port
     cgroups     reaper    allocator
```

### Services

| Service | Protocol | RPCs |
|---------|----------|------|
| `Bunkerd` | gRPC + REST | `ServerInfo`, `ServerMetrics`, `SpawnAgent`, `DestroyAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`, `RunAgent`, `HeartbeatAgent`, `QueryAudit` |
| `Agent` | gRPC + REST (scoped) | `GetInfo`, `Metrics`, `Heartbeat` |

The REST surface is **POST-only** (connect-go, mounted without `WithHTTPGet`):
every method is `POST /bunker.v1.Bunkerd/<Rpc>` (or `/bunker.v1.Agent/<Rpc>`),
and any other HTTP method returns `405`. The auth interceptor runs on the POST
path only, so an unauthenticated request gets `401` **only when sent as POST** —
a `GET` returns `405` before auth. Request fields use proto snake_case names
(`agent_id`, not `id`). Full request/response shapes are in
[specs/api.md](specs/api.md).

## Resource Limits

All limits are enforced at **two levels**:

| Level | Mechanism | Scope |
|-------|-----------|-------|
| User slice | systemd drop-in (`user-<UID>.slice.d/50-bunker.conf`) | All processes running as the agent user |
| Docker unit | `systemd-run --system --property=CPUQuota=...` | The dockerd process and its containers |

| Limit | Config key | CLI flag | Default |
|-------|-----------|----------|---------|
| CPU | `agent.default_cpu_quota` | `--cpu` | 2.0 cores |
| Memory | `agent.default_memory_bytes` | `--memory` | 4 GB |
| Disk | `agent.default_disk_bytes` | `--disk` | 20 GB |
| Processes | `agent.default_max_processes` | — | 4096 |
| Open files | `agent.default_max_open_files` | — | 65536 |
| Docker containers | `agent.default_max_docker_containers` | — | 10 |

`bunker metrics <id>` reads the agent's own cgroup slice; when that slice is
unreadable (e.g. a dead agent user) it falls back to **host-level** values and
prints an explicit `NOTE: host-level fallback` line, and the wire response sets
`host_level_fallback` (proto field 10). Treat those numbers as host figures, not
the agent's.

## Agent isolation (`/tmp` and cross-agent exchange)

After [host provisioning](#provision-host-isolation-before-spawning) and starting
a matching current daemon, agent execution paths have an **enforced private
`/tmp`**. This is not automatic on a fresh host: unprovisioned SSH sessions
share the host `/tmp`.

| Execution path | Mechanism | Scope of the private `/tmp` |
|----------------|-----------|-----------------------------|
| `bunker exec`, `bunker ssh`, scp (`cp`/`deploy`), sshfs (`mount`), docker SSH transport | `pam_namespace` per SSH session | the session, persistent per agent |
| rootless dockerd, `bunker run --detach` | `PrivateTmp=yes` on the transient unit | the unit (and its containers) |

The sshd half is **scoped to agent-named sessions** (the reserved `bunker-*`
username pattern) and **fails closed**, so it is an agent boundary and not a
host-wide policy. Bunker installs this marked session block:

```
session    [success=2 auth_err=ignore default=die]    pam_succeed_if.so quiet user !~ bunker-*
session    [success=ignore default=die]               pam_exec.so quiet /usr/lib/bunker/pam-tmp-guard verify bunker-agents
session    required                                   pam_namespace.so
```

The classifier keys on the NAME (not on group state, so a deleted group cannot
turn an agent session into an operator session); a non-agent session jumps over
all three modules and keeps the host's own `/tmp`. An agent session must then
pass the root-owned `pam_exec` precondition — the exact Bunker `/tmp` rule, the
agent group **and** the membership, the instance parent, and its whole trust
chain (root-owned, non-writable helper directory → root-owned sha256 manifest →
the helper's own bytes) — before `pam_namespace` runs. Any failure (missing
helper, missing or wrong or malformed drop-in, missing group, lost membership,
writable helper directory or manifest) **denies** the session instead of
silently continuing with the shared `/tmp`; the module line also carries no
`ignore_config_error`, which would make it skip a broken config. `--status`
(`--json`) inspects the static host properties used by the helper. It does not
execute a live agent session or prove each agent's membership; use it together
with the connected daemon's status and live session verification.

`bunker status` shows the /tmp policy the connected daemon enforces, per
server: `private` when the per-session `pam_namespace` instance is provisioned
and enforced, `HOST-SHARED` (with a prominent warning) when agent sessions see
the host `/tmp`, `unknown` when the state cannot be verified (e.g. the daemon
is not root), and `not reported` when the daemon predates capability
reporting. The isolation feature is **build-dependent**: the `v0.1.4` release
predates both the isolation implementation and capability reporting. Build and
run a daemon from the same current checkout as the CLI; do not infer isolation
from a version number alone.

Follow the [Quick Start provisioning sequence](#provision-host-isolation-before-spawning)
on the daemon host. The installer is idempotent and does not touch `/etc/fstab`.
It changes host SSH/PAM configuration, so review its plan before applying it.

**Teardown only — do not run this as part of installation:**

```bash
sudo ./bunker host-provision --daemon-binary "$(pwd)/bunkerd" --uninstall --apply
```

Uninstall removes the Bunker-managed host configuration and returns agent SSH
sessions to shared `/tmp`. Do not hand-delete only the namespace drop-in: the
remaining fail-closed PAM precondition can deny all `bunker-*` SSH sessions.

Cross-agent file exchange is **opt-in and explicitly bounded**: the only
sanctioned shared location is `/srv/bunker-share`, whose root is root-owned,
setgid to the agent group `bunker-agents` and NOT writable by it or by the world
(mode `2750` — so an agent cannot create a plain, uncapped entry beside the
capped directories), and where the daemon creates one directory per agent with
the setgid bit set to the agent group and a kernel-enforced per-agent size cap
(a tmpfs — writes past the cap fail with `ENOSPC` instead of filling the host).
If the bounded filesystem cannot be mounted, the directory is not created at
all; there is no unbounded fallback.
Agent-group membership itself is granted to every agent at spawn regardless of
that toggle — the membership is what the fail-closed precondition verifies, not
a feature of the exchange directory.

```bash
# agent A hands a file to agent B
bunker exec a -- sh -c 'cp build.tar /srv/bunker-share/a/build.tar'
bunker exec b -- cp /srv/bunker-share/a/build.tar ./build.tar
```

Defaults, limits and the exact verification steps are in
[specs/agent-tmp-isolation.md](specs/agent-tmp-isolation.md).

## CLI Commands

```
bunker connect     Register a bunkerd server
bunker use         Select the active server
bunker status      Show server status (CPU/memory/disk/uptime)
bunker spawn       Create a new agent (--image-spec <file> for package-add image customization)
bunker list        List agents
bunker info        Show agent details
bunker env         Manage agent environment variables
bunker exec        Run a command inside an agent
bunker run         Run a command in an agent's environment (with --detach for background)
bunker mount       Mount agent filesystem via SSHFS
bunker tunnel      Forward agent Docker socket
bunker ssh         Open an interactive SSH session into an agent
bunker cp          Copy a file into an agent's environment
bunker deploy      Deploy a directory into an agent's environment
bunker systemd     Manage the bunkerd systemd service (install/uninstall/status)
bunker metrics     Show resource usage
bunker heartbeat   Extend agent TTL
bunker destroy     Tear down an agent (removes the local key unless --keep-key)
bunker audit       Inspect the audit trail (verify / list / export — see docs/audit.md)
bunker registry    Maintain the durable agent registry (compact)
bunker host-provision  Provision the per-agent isolation boundary on this host
                   (dry run by default; --apply installs, --status reports)
bunker version     Print version/commit/build metadata (also --version)
```

### Exit codes

The CLI has no per-command exit codes of its own: `0` on success, `1` on any
error (printed as `bunker: <error>` on stderr), plus the two ssh-style
exceptions below — a propagated remote exit code from `exec`/`run`, and a
reported not-found outcome from `destroy`.

| Situation | Exit code | Traceable to |
| --- | --- | --- |
| Any command succeeds | `0` | `cmd/bunker/main.go:22` (nil error ⇒ no explicit exit) |
| Any command fails (`spawn`, `list`, `info`, `heartbeat`, `cp`, `mount`, `audit`, `registry`, …) | `1`, with `bunker: <error>` on stderr | `cmd/bunker/main.go:28-29` |
| `bunker exec <id> <cmd>` — the remote command exits non-zero | the remote code verbatim (e.g. `bunker exec … -- sh -c 'exit 7'` ⇒ `7`), printed silently ssh-style | `internal/cli/exec.go:280-281` → `cmd/bunker/main.go:25-27` |
| `bunker run <id> <cmd>` — the remote command exits non-zero | same as `exec` (verbatim remote code) | `internal/cli/run.go:231-232` → `cmd/bunker/main.go:25-27` |
| `bunker destroy <id>` — agent destroyed | `0` | `internal/cli/destroy.go:94` |
| `bunker destroy <id>` — agent **not found** (never spawned, or already destroyed): a REPORTED outcome, not an error | `0` (prints `Agent <id> not found.`) | `internal/cli/destroy.go:80-82` (RPC `CodeNotFound`) and `internal/cli/destroy.go:90-92` (in-band `not_found`) |
| `bunker destroy <id>` — real RPC failure (agent may still exist; local key kept) | `1` | `internal/cli/destroy.go:86` |
| Invalid arguments/flags rejected locally (missing positional args, bad agent id, invalid `--ttl`, unknown exec flag) | `1`, with no RPC attempted | `internal/cli/spawn.go:79` (agent id), `internal/cli/spawn.go:107` (`--ttl`), `internal/cli/exec.go:137` (exec flag grammar) |

`bunker exec` and `bunker run` are the only commands that propagate a remote
exit code; a `not_found` destroy is the only failure-shaped outcome that exits
`0` on purpose. Both exceptions are deliberate and documented in
`internal/cli/SKILL.md`.

## CLI config & path overrides (DF-BUNKER-16)

The CLI stores its state (registered servers, active server) and the
agent SSH keys client-locally. Every path below is explicit and
overridable — no `~` or root defaults are baked into your invocations:

**CLI config file** (server registry + active server):

```
--config /path/to/config.yaml     # highest precedence (persistent root flag)
BUNKER_HOME=/path/to/state        # config at $BUNKER_HOME/config.yaml
$HOME/.bunker/config.yaml         # default (unchanged)
```

`BUNKER_HOME` relocates the whole CLI state directory — the config file
*and* the agent keys directory (`$BUNKER_HOME/keys/`). An explicit
`--config` moves the key directory with it (`keys/` next to the config
file). Empty or whitespace-only values are treated as unset.

**Local-file command defaults** (`audit list/verify/export/status --path`,
`registry compact --path`): the default is read from the **daemon config**
so the CLI inspects the same files the daemon writes:

```
--daemon-config /path/to/config.yaml   # highest precedence (persistent root flag)
BUNKERD_CONFIG=/path/to/config.yaml    # env tier
/etc/bunkerd/config.yaml               # default
```

The daemon config's `audit.path` / `agent.registry.path` become the
`--path` defaults. A missing, unreadable, or invalid daemon config is
never an error — the documented constants (`/var/log/bunkerd/audit.log`,
`/var/lib/bunkerd/agents.jsonl`) apply. An explicit `--path` on the
command always wins.

> **Note:** `bunker systemd install --config <path>` means the **daemon**
> config file (it always did). Its local flag shadows the new root
> `--config`, which targets the CLI config.

## Tech Stack

| Layer | Technology |
|-------|-----------|
| Language | Go (1.26+, per [go.mod](go.mod)) |
| RPC | [connect-go](https://connectrpc.com/) (gRPC + REST, single binary) |
| HTTP router | [chi](https://github.com/go-chi/chi) |
| CLI | [cobra](https://github.com/spf13/cobra) + [viper](https://github.com/spf13/viper) |
| Auth | [golang-jwt](https://github.com/golang-jwt/jwt) v5, opaque sub-keys |
| TLS | [certmagic](https://github.com/caddyserver/certmagic) (self-signed, Let's Encrypt, mTLS) |
| Docker | Rootless via `dockerd-rootless-setuptool.sh` |
| Isolation | Linux user namespaces, systemd cgroups v2 |
| Networking | Cloudflare tunnels, Tailscale mesh, SSH tunneling |
| CI | GitHub Actions |

## Development

```bash
# Build the CLI + daemon binaries (produces ./bunker and ./bunkerd in the repo root)
go build -o bunker ./cmd/bunker
go build -o bunkerd ./cmd/bunkerd

# Or use make build — same binaries with Version/Commit/BuildDate baked in
make build

# Run tests
go test ./... -short

# Run E2E battery (requires a running bunkerd)
bash e2e-full-battery.sh

# Preview exactly what the battery would use — binaries + certification
# verdict, endpoints, token source, state dirs, which sections run. No root,
# nothing is changed:
bash e2e-full-battery.sh --show-plan

# The E2E battery requires root: it creates Linux users via useradd and
# writes a root-owned daemon log under /var/log, so run it with sudo. The
# harness prints a clear refusal (exit 42) instead of dying silently, and
# any failure names the offending line and command. Verify the harness's
# own diagnostics as a normal user — no root, no side effects:
bash e2e-full-battery.sh --self-test

# Run regression suite (standalone: takes over only daemons nothing else
# manages; a systemd-managed bunkerd is detected and left running, and the
# operator's own CLI config is never read, written or removed)
bash regression-tests.sh
```

### E2E battery inputs

`e2e-full-battery.sh` takes the daemon address and the token from the
environment. Every input is optional, and an explicitly exported value always
wins over the default:

| Variable | Meaning | Default |
|----------|---------|---------|
| `BUNKER_BIN` | CLI binary under test (what the run certifies) | `/usr/local/bin/bunker` |
| `BUNKERD_BIN` | daemon binary the battery starts in coexist mode | `/usr/local/bin/bunkerd` |
| `BUNKER_TOKEN` | auth token for the daemon | `test-regression-token` |
| `BUNKER_DAEMON_URL` | address the battery's `connect` targets | `http://localhost:$REST_PORT` |
| `BUNKERD_REST_ADDR` | REST address: the port input and the port sections 1-13 check | `:18080` standalone, `:28081` coexist |
| `BUNKERD_GRPC_ADDR` | gRPC address: the port input and the port sections 1-13 check | `:19090` standalone, `:29091` coexist |
| `BUNKERD_COEXIST` | non-empty: start the battery's own daemon on its own ports and never sweep production users | unset (standalone: use the host's daemon, take over only daemons nothing manages) |
| `BUNKER_STRICT_BIN` | `1`: a binary certification MISMATCH is fatal before any host mutation | unset (MISMATCH is a loud note) |

`--show-plan` prints the resolved values for all of these without root, and
never prints the token — only whether it came from the environment or the
default, plus a masked fingerprint.

### Who owns the daemon (standalone vs coexist)

Standalone mode never starts, stops, or sweeps a daemon it does not own — the
battery talks to the daemon the host already runs on the production ports
(`:18080`/`:19090` by default):

- **Section 12** runs `regression-tests.sh` with its own throwaway CLI state dir
  and its **own ports** (`:29092`/`:28082`) in both modes, so the nested suite
  can never bind or compete for the ports the battery is testing. The nested
  suite detects a systemd-managed `bunkerd` (`systemctl is-active bunkerd` / the
  unit's `MainPID`) and leaves it alone: it neither stops it nor sweeps its agent
  state (`/run/bunker/*`, `/etc/bunkerd/ssh/*`), and it only stops the daemon
  *it* started. On a host with no systemd daemon (or a container) the historical
  take-over applies.
- **Section 12's verdict is real.** It is the nested suite's own tally
  (`PASS: N / FAIL: M`) plus that suite's exit status: a non-zero exit, a
  non-zero FAIL count, or no tally at all fails the battery. The nested
  transcript is not counted with a grep for the words "PASS"/"FAIL".
- **Sections 13 and 14 probe first.** Both call a cheap reachability probe
  (`bunker list`) before spawning or destroying anything, so an unreachable
  daemon produces ONE actionable cell naming the endpoint and how to bring it
  back (`systemctl restart bunkerd`) instead of cascading into "not found" cells.
- **Coexist mode** (`BUNKERD_COEXIST=1`) is unchanged: the battery starts its own
  daemon on its own ports (`:28081`/`:29091`) and never touches production users.

The production daemon must therefore stay up for the whole standalone run: its
uptime and `NRestarts` should be unchanged afterwards, and its own agents are
only disturbed to the extent the documented standalone take-over already does
(the battery's CLI state and `/root/.bunker` are never touched — see below).

### CLI-state isolation

The battery never reads or writes your CLI config — and neither does the nested
regression suite it runs (both pin `BUNKER_HOME` **and** `HOME` to their own
throwaway dirs, and the nested suite removes only the dir it created itself;
a removal helper refuses any path that is not this harness's own scratch).
Every `bunker` invocation it makes runs with `BUNKER_HOME` **and** `HOME` pointed
at a throwaway state dir under `/tmp` (printed at the start of the run), so
`connect` registers into that dir instead of `~/.bunker/config.yaml`. `HOME` is
relocated as well because a
CLI build older than `BUNKER_HOME` resolves `~/.bunker` only. The operator's
config file is fingerprinted before the first CLI call and re-checked at the end
of the run: if it changed, the run fails loudly instead of reporting green.

### Running against a deployed daemon

Standalone mode talks to the daemon on the production ports and owns nothing:
the host's `bunkerd` must stay up for the whole run (the battery never starts or
stops a daemon in standalone mode), while the nested regression suite in section
12 runs on its own ports (`:29092`/`:28082`) with its own CLI state dir, and
neither the nested suite nor the battery touches the operator's CLI config
(`~/.bunker/config.yaml`). Pass the real token and address explicitly — the
battery does not force the test token when you supply one:

```bash
export PROD_TOKEN='<the daemon auth token from /etc/bunkerd/config.yaml>'
sudo BUNKER_TOKEN="$PROD_TOKEN" \
     BUNKER_DAEMON_URL=http://localhost:18080 \
     BUNKERD_REST_ADDR=:18080 \
     BUNKERD_GRPC_ADDR=:19090 \
     bash e2e-full-battery.sh
```

On a host that already runs the production daemon and you must not disturb it,
use coexist mode with dedicated ports (this is what CI does):

```bash
sudo BUNKERD_COEXIST=1 BUNKERD_REST_ADDR=:28081 BUNKERD_GRPC_ADDR=:29091 \
     bash e2e-full-battery.sh
```

### Deploying a build for the E2E gate

The battery certifies whatever `BUNKER_BIN` resolves to, so a `VERIFY-PASS`
transcript is only meaningful for the build you deployed. Install **both**
binaries from the same build, then confirm the certification reads `MATCH`
instead of a standing `MISMATCH`:

```bash
make build                                  # ./bunker and ./bunkerd from this checkout
sudo install -m 0755 bunker  /usr/local/bin/bunker
sudo install -m 0755 bunkerd /usr/local/bin/bunkerd
sudo systemctl restart bunkerd              # the running daemon must match the install

bash e2e-full-battery.sh --bin-report       # expect: verdict MATCH, exit 0
```

`--bin-report` prints the certification banner (binary path, the commit the
binary reports, repo HEAD, verdict) with no side effects and exits non-zero on
`MISMATCH`, so it doubles as the post-deploy check.

### Quality Gates

- **GitReins Tier 1**: secrets scan, build, lint, tests — enforced on every commit
- **GitReins Tier 2**: LLM-based code review against task criteria
- **Hilo**: dependency graph + blast radius analysis on file changes

## License

Apache 2.0 — see [LICENSE](LICENSE).

Third-party attributions in [NOTICE](NOTICE).

Contributions welcome under the [Contributor License Agreement](CLA.md).

---

Built by the [deployBunker](https://github.com/deployBunker) team. Powered by coding-hermes autonomous foremen.
