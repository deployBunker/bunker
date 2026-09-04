# Bunker — Container-Mode Agent Specification

Version: 0.1.0 (Draft)
Status: Draft — design gate for GAP-063; implementation lands as GAP-064/GAP-065 follow-ups
Last Updated: 2026-09-04

## 0. Overview & Repo Reality

Container-mode is an **opt-in spawn mode** in which the agent workload is a
container run by the agent's *own* existing per-agent rootless dockerd, with the
host home directory bind-mounted into the container and every byte outside that
home staying containerized. The rootless-daemon, socket, port-block, and
SSH-keypair substrate this spec builds on **already exists** — this mode merely
adds one `docker run` on top of it. Everything below is grounded in repo reality
(file:line citations are against the current tree); where a mechanism does **not**
exist yet, this spec says so explicitly instead of assuming it.

### The existing substrate (verified)

| Primitive | Repo reality | Location |
|-----------|--------------|----------|
| Per-agent rootless dockerd | `systemd-run --system --unit=bunker-docker-<id> --uid=<uid> --gid=<gid>` transient unit with `CPUQuota`/`MemoryMax`/`LimitFSIZE`/`TasksMax`/`LimitNOFILE` | `internal/agent/manager_spawn.go` |
| Logical socket | `/run/bunker/<id>/docker.sock` | `manager_spawn.go` |
| Actual rootless socket | `/run/user/<uid>/docker.sock` | `manager_spawn.go` |
| Socket reconciliation | symlink `/run/bunker/<id>/docker.sock` → `/run/user/<uid>/docker.sock`, created by `waitForDockerd` after dockerd is ready | `manager_spawn.go` |
| `DOCKER_HOST` injection | `authorized_keys` `environment=` prefix, `~/.profile`, and ExecAgent's `env(1)` wrapper (`buildAgentExecCommand`) | `manager_spawn.go`, `internal/server/service.go` |
| Docker-over-socket pattern | `docker --host unix://<socket> ps -q` (container counter) | `manager_spawn.go` `countAgentContainers` |
| Port block | default 100 ports/agent from 10000–19999 | `internal/config/config.go`, `internal/resource/portalloc.go` |
| Subuid/subgid | `name:<uid>:65536` | `internal/agent/rootless.go` `configureSubIDs` |
| Exec model | `bunker exec`/`RunAgent`/`cp` ride SSH into the host Linux user | `internal/server/service.go` |
| Tunnels | `bunker tunnel` = `ssh -L 2376:/run/bunker/<id>/docker.sock`, sshfs mount | `manager_spawn.go` |

### Constraints this spec must respect (both stated, not assumed away)

**Constraint A — `SpawnAgentRequest` has no container-mode field.**
`message SpawnAgentRequest` (`proto/bunker/v1/bunker.proto`, lines 106–113)
carries only `agent_id`, `limits`, `network`, `ttl`, `ssh_public_key`, `labels`.
There is no `mode`/`container` field today. Adding one (plus the full-wipe flag
from §2) is follow-up proto work scoped to GAP-064. This spec defines the
*semantics* of such a field only; it does not pretend the field exists.

**Constraint B — there is no restart RPC.** The `Bunkerd` service
(`bunker.proto`) exposes exactly `ServerInfo`, `ServerMetrics`, `SpawnAgent`,
`DestroyAgent`, `ListAgents`, `GetAgent`, `AgentMetrics`, `ExecAgent`,
`RunAgent`, `HeartbeatAgent`, `QueryAudit`; the `Agent` service exposes
`GetInfo`, `Metrics`, `Heartbeat`. No restart RPC exists in the proto or in
`internal/agent/*.go`. Restart in §4 is therefore mapped to **docker
container-recreate primitives**, and any future RPC is explicitly follow-up
work, not an assumption of this design.

### Rootless daemon environment (verified, `rootlessEnv` in `manager_spawn.go`)

```
PATH=~/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
HOME=/home/bunker-<id>
USER=bunker-<id>
XDG_RUNTIME_DIR=/run/bunker/<id>/run
DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns
DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin
DOCKERD_ROOTLESS_ROOTLESSKIT_DETACH_NETNS=false
DOCKER_HOST=unix:///run/bunker/<id>/docker.sock
TMPDIR=/run/bunker/<id>/tmp
```

