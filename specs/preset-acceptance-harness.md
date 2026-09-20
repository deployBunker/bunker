# Bunker — Preset Acceptance Harness Specification

Version: 1.0.0
Status: Design — **spec-only** (GAP-115); implemented and run by GAP-122
Last Updated: 2026-09-20
Related: specs/safety-presets.md (the tier→knob contract this harness verifies),
`e2e-full-battery.sh` (the host-run battery this extends), `AGENTS.md` (the live-server rule)

---

## 0. Purpose

A preset is a promise about **two** things: that the workloads a tier is *for* succeed, and
that the abuse a tier is *against* is contained. Assertions that a tier "should" work do not
survive contact with .NET apps and compose stacks (see the documented breakage classes in
`specs/safety-presets.md` §3). This harness is what turns a preset from a claim into a
verified fact.

It verifies **both halves** of every tier, on a live host, and it enforces the two governing
rules of the preset family at the moment of truth:

- **R1 (experience-budget):** the workload set proves the `standard` tier is still usable.
- **R2 (no-silent-no-op):** every knob is read back from the **live cgroup**; a tier whose
  knob did not land is a **FAIL even if the workload ran**.

---

## 1. Where it runs, and how it is invoked

**Host:** `bunker-mvp` (`78.46.173.180`), per `AGENTS.md` quality-gate #3 — any change to
spawn/exec/docker behavior must run the live battery there and confirm `VERIFY-PASS`. The
preset battery inherits that rule; it never claims a preset is safe from a laptop run.

**Invocation:** a new `preset-battery.sh`, added to the suite and called by
`e2e-full-battery.sh` as one additional section, so the existing `VERIFY-PASS` /
`STATUS: ALL CORE TESTS PASS` contract and exit codes are preserved. It reuses the battery's
existing `assert`/`fail` helpers and its `bcli` CLI-state isolation, so a preset run writes
only the harness's own scratch config.

**Per-tier invocation:** `bash preset-battery.sh --tier <open|standard|guarded|hostile>` runs
one tier (the default target for a fast loop); `--all-tiers` runs the full
workload × abuse × tier matrix for a release. A tier that is skipped is reported as `SKIP`,
never as a pass.

**Cost budget:** the full matrix must fit the release gate's existing runtime envelope. The
per-tier run is the cheap path (a single tier, ~minutes); `--all-tiers` is the release path
and may take longer, but must stay bounded — the IO workload (§2 W5) is the one to watch and
carries its own timeout.

**Preflight:** refuse (exit 42, nothing changed) unless running as root, on a host with
cgroup v2 (`stat -fc %T /sys/fs/cgroup` = `cgroup2fs`), with `cpu io memory pids` present in
`cgroup.controllers` — the same refusal discipline the existing battery uses.

---

## 2. Workload set — "the tier is still usable" (R1)

Each workload must **PASS at `standard` and above**. `open` is exempt from a workload only
where `specs/safety-presets.md` says so.

| # | Workload | The class it exercises | Literal assertion |
|---|---|---|---|
| **W1** | `docker build` of a multi-layer image | the build/redeploy path; memory peak during build | `docker build -t preset-w1 .` exits 0 **and** the image exists: `docker image inspect preset-w1` |
| **W2** | `docker compose up` of a multi-service stack (postgres + app) | multi-process unit; the OOM-subtree-consistency class | `docker compose up -d --wait` exits 0 and `docker compose ps --status running` shows every service `running` (no half-stack) |
| **W3** | a .NET linuxserver-style app (Sonarr/Radarr/Jellyfin image) | the **LimitFSIZE / SIGXFSZ** crash-loop class | container runs ≥ 60 s **and** its config dir is non-empty: `docker exec <c> ls -A /config` non-empty (an `EFBIG` loop leaves it empty) |
| **W4** | node + python workloads | the everyday interpreter path | `node -e 'console.log(1)'` and `python3 -c 'print(1)'` both exit 0 inside an exec session |
| **W5** | a 1.4 GB `docker load` | the **IO** class (timeboxed) | `timeout 900 docker load -i big.tar` exits 0; wall time recorded (the skill documents ~45 min over tailnet — the harness records the number, it does not assume it) |

**A workload FAIL at `standard` is a release-blocking finding**: it means the default tier
broke a real use-case, which is exactly the "most safe" failure the owner ruled out.

---

## 3. Abuse set — "the tier contains the abuse" (R1/R2)

Each abuse case must be **CONTAINED at every tier, including `open`** (every tier keeps a
ceiling). "Contained" means the **refusal/limit is asserted**, not merely that the box
survived.

| # | Abuse | Knob under test | Literal assertion (assert the MECHANISM, not survival) |
|---|---|---|---|
| **A1** | fork bomb: `bash -c ':(){ :\|:& };:'` inside an exec session | `TasksMax` → `pids.max` | the session's forked processes stop growing, **and** the controller reports the refusal: `cat /sys/fs/cgroup/…/pids.events` shows a non-zero `max` count **or** the kernel log carries `cgroup: fork rejected by pids controller` |
| **A2** | memory bomb: `stress-ng --vm 1 --vm-bytes 16G --vm-keep` (or a python allocator) | `MemoryMax`/`MemoryHigh` → `memory.max`/`memory.high` | `cat /sys/fs/cgroup/…/memory.events` shows a non-zero `oom_kill` (or `high` throttling for a `MemoryHigh` tier); `memory.peak` is recorded; the harness states **who paid** (the agent or the host) |
| **A3** | IO hog: a large `dd`/reindex inside an exec session, run concurrently with a timed read on a **peer** agent | `IOWeight`/`IOReadBandwidthMax` → `io.weight`, `io.max` | the peer's read time is recorded **with and without** the hog; containment = the peer's degradation stays below the tier's stated bound |

