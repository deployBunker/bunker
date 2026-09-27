# BFS-043 — the server-side invalidation config surface: acceptance evidence

Row: **BFS-043 (P1)** — the watcher and push knobs, their defaults and ranges, the refusal
path for a value that cannot be honoured, and the runtime read-back.
Design authority: `docs/prd/SPEC-watcher-capability.md` §4.2 (the ceiling arithmetic and the
headroom), §8.2 (the `extensions.watch` block), §9 (the interface this row was handed:
"watcher/push knobs, defaults, validation, the refusal path for an unhonourable value"), and
`docs/prd/SPEC-push-channel.md` §4.4 (the declared relations), §5.3, §6.1, §6.4, §7.2.
Status: the surface is implemented, wired, and reported. Branch `wt/BFS-043`; **nothing pushed**;
the board and `.gitreins/tasks.yaml` were not touched by this worker.

Raw artifacts, all in this directory:

| file | what it is |
|---|---|
| `BFS-043-wire-sample.txt` | the verbatim carriers an operator reads, from a runnable cell (`go test -run TestInvalidationWireSampleForEvidence -v ./internal/server/webdav/`) |
| `BFS-043-cells-green.txt` | the GREEN run of the whole `internal/server/webdav` package with the new cells (`-v`, 80 top-level cells) |
| `BFS-043-mutation-A-red.txt` | negative control A: the read-back reports the CONFIG instead of the running watcher ⇒ 1 cell red |
| `BFS-043-mutation-B-red.txt` | negative control B: the range refusal replaced by a skip ⇒ 11 per-knob arms + 3 further layers red |

Every number below was produced by a run on this host, on this branch. The three commits, in
order, are the shape a reader can re-walk: `48a593e` (the table), `53edd2a` + `8d67b06` (the
wiring; the second is the one-file fix for a `git add` that missed a file — see §6.3), `d02be30`
(the config plumbing and the cells).

---

## 1. The knob table — name, default, range, failure mode

The table is `internal/invalidation.Knobs()`, and it is the SINGLE declaration: the validator
iterates it, the read-back reports its `default`/`min`/`max` per knob, and a cell asserts the
reported range equals the enforced range for every knob. A range documented one way and
enforced another is the defect this row closed.

