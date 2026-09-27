# BFS-044 — the client-side invalidation and hot-file config surface: acceptance evidence

**Row:** BFS-044 (P1, complexity 2) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Worktree:** `/home/kara/worktrees/bunker-BFS-044` on `wt/BFS-044`, base `eb2ecc4` — one branch, unpushed, nothing
landed on `main` by this row.
**Deliverable:** the surface BFS-037 reads. **No hot-file cache is built here** — no tracker, no queue, no
refresher, no single-flight map. This row supplies the knobs, their defaults, their validation and their
runtime readback; BFS-037 implements the subsystem against them.
**Normative sources:** `docs/prd/SPEC-hot-file-policy.md` (every hot-file number), `docs/prd/SPEC-push-channel.md`
(the reconnect/idle shape), `internal/fsmount/options.go` (the mount-flag pattern), BFS-031 (why an ENTRY bound
exists at all).

---

## 0. The decision, and the numbers

| # | the row asked for | what landed | evidence |
|---|---|---|---|
| 1 | **mount flags for the invalidation features and the hot-file knobs**, defaults = the SAFE ones the policies actually specified, not invented | **29 flags**: every knob `SPEC-hot-file-policy` §9.1 pins (25 hot knobs), plus `--cache-max-entries`, `--cache-max-inflight` and `--invalidate-idle-timeout`. Every default is the spec's own number, read from `fsclient.DefaultHotPolicy()` so help text, pflag default and the value the client obeys cannot drift. The feature itself defaults **OFF**. | §2; `BFS-044-flag-table.txt` |
| 2 | **a NEW ENTRY-COUNT bound alongside the byte bound**, because a byte bound alone does not bound a directory | `--cache-max-entries` (default **16384**), plumbed to `CacheConfig.MaxEntries` and enforced. The enforcement landed with BFS-038 inside the cache; what was missing was the surface, the plumbing, the validation and the proof — a `bunker fs status` could show `max_entries` while **no flag could change it**, which is BFS-032's shape one layer up. | §4; `BFS-044-arm-entry-bound-*.txt` |
| 3 | **THE HARDEST TEST: every invalidation feature off must still leave a CORRECT client** | Two arms with **every** invalidate/hot knob at its most degraded *legal* value: `all-off` and `all-off-nosnap` (the second also disables the one-call snapshot). **10/10 correctness cells PASS in both**, byte-identical reads, a 256 KiB streamed write hashing identically on both views, the refusal shape intact, and a mount-time misconfiguration reported rather than fatal. The third arm (`cadence-control`) is the control that **proves the battery can fail**: it is the same mount with the poll at 1 h, and **only** the change-propagation cell goes red. | §3 |
| 4 | **loud failure on an invalid value, no silent fallback** | **28 invalid values refused live**, each naming the flag, the value given and the value the spec pins, each citing the spec rule; a refusal happens **before any request** (`CLASSIFIED: refused at configuration time`). A reflection test zeroes every field of the default policy (both armed and unarmed) and requires a flag-naming, spec-citing refusal from each. The one exception is the spec's own (P-4's clamp), and it is reported as a clamp. | §2, §5 |
| 5 | **the effective values readable at runtime, matching what the mount obeys** | `Options.EffectiveConfig()` → the status document's new `config` block → printed by `bunker fs mount` at mount time and by `bunker fs status` afterwards, from the same resolved block. It carries the **derived** figures too (pool slots + the foreground's floor, P-4's clamp and whether it bit, the reservation ceiling, the decay half-life), which are the numbers a reader would otherwise have to compute. | §5 |
| 6 | **must not regress the read path or the one-call snapshot — give the fast-path cost as a number** | Requests are **identical** between the stock mount and a mount with every hot knob armed, for every operation the fast path rests on: `ls -l` 1 (the one-call snapshot for 200 files), again 0, `ls -lR` 1, `find` 1, `stat` 1, read-all-cold 201, read-all-warm 1 (the channel's poll), idle window 4. The one cell that differs (`read ONE file, warm`: 1 stock vs 0 armed) is the poll landing inside one measurement window and not the other — §6 says so rather than rounding it away. No read-path file was touched. | §6 |
| 7 | **must not make any feature default-on in a way a stock client or an old script notices** | The diff changes **no server file** and no wire shape, and the hot path is off by default. The armed-vs-stock fast-path request counts above are the measured form of the claim. | §6, §8 |
| + | **a REGRESSION the probe found, and what was done about it** | Shrinking the **cache** alone (`--cache-max-entry-bytes 1024` — an existing flag, unrelated to this row) made the mount **exit 1** naming `--hot.max-file-bytes`, a knob the operator never set, for a feature that is off. Fixed per **S-11** ("the mount still works; the hot path is inert and reports itself as such") by splitting validation into RANGE failures (always refused) and RELATION failures (refused when the hot path is **enabled**, **reported** when it is not), with the state and both numbers in the status document and on the mount banner. Kept as a test in three places. | §7 |

**Files added** — `internal/fsclient/hotpolicy.go`, `internal/fsclient/hotpolicy_test.go`,
`internal/fsmount/options_test.go`, `internal/cli/fs_hot_flags_test.go`,
`docs/evidence/BFS-044-probes/{mount-arm,cells,census,fastpath,run-arms,flagtable}.sh` and this bundle.
**Files changed** — `internal/fsclient/status.go` (the additive `config` block), `internal/fsmount/options.go`
(the option fields, the bounds, the effective-config accessors), `internal/fsmount/fs_linux.go` (the plumbing),
`internal/cli/fs.go` (the flags, the conversions, the printer).

---

## 1. How to reproduce

```
go build -o bin/bunker ./cmd/bunker
go build -o bin/davserve ./probes/davserve
W=$(mktemp -d -t bfs044-XXXXXX)          # never rm -rf; a fresh work dir per run
bash docs/evidence/BFS-044-probes/run-arms.sh --bin $PWD/bin/bunker --work "$W" \
  --arms "defaults all-off all-off-nosnap cadence-control entries-4 entries-max byte-1k misconfigured"
bash docs/evidence/BFS-044-probes/run-arms.sh --bin $PWD/bin/bunker --work "$W" \
  --arms "fastpath fastpath-armed"
bash docs/evidence/BFS-044-probes/flagtable.sh --bin $PWD/bin/bunker > docs/evidence/BFS-044-flag-table.txt
go test ./internal/fsclient/ ./internal/fsmount/ ./internal/cli/ -count=1
```

Every arm gets a **fresh** run directory and a **freshly generated** served tree, so nothing is inherited; every
process is killed by explicit PID (never `pkill -f`, whose pattern matches the shell running it); every
`fusermount` is bounded with `timeout`; the reader's **exit code** is the verdict, not a transcript somebody has
to read. The arms are the ones the raw evidence files are named after:

| arm | what it is | verdict |
|---|---|---|
| `defaults` | the stock mount | PASS |
| `all-off` | every invalidate/hot feature at its most degraded legal value | **PASS** |
| `all-off-nosnap` | the same **+ `--no-snapshot`** (every directory read falls back to PROPFIND) | **PASS** |
| `cadence-control` | stock, but the poll pushed to 1 h | FAIL **by design** — the propagation cell only |
| `entries-4` / `entries-max` | the same byte bound, two entry bounds | PASS |
| `byte-1k` | a 1 KiB byte bound (BFS-031's premise) | PASS |
| `misconfigured` | a pre-existing flag combination the hot path cannot honour | PASS (mount works, reported) |
| `fastpath` / `fastpath-armed` | the cost of the fast path, off vs armed | PASS |

---

## 2. THE FLAG TABLE — name, default, range, failure mode

Every default below is the number `SPEC-hot-file-policy.md` pins; the spec rule is named at the constant in
`hotpolicy.go` and asserted in `TestHotPolicyDefaultsAreThePinnedNumbers`. The rendered defaults come from the
binary's own `--help` (`BFS-044-flag-table.txt`), so a drift in either direction changes the evidence.

### 2.1 The pre-existing surface, extended

| flag | default | range | failure mode |
|---|---|---|---|
| `--cache-max-entries` | `16384` | `>= 1` (0 = default) | **refused**: `--cache-max-entries must be >= 1 (got -1); 0 means the default 16384. A byte bound alone does not bound a directory (BFS-031), so there is no value of this flag that turns the entry bound off` |
| `--cache-max-inflight` | `2` | `>= 1` (0 = default) | **refused**, naming both the value given and `2` (BFS-038: the width is what makes the staged reservation a bound) |
| `--invalidate-idle-timeout` | `0` = **derive** from the heartbeat period the server declares | `>= 0` | **refused** on a negative; `0` is not "unset", it is the Invalidator's documented sentinel (BFS-041 §8.1) and the **armed** deadline is reported as `invalidation.idle_timeout_ms` |
| `--concurrency` | `25` | `>= 1` | **refused** on a negative (unchanged otherwise) |
| `--poll-interval` | `2s` | `> 0` | **refused** on a negative (unchanged otherwise) |

### 2.2 The hot-file policy (all 25 knobs, `--hot.*`)

| flag | default | range | failure mode |
|---|---|---|---|
| `--hot.enabled` | **`false`** | bool | — (see §8 for why off is the safe default) |
| `--hot.read-weight` | `1` | positive, finite | **refused**, cites H-1 / Appendix A.1 |
| `--hot.edit-weight` | `8` | positive, finite, **> read-weight** | **refused**, names both weights, cites H-2 |
| `--hot.decay` | `0.9` | `(0, 1]` — **`1.0` is legal** and is AC-1's no-decay control arm | **refused**, cites H-3 |
| `--hot.decay-step` | `5m0s` | `> 0` | **refused**, cites H-3 |
| `--hot.score-ceiling` | `10000` | `> 0` | **refused**, cites H-6 |
| `--hot.read-touch-window` | `5s` | `> 0` | **refused**, cites H-9 / F-6 |
| `--hot.flush-interval` | `30s` | `> 0` | **refused**, cites H-21 |
| `--hot.max-entries` | `4096` | `>= 1` | **refused**, cites H-7 (the tracker is bounded in entries **and** bytes) |
| `--hot.max-tracker-bytes` | `1048576` | `>= 1` | **refused**, cites H-8 |
| `--hot.max-file-bytes` | `8388608` | `>= 1`, **≤ the per-entry cap** and **≤ the cache bound** | **refused** (S-10 / S-9) naming both numbers, **when the hot path is enabled**; reported as `misconfigured` when it is not (§7) |
| `--hot.queue-depth` | `256` | `>= 1` | **refused**, cites Q-1 |
| `--hot.queue-max-wait` | `5m0s` | `> 0` | **refused**, cites Q-6 |
| `--hot.max-concurrent-refresh` | `2` | `>= 1`; **above the pool share it is CLAMPED AND REPORTED**, never refused | **refused** only at 0, cites Q-7; the clamp is `refresh_max_inflight_clamped` |
| `--hot.pool-share` | `1/8` | a fraction, numerator `>= 1`, denominator `>= 1`, `<= 1`; **unparseable is refused** | **refused**, names the value (`3/2`, `0/8`, `eighth`), cites P-3 |
| `--hot.backoff-base-ms` | `250` | `> 0` | **refused**, cites P-10 |
| `--hot.backoff-max-ms` | `30000` | `> 0`, `>= base`, **≤ the client's 30 s op deadline** | **refused**, names both numbers, cites P-10 / A.11 |
| `--hot.backoff-factor` | `2` | `>= 1` | **refused**, cites P-10 |
| `--hot.backoff-jitter` | `full` | `full` \| `none` | **refused**, names the value and both legal ones, cites P-10 / BFS-041's H-8 |
| `--hot.refresh-deadline` | `10s` | `> 0`, **≤ the op deadline** | **refused**, names both, cites P-11 / A.8 |
| `--hot.reacquire-window` | `30s` | `> 0` | **refused**, cites P-12 |
| `--hot.yield-after` | `250ms` | `> 0` | **refused**, cites P-7 |
| `--hot.stop-deadline` | `1.5s` | `> 0`, **≥ tick + yield** | **refused**, names all three numbers, cites Q-12 / A.6 |
| `--hot.tick-interval` | `1s` | `> 0` | **refused**, cites Q-14 |
| `--hot.pool-pressure-ticks` | `5` | `>= 1` | **refused**, cites P-19 |

### 2.3 Reported but NOT configured (the derived half)

These have no flag because they are **computed** from the configured ones, and they are reported because a
derived bound nobody prints is the same defect one layer down (PRD §2.7):

| figure | at the defaults | from |
|---|---|---|
| `pool_slots` / `pool_slots_foreground` | **3** / **22** | `max(1, floor(Concurrency × share))` — P-3 |
| `refresh_max_inflight` + `…_clamped` | **2**, `clamped=false` | `min(cap, pool_slots)` — Q-7 / P-4 |
| `max_inflight_bytes` | **16777216** (16 MiB) | `refresh_max_inflight × hot_max_file_bytes` — Q-9 / A.5 |
| `half_life_ms` | **1973644** (≈32m54s, the spec prints 1973.6 s) | `step × ln ½ / ln decay` — A.2 |
| `queue_replacement` | **`score`** | Q-3: the queue displaces by SCORE, never FIFO |
| `size_rule_inclusive` | **`true`** | S-2: a file exactly at the ceiling IS refreshable |
| `config_state` | `disabled` \| `enabled` \| `cache_disabled` \| `misconfigured` | what the CONFIG resolved to (the runtime states armed/disarmed/stopped are BFS-037's, §5.5 Q-15) |

---

## 3. THE HARDEST TEST — every invalidate/hot feature off, still a correct client

`BFS-044-arm-all-features-off.txt`, `BFS-044-arm-all-off-no-snapshot.txt`,
`BFS-044-arm-cadence-control.txt`. The degraded configuration is **legal on purpose** (`--invalidation poll`,
the hot path off, every hot bound at the smallest value that validates: tracker 1 entry / 1 byte, size rule
1024 B, queue depth 1, one refresh, share 1/64, backoff 1 ms with no jitter, deadlines at their minimums) —
the point is to switch every optional behaviour off *inside* a valid configuration, not to break it.

| cell | `defaults` | `all-off` | `all-off-nosnap` | `cadence-control` |
|---|---|---|---|---|
| `read-cold` (40/40 byte-identical) | PASS | PASS | PASS | PASS |
| `read-warm` (the cache path) | PASS | PASS | PASS | PASS |
| `walk` (population + every size) | PASS | PASS | PASS | PASS |
| `propagation` (an out-of-band edit reaches the reader) | PASS **1079 ms** | PASS **16 ms** | PASS **15 ms** | **FAIL — and that is the point** |
| `write-create` (publish visible on both views) | PASS | PASS | PASS | PASS |
| `write-refused-overwrite` (the O_TRUNC shape: refused **and** non-destructive) | PASS | PASS | PASS | PASS |
| `write-large` (256 KiB streamed, sha256 equal on both views) | PASS | PASS | PASS | PASS |
| `read-after-write` | PASS | PASS | PASS | PASS |
| `unlink` (reaches the served tree) | PASS | PASS | PASS | PASS |
| `cache-census` (entries ≤ the reported entry bound) | PASS | PASS 2/4 | PASS 0/4 | PASS 41/16384 |
| **verdict** | **PASS** | **PASS** | **PASS** | FAIL (1 cell) |

**Why the failure is the proof, not a defect.** `cadence-control` is the stock mount with the poll at one hour:
every cell that asks "are the bytes right" still passes, and the **only** cell that changes is the one that
measures *when* a change becomes visible — which is the invalidate mechanism's declared cadence, not
correctness. Without that arm the propagation cell's green elsewhere would be unfalsifiable. (And it is the
reason `--invalidation` has no `off` value: a knob that could switch correctness off would make correctness a
function of a knob. What a knob *can* do is move the staleness window, which is bounded by the poll period and
the cache's own backstop TTL — and both are reported.)

**A note on the write cells, measured rather than assumed.** This surface supports a *new* path (`>`), a
streamed large file, and unlink; it **refuses** an in-place overwrite of an existing file with `EOPNOTSUPP`
(BFS-030's rule: an `O_TRUNC` half whose write half this surface cannot complete). Cells for shapes it refuses
would measure the refusal, not correctness, so the refusal is one explicit cell of its own — and it proves the
write half of the battery is not vacuous: `the O_TRUNC overwrite was refused and f002.txt is still
3fd4bbcb…`. In every arm the byte content of every file is identical before and after.

---

## 4. THE ENTRY-COUNT BOUND — counted on disk, not asserted from a report

BFS-031's finding is the premise: at a 1 KiB **byte** bound the cache directory reached **30,689 B** while the
client's own figures stayed inside it, so a byte bound alone does not bound a directory. BFS-044's answer is the
second bound. Three arms, all reading the same 40-file fixture through a real mount, all counting the
filesystem rather than the client's report:

### 4.1 The entry bound is the binding one (same byte bound, two entry bounds)

`--cache-max-size` held at its **default 268435456 B** in both arms; the only difference is the flag under test.

| figure | `--cache-max-entries 4` | `--cache-max-entries 16384` |
|---|---|---|
| reads that were byte-identical | 40/40 | 40/40 |
| **entry bound the client REPORTS** (`max_entries`) | **4** | 16384 |
| entries held | **4** | 40 |
| **blobs actually on disk** | **4** | **40** |
| **index records actually in `index.json`** | **4** | **40** |
| **directory size (`du -sb`)** | **4569 B** | **11676 B** |
| used_bytes (the reported byte figure) | 813 B | 7905 B |
| evictions | 36 | 0 |

The byte bound was **never** the binding one (813 B and 7905 B against 268435456 B — three orders of magnitude
of slack), yet the directory shrank by a factor of 2.6 with the entry bound and the on-disk census moved in
lockstep with the flag: **4 vs 40 blobs, 4 vs 40 index records**. The flag reached the cache (the reported
`max_entries` is 4), and the enforcement bounds the directory (the on-disk count is 4). That is the entry-count
proof, counted.

### 4.2 The premise, measured at this head: a byte bound alone does not bound a directory

`BFS-044-arm-byte-bound-1k.txt`: `--cache-max-size 1024`.

| figure | value |
|---|---|
| the client's reported byte figure | **1010 B** (inside its 1024 B bound) |
| the directory's real size (`du -sb`) | **4967 B** = **4.9×** the bound |
| bytes outside the byte accounting | **3957 B** (index + `status.json` + `conflicts.jsonl`) |
| reads | 40/40 byte-identical |

This is BFS-031's class reproduced at the current head, and it is the **premise** for the entry bound rather
than a repair: repairing the directory accounting (`status.json`/`conflicts.jsonl` outside `used_bytes`) is
BFS-031's row, and this bundle claims nothing about it. What it does show is why an entry bound belongs
alongside the byte bound: the byte bound can be satisfied while the directory grows.

### 4.3 The bound is always in force

`--cache-max-entries 0` is not "no bound" — it is the default, and a negative is refused (§2.1). There is no
value of the flag that removes the bound, and the census that proves it is the same census that moved with it.

---

## 5. THE EFFECTIVE VALUES, readable at runtime and matching behaviour

The status document carries a new `config` block (`internal/fsclient/status.go`), resolved by
`Options.EffectiveConfig()` — the same call the mount uses, not a second copy of the numbers written out for
display. It is printed by `bunker fs mount` at the moment of mount and by `bunker fs status` afterwards.

The `all-off` arm's own banner, with the hot path off and every bound degraded, straight from
`BFS-044-arm-all-features-off.txt`:

```
  config       : cache 268435456 B / 4 entries (entry cap 1024, staged 2, age 1h0m0s)
  invalidation : mode=poll poll_interval=2s idle_timeout=derived from the declared heartbeat
  hot policy   : enabled=false state=disabled share=1/64 slots=1 (foreground 24) refresh_inflight=1 clamped=false reserve=1024 B
  hot weights  : read=1 edit=8 decay=1 per 1s (half-life no decay)
  hot bounds   : tracker 1 entries / 1 B, size<=1024 B (inclusive=true), queue 1 (score, wait 1s), ceiling 1
  hot timing   : tick=100ms yield=1ms stop_deadline=150ms refresh_deadline=1s reacquire=1s touch_window=1s flush=1s
  hot backoff  : 1ms x1 cap 1ms jitter=none, pressure_ticks=1
```

and the `fastpath-armed` arm's, with the feature armed and every knob moved off its default — including the
derived figures:

```
  hot policy   : enabled=true state=enabled share=1/4 slots=6 (foreground 19) refresh_inflight=4 clamped=false reserve=16777216 B
  hot weights  : read=1 edit=8 decay=0.85 per 200ms (half-life 1s)
  hot bounds   : tracker 2048 entries / 524288 B, size<=4194304 B (inclusive=true), queue 128 (score, wait 2m0s), ceiling 5000
```

**"Matching behaviour" is asserted where it can be checked**: the cache's own figures move with
`--cache-max-entries` (§4.1), the invalidator's armed deadline is reported in its own block, and the arm that
sets `--concurrency 8` resolves to `slots=1 (foreground 7)` while `--concurrency 25` resolves to `slots=3
(foreground 22)` — the derived arithmetic of P-3, computed from the value the mount actually runs with
(`internal/fsmount/options_test.go`, `internal/cli/fs_hot_flags_test.go`). A status document written before
this block existed prints one line saying so rather than a wall of zeros, because an unexplained null is the
thing BFS-045 keeps being filed about.

---

## 6. THE FAST-PATH COST — as a number, off and armed

`BFS-044-arm-fastpath.txt` / `BFS-044-arm-fastpath-hot-armed.txt` (200-file fixture, 804 KiB; the same harness
BFS-024 used, re-run on the BFS-044 tree). Requests are the client's own transport counter, read from the status
document; wall time is reported beside them but requests are the load-independent figure (the two runs saw
loadavg 7.58 and 11.82 respectively).

| operation | requests — stock | requests — every hot knob armed | wall (stock / armed) |
|---|---|---|---|
| `ls -l` (cold, first look) | **1** | **1** | 20 ms / 39 ms |
| `ls -l` (again) | **0** | **0** | 23 ms / 34 ms |
| `ls -lR` (a walk) | **1** | **1** | 18 ms / 30 ms |
| `find -type f` | **1** | **1** | 7 ms / 10 ms |
| `stat` one file | **1** | **1** | 9 ms / 11 ms |
| read ALL 200 files, cold | **201** | **201** | 176 ms / 378 ms |
| read ALL 200 files, warm | **1** | **1** | 101 ms / 166 ms |
| read ONE file, warm | **1** | **0** | 6 ms / 9 ms |
| idle 6 s window (the channel) | **4** | **4** | — |

Three numbers the row asked for, read off the table:

* **The one-call snapshot is one request for the whole directory**: `ls -l` cold costs **1** request for 200
  files, a walk costs **1**, and the second look costs **0** — unchanged from BFS-024's recorded figures.
* **A warm read costs 0 of its own.** The `1` beside "read ALL, warm" is the invalidation channel's poll
  landing inside the measurement window (the 6 s window at a 2 s cadence is 4 requests — the channel's cost is
  per **interval**, never per path). The cache served 201 hits, 0 misses.
* **The surface is inert until something reads it.** Every request count is identical between the two runs but
  one: `read ONE file, warm` measured `1` stock and `0` armed, which is the opposite of a regression — it is
  the poll landing inside one 1.6 s window and not the other, and the idle-window row (the poll's actual cost)
  reads **4 in both**. The wall times differ (176 ms vs 378 ms for the cold read-all) because the two runs ran
  at loadavg 7.58 and 11.82 respectively; requests are the load-independent figure and that is why the row's
  claim is stated in them. What the identity of those counts buys is the brief's requirement — *"must not make
  any feature default-on in a way that a stock client or an old script notices"* — measured rather than hoped:
  arming every knob adds **zero** requests to every operation the fast path rests on.

---

## 7. THE REGRESSION THE PROBE FOUND (and the fix)

While designing the entry-bound arms the probe ran `bunker fs mount … --cache-max-entry-bytes 1024` — an
existing flag, nothing to do with this row — and the mount **exited 1**:

```
bunker-fs: --hot.max-file-bytes (8388608) exceeds the cache's per-entry cap (--cache-max-entry-bytes 1024);
SPEC-hot-file-policy S-10 refuses the mount here, …
```

A caller shrinking the cache was refused by a knob they never set, for a feature that is **off**. That is
exactly what BFS-044's own brief forbids ("must not regress the existing mount flags"), and the spec already
says what to do — **S-11**: *"Neither refusal is a hot-path feature gate. The mount still works; the hot path is
inert and reports itself as such (`hot_state: "misconfigured"`, with the two numbers)."*

**The fix, in two classes.** Validation now separates

* **RANGE failures** — a knob that is not a value at all (a zero decay, a negative depth): refused **always**,
  armed or not, because an invalid number is invalid whether or not it is in use; from
* **RELATION failures** — two configured numbers that cannot both hold (S-9, S-10, A.6, A.8, A.11, and H-2's
  edit-vs-read weight): refused when the hot path is **enabled**, and otherwise **reported**, with both numbers,
  as `config_state: misconfigured` in the status document and a `MISCONFIG` line on the mount banner.

Both halves are exercised live: `BFS-044-arm-misconfigured.txt` mounts with `--cache-max-entry-bytes 1024`,
serves all 40 files correctly, and reports

```
  hot policy   : enabled=false state=misconfigured share=1/8 slots=3 (foreground 22) refresh_inflight=2 clamped=false reserve=16777216 B
  MISCONFIG   : bunker-fs: --hot.max-file-bytes (8388608) exceeds the cache's per-entry cap (--cache-max-entry-bytes 1024); SPEC-hot-file-policy S-10 refuses the mount here, …
```

while the same incoherence **armed** is still refused before any request (`--hot.enabled
--hot.max-file-bytes 999999999` → refused naming both numbers). The guard is a test in three places so it
cannot come back: `TestHotPolicyRelationFailuresAreReportedWhenDisabled` (fsclient),
`TestNormalizeDoesNotRefuseAPreExistingFlagCombination` (fsmount),
`TestFSMountAcceptsAPreExistingFlagCombinationAndReportsWhy` (cli).

---

## 8. WHAT THIS ROW DID NOT DO, and the boundaries it kept

* **No hot-file cache, tracker, queue, refresher, single-flight map or status counters for them.** Those are
  BFS-037's, which depends on this row; this row supplies the surface it reads. `grep -rni 'hot\.json' --include=*.go`
  finds nothing but the surface's own doc comments.
* **No server file, no wire change, no new request type** (D-10). The diff touches `internal/fsclient`,
  `internal/fsmount` and `internal/cli` only, so a stock WebDAV client and the surface's own verb matrix see
  byte-identical behaviour.
* **No read-path change**: no file under `internal/fsmount`'s read path gained a branch; the only new work on
  the mount is the printed block and the option plumbing. §6's request counts are the measurement.
* **No default-on feature.** `--hot.enabled` defaults to **false**: the subsystem is performance-only (P-0), its
  value is unmeasured (O-1's `no_measured_benefit` exists to say so), and an existing `bunker fs mount`
  invocation must behave identically. Arming it is one flag on a policy that has already been validated.
* **`.coding-hermes/board/**` and `.gitreins/tasks.yaml` untouched** — this row reports; it does not file.
  Nothing was pushed.
* **BFS-031 and BFS-032 are not repaired here.** §4.2 reproduces BFS-031's shape as a *premise* and says so;
  what this row owes them is that the new surface does not add a second unaccounted file and that no counter in
  it can be unreachable (the entry bound's figure moves with its flag, counted in §4.1).

### Residuals, reported rather than smoothed over

1. **S-9's arm is only reachable in an exotic configuration.** With the cache bounds the mount actually
   derives (`CacheMaxEntryBytes ≤ min(CacheMaxBytes, 64 MiB)`), a size rule above the cache bound is *also*
   above the per-entry cap, so S-10 reports first and S-9 never speaks. S-9 fires only when a caller sets
   `--cache-max-entry-bytes` **above** `--cache-max-size` (an explicit override `Normalize` allows). Both arms
   are covered by tests and both messages are in the live transcript; the ordering (tighter bound first) is
   deliberate, and the alternative — reporting S-9 when S-10 is the binding one — would name the less useful
   pair of numbers. Not filed; it is a property of the derivation, not a defect.
2. **`--no-snapshot` disables caching entirely.** Measured: with the snapshot off, a mount that reads 40 files
   stores **0 entries** (independent of this row — it is the pre-existing client's behaviour, reproduced in the
   `all-off-nosnap` arm's census `entries=0`). Correctness is unaffected (all cells pass), and this row
   neither introduces nor repairs it. Worth a row of its own one day: a client that silently stops caching when
   the snapshot is off is the "reported one way, enforced another" family.
3. **The write path's supported shapes are narrow** (create / stream a large file / unlink; an in-place
   overwrite is refused with `EOPNOTSUPP`). That is BFS-021/BFS-030's ground, stated here only so the cell set's
   scope is explicit: the cells cover every shape this surface supports, plus the refusal.
4. **`--invalidate-idle-timeout`'s `0` means "derive".** It is the invalidator's own documented sentinel and
   the armed value is reported, but a reader who expects `0` to mean "no deadline" is wrong — the mount banner
   and the status block say `idle_timeout=derived from the declared heartbeat` in words for exactly that
   reason.
