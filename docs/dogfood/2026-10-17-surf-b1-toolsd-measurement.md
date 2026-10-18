# SURF-B1 — toolsd invocation ledger measurement (MEASURE FIRST)

**Date:** measured 2026-10-17 evening → 2026-10-18 02:00 UTC (all timestamps UTC unless marked -05:00).
**Task:** SURF-B1 — *toolsd invocation ledger analysis* (the measurement-first half of `ADR-toolsd-integration.md` §"Revival triggers" / `SPEC-toolsd-agent-surfaces.md` §3.6).
**Blast radius:** read-only analysis + this document. Zero code changes. `go build ./...` re-verified after the measurements (§7).
**Verdict up front: an in-agent toolsd service is NOT justified on today's data** — measured per-call transport share is ~1.21 s, which puts the ADR Trigger-2 break-even at ~590 calls/day (with a 10× margin gate: ~5,900 calls/day), while the ledger's only caller-tagged session made 78 calls in 3.3 minutes and the ledger contains **zero** `read`/`write`/`list` invocations in 28 days. Numbers below; both revival triggers fail their limbs.

---

## 1. What was searched (ledger sources)

| Source | Result |
|---|---|
| **CHT-038 ledger, this box** (`~/.local/state/toolsd/usage.jsonl*`) | **FOUND — primary dataset.** 4 files (live + 3 rotated), 290 invocation records, 2026-09-20 → 2026-10-17. This is the sink the CHT-037 chain resolves to (CHT-037 = the config-layer row that ships `toolsd config`; CHT-038 = "one silent usage record per toolsd invocation … ON by default", both `complete` in `~/coding-hermes-tools/.coding-hermes/board/tasks.jsonl`). |
| `usage.jsonl*.probe-noise-2026-09-23` files | Excluded from the census (named probe noise, quarantined at rotation time). |
| **Agent-side copies** (orphaned agent homes via `bunker homes`) | **EMPTY** — `entries scanned: 0, stale: 0, kept: 0` on the daemon host (78.46.173.180). No historical agent-side ledgers survive agent destruction; a destroyed agent's ledger dies with its home. |
| **bunker-mvp host itself** (SSH as root) | `toolsd: command not found` — no host-level toolsd, no host-level ledger. |
| Other bunkerd servers (bunker-las-01..04) | No live or stopped agents (`bunker list --status all` empty on all). |

Consequence worth recording: **the only durable toolsd ledger in the fleet is the Hermes-side one**, because the caller (this box) is where toolsd runs today. Agent-side ledgers are ephemeral by construction until GAP-096 delivery makes agents first-class toolsd callers.

## 2. The ledger census (CHT-038, 290 real invocations)

Window: **2026-09-20T03:07:35Z → 2026-10-17T20:37:11-05:00** (28.0 days).

Per-verb invocation counts (all 290):

```
config 79   mcp 33   diff3 27   patch 18   apply 18   replace 18   lease 18
session 17  narrate 16   describe 10   version 9   help 9   probe 8
serve 5     lsp 5
read 0      write 0      list 0          <-- the fsops verbs: ZERO records
```

Outcomes: 230 `ok`, 60 `error` (all errors ≤ 102 ms; they are docs-fixture refusal paths — `apply` on bad-edits JSON, `patch` on broken diffs, `replace` on absent needles, `diff3` conflict exits, `config` invalid-root probes — i.e. deliberate negative-case fixtures, not production failures).

