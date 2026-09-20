# Bunker — Safety Presets Specification

Version: 1.0.0
Status: Implemented design — **spec-only** (GAP-113); implementation is GAP-116..122
Last Updated: 2026-09-20
Related: specs/agent-lifecycle.md (the spawn path these knobs ride),
specs/containment-disclosure.md (the config-surface precedent this follows),
specs/agent-tmp-isolation.md (the other half of the isolation boundary)

---

## 0. Overview and intent

Bunker should ship a **preset system** for different levels of trust in the process or agent
running inside an agent host: a small set of named tiers, each a bundle of containment
settings, with defaults chosen for a **good experience** — explicitly **neither the most
locked-down nor the most open**.

This document is the **design authority** for that family. It fixes the tier model, the
enforcement surface, the config surface and precedence, and two rules that every later row
must satisfy. The implementation rows are GAP-116 (plumbing), GAP-117 (formalise today's
defaults), GAP-118 (DoS-containment knobs), GAP-119 (unit sandboxing), GAP-120 (atomic
teardown), GAP-121 (observability), GAP-122 (the live battery). GAP-114 measures the knob
evidence this spec defers to, and GAP-115 designs the acceptance harness.

### 0.1 The two governing rules (normative)

> **R1 — The experience-budget rule.** Every knob a tier sets MUST carry a measured UX-cost
> line. **A knob whose cost is unmeasured may NOT be default-on in any tier.** The default
> tier is chosen at the *knee* of the safety-vs-experience curve, with the measurement
> recorded in GAP-114's matrix — "good experience" is a first-class acceptance criterion,
> not an afterthought.

> **R2 — The no-silent-no-op rule.** Every applied knob MUST be verified **write-then-read-back
> from the live cgroup/unit**. If a preset's knob cannot be applied on this host, **spawn
> FAILS LOUDLY**; a preset is never half-armed. Rationale: the shadow-proc kernel-wave
> security review (2026-09-12, BLOCKER B2) measured a security property that attached
> successfully and then never fired — an unenforced default is worse than an absent one.

R1 and R2 are acceptance criteria for every implementation row in this family; a row that
violates either is incomplete regardless of what it builds.

---

## 1. Tier model

Four tiers, ordered by trust. Each tier is a **bundle across axes**, not a single knob.
Tier names are a proposal (naming is cheap; the *structure* is what the owner must confirm
in review — see §9).

| Tier | Trust posture | Intended use | The promise |
|---|---|---|---|
| `open` | Trusted operator's own agent | A dev box where the agent is your own code | *Still keeps a ceiling* — a rogue fork or memory bomb must not take the host — but maximises freedom |
| `standard` | **DEFAULT** | General team use | The good-experience tier: an agent can still run `docker build`, a compose stack, a .NET app, and node/python workloads |
| `guarded` | Third-party / less-trusted code | Running someone else's agent code | `standard` + unit sandboxing + IO bounds |
| `hostile` | Adversarial / multi-tenant | Untrusted tenants on shared infrastructure | `guarded` + the strictest knobs |

**Boundary (must not be crossed by this family):** control-plane hardening — exec recording
and API lockdown — is **GAP-074's `hardened.mode`**, *not* re-implemented here. This family
owns the **resource axis** (CPU, memory, PIDs, files, IO) and the **unit-sandbox axis**
(filesystem/kernel/credential protections applied to the transient unit). The two compose:
`hostile` here + `hardened.mode` there is the strongest available posture, and neither implies
the other.

`open` is not "no limits": it still carries a ceiling. The difference between `open` and
`hostile` is the *tightness* of the same axes, not their presence or absence.

---

## 2. Enforcement surface (two points — one is insufficient)

Every resource knob must be applied at **both** of these places, or it does not hold:

| # | Point | Code anchor | Covers |
|---|---|---|---|
| **E1** | The per-user systemd **slice drop-in**: `[Slice]` properties written to `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`, then `systemctl daemon-reload` | `internal/agent/manager_spawn.go:886-928` (`applyUserSliceLimits`) | **Every** process owned by the agent uid — **including every `bunker exec` SSH session** |
| **E2** | The **rootless dockerd transient unit** argv: `systemd-run --property=…` | `internal/agent/isolation.go:109-144` (`buildRootlessArgs`) | The dockerd unit itself and everything it starts (containers) |

**Why one alone is insufficient:**

- **E2 only** (unit limits without the slice drop-in) lets `bunker exec` SSH sessions — which
  are *not* children of the dockerd unit — **escape the ceiling**. An agent could run a fork
  bomb or memory bomb directly in an exec session and the container limits would never see it.
- **E1 only** (slice limits without unit flags) leaves the dockerd unit's own argv
  **inconsistent with its ceiling**: `systemd-run` arguments, `docker info` output, and the
  live cgroup would disagree, and a knob expressed only in the slice cannot carry
  unit-scoped semantics (e.g. unit sandboxing properties, which are per-unit, not per-slice).

