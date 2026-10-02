# Bunker — Network Isolation Mode Surface Specification

Version: 0.1.0 (Draft)
Status: Draft — design authority for NET-BUNKER-001..012; nothing in this spec is implemented yet
Last Updated: 2026-10-02
Related: specs/container-mode.md, specs/agent-tmp-isolation.md (GAP-075), specs/containment-disclosure.md (GAP-067), specs/architecture.md

## 0. Overview & Repo Reality

Bunker's tenants are real Linux users (`useradd -m -s /bin/bash bunker-<id>`,
`internal/agent/manager_spawn.go:248`) that share one host, one host network
namespace, one `/proc`, and one visible `/run/bunker/`. The filesystem half of
that sharing is already fixed: GAP-075 gives every agent a private `/tmp`
(pam_namespace per SSH session, `PrivateTmp=yes` per transient unit —
[agent-tmp-isolation.md](agent-tmp-isolation.md)). The **network** half is not
fixed: two agents on one host share `127.0.0.1`, the global port space, the
process table, and the runtime-directory listing. This spec defines the
**option surface** that fixes it: a named set of network-isolation modes, an
orthogonal firewall layer, a process/path visibility dimension, a declared
default, a compatibility matrix, and a reporting law. It is deliberately a
*surface*, not a single design — the owner's directive is that the system
support every isolation option so an operator can trade performance for
boundary width per agent, and flip that setting when one option is
compromised.

Everything below is grounded in the current tree; where a mechanism does **not**
exist, this spec says so explicitly instead of assuming it.

### The existing substrate (verified, file:line against this tree)

| Primitive | Repo reality | Location |
|-----------|--------------|----------|
| Agents are real Linux users | `useradd -m -s /bin/bash bunker-<id>` at spawn | `internal/agent/manager_spawn.go:248` |
| Per-agent rootless dockerd | `systemd-run --system --unit=bunker-docker-<id> --uid=<uid> --gid=<gid>` transient unit, properties `PAMName=login` + `PrivateTmp=yes` + preset knobs | `internal/agent/isolation.go:981-1033` (`buildRootlessDockerdArgs`), executed at `manager_spawn.go:564` |
| Logical socket | `/run/bunker/<id>/docker.sock` (`spawnRunRoot = /run/bunker`, `manager_spawn.go:39`) | `manager_spawn.go:334,399` |
| Actual rootless socket | `/run/user/<uid>/docker.sock` | `manager_spawn.go:507` |
| Socket reconciliation | symlink logical → actual, created by `waitForDockerd` once dockerd is up | `manager_spawn.go:1050-1056` |
| Socket-directory permissions | `/run/bunker/<id>` created **0755** then `chown`ed to the agent — the directory mode is NOT 0700 today (see §6) | `manager_spawn.go:406-413` |
| Userspace networking already installed | slirp4netns is a **required** prerequisite of the rootless runtime (`Required: true`, probed, packaged or downloaded per distro family) | `internal/agent/rootless_prereqs.go:5-7,216-231` |
| rootlesskit already used, with AppArmor | `ensureRootlesskitAppArmor` writes/loads an AppArmor profile for the agent's rootlesskit binary | `internal/agent/rootless.go:1713-1754` |
| rootlesskit net driver pinned | `DOCKERD_ROOTLESS_ROOTLESSKIT_NET=slirp4netns`, `..._PORT_DRIVER=builtin`, `..._DETACH_NETNS=false` | `internal/agent/isolation.go:990-996` |
| Agents already run under systemd units | the dockerd unit above, plus every detached `RunAgent` unit (`systemd-run` in `internal/agent/run.go:70`) | `manager_spawn.go:564`, `internal/agent/run.go:70` |
| Port block | default 100 ports/agent from `port_range_start=10000` to `port_range_end=19999` (bookkeeping only — see §2) | `internal/config/config.go:477-479,991-993`; `internal/resource/portalloc.go:13-58,62` |
| Ingress modes that exist today | `NetworkConfig.mode` = `MODE_CLOUDFLARE_TUNNEL` / `MODE_TAILSCALE` / `MODE_DIRECT` + `trycloudflare`/`domain` | `proto/bunker/v1/bunker.proto:90-102` |
| What `network` actually does today | exactly two daemon-side consumers: TryCloudflare tunnel start (`tunnelMgr.Start`) and Tailscale start (`tailscaleMgr.Start`); `MODE_DIRECT` and `MODE_CLOUDFLARE_TUNNEL` are set by the CLI (`internal/cli/spawn.go:182-194`) but have **no daemon-side consumer** | `internal/agent/manager_spawn.go:792-805` |
| Exec path | `bunker exec`/`run`/`cp` ride SSH into the host user; `buildAgentExecCommand` injects `DOCKER_HOST=unix:///run/bunker/<id>/docker.sock` and `TMPDIR=/tmp` via `env(1)` | `internal/server/service.go:1301-1337` |
| Docker tunnel contract | `ssh -L 2376:/run/bunker/<id>/docker.sock` | `manager_spawn.go:686-690` |
| Subids | per-agent 65536-wide subordinate-ID block from a global pool | `internal/agent/subid_alloc.go:34-45`, `internal/agent/rootless.go:547-565` |
| Disclosure hook that already exists | `BUNKER_SANDBOX=1` + probe marker, config-gated (GAP-067) | specs/containment-disclosure.md; `internal/config/config.go` `containment.disclosure` |
| Capability reporting precedent | `ServerInfoResponse.tmp_isolation` / `tmp_isolation_detail` — the daemon already reports an enforced boundary with a reason string in proto | `proto/bunker/v1/bunker.proto:116-126` |