Notes grounded in the code: rootlesskit's PID namespace (`--pidns`) is
**intentionally disabled** today (code comment: rootlesskit v1.1.1 cannot mix
`--pidns` with `--detach-netns`); the port driver is the rootless **builtin**
userland driver; `DETACH_NETNS=false` is forced for v1.1.1 compatibility. The
container-mode design must inherit all of these as-is — it adds a container on
top; it does not change the daemon.

## 1. PASS(1) — Home Bind-Mount Layout + UID Mapping

### Layout

Today each agent is a real Linux user created with `useradd -m -s /bin/bash
bunker-<id>`, giving host home `/home/bunker-<id>` owned by that user. The host
uid `<uid>` (and gid `<gid>`) is assigned from the system range at useradd time
and is the exact uid the rootless dockerd runs under via `systemd-run
--uid=<uid> --gid=<gid>`.

Container-mode runs the agent workload container with **that same numeric uid**
and bind-mounts **only** the host home into it at the container home path
`/home/<uid>` (container-home constant per the GAP-063 gate):

```bash
docker --host unix:///run/bunker/<id>/docker.sock run -d \
  --name bunker-<id> \
  --user <uid>:<gid> \
  -v /home/bunker-<id>:/home/<uid> \
  <base-image>
```

```
┌───────────────────────── host ─────────────────────────┐
│  Linux user  bunker-<id>   uid=<uid> gid=<gid>          │
│  rootless dockerd  ── (systemd-run --uid=<uid>)         │
│  home  /home/bunker-<id>  ───┐                          │
└──────────────────────────────┼──────────────────────────┘
                               │ bind mount  -v ...
                               ▼
┌───────────────────────── container ────────────────────┐
│  bunker-<id>  runs with  --user <uid>:<gid>             │
│  /home/<uid>  ◄── receives the host home                │
│  everything else (/, /etc, /usr, ...) stays image-only  │
└─────────────────────────────────────────────────────────┘
```

### UID maps 1:1 — no userns remap needed

The rootless dockerd already runs as host uid `<uid>`: its socket, its cgroup
slice, and its whole ownership world *are* that host user. Because the
container is launched with `--user <uid>:<gid>` **matching the same numeric
uid**, writes a workload makes under the bind-mounted home are owned 1:1 by the
real host user — the same uid on both sides of the mount, so no chown dance, no
idmap layer, and no second namespace indirection is required for the home
bind-mount to be readable/writable by both the workload and the host user.

> ⚠️ Subtlety, flagged (see §6 risk 1): rootlesskit translates container uids
> through the user-namespace mapping (`configureSubIDs` writes
> `bunker-<id>:<uid>:65536`), so a *literal* `--user <uid>` under rootless
> Docker is not byte-identical to a host `chown <uid>` in the general case —
> rootless Docker maps container uid 0 to the dockerd host uid and lays the
> rest of the 65536 subuid range elsewhere. Whether the literal `--user <uid>`
> lands on the correct translated uid for the bind-mounted home is precisely
> what GAP-065 must prove with a live E2E; this spec records it as an open risk
> rather than papering over it.

Everything outside the bind-mounted home stays containerized: the image,
`/etc`, `/usr`, shared libraries, and any host paths are not reachable unless
individually published per §3.

## 2. PASS(2) — Storage Persistence Matrix