**Caller-tag distribution (the ledger's caller field):**

| caller tag | records | share |
|---|---|---|
| `null` (untagged CLI drives: docs examples, describe/registry walks, smoke checks) | 212 | 73.1% |
| `lane-seven` (one `--caller`-tagged session, 2026-09-20T03:07:35Z → 03:10:52Z) | 78 | 26.9% |

**Calls-per-session distribution — what the real data supports:**

- Exactly **one** caller-tagged session exists in 28 days: `lane-seven`, **78 invocations in 196.6 s** (0.40 calls/s sustained peak), verb mix `diff3×12, patch×8, apply×8, replace×8, lease×8, session×7, narrate×6, mcp×6, version/help/describe×4 each, probe×3` — a smoke/exercise pass over the surface (every verb × ~4), **not an editing workload**. toolsd's own in-process time for the whole session: **439 ms total** (sum of `duration_ms`).
- The untagged majority (212 records) has no session identity to group by; it is docs-example and smoke traffic.
- Therefore the honest distribution statement is: **one real tagged session at N=78 calls / 3.3 min; zero tagged editing sessions; and no `read`/`write`/`list` traffic anywhere in the ledger** — the exact verbs Trigger 1 names as the polling-volume signal do not occur at all yet. This is a data-gap finding, not a negative result: agents have never been toolsd callers in production (GAP-096 delivery is still pending), so the ledger measures the tool being *exercised*, not *used*.

## 3. Per-verb latency: local vs through-exec (live measurement)

The ledger's `duration_ms` is toolsd's **in-process** time only. The ADR's Trigger 2 requires two costs kept separate: (1) wall clock through `bunker exec` **including SSH round trip + process start**, (2) local invocation of the same binary with no transport. Both were measured live on 2026-10-17/18:

- **Method:** fresh vanilla agent `surfb1-measure` on bunker-mvp (`--ttl 2h`), toolsd delivered agent-side via the real path (`bunker cp` of a `CGO_ENABLED=0` static build, `chmod +x`), fixtures under `~/surfb1/` (f.txt / d.diff / f.orig). ARM A: 20 timed calls per verb from this box via `bunker exec … -- sh -c 'cd ~/surfb1 && toolsd <verb>'`, wall clock around the whole call. ARM B: 50 timed calls per verb executed **on the agent** (same binary, no transport), wall clock around each `execve`. Identical fixtures and verb grammar both arms. Agent destroyed afterward (`destroy` archived + verified, homes clean) — nothing left running.
- **Correction on record:** the first ARM A pass invoked the verbs from the agent's HOME instead of the fixture dir, so `read`/`replace` took their error paths (wall clock valid, success-path cost understated). Run 2 (reported here) cd's into the fixture dir; the agent-side CHT-038 ledger confirms run-2 verbs all landed `outcome:"ok"` with in-process `duration_ms` of 0–5 ms. Both runs' exec floor agreed (medians 1196 vs 1218 ms), so the correction moved nothing material — it is recorded because the first run's ledger rows are visibly `read … "outcome":"error"`.
- **Binary identity:** `~/coding-hermes-tools` @ `2ed8e53` (HEAD), static ELF, `toolsd version dev` (built without the ldflags stamp — a build fact, not a failure).
- **Host-load caveat:** measurements taken during active fleet load; ARM A p90/max show occasional multi-second scheduler spikes (max 7.5 s). Medians are the comparison basis.

**Per-verb latency table (median wall clock; ARM A n=20 through-exec, ARM B n=50 local):**

| verb | local, agent host (ms) | through `bunker exec` (ms) | transport share (ms) |
|---|---:|---:|---:|
| `list .` | 11.7 (p90 15.4) | 1239.3 | 1227.6 |
| `read f.txt` | 11.7 (p90 15.3) | 1242.8 | 1231.1 |
| `write g.txt` (stdin pipe) | 15.3 (p90 17.7) | 1240.6 | 1225.3 |
| `replace f.txt a b` | 15.2 (p90 18.5) | 1242.3 | 1227.1 |
| `patch d.diff` (+`cp` reset) | 17.8 (p90 20.7) | 1255.1 | 1237.3 |
| `version` (floor ref) | 13.3 | 1224.5 | 1211.2 |
| `/bin/true` — bare process floor | 7.1 | 1217.8 (exec floor) | ~1211 |

Full stat blocks (min/med/p90/max/mean) for both arms:

```
ARM B local (n=50, ms):            ARM A through-exec (n=20, ms):
true     6.5/ 7.1/ 7.9/ 8.2/ 7.2   exec_true    1154/1218/2756/7481/1748
version  9.7/13.3/15.5/18.9/13.2   version      1146/1225/1263/1281/1223
list     9.2/11.7/15.4/18.5/12.4   list         1179/1239/1260/7321/1630
read     8.7/11.7/15.3/18.4/12.1   read         1193/1243/1276/3946/1501
write   11.6/15.3/17.7/21.5/15.4   write        1137/1241/1305/7412/1674
replace 11.1/15.2/18.5/20.6/15.5   replace      1149/1242/1315/3907/1476
patch   11.8/17.8/20.7/29.0/17.6   patch        1169/1255/1279/1317/1246
(format: min/med/p90/max/mean)
```

**Reading of the table:**

- **Transport, not process, is the cost.** toolsd's own in-process time is 0–5 ms per verb (agent-side ledger during ARM A); the local full-verb cost is 12–18 ms (≈7 ms of that is bare process start, measured with `/bin/true`); the through-exec wall clock is ~1.22–1.26 s. The SSH+daemon+CLI round trip contributes **~1.21 s per call — ≈99% of the through-exec cost** — and a socket service would remove essentially all of it, exactly as Trigger 2 assumes.
- **The caveat that keeps this honest:** the tunnel is same-host (Tailscale to the co-located daemon), so 1.21 s is the *floor* transport cost. An inter-continent leg only increases it. Conversely, no tuning of the local side can go below ~7 ms (process start) — the service's own floor.
- Trigger-2 arithmetic is unaffected by which verb: verb choice moves the total by ±18 ms against a ±1211 ms transport term.

## 4. Threshold statement (the deliverable)

The ADR's decision rule: a service pays for its fixed carry cost only when `measured per-session call volume × measured saved per-op overhead > the service's fixed carry cost`.

With **S = saved per-op overhead = 1211 ms** (transport share above; the exec floor's 7–13 ms process start survives any service) and a one-time build cost for the HTTP-shaped surface (SPEC §3.4 audit continuity + lifecycle + contract pinning) estimated conservatively at **3 engineer-days**, amortized over one year:

- Carry cost ≈ 3 × 86400 s ÷ 365 ≈ **710 s/day** of editing time to repay.
- **Break-even: N × 1.211 s > 710 s/day ⇒ N > ~587 calls/day** of exec-delivered toolsd traffic, sustained.
- Applying a 10× ongoing-margin gate (a service that merely breaks even is not worth its lifecycle): **~5,900 calls/day**.

**Pinned threshold, in the form the ADR demands:** *"An in-agent toolsd service is justified when measured exec-delivered toolsd call volume exceeds ~590 calls/day at break-even, or ~5,900 calls/day at a 10× margin — with S = 1.21 s/call measured on the same-host tunnel (inter-host legs raise S, never lower it). Below that, every saved second is noise against the audit-continuity and lifecycle carry cost."*

Per-session form (for the ledger's session framing): a session must make **≥ ~590 calls per working day-of-such-sessions** to break even — i.e. sustained multi-minute sessions at the `lane-seven` peak rate (0.40 calls/s) for ~25 minutes of every hour, all day, would just reach break-even. No observed session is close.

**Verdict against both triggers, on the real numbers:**

- **Trigger 1 (subscription/push): FAILS.** Zero `read`/`write`/`list` invocations in 28 days ⇒ zero polling volume to amortize; no missed-update incidents recorded anywhere in the fleet docs.
- **Trigger 2 (chattiness × overhead): FAILS.** Measured overhead is large (1.21 s/call — 100× the local cost), but measured volume is ~78 calls in one 3.3-minute session over 28 days ≈ 2.8 calls/day ≈ **3.4 s/day of recoverable time — three orders of magnitude below break-even.**

**"Not justified yet" is the verdict, and the gap is a delivery gap, not a performance one:** agents are not yet toolsd callers (GAP-096), so the volume input to the rule does not exist. The number to watch is real agent editing traffic after GAP-096 lands — until a real workload pushes N toward hundreds of calls/day, the exec path (1.25 s/call, audited, authenticated, zero carry cost) is the right surface, and the two-unit-file MCP socket recipe (SURF-001) already covers the no-new-code alternative.

## 5. What would change the verdict (re-measure conditions)

1. **GAP-096 lands** (toolsd delivered in agent images) and real agent editing sessions appear → re-run §2's census on agent-side ledgers (and give the caller tag a session id so per-session distributions are groupable — today only one of 290 records carries any tag).
2. Sustained N > ~590 calls/day across a week of real work, **or** any missed-update incident under polling (Trigger 1's second limb).
3. Any inter-host agent placement — re-measure S first; it only grows.

## 6. Honest limits of this measurement

- n=20 (through-exec) / n=50 (local) on one fresh agent, one host pair, under fleet load (see ARM A max spikes). Medians are robust to the spikes; the tails are not characterized for p99.
- Single tagged session ⇒ the "calls-per-session distribution" is one real point (78) plus an untaggable majority — stated as a data gap, not papered over.
- The 3-engineer-day carry-cost estimate is an assumption (the ADR names the work but no sizing); break-even scales linearly with it (×2 estimate ⇒ ×2 break-even). The 10× margin gate exists precisely to keep the verdict negative even if the estimate is 3–5× too high.
- Raw run data: ARM A `/tmp/surfb1-arm-a2.json`, ARM B embedded above and re-derivable from the recorded method; the census is re-derivable from `~/.local/state/toolsd/usage.jsonl*` in one jq/python pass.

## 7. Acceptance criteria check

```
$ grep -c 'per-verb latency' docs/dogfood/2026-10-17-surf-b1-toolsd-measurement.md
1
$ # table with >= 3 verbs (read/write/list all present), both arms: §3 table (5 verbs)
$ # calls-per-session distribution from real ledger tags: §2 (78 / 196.6 s; 26.9% vs 73.1%)
$ # explicit threshold statement: §4 pinned threshold
$ cd <repo> && go build ./...
(exit 0)
```

---
ch:trace: SURF-B1