### What this spec must answer (and where each answer lives)

1. **The mode taxonomy** — §1.
2. **The per-mode contract** (what each mode does and does not protect,
   dependencies, privilege, port space) — §2.
3. **The compatibility matrix** (what breaks under each mode) — §3.
4. **The declared default** (`shared`, unchanged, until measured) — §5.1.
5. **The reporting law** (announce the mode and the boundary actually
   provided; refuse loudly instead of silent fallback) — §5.2.

### What does NOT exist yet (stated, not assumed)

- **No isolation-mode field anywhere.** `SpawnAgentRequest`
  (`proto/bunker/v1/bunker.proto:215-231`) carries `agent_id`, `limits`,
  `network`, `ttl`, `ssh_public_key`, `labels`, `image_spec`,
  `return_ssh_private_key`, `safety_preset`, `mount_driver`. There is no
  `mode`/`isolation`/`netns` field, and `NetworkConfig` (`:90-102`) is ingress
  selection, not tenant isolation. Adding the field is proto follow-up work
  (NET-BUNKER-010); this spec defines its semantics, it does not pretend the
  field exists.
- **No `PrivateNetwork=`**, no `ip netns`/veth wiring, no per-UID firewall
  rules, no `hidepid`, no pasta/passt reference, and no `SO_PEERCRED` check
  anywhere in `internal/` or `cmd/` (grep-verified: zero hits for each term
  outside this spec).
- **No namespace-aware port accounting.** `PortAllocator` divides one global
  `[start, end]` into fixed sub-ranges (`internal/resource/portalloc.go:13-58`)
  — a bookkeeping convention with no kernel enforcement behind it (§2).

### Constraints this spec must respect (true today, blocking the design)

**Constraint A — the daemon and ingress live in the host namespace.** The
daemon's REST/gRPC listeners are plain addresses (defaults `:8080`/`:9090`,
`internal/config/config.go:970-971`; example `config.example.yaml:12-13`), the
fleet deployment publishes them on `:18080`/`:19090` (README.md:173), and agent
ingress — TryCloudflare (`manager_spawn.go:792-793`) and Tailscale
(`:804-805`) — is started by the daemon on the **host** and forwards to
host-visible agent ports. Any per-agent network namespace changes where those
ports live (§4).

**Constraint B — unix sockets are the only agent-control channel that
survives namespaces for free.** The whole agent surface (docker socket,
`bunker env set` file at `/run/bunker/<id>/env`, exec via SSH) rides the
filesystem, not the network (`service.go:1315-1337`, `manager_spawn.go:399`).
This is the property that makes every mode in §1 affordable, and the socket
half has its own hardening section (§6).

**Constraint C — agents are unprivileged users.** No agent can create a
network namespace, mount anything, or load firewall rules. Every isolation
mechanism is provisioned by the root daemon at spawn/destroy, exactly like the
GAP-075 PAM/tmp machinery ([agent-tmp-isolation.md](agent-tmp-isolation.md)).

**Constraint D — the evidence base is demonstrated, not hypothetical.** The
leaks this surface answers are filed, reproduced rows in
`.coding-hermes/board/tasks.jsonl` (board data is unversioned in this worktree,
so the rows are cited by id, not by file:line):