| # | knob `server.invalidation.…` | default | range | unit | failure mode |
|---|---|---|---|---|---|
| 1 | `watch.enabled` | `false` | `false \| true` (0..1) | bool | the mapping decoder refuses a non-boolean at load; the zero value **is** the declared default (BFS-035's opt-in) |
| 2 | `watch.heartbeat_ms` | `30000` | `[100, 30000]` | ms | refused by name at load **and** at surface construction; never replaced by the default |
| 3 | `watch.install_headroom` | `512` | `[1, 1048576]` | count | same; `0` is refused — §4.2 requires a non-zero headroom |
| 4 | `watch.flush_every_ms` | `50` | `[1, 60000]` | ms | same |
| 5 | `watch.flush_max_paths` | `4096` | `[1, 4096]` | count | same; the maximum **is** the declared `max_paths_per_event`, so the knob may lower the path bound and never raise it past what the document promises clients |
| 6 | `watch.scan_limit` | `100000` | `[1, 2147483648]` | count | same; the default is the poll form's own observation bound (`eventsScanLimit`), pinned by a cell |
| 7 | `watch.max_watches` | `0` (AUTO) | `[0, 2147483648]` | count | `0` is a documented AUTO sentinel (the platform's own ceiling); a positive value the platform cannot give is reported as an **unhonourable value** with configured AND observed, never clamped in silence |
| 8 | `push.subscriber_buffer_bytes` | `4194304` | `[65536, 1073741824]` | bytes | refused by name; never replaced by the default |
| 9 | `push.subscriber_buffer_events` | `256` | `[1, 65536]` | count | same |
| 10 | `push.max_subscribers` | `8` | `[1, 4096]` | count | same |
| 11 | `push.write_deadline_ms` | `10000` | `[100, 29999]` | ms | same, **plus** the relation below |
| 12 | `push.max_event_bytes` | `1048576` | `[65536, 8388608]` | bytes | same; the maximum is the landed consumer's own per-line read cap (8 MiB), because declaring more than a client can read is the reconnect-loop defect SPEC-push-channel §7.2 measures |

**Relations (refused, never adjusted):**

| relation | why | where |
|---|---|---|
| `push.write_deadline_ms < watch.heartbeat_ms` | a stalled subscriber must be detected within one heartbeat period (§4.4). Consequence an operator must know: setting `heartbeat_ms` below 10000 requires lowering `write_deadline_ms` with it, and the refusal names both knobs | `Values.Validate()` |
| `watch.flush_max_paths ≤ max_paths_per_event` | the knob may not raise a bound the capability document publishes | range maximum + a cell |
| `watch.scan_limit ≥ 1` and every other numeric range excludes 0 | a written zero is how a bound stops being a bound | the table |

**The defaults are the safe ones, and the proof is not a claim:** no knob's default turns
invalidation off. The poll form (`X-Bunker-Op: events`) is **not behind a knob at all** — a cell
walks the table and fails if any knob names the poll op, the events op or the revision poll as
its subject — and the only default that is `false` is the watcher's own opt-in, which changes the
served revision's *kind* for a git tree (§7.2 R-V3) and is therefore an operator decision rather
than a silent convenience. The out-of-the-box surface is byte-for-byte the surface this repo had
before the row (§5).

---

## 2. What is obeyed, and how the read-back is produced

**One conversion, one table.** `watchOptionsFrom(inv)` is the only path from the declared surface
to the watcher's options, and `defaultWatchOptions()` is derived from the same defaults rather
than restated. Five of the knobs did not exist as configuration before this row: `heartbeat`,
`install_headroom` and `flush_every` were package-level variables seeded into `watchOptions`,
and `flush_max_paths`/`scan_limit` were read as package variables *directly* by the flush and the
install walk. All five are now options, and the variables are gone.

**The read-back reports the RUNNING watcher, not the request.** `extensions.watch.config` is
rendered per request: for every declared knob, `value`, `default`, `min`, `max`, `unit`, `source`,
`applied` and, where nothing is applying it, `applied_reason`. When a watcher object exists the
values are read out of **its own options** — the same fields the flush, the walk and the heartbeat
read; when it does not, the values are the resolved configuration and every watch row says
`applied:false` with the reason (except `watch.enabled`, which is what decides that state).

**`applied` is claimed only where it can be observed.** A deployment whose install *refused*
(state `absent`) still **used** three knobs — the walk ran under `scan_limit`, and §4.2's ceiling
arithmetic ran under `max_watches` and `install_headroom` — while the heartbeat period and the
flush cadence bound machinery that never started. The read-back says exactly that
(`applied:true` for the three, `applied:false` + `state=absent` reason for the rest): a knob
reported as applied while nothing observes it is the same class of claim as a counter that can
never move (BFS-032).

**The push knobs are declared, validated and reported as `applied:false`** with the reason: this
build has no push endpoint (BFS-036 owns the wire form), and reporting a value as in force for a
channel that does not exist would be a lie about a boundary this row was told not to cross.

---

## 3. The loud-failure result

Three layers, one table, and every arm asserts the same law: the value is refused BY NAME with
the value and the range, and the declared default is **never** substituted.

| layer | cell | arms | result |
|---|---|---|---|
| the table (`internal/invalidation`) | `TestEveryKnobRefusesAnInvalidValueLoudly` | **11** (one per numeric knob, driven off the table so a new knob cannot be added without a cell) | PASS |
| the table | `TestZeroIsRefusedForEveryNumericKnob` | every numeric knob with `0` written (the typo'd-limit case), plus the one documented AUTO sentinel | PASS |
| the table | `TestTheRangeCheckAndTheRelationCheckAreIndependent` | a range violation is refused by the table; a pair that is legal field by field and illegal together is refused by the relation, and neither is mis-reported as the other | PASS |
| the surface (`internal/server/webdav`) | `TestInvalidationConfigRefusesInvalidValuesAtConstruction` | **8** (heartbeat 0, headroom 0, scan_limit 0, flush paths above the declared bound, buffer below the floor, write deadline above its maximum, max subscribers 0, and a heartbeat below the declared write deadline) — each asserts `New` returns an error, names the knob and hands back **no handler** | PASS |
| the loader (`internal/config`) | `TestLoad_InvalidationInvalidValueFailsBeforeListen` | **4** (a zeroed headroom, a heartbeat above the bound, a buffer below the floor, a write deadline at the heartbeat) — each asserts `Validate()` fails before any listener binds | PASS |
| the loader | `TestLoad_InvalidationNonBooleanFailsAtDecode` | `enabled: "yes"` is a load error, never read as `false` | PASS |

The refusal's own bytes, verbatim (this is the range branch of `rangeErr`):

```
server.invalidation.watch.heartbeat_ms: 60000 is outside the declared range [100, 30000] ms —
refused rather than replaced by the declared default 30000 (fix the value, or remove the key to
take the default)
```

and the relation's:

```
server.invalidation.push.write_deadline_ms: 10000 ms is not below
server.invalidation.watch.heartbeat_ms (1000 ms) — a stalled subscriber must be detected within one
heartbeat period (SPEC-push-channel §4.4: push_write_deadline < heartbeat_ms); the pair is refused
rather than adjusted
```

**Absent is not invalid.** A key that is not written takes the declared default (a fact about the
file), which is why the `Spec` fields are pointers: `nil` is "not written", a written key is
obeyed or refused. `TestAbsentKeysTakeDefaultsWrittenKeysAreObeyed` pins both halves, including
that the read-back labels the two sources differently.

---

## 4. `capability_unavailable` with CONFIGURED **and** OBSERVED

The worked example the row asks for — *"you asked for 8192 watches and this kernel gives 128"* —
from `BFS-043-wire-sample.txt`, produced by `TestInvalidationWireSampleForEvidence` on a served
tree of 3 directories with the declared headroom:

```http
X-Bunker-Verdict: capability_unavailable
X-Bunker-Capability: watch;scope=target;mode=poll;reason=watch_limit_exhausted
```

```json
{
  "ok": false, "op": "watch", "verdict": "capability_unavailable",
  "error": {
    "capability": "watch", "scope": "target", "mode": "poll",
    "reason": "watch_limit_exhausted",
    "unhonoured": {
      "knob": "server.invalidation.watch.max_watches",
      "configured": 8192,
      "observed": 128,
      "detail": "the deployment asked for a watch ceiling of 8192 but the platform's own ceiling is 128, so 128 is the ceiling in force; the per-user watch total in use is not observable, so the platform's ceiling cannot be raised from inside this process (the remedy is an operator action on fs.inotify.max_user_watches)"
    },
    "detail": "the watch set needs 515 watches (3 directories plus 512 headroom); the ceiling fs.inotify.max_user_watches in force is 128, which this tree does not fit under, and it is the platform number (requested 8192, platform 128, per-user total in use not observable); the remedy is an operator action on the ceiling"
  }
}
```

The same numbers reach the document twice — beside the ceiling in force, and as the pair — so a
client that re-reads the capability document (rather than probing the op) sees them too:

```json
"limits": { "watching": {
  "limit_name": "fs.inotify.max_user_watches",
  "configured": 128, "ceiling_in_force": 128, "platform_max_user_watches": 128,
  "requested_max_watches": 8192, "desired": 3, "headroom": 512,
  "watches_held_by_this_process": 0, "errno": "" } }
"config": { "ceiling": {
  "requested_max_watches": 8192, "platform_max_user_watches": 128,
  "ceiling_in_force": 128, "binding": "platform", "watches_desired": 3, "headroom": 512 } }
```

**Three shapes, and the refusal is only one of them** (the reason the pair is a *field* and not a
second verdict code):

| shape | state | what the surface says | cell |
|---|---|---|---|
| the request is above the platform's ceiling **and it binds** | `absent`, `watch_limit_exhausted` | the refusal above, with `unhonoured{configured, observed}` | `TestUnhonouredValueIsReportedWithBothNumbers` |
| the request is above the platform's ceiling but **does not bind** (the tree fits) | `watching` | the channel is UP; the pair is reported in `config.unhonoured`, in the refusal the `watch` op still answers (scope=build/C6), and as a `blocks_push:false` degradation entry — **not** as `capability_unavailable`, because a client that reacted to a working watcher by switching mechanisms would have changed nothing | `TestUnhonouredValueThatDoesNotBindIsAWarningNotARefusal` |
| the request is legal but the tree does not fit under it | `absent`, `watch_limit_exhausted` | configured = the ceiling in force, observed = what the tree needs (with the directory count and the headroom in the detail) | `TestWatchCeilingUnhonoured` (the table-driven arm, 5 shapes) |

The `binding` field names which number the ceiling in force came from (`requested`, `platform`,
`requested (= the platform's own ceiling)`, or `none (no ceiling was readable)`), so "the ceiling
is 128" never has to be read as "your number did not apply".

---

## 5. The runtime read-back proof

**5.1 Every knob, with the range the validator enforces.**
`TestInvalidationReadBackCoversEveryKnobWithItsEnforcedRange` reads the served document and
asserts, per knob: the row exists, `min`/`max` equal `invalidation.Knobs()`'s, `default` equals the
table's, `value` is inside the range, and `source` is one of the two declared values. A knob the
config accepts with no read-back row is a knob invisible at runtime, and the cell fails on it.

**5.2 The read-back follows the RUNNING watcher (the defect-class arm).**
`TestInvalidationReadBackFollowsTheRunningWatcher` configures the handler at the DECLARED
heartbeat (30000 ms) and then replaces the running watcher — through the in-package seam BFS-035
built — with one that beats every **7 ms**. The document reports `heartbeat_ms: 7` and the knob's
row reports `value: 7, applied: true`. A read-back that echoed the configuration would say 30000,
which is exactly the failure this row exists to prevent.

**5.3 The value the code OBEYS, one arm per observable knob.** Each of these reads a number out of
the document AND makes the code show the same number:

| knob | what the code did | cell |
|---|---|---|
| `watch.install_headroom` = 8 | the install SUCCEEDED under a 16-watch ceiling where the declared default (512) would have refused it, and `coverage.headroom` is 8 | `TestInvalidationHeadroomKnobReachesTheInstall` |
| `watch.heartbeat_ms` = 200 | `heartbeats_total` reached ≥ 3 within the period, and `heartbeat_ms` = 200 | `TestInvalidationHeartbeatKnobIsThePeriodTheWatcherBeats` |
| `watch.flush_max_paths` = 2 | 3 changed paths produced a **rescan** (an `overflow` line), not a 3-path `invalidate` | `TestInvalidationFlushMaxPathsKnobBoundsOneEvent` |
| `watch.scan_limit` = 2 | the install refused coverage with `watch_partial_coverage` and the detail names the bound `2` | `TestInvalidationScanLimitKnobBoundsTheWalk` |
| `watch.enabled` | `false` ⇒ no watcher object and state `absent`; `true` ⇒ a watcher object exists | `TestInvalidationEnableKnobIsWhatStartsTheWatcher` |
| `watch.max_watches` | the ceiling in force, the binding number and the pair move together with the configured value and the injected platform ceiling | §4's three shapes |

**5.4 Stock-client behaviour is unchanged** (the row's "must not add a new server-side requirement
for a stock HTTP/1.1 client"). `TestInvalidationSurfaceLeavesAStockClientAlone` drives a real
`PROPFIND Depth:1` and a `GET` with **no `X-Bunker-*` headers** against two handlers serving the
SAME tree — one at the declared defaults, one at the legal extremes of every watcher knob
(heartbeat at its maximum, headroom at its minimum, flush cadence at its maximum, path bound at
its minimum, scan bound at its minimum, every push knob at an extreme) — and requires the status
codes and the bodies to be **identical**. It also requires the declared poll op
(`X-Bunker-Op: events`) to answer `200` in both, and the table walk in §1 requires that no knob
names that channel at all.

**5.5 The read-back is live, not startup-only.** `read_at` is stamped per request, and every
number in §5.2/§5.3 was read from a served document by driving the op — not from the Go values
that produced it.

---

## 6. The negative controls (the cells can fail)

**6.1 Mutation A — the read-back reports the CONFIG instead of the running watcher.**
`internal/server/webdav/invalidation_config.go`, the `rows[i].Value = v` override in
`invalidationRows` replaced by `_ = running`:

```
--- FAIL: TestInvalidationReadBackFollowsTheRunningWatcher (0.00s)
    invalidation_config_test.go:320: the read-back reports heartbeat_ms 30000, want the running
    watcher's 7
```

Restored: `sha256(invalidation_config.go) = ca9d082a89ea31696e86225e80ce50aa898acbb635f4965cb17e2e6c32e04cb3`
before and after. Full raw output: `BFS-043-mutation-A-red.txt`.

**6.2 Mutation B — the range refusal replaced by a skip** (`internal/invalidation/values.go`,
`return rangeErr(k, got)` → `continue`): **11 per-knob subtests**,
`TestZeroIsRefusedForEveryNumericKnob`, **8 surface construction arms** and **4 load arms** all go
red. Restored: `sha256(values.go) = 6ef4d1295b7eda221e786edcb89335f3a5ca38c10eb8c251c9ecdef483d2c5d8`
before and after. Raw output: `BFS-043-mutation-B-red.txt`.

Two findings from running this control, both reported rather than smoothed over:

1. **A mutation can change WHICH refusal fires.** With the range branch skipped, the cell that
   writes `heartbeat_ms = 99` was still refused — by the *relation*, which then read
   `write_deadline_ms (10000) is not below heartbeat_ms (99)`. The arm caught it either way, and
   the message would have misled an operator about the cause; that is why
   `TestTheRangeCheckAndTheRelationCheckAreIndependent` now pins that a range violation is
   reported as a range violation and a relation violation as a relation violation.
2. **An arm was written, found vacuous, and removed rather than kept.** The first attempt at that
   independent-checks arm used a pair the relation does not actually forbid (`30000`/`29999` is
   legal: the deadline IS below it), so it failed against the correct code. The premise — "an
   in-range pair that a range-only validator lets through" — does not exist, because the range
   check and the relation check are independent refusals. The arm that replaced it tests both
   refusals and their classification, and the vacuous version is not in the tree.

**6.3 A committed tree that could not build (and the check that catches it).** Commit `53edd2a`
staged the webdav files and missed `internal/invalidation/values.go`, which the same change had
extended with two methods the wiring calls. Every local signal was green (the guard builds the
working tree), so the only check that caught it was reading the COMMITTED blob
(`git show HEAD:internal/invalidation/values.go | grep -c RequestedExceedsPlatform` = 0). Fixed in
`8d67b06`, and the working tree equals that commit (`git status --porcelain` empty), which is what
makes "the guard's full-suite run applies to this commit" true.

---

## 7. Residuals and holes found while doing this (reported, not filed)

| # | finding | evidence | disposition |
|---|---|---|---|
| **R-1** | **The document's `degradations[]` does not carry the watcher's own entries.** `watchDegradations` renders them and has **no production caller**: `grep -rn watchDegradations --include=*.go` returns the definition plus two *cells*. `degradations()` in `ops.go` builds the watch entry from a hardcoded string instead, so the reason-carrying entry (`reason=…`) §8.2 promises a client never reaches the wire. The served list, verbatim from `BFS-043-wire-sample.txt` (`capabilities.degradations`): `{"capability":"watch","detail":"inotify watcher absent on this target: the push form is not served; the declared poll form X-Bunker-Op: events is","mode":"poll","scope":"target"}` — no `reason`, and no entry for the unhonourable value. | the grep above; the served document in `BFS-043-wire-sample.txt` | **Not fixed here**: it is BFS-035's/§8.2's surface, not a config knob, and the row was scoped to the config surface. The unhonourable pair is on the wire regardless (the refusal's `error.unhonoured` and the document's `config.unhonoured`), and the cell asserts the FUNCTION's output with a comment saying why. Recommended owner: a follow-up row to BFS-045 (observability) or BFS-035's owner. **Not filed as a row by this worker** (the brief forbids it). |
| **R-2** | **Config keys are not strict, so a typo'd knob NAME is silently ignored** while a typo'd VALUE is refused (the row's law is about values, and that half is closed). | measured, not assumed: `TestLoad_InvalidationUnknownKeyIsAcceptedGap` loads `watch.heartbeet_ms: 5000`, the load succeeds and the knob keeps its declared `30000`. The cell fails if either side of that changes, so the residual closes with the fix. | Wider than this surface (every section of the config file behaves the same way — the loader does not set `ErrorUnused`); it is a config-layer change with its own blast radius. Named here with its measurement rather than fixed. |
| **R-3** | **The QUIC idle-timeout relation is not validated here.** §4.4 requires `heartbeat_ms` strictly below the QUIC idle timeout **or** `KeepAlivePeriod` set on the h3 server; `h3.go` sets neither, so both are 30 s and the declared maximum `heartbeat_ms = 30000` sits exactly on the boundary. | SPEC-push-channel §4.4 relation table and H-5, which names the owner as "whoever lands it" | Reported. This row validates `heartbeat_ms ≤ 30000` (the inherited declaration) and the write-deadline relation; the h3-side relation needs a change in `internal/server/server.go`'s QUIC config, which is BFS-007's surface. |
| **R-4** | **A heartbeat below the default write deadline requires lowering it too.** `heartbeat_ms: 5000` alone is refused, because the declared `write_deadline_ms` of 10000 is then not below it. | the relation refusal in §3 | Deliberate (refuse, never adjust — a derived default would be a silent substitution), and documented in the knob table's failure column so an operator meets it in the message rather than in the docs. |
| **R-5** | **The push knobs are surfaced and validated but nothing consumes them.** | §2 | By design for this row (BFS-036 owns the endpoint); reported as `applied:false` with the reason on every push row, so the record never implies a channel that does not exist. |

---

## Appendix — how the surface relates to the documents it inherits

- **`SPEC-watcher-capability.md`** §9 hands this row "watcher/push knobs, defaults, validation, the
  refusal path for an unhonourable value", and fixes what a config surface must be able to
  express: `limits.*` (§8.2), the `coverage.headroom` of §4.2, and the state/reason vocabulary
  the watcher reports through. §4.2's "desired + held + headroom" probe is the arithmetic
  `invalidation.WatchCeiling` now implements once, so the refusal, the document and the
  degradation entry cannot disagree. This row changes no reason code and adds no verdict.
- **`SPEC-push-channel.md`** §4.4's relation table, §5.3's write deadline, §6.1's buffer bounds,
  §6.4's subscriber cap and §7.2's `max_event_bytes` are the values this surface expresses and
  validates; the names, defaults and ranges are this row's, as that spec's §11 hands them over.
- **BFS-035** built the watcher and left `Config.WatchEnabled` as an explicit seam with the
  comment "the operator-facing knob names, defaults and validation are BFS-043's". That seam is
  now the validated surface; the two defaults BFS-035's cells assert (heartbeat 30000, headroom
  512) now come from the table, with the same numbers.
- **BFS-031 / BFS-032** are used as failure shapes, not repaired: this surface's answer to
  "a bound reported and not enforced" is the loud refusal plus §5's read-back arm, and its answer
  to "a counter that can never move" is `applied` being claimed only where a running component
  observes the value.
