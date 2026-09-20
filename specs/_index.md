# Bunker — Specifications Index

Landing page for the `specs/` directory. Six specs cover the Bunker platform
(a daemon, `bunkerd`, that hosts isolated agent environments; a CLI, `bunker`,
that controls it). Start here, pick your audience below, and follow its reading
order.

Last verified against repo HEAD `9ee17c6` (2026-09-16).

## The specs at a glance

| Spec | Role in the suite | Audience | Status |
|------|-------------------|----------|--------|
| [api.md](api.md) | The contract | integrator, operator | implemented |
| [architecture.md](architecture.md) | The components | contributor, operator | implemented |
| [agent-lifecycle.md](agent-lifecycle.md) | The state machine | contributor, operator | implemented |
| [agent-tmp-isolation.md](agent-tmp-isolation.md) | The isolation boundary | operator, contributor | implemented (GAP-075) |
| [containment-disclosure.md](containment-disclosure.md) | The disclosure contract | operator, integrator | implemented (GAP-067), config-gated |
| [safety-presets.md](safety-presets.md) | The trust-tier preset system | operator, contributor | not implemented (design authority, GAP-113) |
| [preset-acceptance-harness.md](preset-acceptance-harness.md) | The preset verification battery | contributor, operator | not implemented (design, GAP-115) |
| [knob-safety-matrix.md](knob-safety-matrix.md) | The measured knob evidence | contributor | methodology (GAP-114), findings pending measurement |
| [container-mode.md](container-mode.md) | The proposed execution mode | contributor | not implemented (design draft) |

This file is `_index.md`; every link above is relative and resolves from
inside `specs/`.

## Spec entries

### [api.md](api.md) — API Specification (v1.1.0)

The wire contract: every RPC on the `Bunkerd` and `Agent` services
(`ServerInfo`, `ServerMetrics`, `SpawnAgent`, `DestroyAgent`, `ListAgents`,
`GetAgent`, `AgentMetrics`, `ExecAgent`, `RunAgent`, `HeartbeatAgent`,
`QueryAudit`), request/response fields, auth (JWT or static token in the
`Authorization` header), and the dual gRPC (`:9090`) + REST (`:8080`) ports
served by connect-go. Based on `proto/bunker/v1/bunker.proto`.

- **Who should read it:** integrators writing clients against the daemon, and
  operators who need to know what a token can reach.
- **Status: implemented** — the documented RPCs are the live connect-go
  surface (see proto/bunker/v1/bunker.proto:15).

### [architecture.md](architecture.md) — Architecture Specification (v1.1.0, stable)

The component map: CLI → `bunkerd` (agent manager, auth, network ingress,
resource tracker/port allocator) → per-agent instances, each a non-root Linux
user with its own rootless dockerd under a systemd transient unit, plus the
on-disk layout (`/home/bunker-<id>/`, `/run/bunker/<id>/`,
`/etc/bunkerd/config.yaml`).

- **Who should read it:** contributors who need the big picture before
  touching code, and operators who want to know what runs where on their host.
- **Status: implemented** — describes the running daemon; the per-agent
  rootless dockerd via `systemd-run` it documents is at
  internal/agent/manager_spawn.go:415.

### [agent-lifecycle.md](agent-lifecycle.md) — Agent Lifecycle Specification (v1.2.0, stable)

The state machine from spawn to destroy, in canonical code order
(`internal/agent/manager_spawn.go`): validation (TTL, image spec), port
allocation, `useradd`, SSH keypair, subids, rootless dockerd unit, tunnels,
agent API key — then runtime (heartbeat/TTL expiry) and destroy, including
the durable append-only JSONL registry that survives daemon restarts, its
size-capped rotation, and offline compaction via `bunker registry compact`.

- **Who should read it:** contributors changing spawn/destroy behavior (the
  spec names the code as authoritative where numbering differs), and
  operators doing registry maintenance or boot-reconciliation triage.
- **Status: implemented** — the durable registry and `bunker registry
  compact` are live (see internal/cli/registry.go:45).

### [agent-tmp-isolation.md](agent-tmp-isolation.md) — Agent Isolation Boundary Specification (GAP-075)