- **SEC-BUNKER-001** — the daemon's control plane is published to the open
  internet: wildcard binds (`:9090`/`:8080` shape; the deployed daemon serves
  `:19090`/`:18080`, README.md:173) with ufw allowing them from Anywhere;
  `GET /healthz` returned 200 unauthenticated. The standby fleet instead binds
  the tailnet address. (Network-isolation modes do not replace fixing the
  listener bind — that row stays open on its own — but the mode surface must
  not make it worse.)
- **SEC-BUNKER-002** — agents share one network namespace: `127.0.0.1` of
  agent A is reachable by agent B, and ports above 1024 are not uid-scoped, so
  the per-agent 10000-19999 block is **bookkeeping, not enforcement**: agent B
  can `bind()` a port inside A's block and squat it or impersonate A's service.
- **ISO-001** — reproduced live (`dogfood-2026-10-02-iso`): agent A ran `ps`
  and saw agent B's rootlesskit/containerd processes **with full command lines
  including B's username** — `/proc` is global on a shared host.
- **ISO-002** — reproduced live (same run): agent A listed `/run/bunker/` and
  saw 23 entries — every other agent's runtime directory and docker.sock path.

---

## 1. The Mode Taxonomy

Seven named modes plus one orthogonal layer plus one visibility dimension.
Every mode composes with the GAP-075 filesystem boundary (private `/tmp`,
bounded scratch) and with container-mode's execution redesign; none replaces
them.

### 1.1 `shared` — today's default (unchanged)

The agent runs in the host network namespace with no isolation beyond uid
separation of files. **Protects against:** nothing on the network axis — not
internet exposure of agent-published ports (SEC-BUNKER-001 class), not
co-tenants (SEC-BUNKER-002: shared loopback, unenforced port block). What it
does provide is everything that is not network: uid-owned homes, GAP-075
private `/tmp`, cgroup limits, and the per-agent rootless dockerd whose socket
is the only agent-control channel. **Does not protect against:** everything in
§2's threat list. **New dependency:** none. **Privilege:** none beyond today's.
**Port space:** the global allocator as today.

### 1.2 `systemd` — `PrivateNetwork=` on the agent's existing unit

The dockerd transient unit already exists (`manager_spawn.go:564`); this mode
adds `--property=PrivateNetwork=yes` to its argv builder
(`isolation.go:1004-1027`) and to detached `RunAgent` units
(`internal/agent/run.go:70`). The unit gets its own network namespace
containing only loopback. **Key property (load-bearing):** unix sockets are
filesystem objects and cross network namespaces — `/run/bunker/<id>/docker.sock`
keeps working while the network goes private, so exec, `docker tunnel`, and
sshfs are untouched. **Cost:** the agent loses ALL networking — inbound and
outbound; there is no NAT, no DNS, nothing. This is the cheapest real
boundary: no new dependency (systemd is already load-bearing), no new code
path beyond the property. It is the right mode for agents whose work is
compute/file-only, and the honest fallback when slirp4netns-class options are
compromised.

### 1.3 `rootlesskit` — per-session netns via rootlesskit + slirp4netns

The substrate's own userspace-NAT pair, already installed and pinned
(`rootless_prereqs.go:216-231`, `isolation.go:990`): the agent's workload gets
its own network namespace with user-mode NAT for outbound (slirp4netns) — DNS
and outbound work, inbound exists only through explicit port publishing through
the rootlesskit builtin port driver (`isolation.go:991`). This is the mode the
rootless dockerd's own containers already live behind; extending it to the
agent session is reuse, not new machinery. **New dependency:** none (already
required). **Privilege:** none (user-mode).

### 1.4 `pasta` — the same role via pasta/passt