**Consequence for implementation:** any knob in §3 marked `E1+E2` must appear in both the
drop-in writer and the unit-arg builder; the effective-set read-back (R2) must read the
**live** values from both `/sys/fs/cgroup/user.slice/user-<uid>.slice/…` and the transient
unit's cgroup, and assert they match what the tier requested.

---

## 3. Knob matrix (tier × axis → knob)

Columns: **Enforcement** = E1/E2 per §2. **UX-cost** = measured / *UNMEASURED* / **documented
breakage**. Under R1, anything *UNMEASURED* may not be default-on until GAP-114 measures it.

| Axis | Knob (systemd property → cgroup v2) | Enforce | UX-cost status (from the isolation-skill incident record) | open | standard | guarded | hostile |
|---|---|---|---|---|---|---|---|
| **CPU ceiling** | `CPUQuota=P%` → `cpu.max` | E1+E2 | measured (throughput cap) | 2.0 cores | 2.0 cores | 2.0 cores | 1.0 core |
| **Memory ceiling** | `MemoryMax=B` → `memory.max` | E1+E2 | *UNMEASURED* (too tight → OOM mid-`docker build`) — GAP-114 (a) | 4 GiB | 4 GiB | 4 GiB | 2 GiB |
| **Swap accounting** | `MemorySwapMax=B` → `memory.swap.max` | E1+E2 | **documented breakage** (memlock/swap interactions; peak builds can die) — GAP-114 (b) | unset | unset | `0` | `0` |
| **Memory soft limit** | `MemoryHigh=B` → `memory.high` | E1+E2 | *UNMEASURED* (throttle vs kill) — GAP-114 (c) | unset | unset | 3 GiB | 1.5 GiB |
| **OOM atomicity** | `MemoryOOMGroup=1` → `memory.oom.group` | E1+E2 | *UNMEASURED* (reaps the whole subtree — the compose-stack-consistency fix) — GAP-114 (d) | unset | unset | `1` | `1` |
| **Process ceiling** | `TasksMax=N` → `pids.max` | E1+E2 | measured (compose stack trips low values; fork rejection is the mode) — GAP-114 (f) | 8192 | 4096 | 2048 | 1024 |
| **File descriptors** | `LimitNOFILE=N` → `RLIMIT_NOFILE` | E1+E2 | measured (low values break builds) | 65536 | 65536 | 8192 | 4096 |
| **Per-file size** | `LimitFSIZE=B` → `RLIMIT_FSIZE` | E1+E2 | **documented breakage — .NET crash-loop class.** It is a per-*file* cap, not a usage quota; Sonarr/Radarr/Jellyfin `ftruncate` a 2 TiB sparse file at first boot → `EFBIG` → `SIGXFSZ` → restart loop. Historical fix: `default_disk_bytes: 0` | 0 (off) | 20 GiB (today's value) | 20 GiB | 20 GiB |
| **IO weight** | `IOWeight=N` → `io.weight` | E2 | *UNMEASURED* — GAP-114 (e) | unset | unset | `50` | `10` |
| **IO bandwidth** | `IOReadBandwidthMax=…` | E2 | *UNMEASURED* (protects host during a 1.4 GB `docker load`) — GAP-114 (e) | unset | unset | unset | set |
| **Unit sandbox** | `NoNewPrivileges=`, `ProtectKernelTunables=`, `ProtectKernelModules=`, `ProtectControlGroups=`, `RestrictNamespaces=`, `RestrictSUIDSGID=`, `CapabilityBoundingSet=`, `SystemCallFilter=` | E2 | **mixed — classification is GAP-114 (g):** `ProtectSystem=strict` is *known to break spawn* (`useradd` cannot lock a read-only `/etc` → misleading lock-contention error); other properties may break rootlesskit/dockerd | none | none | subset (GAP-114 g) | full (GAP-114 g) |
| **Teardown atomicity** | cgroup kill / `KillMode=control-group` (see GAP-120) | E1+E2 | *UNMEASURED* — closes the destroy race | off | off | on | on |
| **Observability** | `memory.events`, `memory.peak` read-out (see GAP-121) | E1 | no UX cost (read-only) | off | off | on | on |

**Reading the matrix:** the `standard` column is deliberately **exactly today's shipped
defaults** (`DefaultCPUQuota 2.0`, `DefaultMemoryBytes 4 GiB`, `DefaultMaxProcesses 4096`,
`DefaultMaxOpenFiles 65536`, `DefaultDiskBytes 20 GiB` — `internal/config/config.go:394-398`).
That is not an accident: it is what makes GAP-116's zero-delta assertion achievable, and it is
the honest starting point under R1 — none of the *new* knobs may enter `standard` until
GAP-114 has measured their UX cost. The preset system as first shipped adds **structure and
honesty**, not new restrictions; the restrictions arrive with their measurements.

`open` is identical to `standard` except where a ceiling is deliberately relaxed
(`TasksMax 8192`, `LimitFSIZE 0`); `guarded` and `hostile` are where the new containment
lands, and they are opt-in precisely because their costs are the ones being measured.

---

## 4. Config surface and precedence

Follows the `containment.disclosure` precedent (GAP-067) for a global default with a
per-invocation override and an env override.

```yaml
# /etc/bunkerd/config.yaml
safety:
  preset: standard   # open | standard | guarded | hostile  (default: standard)
```

| Source | Key / flag | Notes |
|---|---|---|
| Global default | `safety.preset` in `/etc/bunkerd/config.yaml` | `config.SafetyConfig{Preset string}`, `mapstructure:"safety"` |
| Per-spawn override | `--preset` on `bunker spawn` and `bunker run` | Applies to that spawn only |
| Env override | `BUNKERD_SAFETY_PRESET` | Explicit `BindEnv`; **environment beats the config file** (the established `BUNKERD_*` rule) |

**Precedence:** per-spawn flag **>** env **>** global config **>** built-in default
(`standard`).

**Unknown preset is a HARD ERROR.** `safety.preset: gaurded` (typo) must fail at config load
or spawn, never silently fall back to `standard`. The same applies to an unknown knob or an
out-of-range value: reject at load, fail fast, never a partial apply.

**Effective-set reporting.** `bunker info <agent-id>` (and/or `bunker metrics`) MUST report
the **effective preset** *and* the **effective knob set actually landed**, so an operator
never has to guess whether a preset took effect — the human-facing half of R2.

---

## 5. Interaction with existing defaults and rows

- **Today's five knobs become the `standard` tier** (GAP-117). The current `agent.*` default
  fields remain the source of those numbers; the preset selects *which bundle*, it does not
  duplicate the arithmetic. GAP-117 must prove the `standard` spawn is **byte-identical** to
  pre-change (same drop-in content, same `systemd-run` argv).
- **`containment.disclosure` (GAP-067)** is orthogonal and unchanged. It is a *disclosure*
  axis, not a containment axis; a preset does not imply or alter it.
- **`hardened.mode` (GAP-074)** is orthogonal and unchanged — see §1's boundary.
- **`DestroyHomePolicy`** is unaffected; it is a data-retention choice, not a containment knob.
- **Subuid/subgid allocation** (GAP-140) is a separate P0 correctness fix on the isolation
  boundary; it is not a preset knob and must not be folded into this family.

---

## 6. Acceptance criteria (what "done" means for this spec)

1. This file exists at `specs/safety-presets.md`, is index-linked from `specs/_index.md`, and
   is implementation-ready: file paths, **exact systemd property names**, a precedence table,
   and a tier→knob matrix.
2. Every proposed knob names its **axis**, its **mechanism** (property → cgroup v2 file), its
   **enforcement point(s)**, and its **UX-cost status** (measured / *UNMEASURED* /
   documented breakage).
3. **R1 and R2 are stated as acceptance criteria** (§0.1) and every implementation row
   inherits them.
4. §2 names **both** enforcement points and explains why one alone is insufficient.
5. The **GAP-074 boundary is stated** (§1): this family does not re-specify control-plane
   hardening.
6. The `standard` tier is **provably today's behaviour**, so the family can ship with a
   zero-delta default and add restrictions only as evidence arrives.

## 7. What this spec does NOT decide (deferred, with owners)

- **Which unit-sandbox properties are SAFE vs NEEDS-EXCEPTION vs BREAKS** for rootlesskit and
  dockerd → **GAP-114 (g)**.
- **The measured headroom multiples** for memory/swap/IO → **GAP-114 (a),(b),(e)**.
- **Tier names** → owner confirmation at review (§9); the structure is fixed here.
- **The battery's exact PASS/FAIL contract** → **GAP-115**, implemented in **GAP-122**.
- **Whether `open` should carry any ceiling at all** → owner question, §9.

## 8. Open questions for the owner

1. **Tier names** — is `open / standard / guarded / hostile` the vocabulary you want, or do
   you prefer neutral names (`dev / default / strict / locked`)? Names are cheap; say and we
   change them in one commit.
2. **`open` and the ceiling** — confirm `open` keeps a hard ceiling (fork/memory bomb
   protection) but otherwise relaxes limits. The alternative is a true "no limits" tier, which
   this spec deliberately does *not* propose.
3. **Adding to `standard`** — once GAP-114 measures the new knobs, should any of them graduate
   into `standard` (changing what "good experience" means for existing users), or does
   `standard` stay frozen at today's defaults forever, with all new containment living in
   `guarded`/`hostile`? This is the "not the most safe, not the most open" calibration, and it
   is yours to make.

## 9. Provenance

Written to satisfy GAP-113, which encodes an owner directive of 2026-09-20: ship a preset
system for different trust levels, with defaults tuned for a good experience. The knob
inventory and the documented breakage classes (LimitFSIZE/.NET, memlock, `ProtectSystem`)
are drawn from the `bunker-agent-isolation` incident record and are to be reproduced live by
GAP-114. R2 is drawn from the shadow-proc kernel-wave security review's B2 finding.