The filesystem isolation boundary: per-agent private `/tmp` (a
`pam_namespace` mount namespace for SSH sessions, `PrivateTmp=yes` on the
rootless-dockerd and detached-run systemd units), the fail-closed PAM
classifier (agent-name pattern, integrity-checked helper, deny on any
anomaly), the bounded host `/tmp` tmpfs, and the single sanctioned
cross-agent exchange point `/srv/bunker-share` (root-owned, setgid, mode
2750, per-agent kernel-enforced tmpfs size cap). Supersedes the old
`TMPDIR=/run/bunker/<id>/tmp` note in architecture.md.

- **Who should read it:** operators who must run `bunker host-provision
  --apply` on the host (and audit what it installs), and contributors working
  on isolation, PAM, or host setup.
- **Status: implemented (GAP-075)** — `--property=PrivateTmp=yes` in the
  unit builder (see internal/agent/isolation.go:117); scratch layout in
  internal/hostsetup/scratch.go:12; the `bunker host-provision` CLI at
  internal/cli/hostprov.go:42.

### [containment-disclosure.md](containment-disclosure.md) — Containment Disclosure Specification (v1.0.0, GAP-067)

The disclosure contract: an admin-controlled, hidden-by-default feature that
lets an operator make agents honestly disclose their sandbox — every exec
session gets `BUNKER_SANDBOX=1` and an allowlisted system-info probe gets a
fixed marker line appended to stdout. Covers the `containment.disclosure`
config key, the `BUNKERD_CONTAINMENT_DISCLOSURE` env override, the exact
injection point per session type, and the byte-identical-when-disabled
guarantee.

- **Who should read it:** operators deciding whether to enable disclosure on
  their daemon, and integrators whose automation may observe
  `BUNKER_SANDBOX=1` or the probe marker.
- **Status: implemented (GAP-067), config-gated** — off by default;
  `Containment ContainmentConfig` in the server config (see
  internal/config/config.go:26), canonical env constant at
  internal/config/config.go:46, exec-path injection at
  internal/server/service.go:736.

### [safety-presets.md](safety-presets.md) — Safety Presets Specification (v1.0.0, GAP-113)

The trust-tier preset system: four tiers by trust (`open`, `standard`, `guarded`,
`hostile`), each a bundle of containment settings across the resource and unit-sandbox axes,
with `standard` as the good-experience default. Fixes the two enforcement points (the
user-`<uid>.slice` drop-in and the rootless-dockerd transient unit) and why one alone is
insufficient, the `safety.preset` / `--preset` / `BUNKERD_SAFETY_PRESET` precedence, and two
governing rules: the **experience-budget rule** (an unmeasured knob may not be default-on) and
the **no-silent-no-op rule** (every knob is read back from the live cgroup or spawn fails).

- **Who should read it:** operators choosing a trust posture per agent, and contributors
  implementing GAP-116..122.
- **Status: not implemented — design authority (GAP-113)** — no `safety` config key exists
  yet; the tier→knob matrix is the contract the implementation rows build to, and `standard`
  is pinned to today's shipped defaults (`internal/config/config.go:394-398`).

### [preset-acceptance-harness.md](preset-acceptance-harness.md) — Preset Acceptance Harness Specification (v1.0.0, GAP-115)

The verification battery for the preset tiers: a workload set (docker build, compose stack,
a .NET app, node+python, a 1.4 GB `docker load`) that proves a tier is still usable, and an
abuse set (fork bomb, memory bomb, IO hog) that proves abuse is contained **by asserting the
mechanism** (`pids.events`, `memory.events`, peer impact), not by surviving. Mandates the
**no-silent-no-op landing check** — every knob is read back from the live cgroup at **both**
enforcement points, and a contained abuse case with an unlanded knob is a FAIL. Runs on
`bunker-mvp` and rolls up into the existing `VERIFY-PASS` contract.

- **Who should read it:** contributors implementing GAP-122, and operators who want to know
  what "a tier passed" actually means.
- **Status: not implemented — design (GAP-115)** — implemented and run by GAP-122; no
  `preset-battery.sh` exists yet.