pasta (passt) fills rootlesskit's role — per-session namespace with user-mode
networking — with a different mechanism (no persistent slirp daemon; pasta
connects the namespace to the host's own interface addresses). It is a separate
mode because it has **its own dependency** (pasta is not in the prerequisite
table today — `rootless_prereqs.go` has zero pasta entries; grep-verified), its
own failure mode, and its own performance envelope. It exists so the surface
does not have a single point of failure at the userspace-NAT layer: if the
slirp4netns path is compromised or broken, `pasta` is the substitute an
operator flips to, accepting the performance difference.

### 1.5 `netns-veth` — explicit `ip netns` + veth/bridge

The kernel-routing option: the daemon creates a named network namespace per
agent and wires it to a bridge with veth pairs, giving per-tenant **routing**
and per-tenant **firewalling** (nftables/iptables rules scoped to the tenant's
namespace — the only mode where co-tenant traffic can be policed at L3 per
tenant rather than globally). It needs **privilege** (CAP_NET_ADMIN for the
daemon: namespace creation, veth, bridge) — a real posture change for bunkerd,
declared here rather than smuggled in. **Residue law (mandatory):** namespace
creation without symmetric teardown leaks kernel state per agent; destroy MUST
remove the agent's veth, bridge-port membership, namespace, and any
namespace-scoped rules, and the destroy path must VERIFY the residue is gone
(the QA-BUNKER-4 class of leak — below — is exactly what happens when destroy
skip-on-failure is allowed). **New dependency:** `iproute2` (near-universal).
**Privilege:** yes, as above.

### 1.6 `container` — container-per-agent

Full container-per-agent is the execution redesign **container-mode.md already
drafts**: the workload container runs under the agent's own rootless dockerd
behind rootlesskit userland networking, with its own network namespace
(container-mode.md §5 explicitly excludes `--network=host`), home bind-mounted.
This spec does NOT re-specify it — for everything container-shaped
(spawn/destroy mapping, exec parity, exclusions, risks) the authority is
[container-mode.md](container-mode.md); this surface merely names it as the
widest-boundary mode and requires its port publishing (container-mode.md §3)
to compose with §4 here.

### 1.7 Orthogonal layer (not a mode): per-UID firewall enforcement

`iptables -m owner --uid-owner <agent-uid>` rules on the **host** namespace
compose with ANY mode, including `shared`. They close cross-tenant
**reachability**: agent B's packets to A's service can be dropped by uid match
even though B can still name A's addresses. **Honest limit (stated verbatim in
substance): a firewall is not a namespace.** Owner-match rules decide packets
AFTER a socket exists; the kernel still permits agent B to `bind()` port 10042
inside A's allocated block, so BIND-SQUATTING (SEC-BUNKER-002's second half)
is NOT stopped by this layer — only a namespace (per-agent port space) stops
it. This layer is also host-global ordering state (rule placement relative to
ACCEPT rules matters), which is a new class of fragility for the daemon to own;
it must be provisioned and torn down with the same residue discipline as
§1.5. (Implementation row: NET-BUNKER-006.)

### 1.8 The visibility dimension (processes and paths — ISO-001/ISO-002)

Orthogonal to networking, two leaks were demonstrated live and need their own
toggles:

