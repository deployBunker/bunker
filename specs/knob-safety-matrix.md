# Bunker — Knob-Safety Matrix Specification

Version: 1.0.0
Status: Methodology — **findings recorded by live measurement** (GAP-114); feeds GAP-113's matrix
Last Updated: 2026-09-20
Related: specs/safety-presets.md (the tier→knob contract this measures),
specs/preset-acceptance-harness.md (the battery these probes are folded into)

---

## 0. Why this document exists

The owner's ask is *"not the most safe, not the most open — something focused on a good
experience."* That is a **measurement question**, not a taste question. This document is the
methodology and the findings table that turns knob selection from an opinion into data. Its
output is what lets `specs/safety-presets.md` §3 move a knob from *UNMEASURED* to *measured*
and therefore — under rule R1 — become eligible to be default-on.

**Rule of this document:** you may not write a verdict here that a probe did not produce. A
knob that could not be measured is recorded **UNMEASURED**, which under R1 blocks it from
being default-on. **UNMEASURED is a valid, honest result; an invented verdict is not.**

---

## 1. Method

- **Host:** the same host class the tier targets (8c/16G where a memory or IO figure is
  load-bearing) and, for the release-relevant confirmations, `bunker-mvp` per `AGENTS.md`.
- **Shape:** every finding is a **re-runnable probe** — a literal command sequence — plus its
  **observed output**. The probe is storable as a script under the harness's directory so
  GAP-122 can fold it into `preset-battery.sh`.
- **Two sides per knob:** the **workload side** (does it break a real workload?) and the
  **abuse side** (does it stop the abuse it exists to stop?). A knob is only defensible when
  both are measured.
- **Two columns per knob:** the *documented* class from the `bunker-agent-isolation` incident
  record (below) and the *measured* result on this host. Where they disagree, the measurement
  wins and the discrepancy is recorded.

### 1.1 The three documented breakage classes (to **reproduce live**, not merely quote)

These are real incidents; GAP-114 must reproduce each on a live host, not cite them:

| Class | Symptom | Why |
|---|---|---|
| **LimitFSIZE / .NET** | linuxserver Sonarr/Radarr/Prowlarr/Jellyfin `ftruncate` a **2 TiB sparse** file at first boot → `EFBIG` → `SIGXFSZ` → s6 restart loop; **config dir stays EMPTY** | `RLIMIT_FSIZE` is a **per-file** cap, not a usage quota |
| **memlock / rootless Docker** | `docker build`/compose rejects `ulimits memlock: -1` → `EPERM` → container never starts → `compose up` **aborts** → every redeploy fails | rootless dockerd cannot set an unlimited memlock rlimit |
| **ProtectSystem=strict / spawn** | `useradd` cannot lock `/etc/passwd` (read-only `/etc`) → **spawn fails** with a misleading lock-contention error | the spawn path writes `/etc` after the unit is sandboxed |

Reproducing these live is **PASS criterion (3)** of GAP-114: a quoted incident does not prove
the knob still behaves that way on the current host.

---

## 2. Findings table (filled by measurement)

One block per candidate knob. Empty cells are **UNMEASURED** until a probe fills them — and an
UNMEASURED cell blocks the knob from being default-on (R1, `specs/safety-presets.md` §0.1).

Template per knob:

```
### <knob> (<property> → <cgroup v2 file>)
Axis: <resource|unit-sandbox>          Enforcement: E1 / E2 / E1+E2
Documented class: <none | one of §1.1>
PROBE (re-runnable):
  <literal commands>
OBSERVED:
  workload side : <observed output / breakage>            -> verdict per tier: open/standard/guarded/hostile
  abuse side    : <observed output / containment proof>    -> verdict per tier
UX COST: <measured cost, or UNMEASURED>
SUGGESTED TIER: <which tiers may default it on>
```

### 2.1 Knobs to measure (from `specs/safety-presets.md` §3)

| # | Knob | Probe focus | Status |
|---|---|---|---|
| a | `MemoryMax` | headroom multiple that keeps a realistic `docker build` alive on 8c/16G | UNMEASURED |
| b | `MemorySwapMax=0` | does a legitimate build complete without swap absorbing the peak? peak RSS vs MemoryMax for a compose stack and a build | UNMEASURED |
| c | `MemoryHigh` | throttle-vs-kill; record the observed stall | UNMEASURED |
| d | `MemoryOOMGroup` | subtree reaped atomically (the compose-stack-inconsistency fix) | UNMEASURED |
| e | `IOWeight` / `IOReadBandwidthMax` | protect the host during a 1.4 GB `docker load` / reindex; quantify peer slow-down | UNMEASURED |
| f | `TasksMax` | fork-rejection is the failure mode (`cgroup: fork rejected by pids controller`); the value below which a normal compose stack trips it | UNMEASURED |
| g | unit-sandbox set | which of `NoNewPrivileges`, `Protect{KernelTunables,KernelModules,ControlGroups}`, `RestrictNamespaces`, `RestrictSUIDSGID`, `CapabilityBoundingSet`, `SystemCallFilter` break rootlesskit/dockerd — classify each SAFE / NEEDS-EXCEPTION / BREAKS | UNMEASURED |
| +1 | `LimitFSIZE` | reproduce the .NET/§1.1 class live | UNMEASURED |
| +2 | memlock | reproduce the rootless-Docker/§1.1 class live | UNMEASURED |
| +3 | `ProtectSystem=strict` | reproduce the spawn/§1.1 class live | UNMEASURED |

*(Each row becomes a §2 block with its probe and observed output as it is measured.)*

---

## 3. Required outputs (GAP-114 PASS criteria)

1. A table (above) covering **every knob proposed in `specs/safety-presets.md` §3** plus the
   three documented breakage classes, each with a **re-runnable probe** and its **observed
   output**.
2. Every knob carries a **verdict for each tier** (`open`/`standard`/`guarded`/`hostile`).
3. At least the **.NET/LimitFSIZE, memlock, and ProtectSystem** classes are **reproduced
   live** (not quoted).
4. An explicit **two-list split**:
   - **NO measured UX cost** → candidate for default-on (subject to R1).
   - **MUST stay opt-in** → the knobs with a measured or documented cost.
5. Every knob that could not be measured is recorded **UNMEASURED** — which, by R1, **blocks
   it from being default-on**. (This is the mechanism that keeps "good experience" honest: a
   knob cannot silently become a default on the strength of a hunch.)

---

## 4. How this feeds the other rows

- **→ `specs/safety-presets.md` §3:** each measured knob replaces its *UNMEASURED* status with
  a measured cost line, which is the precondition for moving it into a tier's default set.
- **→ GAP-118/119 (implementation):** the SUGGESTED TIER column and the SAFE/BREAKS
  classification (g) are the exact inputs those rows need; they must not re-decide them.
- **→ `specs/preset-acceptance-harness.md`:** the workload-side probes (build, compose, .NET,
  IO load) are the same workloads the harness asserts; the abuse-side probes are its A1–A3.
  One set of probes, two consumers — measure once, assert forever.

## 5. Provenance

GAP-114, in service of the owner directive of 2026-09-20. The documented breakage classes are
from the `bunker-agent-isolation` incident record; the knob inventory is
`specs/safety-presets.md` §3. This document is deliberately **methodology + findings**, not
opinion: its acceptance is measured output, and its honest failure mode is `UNMEASURED`.