A contained case whose target knob did **not** land (§4) is a **FAIL** — this is R2.

---

## 4. The no-silent-no-op landing check (R2 — mandatory for every knob)

For **every knob the tier claims**, read the **live** value back and assert it equals the
requested value. This runs before (or alongside) the workload/abuse assertions, and a landing
failure is a hard FAIL for the tier.

| Knob | Read-back source | Assertion |
|---|---|---|
| `CPUQuota` | `systemctl show <unit> -p CPUQuotaPerSecUSec` and `cat /sys/fs/cgroup/…/cpu.max` | requested % == landed |
| `MemoryMax` | `cat /sys/fs/cgroup/…/memory.max` | requested bytes == landed |
| `MemoryHigh` | `cat /sys/fs/cgroup/…/memory.high` | requested == landed |
| `MemorySwapMax` | `cat /sys/fs/cgroup/…/memory.swap.max` | requested == landed |
| `MemoryOOMGroup` | `cat /sys/fs/cgroup/…/memory.oom.group` | `1` when the tier sets it |
| `TasksMax` | `cat /sys/fs/cgroup/…/pids.max` | requested == landed |
| `LimitNOFILE` | `systemctl show <unit> -p LimitNOFILE` | requested == landed |
| `LimitFSIZE` | `systemctl show <unit> -p LimitFSIZE` | requested == landed |
| `IOWeight` | `cat /sys/fs/cgroup/…/io.weight` | requested == landed |
| unit sandbox (`Protect*`, `SystemCallFilter`, …) | `systemctl show <unit> -p <Prop>` | each requested property == landed |

**Both enforcement points are checked** (`specs/safety-presets.md` §2): the read-back covers
the `user-<uid>.slice` cgroup (E1, which every exec session inherits) **and** the transient
dockerd unit's cgroup (E2). A knob present in one and absent in the other is a FAIL — that is
the "exec sessions escape the ceiling" defect the spec's §2 exists to prevent.

> **The rule in one line:** a contained abuse case with an unlanded knob is a FAIL. A knob
> that attached but never fired is worse than a knob that was never set (the shadow-proc B2
> lesson; `specs/safety-presets.md` §0.1 R2).

---

## 5. PASS/FAIL contract per tier (implementable as written)

For a tier `T`, the harness prints one line per case and a verdict:

```
TIER <T>
  W1 docker-build .......... PASS|FAIL   (exit, image present)
  W2 compose-up ............ PASS|FAIL   (all services running)
  W3 dotnet-linuxserver .... PASS|FAIL   (ran >=60s, config non-empty)
  W4 node+python ........... PASS|FAIL
  W5 docker-load-1.4G ...... PASS|FAIL   (wall time: <n>s)
  A1 fork-bomb ............. CONTAINED|ESCAPED  (pids.events max=<n>)
  A2 memory-bomb ........... CONTAINED|ESCAPED  (oom_kill=<n>, who paid: <agent|host>)
  A3 io-hog ................ CONTAINED|ESCAPED  (peer read +<p>% vs baseline)
  LANDING E1 (slice) ....... OK|MISMATCH  (<n>/<m> knobs landed)
  LANDING E2 (unit) ........ OK|MISMATCH  (<n>/<m> knobs landed)
TIER <T> VERDICT: PASS|FAIL
```

**Verdict rules (exact):**

- **FAIL** if any `W*` (required for the tier, per §2) is FAIL.
- **FAIL** if any `A*` is ESCAPED.
- **FAIL** if either `LANDING` line is MISMATCH — **regardless of workload results**. R2.
- **FAIL** if any knob the tier requests is absent from the read-back (not requested==landed).
- **PASS** only when every required workload is PASS, every abuse case is CONTAINED, **and**
  both landing checks are OK.
- **SKIP** is reported explicitly and is never counted as PASS.

**Roll-up into `VERIFY-PASS`:** the battery's final line becomes `VERIFY-PASS` only if every
targeted tier reports `VERDICT: PASS`; otherwise the failing tier and case are printed and
the battery exits non-zero (the existing contract: 0 = all pass, 1 = ran and failed).

---

## 6. Explicit statements this spec makes (so a worker does not re-decide)

1. **A contained abuse case with an unlanded knob is a FAIL.** (R2, §4.)
2. **Assert the mechanism, not survival** — fork *refusal* (`pids.events`/kernel log), OOM
   *kill count* (`memory.events`), **who paid** for memory, and peer impact for IO.
3. **`standard` failing any required workload is release-blocking** — it is the
   "most-safe" failure mode the owner explicitly ruled out.
4. **Both enforcement points are checked**; one-sided landing is a FAIL.
5. **SKIP ≠ PASS**; a skipped tier is reported and blocks a full `--all-tiers` `VERIFY-PASS`.

## 7. What this spec does NOT decide (owner: GAP-122)

- The literal shell implementation and helper names → GAP-122.
- The exact peer-degradation bound for A3 and the wall-time bound for W5 → set in GAP-122
  from GAP-114's measurements.
- Whether `preset-battery.sh` is a new file or a section inside `e2e-full-battery.sh` → GAP-122
  picks, provided the `VERIFY-PASS` contract and exit codes are preserved.

## 8. Provenance

GAP-115, in service of the owner directive of 2026-09-20 (ship a preset system tuned for a
good experience). The workload and abuse sets encode the documented breakage classes from the
`bunker-agent-isolation` incident record; the landing check encodes R2 from the shadow-proc
kernel-wave security review; the invocation and host rules follow `AGENTS.md` and the existing
`e2e-full-battery.sh` contract.