- **Process visibility (ISO-001).** `/proc` is global: agent A sees agent B's
  rootlesskit/containerd command lines, including B's username. Options:
  - `hidepid=2` on the host's `/proc` mount (+ `proc` group for the daemon) —
    cheap, host-wide, works on today's `shared` mode; but it is a **HOST
    setting**: it cannot be per-agent, it affects every non-agent process
    view on the host, and it needs a remount (an operator/host-provision
    decision, like GAP-075's PAM work).
  - a per-agent **PID namespace** — per-tenant, no host-wide side effect, but
    heavier (every agent process tree is namespaced; process supervision,
    signal semantics, and the destroy reaper must become namespace-aware).
    (Implementation row: NET-BUNKER-011.)
- **Path visibility (ISO-002).** `/run/bunker/` enumerates the tenant set.
  Options:
  - **restricted `/run/bunker`** — root:0700 on the parent and per-agent 0700
    children: any agent loses enumerate AND traverse rights on peers'
    directories (note: children are already agent-owned but the parent is
    created 0755, `manager_spawn.go:406` — see §6); the daemon does its
    socket/symlink work as root so it does not need to traverse as the agent.
  - a **per-agent mount namespace** over `/run/bunker` (each agent bind-mounts
    only its own directory at that path) — stronger (the peer set is not even
    nameable) but adds mount-namespace lifecycle to every session type
    (sshd sessions would need pam_namespace-style plumbing, exactly the
    GAP-075 mechanism, pointed at `/run/bunker` instead of `/tmp`).
    (Implementation row: NET-BUNKER-012.)

---

## 2. Threat Model and the Per-Mode Contract

Threats, concretely (all demonstrated or demonstrated-adjacent — §0):

- **T1 internet** — an internet host reaching agent-published ports or the
  control plane (SEC-BUNKER-001).
- **T2 co-tenant reach** — agent B connecting to agent A's service on the
  shared loopback or host network (SEC-BUNKER-002).
- **T3 bind-squat / impersonation** — agent B `bind()`ing a port inside A's
  10000-19999 block: A's service fails to start, or B answers as A
  (SEC-BUNKER-002; the block is bookkeeping only — `PortAllocator` hands out
  ranges, `portalloc.go:62`, but nothing stops another uid from binding them).
- **T4 process identity leak** — agent A enumerating B's processes/command
  lines via global `/proc` (ISO-001).
- **T5 tenant-set enumeration** — agent A listing `/run/bunker/` (ISO-002).

Per-mode contract. "Port space" states what happens to the 10000-19999 block;
"deps/priv" = new dependency / privilege needed.

| Mode | Protects against | Explicitly does NOT protect against | Deps / Priv | Port space |
|------|------------------|-------------------------------------|-------------|------------|
| `shared` (default) | nothing network-wise; keeps non-network guarantees (uid files, GAP-075 `/tmp`, cgroups) | T1, T2, T3, T4, T5 — all live today | none / none | global allocator, unenforced |
| `systemd` (`PrivateNetwork=yes`) | T2, T3 (own netns ⇒ own loopback + own port space); most of T1 for agent-published ports | outbound-exfil is not *reduced* in-band (there IS no outbound — that is the cost); T4, T5 (not network axes) | none / none | per-namespace; global allocator unnecessary |
| `rootlesskit` | T2, T3; T1 for anything not explicitly published (userland NAT binds published ports, not the host's) | a mis-published port is still host-reachable (T1 via publishing mistakes); T4, T5 | none (already installed) / none | per-namespace; global allocator unnecessary |
| `pasta` | same class as `rootlesskit` (T2, T3; T1 except via publishing) | same as rootlesskit, plus its own maturity/perf envelope (unmeasured — NET-BUNKER-008) | pasta binary / none | per-namespace; global allocator unnecessary |
| `netns-veth` | T2, T3; T1 subject to per-tenant firewalling; the ONLY mode with per-tenant L3 policy | anything the tenant firewall rules allow through; T4, T5 | iproute2 / CAP_NET_ADMIN for the daemon | per-namespace; global allocator unnecessary; per-tenant firewall rules are new state |
| `container` | T2, T3; T1 except published ports (behind rootlesskit NAT, container-mode.md §5) | T1 via published ports; T4 unless a PID namespace is added (container-mode §5 already excludes `--pid=host` for the workload, but the dockerd itself still runs in the host PID space) | none new / none | container-internal ports published via §3 of container-mode.md |
| `firewall` layer (orthogonal) | T2 (reachability) | **T3 — cannot stop bind-squatting: the kernel still permits the bind**; T1 (it is egress/tenant-to-tenant, not ingress); T4, T5 | iptables-nft / root (daemon) | unchanged — global allocator stays as bookkeeping |
| `hidepid=2` | T4 | T1, T2, T3, T5; host-wide side effect (affects non-agent views; cannot be per-agent) | none / host remount (provision-time) | unchanged |
| per-agent PID ns | T4 | T1, T2, T3, T5 | none / user-ns (unprivileged) or daemon-held ns | unchanged |
| restricted `/run/bunker` | T5 | T1-T4 | none / none (daemon already runs the path work as root) | unchanged |
| per-agent mount ns over `/run/bunker` | T5 | T1-T4 | none / mount ns plumbing per session type | unchanged |

A mode that does not state its limits is not specified — the table above is
the normative minimum; implementation rows may narrow their mode's claims but
never widen them.

---

## 3. The Compatibility Matrix — what BREAKS under each mode

Isolation that silently breaks the product is worse than no isolation. Per
mode, the known breakage class:

| Breaks | `shared` | `systemd` | `rootlesskit` | `pasta` | `netns-veth` | `container` |
|--------|----------|-----------|---------------|---------|--------------|-------------|
| Tunnel ingress (cloudflared / tailscale forward to a **host-side** port today, `manager_spawn.go:792-805`) | works (today) | **BROKEN** for agent services — a loopback-only agent has no host-reachable port to forward to; inbound must ride exec/socket channels or be declared unsupported in this mode | works ONLY if publishing lands the port in the namespace/host path the tunnel forwards into — the daemon must know WHICH netns to publish into; publishing into the wrong namespace is the silent-failure class this matrix exists to kill | same as rootlesskit | same — publishing must target the tenant ns | container-mode.md §3 already owns this (`-p` on the agent's block); same publish-into-netns requirement |
| Port publishing | n/a (ports are host ports already) | no host port exists | builtin port driver (`isolation.go:991`) binds per-netns; daemon must resolve agent-netns → host visibility explicitly | pasta equivalent, own mechanism | veth/bridge + explicit host-side DNAT or firewall rule per published port | `-p` per container-mode.md §3 |
| Daemon exec path (`docker exec` via `/run/bunker/<id>/docker.sock`, `service.go:1315-1337`) | works | **works unchanged — unix sockets cross netns** (the property that makes this affordable) | works unchanged | works unchanged | works unchanged | works unchanged (socket contract is container-mode.md's §3 client-invariance) |
| DNS and outbound | works | **GONE — all outbound lost, by design** | works (userland NAT) | works (pasta NAT) | works (routed bridge) | works (rootlesskit NAT, container-mode.md §0) |
| Port-block accounting (global 10000-19999, `portalloc.go`) | as today | allocation becomes meaningless — the agent's ports live in its own ns; keeping the global allocator as bookkeeping is harmless but must not be *relied on* for exclusivity claims | same | same | same | same (container-mode.md §3 reuses the block for `-p <ext>` today; in isolated modes that block lives in the agent ns, not the host) |
| `hidepid`/PID-ns additions | T4 leak remains until added | adds T4 boundary | adds T4 boundary | adds T4 boundary | adds T4 boundary | per-container PID ns by default (docker default), dockerd itself still host-side |
| `/run/bunker` visibility | T5 leak remains until added | unchanged | unchanged | unchanged | unchanged | unchanged |

Two non-negotiable composition rules:

1. **The exec/socket contract never changes.** Every mode preserves
   `DOCKER_HOST=unix:///run/bunker/<id>/docker.sock`, the `ssh -L 2376:`
   tunnel (`manager_spawn.go:686-690`), and sshfs. A mode that cannot keep
   this contract is rejected, not adapted (this is why `systemd` mode is
   viable at all).
2. **Ingress that needs a host port must either work in the mode or be
   declared unavailable for that mode** — never half-working. The
   `NetworkConfig` ingress selection (cloudflare/tailscale/direct,
   `bunker.proto:90-102`) is orthogonal to isolation mode, but a spawn that
   requests BOTH `MODE_CLOUDFLARE_TUNNEL` and `systemd` isolation is a
   contradiction the daemon must refuse at spawn time (§5, refuse-loudly).

---

## 4. The Design Payoff — per-namespace port space retires the global range

This is the argument FOR the namespace modes, and it is a simplification, not
a cost. **With a per-tenant network namespace, port space is
per-namespace**: every tenant can use the same port numbers, nothing is
global, and there is nothing to allocate. The existing global 10000-19999
allocator (`internal/resource/portalloc.go:13-58`, defaults
`internal/config/config.go:991-993`) then becomes **unnecessary in isolated
modes** — not deprecated globally (it stays exactly as-is for `shared`, the
declared default), but bypassed by any namespace mode.

Four existing filed rows trace directly to that global range being a single
contended, leak-prone bookkeeping surface:

- **GAP-010** — the configured range was 10x smaller than documented
  (10000-10100, 10/agent vs the documented 10000-19999, 100/agent); fixed,
  but the class it represents — a global integer range silently mismatching
  its documentation — exists because the range is global.
- **QA-BUNKER-1** — "port range pool exhausted on live server with only 2/8
  agents live": the in-memory allocator leaked ranges since process start and
  spawns failed with `pool exhausted` while the host was mostly idle.
- **QA-BUNKER-4** — allocator leak on the destroy path: a non-force destroy
  that returned not_found after `userdel` failure never freed the agent's
  range; TTL-expired agents exhausted the whole pool (proven live 2026-09-03).
- **QA-BUNKER-B15** — the allocator is in-memory only; after a daemon restart
  the re-registration path can double-allocate a live agent's range.

None of these can exist for ports inside a per-tenant namespace: there is no
shared counter to exhaust, no cross-agent collision to leak into, and no
restart re-registration race, because there is no shared registry of port
ownership at all. The allocator survives only where it is honest — `shared`
mode, where it is explicitly bookkeeping — and every isolated mode deletes
the problem class instead of patching it. (Implementation row: NET-BUNKER-009.)

---

## 5. Declared Default and the Reporting Law

### 5.1 The declared default is `shared`

The default is `shared`, unchanged, **until measured**. NET-BUNKER-008 owns
the measurement battery; this spec does not guess its outcome. Policy,
stated as law:

- **Additive, never replacing.** A new mode ships as a new option; nothing
  existing is replaced by default, and there is never a silent switch. Today's
  spawn behavior is byte-identical until an operator asks for a mode — the
  same zero-delta discipline container-mode.md and GAP-067 pin with exact-
  string tests.
- **No default change without evidence.** If the battery ever shows another
  default is correct, that is a new decision with its own row and its own
  numbers — not a rider on an implementation PR.
- **Per-agent selection.** The mode is chosen per spawn (the eventual proto
  field, NET-BUNKER-010), server-defaulted to `shared`. The precedent for
  "unknown name is a hard error — never a silent fallback" already exists in
  this repo's proto conventions (`bunker.proto:224-231`, `safety_preset` and
  `mount_driver`).

### 5.2 The reporting law

The active mode and the boundary it ACTUALLY provides must be announced:

- **Per agent, in status output** — `bunker list`/`GetAgent` shows the mode
  and the enforced boundary.
- **In any system-info marker** — reusing the GAP-067 containment-disclosure
  idiom ([containment-disclosure.md](containment-disclosure.md)): a fixed,
  greppable marker line an agent session can discover through normal
  reconnaissance, plus the existing capability-report precedent
  (`ServerInfoResponse.tmp_isolation` / `tmp_isolation_detail`,
  `bunker.proto:116-126` — a proto field that reports an enforced boundary
  with a reason string). The network-isolation surface follows the same
  shape: mode + actually-provided boundary + reason when less than requested.

The governing rule, stated verbatim in substance:

> **A bound that is not reported is not a bound.** A security mode an operator
> cannot see is WORSE than none, because it manufactures confidence.

Two corollaries with the force of law:

- **Refuse loudly, never fall back silently.** A spawn that requests a mode
  the host cannot provide (missing dependency, no CAP_NET_ADMIN, contradiction
  with the requested ingress) FAILS with a named error — it never degrades to
  `shared` and reports success. A silent fallback is a manufactured bound,
  which §5.2 forbids. (Precedent: GAP-075's fail-closed PAM design —
  [agent-tmp-isolation.md](agent-tmp-isolation.md) G8 — and the proto's
  no-silent-fallback convention.)
- **"Requested" and "provided" are different fields.** The daemon reports
  what it REQUESTED and what it actually ENFORCED (verified at spawn, the
  read-back discipline GAP-075 calls no-silent-no-op). They may differ only
  when the spawn failed.

---

## 6. The Unix-Socket Half

The agent-control surface is structurally sound — unix sockets' privacy comes
from the **socket file and its directory**, not from an address — but two
things must be true rather than assumed.

### 6.1 What actually protects a unix socket

- The socket file lives at `/run/bunker/<id>/docker.sock`, a symlink to
  `/run/user/<uid>/docker.sock` (`manager_spawn.go:399,507,1050-1056`). Its
  privacy is the union of the parent directory's traversal permissions and
  the socket file's own permissions — the listening process's `connect()`
  gate is the filesystem check, nothing else.
- **`chown` does NOT set the mode.** The spawn creates `/run/bunker/<id>` with
  `MkdirAll(sockDir, 0755)` and then only `chown`s it to the agent
  (`manager_spawn.go:406-413`) — **the mode is NOT 0700 today**. The
  directory is 0755: on a default-umask host that means every agent can
  list and traverse every agent's runtime directory — which is exactly what
  ISO-002 demonstrated (`ls /run/bunker/` showed 23 tenants). "Covered by
  user permissions" is only true if the directory is actually 0700 AND the
  socket file is not group/world accessible; neither property is asserted by
  any code today.

### 6.2 SO_PEERCRED — how a socket is made safe even in a shared path

`SO_PEERCRED` lets the *server* ask the kernel which uid connected and accept
only its own. Applied here: dockerd (or a bunker-controlled proxy in front of
it) checks the peer uid on every connection and refuses anyone but `<uid>`
— then even in the 0755 world, a peer that can *see* the socket cannot *use*
it. This is the defense that composes with every mode including `shared`,
and it does not exist yet (grep-verified: zero `SO_PEERCRED` references in
`internal/` or `cmd/`). (Implementation row: NET-BUNKER-007.)

### 6.3 The assertion law

Both properties — directory 0700 where claimed, peer-uid enforcement where
claimed — **MUST BE ASSERTED BY A TEST, not assumed**:

- the socket directory mode is read back from the real filesystem after
  creation (the same stat-it-back discipline GAP-075 applies to the scratch
  root — `EnsureSharedScratch` "STATS the directory back afterwards" because
  "a `chmod` that exited 0 is not proof"; [agent-tmp-isolation.md](agent-tmp-isolation.md) §3.3);
- a connection from a non-owner uid to the socket is refused, and the refusal
  is observable in the spawn/verify battery.

Until those assertions exist, any doc claim of socket privacy is withdrawn.

---

## 7. Trap to Avoid — Abstract Unix Sockets

**Nothing in this codebase may use an abstract unix socket** (the Linux
`@name` address family — `bind()` on a name starting with NUL). Reasons,
stated as design law:

- Abstract socket names live in a **global namespace with no file
  permissions**: ANY process on the host that can guess or read the name can
  connect. They are strictly worse than a pathname socket for multi-tenant
  use — they give up exactly the property (filesystem-based privacy,
  §6.1) that makes unix sockets the right agent-control channel.
- They only become per-tenant **inside a network namespace** (abstract
  namespaces are per-netns); under `shared` — the declared default — they
  would be a wide-open cross-tenant channel.
- Reviewers and implementations should treat an abstract-socket bind in
  agent-reachable code as a defect, not a style choice.

---

## 8. Non-Goals

This spec deliberately does NOT decide:

- **The default.** `shared` is the declared default *until measured* (§5.1);
  whether that changes is the measurement row's job (NET-BUNKER-008).
- **The measurement methodology** — what throughput/latency/usability each
  mode must show, and the battery that shows it (NET-BUNKER-008's scope).
- **Per-mode implementation details** — the exact systemd-unit edits, pasta
  invocation, veth/bridge topology, firewall rule ordering, PID-namespace
  plumbing, and `/run/bunker` mount-namespace mechanics each belong to their
  mode's implementation row (NET-BUNKER-002..006, 011, 012). This spec fixes
  their contracts and limits, not their scripts.
- **The proto surface shape** — field names, enum values, and response
  fields are NET-BUNKER-010's design work; this spec constrains semantics
  (§5.2) only.
- **Container-mode internals** — spawn/destroy/exec/persistence for
  container-per-agent remain container-mode.md's authority (§1.6).
- **The control-plane exposure problem** — SEC-BUNKER-001's wildcard binds
  and ufw rules are a listener-bind fix on their own row; no isolation mode
  in this surface substitutes for it.

---

## 9. References

- [container-mode.md](container-mode.md) — container-per-agent mode: spawn
  mapping, port publishing (§3), exclusions (§5), risks (§6)
- [agent-tmp-isolation.md](agent-tmp-isolation.md) — the implemented GAP-075
  filesystem boundary this surface composes with; fail-closed and stat-back
  disciplines this spec reuses
- [containment-disclosure.md](containment-disclosure.md) — the GAP-067
  disclosure idiom §5.2 reuses
- [agent-lifecycle.md](agent-lifecycle.md) — spawn/destroy step order the
  mode provisioning must slot into
- [api.md](api.md) — the RPC surface the eventual mode field extends
- `proto/bunker/v1/bunker.proto:90-102` — `NetworkConfig` (ingress modes);
  `:215-231` — `SpawnAgentRequest` (no isolation field today);
  `:116-126` — `tmp_isolation` capability-report precedent
- `internal/agent/manager_spawn.go` — useradd (:248), socket dir 0755+chown
  (:406-413), legacy tmp 0700 (:420-423), systemd-run (:564), symlink
  (:1050-1056), tunnel/tailscale consumers (:792-805), `ssh -L 2376:` (:686-690)
- `internal/agent/isolation.go:981-1033` — `buildRootlessDockerdArgs`
  (slirp4netns env :990-996, `PrivateTmp=yes` :1013)
- `internal/agent/rootless_prereqs.go:216-231` — slirp4netns required;
  `internal/agent/rootless.go:1713-1754` — rootlesskit AppArmor profile
- `internal/agent/subid_alloc.go:34-45` — per-agent 65536 subid blocks
- `internal/config/config.go:477-479,991-993` — port range defaults;
  `:185` — `IsolationTmpDir`
- `internal/resource/portalloc.go:13-58` — the global allocator
- `internal/server/service.go:1301-1337` — exec path, env(1) injection
- Filed rows (`.coding-hermes/board/tasks.jsonl`, unversioned board data —
  cited by id): SEC-BUNKER-001, SEC-BUNKER-002, ISO-001, ISO-002,
  NET-BUNKER-001 (this spec) .. NET-BUNKER-012, GAP-010, QA-BUNKER-1,
  QA-BUNKER-4, QA-BUNKER-B15
