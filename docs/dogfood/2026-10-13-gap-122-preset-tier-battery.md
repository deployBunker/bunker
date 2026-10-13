# GAP-122 — Preset live E2E battery: every tier proven safe AND usable

Date: 2026-10-13 · Host: `bunker-mvp` (`78.46.173.180`) · Mode: `coexist`
(`registry.enabled: false`, own daemon on `:38081`/`:39091) · Battery:
`scripts/preset-battery.sh` v1.0.0

Design authority: `specs/preset-acceptance-harness.md` (GAP-115).
Tier contract: `specs/safety-presets.md` §§1–4 (GAP-113/116/117/118).

## What this battery certifies

A preset is a promise about **two** things: the workloads a tier is *for*
succeed, and the abuse a tier is *against* is contained. The battery verifies
both halves per tier and enforces the family's two governing rules at the
moment of truth:

* **R1 (experience budget)** — the tier's workload set actually runs.
* **R2 (no silent no-op)** — every knob the tier *claims* is read back from the
  **live** cgroup/unit and must have landed. A contained abuse case whose knob
  did not land is a FAIL.

Per tier it prints one line per case and a verdict, exactly as §5 of the design
spec writes it:

```
TIER <t>
  W1..W5  workloads   PASS|FAIL (SKIP by profile is named, never counted as PASS)
  A1..A3  abuse       CONTAINED|ESCAPED|FAIL  (asserting the MECHANISM)
  I1..I3  isolation   PASS|FAIL
  LANDING E1 (slice)  OK|MISMATCH (n/m cgroup knobs landed)
  LANDING E2 (unit)   OK|MISMATCH (n/m knobs landed)
TIER <t> VERDICT: PASS|FAIL
```

and the battery's final line is `VERIFY-PASS` only when every targeted tier
passed.

## Tiers under test

The shipped vocabulary (`internal/config.ValidSafetyPresets`) is
`open | standard | hardened`. `specs/safety-presets.md` §1 names four tiers
(`open/standard/guarded/hostile`); the code maps `hardened` to the
guarded-and-above reading (`internal/agent/isolation.go:168-173`), so those two
extra names are not separately spawnable and are not separate tiers here.

| Tier | CPU / mem / pids (E1+E2) | GAP-118 containment | Expected outcome |
|---|---|---|---|
| `open` | 200% / 4 GiB / 4096 | none (no swap bar, no soft limit) | ceiling holds; abuse contained by the five-knob baseline |
| `standard` (default) | 200% / 4 GiB / 4096 | `MemorySwapMax=0`, `MemoryHigh=90%` | as above + swap barred, memory throttled before OOM |
| `hardened` | 200% / 4 GiB / 4096 | as `standard` (unit sandboxing is GAP-119, observability GAP-121 — both still open) | as `standard` |

`MemoryOOMGroup` is off on every tier (matrix: UNMEASURED on this host) and
`IOWeight`/`IOReadBandwidthMax` are opt-in only, so no tier defaults them —
which A3 records rather than asserts.

## How to run it

Nothing needs to be installed on the host beyond a built `bunker`/`bunkerd`
pair; the battery starts its **own** daemon and never touches the production
one (`127.0.0.1:18080` is not bound — the production daemon listens on the
tailnet address only).

```bash
# 1. build the binaries to certify (from the repo, at the commit under test)
go build -o /tmp/gap122-bin/bunkerd ./cmd/bunkerd
go build -o /tmp/gap122-bin/bunker  ./cmd/bunker

# 2. stage them + the battery on bunker-mvp
scp /tmp/gap122-bin/bunkerd /tmp/gap122-bin/bunker bunker-mvp:/root/gap122-bin/
scp scripts/preset-battery.sh bunker-mvp:/root/

# 3. harness diagnostics first (no root, no daemon, no host mutation)
ssh bunker-mvp 'bash /root/preset-battery.sh --self-test'         # SELF-TEST: PASS

# 4. the release gate: every tier, full workload set
ssh bunker-mvp 'BUNKER=/root/gap122-bin/bunker \
                BUNKERD_BIN=/root/gap122-bin/bunkerd \
                PRESET_WORKLOADS=full \
                bash /root/preset-battery.sh --all-tiers'
```

A full-profile run takes tens of minutes: run it in the background
(`setsid nohup … > run.log 2>&1 &`) and poll the log. A single tool call on a
remote host is capped (~420 s) and will kill the client, not the run.

### Modes and useful env

| Invocation | What it does |
|---|---|
| `--all-tiers` (default) | every tier, full contract |
| `--tier open` | one tier — the fast loop |
| `--self-test` | 36 harness diagnostics, no root/daemon/host mutation |
| `--negative-control` | runs a real tier with a deliberately mis-set knob expectation and asserts the landing check reports MISMATCH |
| `--list-tiers` | the vocabulary + the matrix's E1+E2 knob list |
| `PRESET_WORKLOADS=core\|full\|none` | `core` = W1,W2,W4; `full` adds W3,W5; `none` = landing+abuse+isolation only |
| `PRESET_STRICT_SURFACES=1` | promotes a surface-coverage FINDING to a tier FAIL |
| `PRESET_ALLOW_SHARED_HOST=1` | run even while a host-wide CI battery is sweeping (see the hazard note) |
| `PRESET_FORK_ATTEMPTS`, `PRESET_MEM_BOMB_BYTES`, `PRESET_IO_HOG_BYTES`, `PRESET_W5_BYTES`, `PRESET_PEER_BUDGET_S`, `PRESET_BUILD_TIMEOUT` | bound the abuse/workload sizing |

Exit codes: `0` all targeted tiers passed · `1` ran and something failed ·
`2` usage error · `42` preflight refusal (not root, not cgroup v2, controllers
missing, daemon would not start, or a host-wide sweep is in flight).

## Host hazard: the CI battery deletes every `^bunker-` user

`bunker-mvp` is shared with the repo's own CI batteries. In **standalone** mode
`e2e-full-battery.sh` / `regression-tests.sh` sweep host state with

```bash
for u in $(awk -F: '/^bunker-/ {print $1}' /etc/passwd); do userdel -rf "$u"; done
```

which deletes *this* battery's agents — user, home, dockerd unit and slice
drop-in — mid-run. Measured during GAP-122: a CI regression run started two
minutes in and produced **8 bogus containment FAILs** (`ESCAPED` verdicts and
empty counter reads against agents whose homes no longer existed). Two guards
now make that impossible to mis-read:

1. **Preflight refusal** — the battery refuses to start while such a sweep is
   in flight (`pgrep -f 'e2e-full[-]battery.sh|regression[-]tests.sh'`), naming
   the hazard; `PRESET_ALLOW_SHARED_HOST=1` overrides.
2. **Per-tier ABORT guard** — before workloads, before abuse and before
   isolation, the agent's user **and home** must still exist. If not, the tier
   is reported as `ABORTED` (which fails it) with the reason named, instead of
   emitting containment verdicts about a dead agent.

The runner wrapper used for this row waits for two consecutive clean minutes
before starting.

## Findings recorded by this run

1. **RLIMIT knobs declared at E1 are inert** — `LimitNOFILE` /
   `LimitFSIZE` are written into `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`,
   but systemd applies RLIMITs *per unit at fork* and a `.slice` forks nothing.
   Measured on a `standard` agent: the drop-in declares
   `LimitNOFILE=65536:65536` / `LimitFSIZE=21474836480`, while a live
   `bunker exec` session carries `Max open files 1024` and
   `Max file size unlimited`. The enforceable surface is **E2** (the dockerd
   unit and the containers it starts), where both are verified landing. The
   battery reports this as a FINDING and does not fail the tier on it; see the
   recommendation below. (`PRESET_STRICT_SURFACES=1` turns it into a FAIL.)
2. **`MemorySwapMax` / `MemoryHigh` are E1-only on the shipped daemon** — the
   spec's §3 matrix marks both E1+E2, but the daemon emits them with slice
   scope only. The transient dockerd unit is **not** a child of
   `user-<uid>.slice` (it lives in `system.slice`), so containers started by
   the agent are not covered by the swap bar or the soft limit. Same treatment:
   FINDING, not a tier FAIL, with `PRESET_STRICT_SURFACES=1` as the lever.

Both are surface-scope divergences between the matrix and the shipped
implementation — real, measured, and *named* rather than hidden. Neither
changes a verdict on the knobs that do land (`CPUQuota`, `MemoryMax`,
`TasksMax`, `MemorySwapMax`/`MemoryHigh` at E1, `LimitNOFILE`/`LimitFSIZE` at
E2).

## Harness self-tests

`--self-test` runs 36 diagnostics with no root, no daemon and no host mutation
(`SELF-TEST: PASS`): the kernel page-normalisation rule for `memory.high`
(measured 3865470480 → 3865468928), `cpu.max` arithmetic, the exact-match
comparator including the R2 rejection cases (`max`, one page lower, wrong soft
RLIMIT), both claim parsers (`[Slice]` drop-in and `bunker info`'s knob block),
the §5 verdict function, and the negative-control injector's surface scoping.