### [container-mode.md](container-mode.md) — Container-Mode Agent Specification (v0.1.0, draft)

The proposed execution mode: an opt-in spawn mode where the agent workload is
a container on its own per-agent rootless dockerd, home bind-mounted in,
everything else containerized. Grounded in the existing substrate (rootless
daemon, sockets, port blocks, SSH keys) and explicit about what does not
exist yet (no `mode`/`container` field on `SpawnAgentRequest`, no restart
RPC). Design gate for GAP-063; GAP-064 landed as per-agent image specs
instead, which is a different, already-shipped feature.

- **Who should read it:** contributors picking up the GAP-063/064/065
  implementation line, and integrators who want early awareness of a future
  proto field.
- **Status: not implemented — design draft** — `SpawnAgentRequest` carries
  no container-mode field (see proto/bunker/v1/bunker.proto:107); README.md:64
  labels container mode "(not implemented yet)".

## Recommended reading order

### Operator

1. [architecture.md](architecture.md) — what the daemon is and what it puts
   on your host
2. [api.md](api.md) — the RPC surface the CLI drives (skim for auth and
   ports)
3. [agent-lifecycle.md](agent-lifecycle.md) — what spawn does to your host,
   registry durability and maintenance
4. [agent-tmp-isolation.md](agent-tmp-isolation.md) — the isolation you must
   provision with `bunker host-provision --apply`
5. [containment-disclosure.md](containment-disclosure.md) — decide whether
   your daemon discloses the sandbox
6. [container-mode.md](container-mode.md) — optional; a planned mode with no
   operator action today

### Integrator

1. [api.md](api.md) — the contract: RPCs, auth, transports
2. [architecture.md](architecture.md) — component boundaries behind those
   RPCs
3. [agent-lifecycle.md](agent-lifecycle.md) — the states and transitions your
   client observes
4. [containment-disclosure.md](containment-disclosure.md) — the env var and
   probe marker your client may encounter
5. [agent-tmp-isolation.md](agent-tmp-isolation.md) — filesystem guarantees
   (`/tmp`, `/srv/bunker-share`) your jobs can rely on
6. [container-mode.md](container-mode.md) — future-mode awareness only

### Contributor

1. [architecture.md](architecture.md) — component map and directory layout
2. [agent-lifecycle.md](agent-lifecycle.md) — the canonical spawn order and
   its code anchor in `internal/agent/`
3. [agent-tmp-isolation.md](agent-tmp-isolation.md) — GAP-075 boundary
   mechanics and the test expectations around them
4. [containment-disclosure.md](containment-disclosure.md) — GAP-067 injection
   paths and the byte-identical-when-disabled regression contract
5. [container-mode.md](container-mode.md) — the GAP-063 design gate; where
   future work lands
6. [api.md](api.md) — the proto contract to keep in sync when touching the
   service surface

## How the specs relate

- [api.md](api.md) is **the contract** — the RPCs, auth, and ports every
  other spec defers to. Proto changes start there.
- [architecture.md](architecture.md) is **the components** — how CLI,
  `bunkerd`, and agent instances compose, and what lives where on disk.
- [agent-lifecycle.md](agent-lifecycle.md) is **the state machine** — the
  behavioral detail behind `SpawnAgent`/`DestroyAgent` from api.md and the
  Agent Manager box in architecture.md.
- [agent-tmp-isolation.md](agent-tmp-isolation.md) is **the isolation
  boundary** — it hardens the runtime-state layout described in
  architecture.md (its `/run/bunker/<id>/tmp` TMPDIR note is superseded) and
  constrains what any agent spawned per agent-lifecycle.md can touch.
- [container-mode.md](container-mode.md) is **the proposed execution mode** —
  a design layer over the same spawn substrate documented in
  agent-lifecycle.md; if it ships, api.md grows the missing proto field.
- [containment-disclosure.md](containment-disclosure.md) is **the disclosure
  contract** — an opt-in layer over the exec paths defined by api.md
  (`ExecAgent`/`RunAgent`) that shares the containment substrate with
  container-mode.md.