**Stated default:** the host home bind-mount **survives destroy** — it is
host-backed real storage (the agent's fixed workspace), consistent with today's
host-user agents whose files live under `/home/bunker-<id>`. The container
writable layer and the container itself are **ephemeral** and are removed on
destroy. The **only opt-in** is a per-spawn full-wipe flag that additionally
clears the host home.

### Default persistence matrix (DEFAULT mode: home bind-mounted)

| Storage object | On destroy | On daemon restart | On re-spawn (same agent_id) |
|----------------|------------|-------------------|-----------------------------|
| Host home bind-mount `/home/bunker-<id>` | **PERSISTS** *(1)* | **PERSISTS** | **PERSISTS** — re-mounted into the new container |
| Container writable layer | **EPHEMERAL** — removed with the container | **EPHEMERAL** — container gone *(2)* | **EPHEMERAL** — fresh layer |
| Container `bunker-<id>` | **EPHEMERAL** — stopped + removed | **EPHEMERAL** — orphan stopped by dockerd *(2)* | **EPHEMERAL** — recreated |
| Images (agent base image + pulled) | destroyed with the daemon/user *(3)* | persist (daemon data dir under the surviving home) | re-pulled / re-provisioned on spawn |
| Named volumes | **EPHEMERAL** — pruned on destroy *(4)* | persist with daemon data | new on re-spawn |
| `/run/bunker/<id>` runtime dir (socket, tmp, env) | wiped on destroy (existing cleanup) | recreated on spawn | recreated on spawn |

*(1) Requires a container-mode destroy deviation: today's destroy runs
`userdel -rf bunker-<id>` which deletes the home. Container-mode destroy must
preserve `/home/bunker-<id>` (e.g. `userdel` without `-r` plus a documented
home re-ownership/re-uid policy) — flagged as §6 risk 6.*

*(2) Mirrors the existing behavior recorded at the end of
[agent-lifecycle.md](agent-lifecycle.md#zombie-process-reaping): orphaned
containers are stopped by dockerd on daemon restart — so the container is gone
while the host home stays intact.*

*(3) Rootless Docker data lives under the agent's home; because the daemon is
per-agent and destroyed with the user, images do not outlive destroy in the
default flow.*

*(4) Named volumes are opt-in usage by workloads; container-mode destroy adds a
`docker volume prune`-style sweep of that agent's volumes so none leak (see §4).*

### Opt-in: full-wipe flag

The single opt-in is a per-spawn full-wipe flag (server config + follow-up
proto field, GAP-064 scope). When set:

| Storage object | Effect of full-wipe on destroy |
|----------------|-------------------------------|
| Host home bind-mount `/home/bunker-<id>` | **DELETED** — `userdel -r` equivalent (the bind source is cleared) |
| Container + writable layer + volumes | **DELETED** — normal ephemeral teardown |
| Images | **DELETED** — daemon data removed with the user |
| `/run/bunker/<id>` runtime dir | **DELETED** — existing cleanup |

Under the full-wipe flag the spawn `docker run` **omits the `-v` bind-mount**
(§4) — there is deliberately no persistent home to carry across a destroy; the
home lives inside the container layer and dies with it.

### Behavior across daemon restarts (stated)

- **Default mode:** daemon restart ⇒ container gone (orphan-stopped), host home
  intact and re-mounted on the next spawn of the same agent_id.
- **Full-wipe mode:** daemon restart ⇒ container gone; nothing persists by
  design.

This storage matrix is the groundwork GAP-064's persistence work depends on.

## 3. PASS(3) — Networking / Port-Forwarding with Client-Invariance

### Publishing onto the existing per-agent port block

Container-mode publishes ports from the container onto the agent's **existing
allocated host port block** using the rootless **builtin** userland port driver
(`DOCKERD_ROOTLESS_ROOTLESSKIT_PORT_DRIVER=builtin`, already set in
`rootlessEnv`). Ports are allocated by `PortAllocator.Allocate` — default block
size **100** ports from `port_range_start=10000` to `port_range_end=19999`
(`internal/config/config.go`, `config.example.yaml`).

```bash
docker --host unix:///run/bunker/<id>/docker.sock run -d \
  --name bunker-<id> --user <uid>:<gid> \
  -v /home/bunker-<id>:/home/<uid> \
  -p 127.0.0.1:10042:8080 \        # <ext>=10042 is INSIDE the agent's 100-port block
  <base-image>
```

`<ext>` must be chosen from the agent's allocated block
(`port_range_start`…`port_range_end`, 100 wide); the container's internal port
(here 8080) is arbitrary. The rootless builtin driver translates the published
port through rootlesskit's userland networking (slirp4netns), so the port is
reachable from the host and, via the host's existing ingress paths, from
outside it.

### Network-mode mapping

| Network mode (existing `NetworkConfig.mode`) | Container-mode publish behavior |
|----------------------------------------------|--------------------------------|
| `DIRECT` | `-p 127.0.0.1:<ext>:<containerPort>` per §3; external reachability depends on the host's direct-port exposure (see §6 risk 4) |
| `CLOUDFLARE_TUNNEL` | unchanged — cloudflared proxies to the published host port in the agent block; no client delta |
| `TAILSCALE` | unchanged — tailnet routes to the published host port; no client delta |

### Client-invariance argument

The client-visible contract is **byte-for-byte unchanged** in container mode:

- The logical socket is still `/run/bunker/<id>/docker.sock` (symlinked to
  `/run/user/<uid>/docker.sock`), injected through the same three channels as
  today: the `authorized_keys` `environment="DOCKER_HOST=unix://..."` prefix,
  `~/.profile`, and ExecAgent's `env(1)` wrapper.
- `bunker spawn`/`list`/`info` responses, `docker_host_ssh`
  (`DOCKER_HOST=ssh://bunker-<id>@host`), `docker_host_tunnel` (`ssh -L
  2376:/run/bunker/<id>/docker.sock`), and `sshfs_mount` are all unchanged — a
  client pointing any docker CLI at the socket, at `ssh://`, or through `bunker
  tunnel` sees an ordinary Docker engine either way.
- The **only** delta is *where the workload lives*: published ports now live on
  the container, not as host processes of the agent user directly. `bunker
  exec` / `cp` / `run` / `tunnel` remain identical because they ride the SSH
  exec model and the socket contract, not the container's internals.

### Exec parity decision (the one real delta — decided here, not hand-waved)

Today `ExecAgent`/`RunAgent`/`cp` SSH into a real Linux user running sshd
(`buildAgentExecCommand` sets `DOCKER_HOST` and `TMPDIR` and runs the user
command). In container mode the workload lives inside `bunker-<id>` the
container, so exec parity must be chosen explicitly:

- **(i) sshd-in-container**: the base image runs an sshd carrying the same
  `authorized_keys`; clients keep SSHing "into the agent" and land inside the
  container.
- **(ii) server-side docker exec (RECOMMENDED)**: bunkerd routes exec to
  `docker --host unix:///run/bunker/<id>/docker.sock exec -it bunker-<id> ...`
  — the same docker-over-this-socket pattern already proven by
  `countAgentContainers` (`docker --host unix://<socket> ps -q`).

**Recommendation and justification: (ii) as the default.** It adds no
sshd-in-container attack surface, reuses a pattern already exercised against
this exact socket, and keeps the raw-docker-CLI and `bunker tunnel` contracts
invariant — the client never knows whether the exec landed via sshd or the
socket. Option (i) is recorded as the documented alternative for workloads that
need a real interactive SSH login inside the container. The choice is a
follow-up implementation decision (GAP-064/065) that must not leak into the
client contract; see §6 risk 2.

## 4. PASS(4) — Lifecycle Mapping to Docker Primitives

### Primitive mapping

| Lifecycle event | Docker primitive (container-mode) | Notes |
|-----------------|-----------------------------------|-------|
| **spawn** | `docker --host unix:///run/bunker/<id>/docker.sock run -d --name bunker-<id> --user <uid>:<gid> -v /home/bunker-<id>:/home/<uid> [-p ...] <base-image>` | `-v` omitted under the full-wipe flag; runs only after the existing user-agent bootstrap reaches dockerd-ready |
| **restart** | `docker rm -f bunker-<id>` + `docker run -d` recreate with the SAME image/bind-mount/publish flags | **No restart RPC exists in the proto today** (Constraint B) — recreate is the design for the follow-up, not an existing hook |
| **destroy** | `docker stop bunker-<id>` + `docker rm bunker-<id>`, then the existing destroy teardown | New step inserted **before** dockerd stop (below); `docker rm -f` when `force=true` |
| **destroy --force** | `docker rm -f bunker-<id>` (no graceful stop) | plus the same teardown order |

### Spawn (what changes vs. the existing 13-step flow)

Container-mode reuses the existing user-agent bootstrap unchanged through
`waitForDockerd` (per [agent-lifecycle.md](agent-lifecycle.md#spawn-step-by-step)
steps 1–9: useradd, subuid/subgid, installRootlessDocker, AppArmor ensure,
systemd-run unit `bunker-docker-<id>`, socket symlink) and then adds exactly one
step: `docker run -d` of the agent container as in the table above (image
provisioning is GAP-065 scope). Tracker record and status transitions stay the
same (`pending → starting → running`), and `SpawnAgentResponse`'s surface is
unchanged (optionally extended by follow-up proto for the container-mode flag).
The existing `maxDockerContainers` gate — `countAgentContainers` over the
socket — naturally counts the container-mode container too.

### Restart (recreate, not a host process restart)

Restart maps to **container recreate**: `docker rm -f bunker-<id>` followed by
`docker run -d` with identical image/bind-mount/publish flags. This mirrors the
existing stopping→running state machine at the container layer: the container
layer resets to image state while the host home persists per the §2 DEFAULT
policy. There is **no restart RPC** in `bunker.proto` or `internal/agent/*.go`
(Constraint B) — this mapping is the design GAP-064 may later expose behind a
new RPC or hook.

### Destroy (order, with the new container step)

Container-mode destroy keeps today's teardown order
(`internal/agent/manager_destroy.go`) and inserts **one new step before the
dockerd stop** so no container leaks past the daemon:

```
1.  NEW  docker --host unix:///run/bunker/<id>/docker.sock stop  bunker-<id>
          docker --host unix:///run/bunker/<id>/docker.sock rm    bunker-<id>
          (docker rm -f when force=true); prune that agent's volumes
2.  removeUserSliceLimits
3.  stopDockerdDirect / systemctl stop + disable bunker-docker-<id>
4.  userdel -rf bunker-<id>            (DEFAULT mode: preserve home — see §6 risk 6)
5.  rm -rf /run/bunker/<id>/
6.  rm /run/user/<uid>/docker.sock
7.  rm persisted SSH key (/etc/bunkerd/ssh/<id>)
8.  tunnel stop (cloudflared) + tailscale stop
9.  tracker.Unregister
10. portAlloc.Free
```

### Daemon-restart orphan handling

On a host/bunkerd restart the rootless dockerd restarts and stops orphaned
containers — the existing behavior recorded at the end of
[agent-lifecycle.md](agent-lifecycle.md#zombie-process-reaping) ("Orphaned
containers stopped by dockerd on daemon restart"). For container-mode that
behavior must be extended: the **orphan sweep must also clean up
container-mode images and volumes** that belong to dead container agents, since
no container-mode-aware sweep exists today (see §6 risk 3).

```
┌──────────┐   useradd → dockerd-ready → docker run -d   ┌──────────┐
│ pending  │ ──────────────────────────────────────────► │ starting │
└──────────┘                                            └────┬─────┘
                                                             │ socket ready + container running
                                                             ▼
                                                        ┌──────────┐
  destroy ─── docker stop/rm ──► dockerd stop ──► user  │ running  │ ◄── restart = docker rm -f
  (TTL / RPC / force)                                   └──────────┘      + docker run -d recreate
                                                             │
                                                             ▼
                                                        ┌──────────┐
                                                        │ stopped  │
                                                        └──────────┘
```

## 5. PASS(5) — Security Exclusions

Container-mode explicitly **excludes** all of the following — none of these are
part of the design:

- ❌ **No docker socket mounted inside the agent container** — neither
  `/run/bunker/<id>/docker.sock` nor `/run/user/<uid>/docker.sock` is ever
  bind-mounted into the workload. The container cannot reach its own or any
  other daemon's socket.
- ❌ **No `--privileged`** — the container is never granted privileged mode.
- ❌ **No host PID namespace sharing** — no `--pid=host`; the rootless daemon's
  `--pidns` isolation remains **disabled today** (code comment, rootlesskit
  v1.1.1) and must not be reintroduced by this mode.
- ❌ **No host network namespace sharing** — no `--network=host`; the workload
  sits behind rootlesskit userland networking and publishes only via §3
  `-p` on its own port block.
- ❌ **No host IPC namespace sharing** — no `--ipc=host`.
- ❌ **No host bind-mounts beyond the home dir** — `-v /home/bunker-<id>:/home/<uid>`
  is the only bind-mount; no `/etc`, `/var/run`, `/run/user`, or device mounts.
- ❌ **No container drift of daemon security posture** — the container inherits
  the daemon's exclusions; there is no separate base-image/USER/EXPOSE/VOLUME/ENV
  policy drift. Images are provisioned by bunkerd policy (image allowlist/supply
  is §6 risk 5), not chosen ad hoc by clients.
- ❌ **No root inside the container** — the container runs `--user <uid>:<gid>`,
  never as uid 0 inside, and only the configured 65536 subuid/subgid mapping
  (`name:<uid>:65536`) exists for the user.
- ✅ **Reachability is daemon-scoped** — the container is reachable only through
  its own per-agent rootless engine socket; there is no shared docker socket and
  no cross-agent container visibility.

### Resource limits mapping (from existing limits to `docker run`)

The per-agent limits enforced today on the dockerd unit (CPUQuota / MemoryMax /
TasksMax / LimitNOFILE — see [agent-lifecycle.md §5 Resource Limits
Enforcement](agent-lifecycle.md#5-resource-limits-enforcement)) are additionally
mirrored onto the container at `docker run` time so the workload itself is
capped even before the cgroup slice applies:

| systemd property (existing) | `docker run` flag | Notes |
|-----------------------------|-------------------|-------|
| `CPUQuota` (percent) | `--cpus` (cores = quota% / 100) | e.g. CPUQuota=200% ⇒ `--cpus=2` |
| `MemoryMax` (bytes) | `--memory` | byte-for-byte |
| `TasksMax` | `--pids-limit` | process cap inside the container |
| `LimitNOFILE` | `--ulimit nofile=<n>:<n>` | fd cap |
| `LimitFSIZE` | `--ulimit fsize=<bytes>` | per-file size cap (disk quota proxy) |

## 6. PASS(6) — Open Risks

1. **Rootless uid-mapping correctness for the bind-mounted home (highest).**
   Whether a literal `--user <uid>:<gid>` under rootless Docker lands on the
   translated uid that owns the bind-mounted home is not proven — rootless
   Docker maps container uid 0 to the dockerd host uid and lays the 65536
   subuid range elsewhere. GAP-065 must prove ownership/write parity with a
   live E2E before this design is locked. (Cross-ref §1.)
2. **Exec parity decision.** sshd-in-container vs server-side docker exec
   (option (i) vs (ii) in §3) changes host-user tooling assumptions
   (e.g. `bunker env set`, detached `RunAgent` units, sshfs of `~/.bunker`).
   The choice must be made and tested by GAP-064/065 without leaking into the
   client contract.
3. **No container-mode orphan sweep exists.** Today's Destroy handles only the
   dockerd unit + Linux user; containers/images/volumes of dead container-mode
   agents after a daemon restart are uncovered. The §4 orphan-handling note
   requires new sweep logic (GAP-065 scope).
4. **Rootless builtin port-driver reachability.** The builtin userland driver
   binds published ports to localhost by default, so external clients of
   `DIRECT`-mode published ports may need `ssh -L`/tunnel ingress; the exact
   bind address for `-p` is an implementation decision with an availability
   tradeoff.
5. **Image pull policy / supply chain.** There is no base-image allowlist,
   pinned digest policy, or pull-time verification today; container-mode makes
   image supply a first-class security control that must be defined (image
   registry, digest pinning, pull policy) before general availability.
6. **Persistence vs. today's `userdel -rf`.** The §2 DEFAULT (home survives
   destroy) conflicts with the existing destroy step `userdel -rf bunker-<id>`,
   which deletes the home. Container-mode destroy must deviate (preserve home;
   define uid reuse/ownership policy on re-spawn of the same agent_id) — until
   then the matrix's destroy column is aspirational.
7. **TTL reaper is container-mode-unaware.** The TTL monitor calls the existing
   Destroy path; until the follow-up lands, expiring a container-mode agent
   would tear down the daemon/user without the new container stop/rm step.

## 7. References

- [agent-lifecycle.md](agent-lifecycle.md) — spawn steps 1–13, destroy steps
  1–7, resource limits (§5), zombie/orphan behavior (end of doc)
- [api.md](api.md) — SpawnAgent / DestroyAgent / ExecAgent / RunAgent surfaces
- [architecture.md](architecture.md) — directory layout, networking, per-agent
  port isolation
- `proto/bunker/v1/bunker.proto` — `SpawnAgentRequest` (lines 106–113),
  service RPC lists (lines 11–35)
- `internal/agent/manager_spawn.go` — authorized_keys env prefix, systemd-run
  args, `rootlessEnv`, `waitForDockerd` symlink, `countAgentContainers`
- `internal/agent/manager_destroy.go` — teardown order
- `internal/agent/rootless.go` — `configureSubIDs` (`name:<uid>:65536`)
- `internal/resource/portalloc.go` — `Allocate`/`Free`, 100-port blocks
- `internal/config/config.go` + `config.example.yaml` — port defaults
  10000–19999, 100/agent
