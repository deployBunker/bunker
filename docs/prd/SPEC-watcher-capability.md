# SPEC: the watcher capability contract and its degradation matrix

**Project:** bunker (owning) · **Row:** BFS-040 (P0, complexity 2) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Status:** proposed — spec of record for what the server may *claim* about watching a target, and what it must say when it cannot. **No product code changed by this row.**
**Deliverable:** this document, plus the refusal strings it mandates (§3, §4).
**Design authority (inherited by reference, not restated):** `docs/prd/PRD-bunker-invalidation.md` §1 (R1, R2) and §2.1–§2.3 — that document is the *why*; this one is the *contract an implementer builds against*. It supersedes the invalidation sketch in `PRD-bunker-fs.md` §slice C5 exactly as that PRD says.
**Implemented by:** BFS-035 (the server-side watcher).
**Consumed by:** BFS-036 (push form), BFS-037 (hot-file refresh), BFS-041, BFS-042, BFS-043, BFS-045, BFS-046.
**Depends on (landed):** BFS-026 — `internal/server/webdav/events.go` (the poll form this contract degrades to), BFS-004 §3 E-4/E-6, §4.2, §5.1–§5.3, §10.5, BFS-005 §4.1–§4.4, BFS-014 (the three E-6 declarations).
**Boundaries (named, not crossed):** BFS-041 (push wire form, resume cursor, reconnect/backoff), BFS-042 (hot-file policy, queue stop semantics, pool share), BFS-043 (server config knobs), BFS-044 (client config flags), BFS-045 (observability record layout), BFS-046 (the test program). §9 states the exact interface handed to each.

**Why this is a new file, not a section appended to `PRD-bunker-invalidation.md`.** Two reasons, one of them operational: (1) **concurrency** — five sibling specs (BFS-041/042/043/045/046) are being authored alongside this one against a fleet that is committing to this repo concurrently, so each row needs its own path to keep every commit path-limited and rebasable; two rows editing one PRD is a guaranteed conflict. (2) **kind of document** — the PRD is a design authority that argues for a decision; this is a contract with a closed vocabulary, mandated refusal bytes, and acceptance criteria an implementer is graded against. The house already separates the two (`docs/prd/PRD-*.md` vs `docs/spec/BFS-004-*.md`, `docs/prd/SPEC-*.md`).

---

## 0. Verdict in one screen

| # | what the row asked for | the answer this document fixes | where |
|---|---|---|---|
| 1 | **the probe matrix** — each reason a watcher can be missing/partial, its own NAMED verdict, its own fallback | Seven named reasons (`watch_unsupported_platform`, `watch_limit_exhausted`, `watch_target_netbacked`, `watch_partial_coverage`, `watch_install_failed`, `watch_lost`, `watch_boundary_split`) in a **closed vocabulary**, carried in a new `reason` field on the *existing* `capability_unavailable` refusal — no new verdict code, so the refusal table of BFS-004 §5.1 stays complete and an old client's branch is untouched. Each row fixes the probe, the bytes, the fallback, and what a client may conclude. | §3, §4 |
| 2 | **the overflow guarantee** — normative, and its client-visible consequence | **"An interval the watcher cannot vouch for is NEVER reported as quiet."** An overflow is always a full rescan, always counted, never partial, and the number of dropped events is never invented. Client-visible: `overflow` (resync) is a distinct observable from a quiet poll, and a stalled reader is a third observable — not a fourth flavour of quiet. | §5 |
| 3 | **what the watcher does not promise** | Six non-guarantees stated as prohibitions: no content hashes, no ordering beyond the journal cursor, no delivery while down, nothing on a wholesale replacement (a mount swap is a **resync**, not a change), no writer attribution, no file-level watches. | §6 |
| 4 | **the revision contract** — which change moves the served revision, and which derived artifacts are covered | The derived-artifact table (D1–D6): which change moves each one **today** vs after the watcher lands, including the two blind spots measured today (`X-Bunker-Rev` does not move for an out-of-band edit on a git tree; the server's hash cache keys on size+mtime with **no ctime**, unlike the event ledger). R2's second job — the server aligning its **own** derived state — is specified as a change of D1/D2/D4/D5, not only the client's cache. | §7 |
| 5 | **capability negotiation** — discoverable, never inferred from a flag | The state is reported in three machine-readable carriers (the `capabilities` op's `extensions.watch` block, its `degradations[]` entry, and the refusal itself). **A client must never infer a watcher from a build flag, an option value, or a negotiated HTTP version** — the only source of truth is the probe. | §8 |
| 6 | **the fallback is contractual** | The host with no watcher keeps working **exactly as it does today**, on the served `X-Bunker-Op: events` poll (BFS-026): §2 is a measured inventory of what is served, and every mandated fallback in §4 points at one of those three mechanisms and no other. A client that ignores this whole document behaves as it does now. | §2, §4 |
| + | **a hole found now, not smoothed over** | Both are named with evidence and an owner recommendation rather than folded in: the **boundary-split** case (a writer whose bytes never land in the served superblock — a container overlay upperdir, a bind of a copy; **no channel can detect it**, so it is reported as a topology warning, not as a channel degradation), and the **library trap** that makes the overflow notice invisible to a watcher that only reads the event channel (§5.4). | §4.8, §5.4, §10 |

---

## 1. The failure class this contract closes, and the test it must pass

Everything else in the invalidation release depends on the watcher being **honest about itself**. A watcher that is absent, partial, or silently lossy but reports itself healthy is *worse than the poll it replaces*, because it converts a visible latency into an invisible wrong answer. This repository has already paid for this class three times: a cache bound that did not bound the directory (BFS-031), a counter that could never move (BFS-032), and a verification that could not fail (the control arm of BFS-026 §6). The row is P0 for that reason, and not because the watcher is hard.

Today every absence collapses into one string — `no inotify watcher on this target` (`internal/server/webdav/ops.go:89`) — and that one string is **wrong for at least four of the seven cases** in §4. Two of them are not even the same kind of fact:

- `watch_target_netbacked` is *permanent for this target*: no amount of building, tuning or restarting will ever produce local events for a tree the local kernel does not own. An operator who reads "no watcher" and goes looking for a missing build flag will never find one.
- `watch_limit_exhausted` is *transient and fixable*, and the fix is an operator action with a number attached (`fs.inotify.max_user_watches`). An operator who reads "no watcher" cannot know that the remedy is a sysctl, or which sysctl.
- `watch_partial_coverage` is *silently wrong*: the watcher exists, events flow, and a subtree is simply never covered. This is the one case where a healthy-looking channel produces exactly the invisible wrong answer the PRD's `§2.1` calls a silent-corruption machine.
- `watch_lost` is *mid-life knowledge loss* with a window: there is a vouched-for interval before it and an unvouched-for one after it, so a client that keeps its cursor across it has a stale view it believes is current.

**The honesty test this document must pass, stated so an implementer can be graded on it:** *for any state of the system, a client that has read only this contract plus the server's own bytes can name (a) whether the events it is receiving are complete for the interval it cares about, and (b) if not, what to do instead.* If a state exists where the honest answer is "the client cannot tell", that state must be reported as an explicit **unvouched** marker (§5), never as quiet.

---

## 2. What is served today — the fallback this contract degrades to (measured, not assumed)

This section exists because a mandated fallback that does not exist is a lie in a spec-shaped document. Everything below was read out of the landed tree on this branch.

### 2.1 The three mechanisms the surface actually serves

| mechanism | what it is | served? | where |
|---|---|---|---|
`watch` (push) | a long-lived NDJSON stream of `invalidate`/`heartbeat`/`overflow` lines | **no** — refused `501 capability_unavailable`, `scope=target`, `mode=poll` | `internal/server/webdav/ops.go:84–90` |
`events` (poll) | one bounded observation per call — a stat per entry, plus a **streamed content read** for the regular files whose metadata cannot rule an edit out (§2.2, QA-BUNKER-36) — diffed against the identity last seen, answered as an E-4 envelope | **yes** | `internal/server/webdav/events.go` (BFS-026) |
`rev` (revision poll) | one cheap request per interval whose `X-Bunker-Rev` answers for the tree at the revision's own granularity — `git:<HEAD>` moves on committed ref movement only, so an uncommitted out-of-band edit moves nothing (§2.4; BFS-048) — the client's last-resort mechanism | **yes** (every response carries it) | `internal/server/webdav/handler.go:233`; client `internal/fsclient/invalidate.go:508–547` |

The client's own vocabulary, verbatim (`internal/fsclient/invalidate.go:44–62`): **mode** ∈ `push` | `poll`; **mechanism** ∈ `watch` | `events` | `rev` | `none`. The mechanism is reported next to the mode because "per-path drops" and "a whole-tree resync" are a cost, not a detail.

### 2.2 The `events` poll, exactly as served (this is the degraded mode, so its bounds are part of this contract)

| property | served value | consequence a client may rely on |
|---|---|---|
observation | one `filepath.WalkDir` of the served root, one stat per entry, **plus** a streamed read (io.Copy, never buffered) of each regular file whose metadata cannot vouch for its bytes — the coarse-clock row below | cost is O(paths) per poll, plus O(bytes) for the paths last written inside the coarse-clock window; a settled tree reads nothing |
identity | `(size, mtime, ctime)`, ctime deliberately included | an edit that preserves size **and** mtime still moves ctime wherever the kernel can separate the two writes → is reported; where it cannot (a coarse clock) the content digest below is what reports it (QA-BUNKER-36) |
coarse clock | the kernel stamps filesystem metadata from the COARSE clock, so an edit landing in the same tick (1/HZ) as the write before it is stamped identically; a record whose `(size, mtime, ctime)` cannot rule that out is verified by streamed sha256, bounded by `eventsCoarseClockWindow = 50ms` | a same-size, mtime-restored rewrite is reported even where ctime was frozen — measured at CONFIG_HZ=250 (a 4 ms tick): ctime delta 0 ns, 5/5, on ext4 and tmpfs — and a record older than the window is still taken on metadata alone, so no post-window rewrite is ever silently dropped either |
scan bound | `eventsScanLimit = 100000`; a truncated observation is answered `overflow`, never a diff of the part that fit | no partial diff is ever presented as complete |
path-list cap | `eventsMaxPathsPerEvent = 4096`; above it, `overflow` | a client's drop loop needs a bound and has one |
journal | `eventsJournalEvents = 256`; a cursor **above zero** that is older than the retained journal is answered with the retained events whose first `seq` is a gap; a cursor **of zero** in that state is answered `overflow` | the missing range is never re-requested (BFS-005 §4.1); the cursor-0 case is BFS-063: the tail's gap IS visible to a client with a cursor, and is invisible to one without (the client's own rule is guarded on a non-zero cursor), so the server says it itself |
first observation | the ledger's knowledge begins there and the interval before it is **unknown**, so the first poll emits `overflow` | a client that binds with a whole-tree snapshot has its baseline seeded from that snapshot (`seedEvents`) — no unearned first overflow |
resume declaration | `since_seq` **absent** = "I hold no observation of the tree"; `since_seq: N` = "my view is the tree as of ledger seq N" | the two are different requests and get different answers: no declaration is answered `overflow` (nothing is vouched for a client that has observed nothing), a declaration is answered the retained tail from N. Without the declaration the server cannot tell a client that bound with a snapshot from one that has observed nothing — BFS-063 |
the minted cursor | a whole-tree `snapshot` answer carries `result.head_seq`: the ledger's cursor read **before** the observation walk | it is the only cursor a client may declare (the server issued it, so the claim is checkable), and reading it before the walk means a change the walk raced is delivered rather than skipped — BFS-063 |
cursor ahead of the ledger | `seq` is advanced past the cursor, then `overflow` | a restarted process or a foreign cursor is knowledge loss, not an empty answer |
answer fields | `events`, `count`, `scanned`, `head_seq` (+ the envelope's `truncated`, `rev`, `tree`) | `head_seq` is how a client/test sees "caught up" as distinct from "channel dead" |
envelope | one single-envelope request; no long-poll, no stream; no request timeout governs it | nothing is held open on a dead client — a poll that answers is its own liveness signal |

### 2.3 The refusal this contract formalises, verbatim today

```http
HTTP/1.1 501 Not Implemented
X-Bunker-Verdict: capability_unavailable
X-Bunker-Capability: watch;scope=target;mode=poll
```
```json
{"ok":false,"op":"watch","verdict":"capability_unavailable",
 "error":{"capability":"watch","scope":"target","mode":"poll",
          "detail":"no inotify watcher on this target; the declared poll form X-Bunker-Op: events carries the channel (mode=poll), or poll with HEAD/ETag"}}
```

The capability document says the same thing in two other places (`ops.go:456–461`, `ops.go:494–499`): `extensions.watch` = `{"mode":"poll","modes":{"push":"inotify→stream","poll":"X-Bunker-Op: events"},"max_paths_per_event":4096}` and a `degradations[]` entry `{"capability":"watch","scope":"target","mode":"poll","detail":"inotify watcher absent on this target: the push form is not served; the declared poll form X-Bunker-Op: events is"}`.

**Backward-compatibility fact that constrains every decision in §3:** the client discovers a watcher by *probing the op* (`internal/fsclient/capabilities.go:131–140`), and takes the poll branch on `verdict == capability_unavailable || status == 501 || op_unknown || extension_op_missing` (`internal/fsclient/invalidate.go:305–307`). It branches on the **verdict** and the mode, not on the detail text. Therefore: the discriminator this contract adds **must be a new field, not a new verdict code** — otherwise the client's existing branch breaks and BFS-004 §5.1's "this table is the whole vocabulary" becomes false.

### 2.4 Two blind spots the fallback has today (measured; R2's reason for existing)

1. **`X-Bunker-Rev` does not move for an out-of-band edit.** For a git tree the token is the resolved HEAD hash (`"git:<40 hex>"`, `tree.go:281–292`); `bumpRev()` advances only the non-git counter and clears the 500 ms memo (`tree.go:269–276`). So a working-tree write — ours or anyone else's, committed or not — moves **nothing** the client's `rev` poll can see. The client's own comment claims the rev poll answers "is my view still the tree's view?" for the whole tree (`invalidate.go:508–513`); **that claim is false for a git tree and is a defect of exactly this class**, named as R-2 in §10.
2. **The server's hash cache is blind where the event ledger is not.** `hashEntry` keys on `(size, mtime UnixNano)` with no ctime (`tree.go:32–36, 165–192`), while the event ledger's `identity` deliberately includes ctime because "an edit that preserves size AND mtime is exactly the shape a metadata-keyed observer would otherwise miss" (`events.go:31–36, 97–102`). The two derived artifacts disagree about the same class of change.

Neither is a watcher defect; both are why R2 is normative (§7) rather than a cosmetic follow-up.

---

## 3. The vocabulary (normative)

### 3.1 The `reason` field, and where it is carried

**`capability_unavailable` stays the verdict. `reason` is the discriminator.** It is a closed set of seven values (§4), carried in **three** places so that no consumer is forced into a second decoding path:

1. the E-4 envelope's `error.reason` (a string),
2. `X-Bunker-Capability`, as a fourth `;`-delimited part: `watch;scope=target;mode=poll;reason=<value>` — safe because the client's parser looks only for `mode=` (`invalidate.go:588–596`) and every existing reader ignores unknown parts,
3. the `capabilities` op's `extensions.watch` block, as `reason` + `state` (§8.2).

| field | required | meaning |
|---|---|---|
`capability` | yes | always `"watch"` |
`scope` | yes | `"target"` — this is a property of the target, never `build` (BFS-004 §5.2) |
`mode` | yes | the mode actually in force. **Always `poll`** on this refusal path — the push form is not served in any of the seven cases with `blocks_push: true`. Unchanged from today. |
`reason` | yes | one of the seven §4 values. New. |
`detail` | yes | one sentence, and it **must name the observed evidence**, not the category: the sysctl name and the numbers for a limit, the mount type for a netbacked target, the errno for an install failure, the count for a partial cover. A detail that could have been written without probing anything is a defect. |
`blocks_push` | document only | `true` for W-1…W-6, **`false` for W-7** — the only reason that is a warning rather than a degradation (§4.8) |

### 3.2 The runtime state vocabulary (and why it is not the same list)

| value | meaning | where reported |
|---|---|---|
`watching` | the watcher exists, its watch set covers the tree, and no unvouched interval is outstanding | document `extensions.watch.state` |
`overflow` | the watcher exists but an interval is **unvouched** (the kernel dropped events). The mechanism is unchanged; the knowledge is not. | document state **and** an `overflow` event on the channel (§5) |
`lost` | the watcher stopped mid-life and could not be re-established (§4.6) | document state; and `watch_lost` if the op is then refused |
`absent` | no watcher for this target, for a named reason | document state; and the refusal with that `reason` |

The document (and `§8.2`'s block) reports `blocks_push`; **W-7 is the only `false`**.

An **absence** is a property of the bind: a client learns it once, at handshake, and picks a mechanism. An **overflow** is a runtime event: a client learns it at any moment and must rescan. Conflating them is how "we degraded once" becomes "we are degraded forever" — and equally how "we resynced" gets mistaken for "we went to poll". **An overflow never changes `mode`** and never changes the mechanism; it invalidates an *interval*.

### 3.3 The honesty rules attached to the vocabulary

- **Never report a reason that was not probed.** Each `reason` has a named probe in §4; a build that has not run that probe reports `absent`/`null` with the detail saying so, or does not report a state at all. There is no default reason.
- **Never report a number that is not knowable.** `fs.inotify`'s per-user **total** in-use watches is not exposed anywhere in procfs (measured: `/proc/sys/fs/inotify` contains only the three ceilings — App. A.3). So the limit case reports `limit_configured` + `watches_seen_by_this_process` (+ `desired`), and the number of events the kernel dropped is reported as `null` with `overflow_dropped_reason` set — **the kernel reports one marker, not how many events it ate** (measured: 12 000 creates ⇒ 16 384 delivered + 1 marker ⇒ ≈31 600 dropped, unreported and uncountable — App. A.4).
- **A bound the owner cannot see is not a bound** (PRD §2.7). The documents in §8 are where these become seeable; the *counting* of them is BFS-045's deliverable and this contract only fixes that they must exist and what they measure.

---

## 4. THE PROBE MATRIX (normative)

Seven reasons. Every one is a distinct fact with a distinct consequence; each must be detected by its own probe, reported under its own name, and degraded to a mechanism named in §2.1 and no other.

| # | `reason` | what it means | probe | degrades to | `blocks_push` |
|---|---|---|---|---|---|
W-1 | `watch_unsupported_platform` | there is no watch facility to use on this build/kernel | compile-time platform fact + a runtime backend probe (`inotify` available) | poll → `events`, else `rev` | true |
W-2 | `watch_limit_exhausted` | a watch ceiling was hit: the per-user watch total, or the per-user instance total, or the per-process fd limit | pre-flight count vs configured ceiling **with headroom**; an `ENOSPC`/`EMFILE` from the add is the authoritative signal | poll → `events`, else `rev` | true |
W-3 | `watch_target_netbacked` | the local kernel is not the writer: the target is on a network/fuse mount | `statfs`/mount-table fstype of the served root | poll → `events`, else `rev` | true |
W-4 | `watch_partial_coverage` | a watcher exists and its watch set does not cover the whole tree | continuous coverage accounting: directories desired vs watched | poll → `events`, else `rev` — **and a bounded `missing[]` list** | true |
W-5 | `watch_install_failed` | the watch set could not be established (first install, re-install after a change, or a refused add) | the errno of the failing `inotify_add_watch` / backend `Add` | poll → `events`, else `rev` | true |
W-6 | `watch_lost` | a running watcher stopped mid-life and could not be re-established | the watcher's own error channel / loop exit, plus the re-install outcome | poll → `events`, else `rev`; the stream ends and the client re-handshakes | true |
W-7 | `watch_boundary_split` | the served root's superblock is not the one a writer's path resolves to (container overlay upperdir, bind of a copy, namespace divider) | mount-table fstype of the served root + the deployment's knowledge of co-writers (**partially detectable — see §4.8**) | **nothing** — the channel is fine; this is a topology warning | **false** |

### 4.1 W-1 — `watch_unsupported_platform`

**Probe.** The backend is a compile-time fact and must be probed, not assumed: on Linux the facility is `inotify` (7.0 kernel is the deployment floor); on other kernels the sibling facility is `kqueue` (BSD/macOS) / `fen` (illumos) with **different guarantees** — and the release target set is `linux/amd64`, `linux/arm64` only (`Makefile:15`), so a non-Linux build is a *build* fact that must still be reported as a *target* fact when it happens. A build with no backend at all reports this reason with `backend:"none"`.

**Mandated refusal** (`X-Bunker-Verdict: capability_unavailable`, `X-Bunker-Capability: watch;scope=target;mode=poll;reason=watch_unsupported_platform`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_unsupported_platform",
 "backend":"none","detail":"no filesystem watch facility on this build/kernel (backend=none); the push form is not served and cannot be"}
```

**Degrades to:** poll — `X-Bunker-Op: events` when the op is served, else the `rev` poll. **A client may conclude:** no future build of *this* target will ever push; do not retry the stream on a timer, and do not treat the absence as a transient fault. **Must never be reported as:** `watch_install_failed` (nothing was attempted) or `watch_limit_exhausted` (no ceiling was reached).

### 4.2 W-2 — `watch_limit_exhausted` (and the headroom discipline)

**What the design said, and what is actually true (measured).** The PRD says exceeding the limit "fails, often quietly". Measured: the *failing add* is not quiet — `inotify_add_watch` returns `ENOSPC`, and the library returns that errno unwrapped (`fsnotify@v1.10.1/backend_inotify.go:275–278`, so `errors.Is(err, syscall.ENOSPC)` works). What *is* quiet is everything around it:

1. the per-user **total in use** is not observable from procfs — only the ceiling is (App. A.3), so a pre-flight "do we fit?" answer is unknowable in principle and the probe must be built as *attempt + classify*, not *compute and refuse*;
2. an install loop that logs the failing add and continues ends with **partial coverage presented as success** — that is W-4 arriving through W-2's front door;
3. a tree that grows after install crosses the ceiling later, at which point the failure is mid-life (W-5/W-6 shape).

**The three ceilings, and which errno each produces:**

| ceiling | named | errno on failure | scope |
|---|---|---|---|
per-user watch total | `fs.inotify.max_user_watches` | `ENOSPC` from the add | the **user**, shared with every other process of that user — so this ceiling can be hit by someone else |
per-user instance total | `fs.inotify.max_user_instances` | `EMFILE` from `inotify_init` | one fd per watcher; matters as targets multiply, and as fds leak |
per-process file descriptors | `RLIMIT_NOFILE` | `EMFILE`/`ENFILE` | this process only |

**Mandated probe, in order.** (a) read the configured ceilings; (b) count the directories the watch set *desires* (a walk of the tree, the same walk the installer needs — it is not extra work); (c) count what this process already holds (its own watch set; the process's held watches are readable from `/proc/self/fdinfo/<fd>`, one `inotify wd:…` line per watch — App. A.3); (d) proceed only if `desired + held + headroom` fits under the configured ceiling, where **`headroom` is a required, reported number and not zero**: the tree grows, other processes of the same user exist, and the deployment shares a uid. (e) **Install, and classify every per-add error** — never a loop that continues past one. (f) Any `ENOSPC`/`EMFILE` anywhere in the install ⇒ this reason, and the partial watch set is **torn down** rather than kept.

**Configured-vs-observed, reported:** `limit_name`, `limit_configured`, `watches_held_by_this_process`, `watches_desired`, `headroom` — with `watches_added` reported even on failure, so "we added 3000 of 12000 and then stopped" is visible as three numbers and not as a boolean.

**Mandated refusal** (`…mode=poll;reason=watch_limit_exhausted`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_limit_exhausted",
 "limit_name":"fs.inotify.max_user_watches","limit_configured":8192,
 "watches_held_by_this_process":0,"watches_desired":11000,"watches_added":8100,
 "headroom":512,"errno":"ENOSPC",
 "detail":"the watch set needs 11000 watches with 512 headroom; the per-user ceiling fs.inotify.max_user_watches is 8192 and the add failed at #8101 with ENOSPC because this uid's watch total is shared with other processes and the per-user total in use is not observable; the partial watch set was torn down rather than kept"}
```

**Degrades to:** poll (`events`, else `rev`). **A client may conclude:** the absence is real *now* and is fixable by an operator action the message names; the tree is **not** partially watched (the contract forbids keeping a partial set as a success), so a rescan is the only truthful answer. **Must never be reported as:** `watch_partial_coverage` (that reason is for a set that was accepted and is short, see §4.4) or as a success with a warning.

### 4.3 W-3 — `watch_target_netbacked`

**Probe.** The fstype of the **served root**, resolved through the mount table (longest-mountpoint-prefix match; measured working, App. A.5) or `statfs`'s `f_type` where the platform exposes it. Network-backed means: `nfs`, `nfs4`, `cifs`/`smb`, `smbfs`, `sshfs`/`fuse.sshfs`, `9p`, `ceph`, `glusterfs`, and **any `fuse*` whose userspace source is remote**. The distinction the spec insists on: `fuse` alone is not the verdict — `fuseblk` over a local disk is watchable; the verdict is "the local kernel is not the writer", which for fuse means "the writer is a userspace daemon, possibly on another host".

**Mandated refusal** (`…mode=poll;reason=watch_target_netbacked`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_target_netbacked",
 "mount_type":"fuse.sshfs","mount_point":"/srv/tree","local_writer":false,
 "detail":"the served root /srv/tree is a fuse.sshfs mount: the local kernel is not the writer and no local watch can observe it; this is not a build gap and will not change for this target"}
```

**Degrades to:** poll (`events`, else `rev`). **A client may conclude:** this absence is **permanent for this target** — stop looking for a fix, and treat the poll as the only mechanism; the correct engineering answer is to watch at the *writer* (the remote side), which is out of scope here and named in §10 as R-4. **Must never be reported as:** "no watcher" without the mount type, or as a transient condition a retry could clear.

### 4.4 W-4 — `watch_partial_coverage`

**Probe.** Continuous accounting of the watch set against the tree: the watcher owns the directory walk, so it can count `directories_desired` vs `directories_watched` at install and at every re-scan. **The library's own recursive watch is not available** — measured: `recursivePath()` returns `false` unless an unexported test-only flag is set (`fsnotify@v1.10.1/fsnotify.go:503–515` — `enableRecurse = false` at `:503`, `recursivePath()` at `:507`), so the watch set is ours to maintain and partial coverage is the **default** failure shape, not an exotic one. Coverage must therefore be maintained **forward** as well as counted: a directory created under a watched tree must be added when its `CREATE`/`MOVED_TO` event arrives, and a directory the installer could not add (EACCES, ENOENT mid-walk, a symlink that leaves the root) must be recorded as missing rather than skipped.

**Mandated refusal** (`…mode=poll;reason=watch_partial_coverage`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_partial_coverage",
 "directories_desired":1200,"directories_watched":1197,"missing_count":3,
 "missing":["vendor/cache","build/tmp","node_modules/.pnpm"],
 "detail":"3 of 1200 directories are not watched (vendor/cache, build/tmp, node_modules/.pnpm): the watch set is short and a change under a missing directory produces no event"}
```

The `missing[]` list is capped by the same declared bound the surface already uses for one path list (`extensions.watch.max_paths_per_event`, 4096); above the cap the list is **empty** and `missing_count` carries the full number — never a truncated list presented as complete (the `events` op's existing rule, reused verbatim: spill, never a longer or partial list).

**Degrades to:** poll (`events`, else `rev`). **A client may conclude:** the channel is **not** trustworthy for the listed subtrees — it may be trustworthy for the rest, but a client that cannot bound its reads to "the rest" must treat the whole channel as degraded. **Must never be reported as:** `watching`. A partial watch set is the one case where the watcher *looks* healthy and is quietly wrong; the counter that makes it visible (`directories_watched` of `directories_desired`) is why this reason exists as its own name.

### 4.5 W-5 — `watch_install_failed`

**Probe.** The errno of the failing add, classified — this is the difference between "the system is out of a resource" and "this path was not installable":

| errno | classification | disposition |
|---|---|---|
`ENOSPC` | W-2 (watch ceiling) | tear down the partial set, report W-2 with numbers |
`EMFILE` / `ENFILE` | W-2 (instance ceiling / `RLIMIT_NOFILE`) | same; `limit_name` distinguishes them |
`ENOENT` / `ENOTDIR` | W-5 | the path vanished between the walk and the add — rescan; if it recurs, report W-5 |
`EACCES` / `EPERM` | W-5 | not installable as this uid; record the path in `missing[]` and report W-4 if others succeeded |
`ENOMEM` | W-5 | kernel memory; report and degrade |
`EINVAL` | W-5 | the mask/flag set is not supported by this kernel — a build/portability defect, reported with the errno |

**Mandated refusal** (`…mode=poll;reason=watch_install_failed`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_install_failed",
 "errno":"ENOENT","path":"src/gen","detail":"watch install failed on src/gen: ENOENT (the directory vanished between the walk and the add); the incomplete watch set was torn down"}
```

**Degrades to:** poll (`events`, else `rev`). **A client may conclude:** nothing was watching — there is no window to reason about, only "before this moment nothing was vouched for". **Must never be reported as:** `watching` with a warning, or as W-2 when the errno does not say the kernel is out of watches.

### 4.6 W-6 — `watch_lost`

**Probe.** Two facts, both required: (a) the watcher's own error/exit path observed a stop (its error channel fired, the backend closed, the loop exited), and (b) the re-install that follows **also** failed. Only (a) without (b) is a transient fault to be repaired, not a reason to report an absence — but the interval between (a) and a successful re-install **is unvouched** and must be published as an `overflow` marker to any client whose cursor predates the loss.

**Mandated refusal** (`…mode=poll;reason=watch_lost`):

```json
{"capability":"watch","scope":"target","mode":"poll","reason":"watch_lost",
 "errno":"ENOSPC","since":"2026-09-27T14:22:31-05:00",
 "detail":"the watcher stopped at 2026-09-27T14:22:31-05:00 and could not be re-established (ENOSPC); the interval after that instant is unvouched for"}
```

**Degrades to:** poll (`events`, else `rev`); an open stream **ends**, and the client re-handshakes. **A client may conclude:** the events it received before `since` are complete, the interval after `since` is not — advance past it with a rescan, never with a cursor. **Must never be reported as:** quiet, or as an absence that predates the session (the `since` field exists to say exactly which interval lost its witness).

### 4.7 The overflow marker is not a reason (cross-reference)

`watch_overflow` is deliberately **not** in the seven: an overflow neither removes the watcher nor changes the mode, it invalidates an interval. It is delivered as the `overflow` event (§5), and its state is reflected in `<state>overflow</state>` — not as a refusal reason. A design that folded it into the refusal list would tell a client "you have no watcher" when the truth is "you have a watcher and one lost interval".

### 4.8 W-7 — `watch_boundary_split` (a case the design had not considered; reported, not smoothed over)

**What it is.** A writer whose bytes never land in the superblock the served root resolves to. The concrete shape in this project's own topology: a `bunker` agent's writer lives **inside a container whose view of the tree is an overlay** (`lowerdir` = the served directories, `upperdir` = container-local scratch) — measured present on this host's own docker/containerd stack (mount-table entries of type `overlay` with containerd snapshot layers, App. A.5). A write inside that writer's view is a copy-up into `upperdir`; the served path still resolves to the untouched `lowerdir` file. The local watcher is **right** that the served tree did not change — the served tree genuinely did not — and the poll is **right** for the same reason. The failure is not in the channel at all: the operator believes two paths are one tree, and nothing in the filesystem says otherwise.

**Why it is in this document and not silently omitted.** The row's matrix has five cases; this is a sixth fact with the same client-visible shape ("I changed the file and the reader saw the old bytes"), and it is the one class where *every* mechanism this release builds — push, poll, and the revision poll — is blind together. Folding it into "no watcher" would be exactly the smoothing this contract exists to prevent.

**What can honestly be detected, and what cannot.** Detectable at install, from the mount table: the served root's fstype is `overlay`, or the served root's superblock is not the superblock the deployment's writers use (a `st_dev` comparison made *by the writer's own process*, e.g. an agent-side probe that reports its view's `st_dev` for the tree root). **Not detectable by any watch mechanism**: a write that lands in a different superblock. So the honest reporting is a **warning that is never a degradation**:

- `blocks_push: false` — the watcher is up and correct for the served tree;
- the document carries `target.boundary_split: true|false|"unknown"` + `target.mount_type` + `target.mount_point`, and a `degradations[]` entry with `reason:"watch_boundary_split"` only when it is `true`;
- the detail must state the consequence, not the category: *"the served root is an overlay mount; a writer whose view resolves into a container upperdir writes bytes no watch on this superblock can see, and neither can the poll"*;
- `"unknown"` is a legitimate value and must not be silently rendered as `false` (Bane's null doctrine: a null/gap carries a reason, never a clean default).

**Client consequence, stated for the implementer:** this case must **never** be presented as a channel degradation, because a client that reacts to it by switching mechanisms will have changed nothing. The correctness answer lives on the write path (BFS-004 §6.1's content-hash re-validation), and the operational answer is a topology rule for the operator. Both are named in §10.

---

## 5. THE OVERFLOW GUARANTEE (normative — the most important section in this document)

> **An interval the watcher cannot vouch for is NEVER reported as quiet.**
>
> **An overflow is always a full rescan, never a partial one, and it is always counted.**
>
> **A cursor older than the retained journal forces a resync, never an empty answer.**
>
> **The number of events lost is never invented.**

### 5.1 The three claims, unpacked

1. **Unvouched ≠ quiet.** A quiet answer means "I observed, and nothing moved in the interval I am accountable for" (`events`' cheap answer with `head_seq` at the client's cursor; the stream's equivalent is the `heartbeat` that carries no path claim). Silence is **not** a quiet answer. There are exactly two admissible readings of silence in this contract: *the channel is stalled* (a fault, §5.4) or *the interval is unknown* (`overflow`).
2. **Overflow ⇒ full rescan, always, and never partial.** The events channel already obeys this (`events.go:280–285`: past the path cap **or** an incomplete observation ⇒ `overflow` with an empty `paths[]`, never a diff of the part that fits) and the client already obeys it (`invalidate.go:249–269`: `overflow`, a gap, a tree change, a zero-path `invalidate`, or a path list over the declared cap all become `Resync`, with the reason recorded). The watcher inherits the rule unchanged: **a write-path event that flushes the watcher's own pending set must also be a full rescan** if the set cannot be vouched for.
3. **Counted.** An overflow increments a monotonic counter that is visible in the status record (BFS-045's deliverable; this contract fixes that the counter exists, that it never decrements, and that it counts **overflow events**, not lost changes). A counter that can never move is a gap, not a green check (BFS-032), so the *test* for this counter must include the arm that makes it move (BFS-046).

### 5.2 The forced-resync cases, collected (one rule, five arrivals)

| arrival | why it forces a resync | client-visible |
|---|---|---|
kernel `IN_Q_OVERFLOW` | the kernel's queue dropped events; the loss is unbounded and unreported | `overflow` event |
watcher's internal queue full (its own back-pressure) | ditto, caught before the kernel does | `overflow` event |
a gap in `seq` | a line was lost in transit | client detects from `seq` monotonicity, resyncs, records a gap |
cursor older than the retained journal (server restarted, client offline, foreign cursor) | the range was never retained here | `overflow` event (the ledger's existing rule; the stream must match it) |
tree identity changed | the object watched is not the object bound | per-line `tree` mismatch ⇒ resync |

### 5.3 What a client sees in each case, specified so that resync cannot be mistaken for quiet

| the interval was | what arrives | what `bunkerd` reports | what the client must do | what it must NOT do |
|---|---|---|---|---|
vouched, nothing moved | `heartbeat` at ≤ 30 s (stream) / a cheap empty answer (poll) | `head_seq` equal to the client's cursor; `channel_available:true` | nothing | treat "no line" as "no change" |
unvouched | `overflow` (empty `paths[]`, non-empty `tree`) | `overflow` counter incremented; state `overflow` in the document | drop everything, re-snapshot, advance the cursor **past** the reported `seq` | re-request the missing range; drop only the paths it can see |
unknown because the watcher was never there | the refusal of §4 with its `reason` | the same refusal, three carriers | take the poll mechanism; report the mode/mechanism/reason | retry the stream on a timer |
unknown because the client is behind the journal | `overflow` on the first answer back | retained events whose first `seq` is the gap | resync once, do not page back | treat the retained tail as complete |

### 5.4 The library trap that makes the guarantee a **requirement** rather than a nicety (measured)

This is the single most important implementation fact in this document, and it is not obvious from the library's API surface. In `fsnotify@v1.10.1` (already in this repo's `go.mod:21`, as an indirect dependency):

1. the inotify read loop turns `IN_Q_OVERFLOW` into an error **on the `Errors` channel** — `w.sendError(ErrEventOverflow)` (`backend_inotify.go:397–399`);
2. it then falls through to `handleEvent`, which cannot resolve `wd=-1` in its watch table and returns a **zero `Event{}`** (`backend_inotify.go:428–431` — `byWd(uint32(-1))` misses, `return Event{}, true`);
3. and `sendEvent` **silently drops any event with `Op == 0`** (`shared.go:21–24`: `if e.Op == 0 { return true }`).

So **a watcher that only reads `Events` receives nothing at all when the kernel drops a burst** — no marker, no empty event, no error. Silence. That is precisely the "silent-corruption machine" the PRD names, one layer below where the PRD was looking for it.

And the `Errors` channel is **unbuffered** (`make(chan error)`, `fsnotify.go:280`), as is `Events` on this backend (`defaultBufferSize = 0`, `backend_inotify.go:145`), and `sendError` **blocks** until someone receives (`shared.go:34–45`). The full chain, therefore, is:

```
slow consumer → library reader blocks → kernel queue fills (max_queued_events) → IN_Q_OVERFLOW
→ the overflow notice itself blocks → the reader never advances → the watcher is silent while the process looks perfectly alive
```

Both ends of that chain are **weakened into obligations** on the implementation:

- **O-1 — a dedicated drain.** The watcher runs a goroutine whose only job is to receive from the backend's error channel continuously, and it must never do slow work on that receive. `errors.Is(err, ErrEventOverflow)` ⇒ the state becomes `overflow`, the full rescan runs, the counter increments. (Design consequence: an implementation that "handles errors inline in the read loop" is non-compliant, because the receive that unblocks the library is the same receive that must keep up.)
- **O-2 — liveness is never inferred from the absence of events.** "No events for N seconds" is evidence of nothing: a healthy idle tree and a stalled reader are indistinguishable from outside. Liveness must be produced by the watcher's **own** loop (a heartbeat it emits on a period it controls), and it must be *distinguishable* from the absence of changes. A quiet channel must never be reported as healthy (§5.1 claim 1), and an event channel that cannot prove its own loop is turning must not be reported as `watching`.

**Measured, not argued** (App. A.4): with `max_queued_events = 16384`, 12 000 file creations on a watched directory with the descriptor never read produced the kernel's marker at **event 16 385** — one `IN_Q_OVERFLOW` (wd `-1`, mask `0x4000`) — after ≈48 000 generated events, i.e. ≈31 600 were dropped with **no count available anywhere**. The reproducer is 1.1 s of work and is handed to BFS-046 as the first negative-control cell (§9).

---

## 6. WHAT THE WATCHER DOES NOT PROMISE (normative non-guarantees)

Each line is a property an implementer might otherwise assume. Being explicit is what stops the assumption being built on.

1. **No content hashes.** A change notification says *a path's metadata moved*, never *the bytes are what you expect*. Nothing in this channel verifies content; verification is the write path's re-validation (BFS-004 §6.1, BFS-015) and the client's read-path hash check. **An implementer must not use a watch event as an identity.**
2. **No total ordering beyond the journal cursor.** `seq` is monotonic per tree and is the only ordering promised. Two events about the same path may arrive in either order relative to the kernel's own ordering; *within* one event the `paths[]` list is sorted (deliberately: one tree state produces one path list, `events.go:216–230`) and that sortedness must not be read as causal order either.
3. **No delivery guarantee while the server is down.** Events generated while the process is not running are lost. The recovery is the journal bound: a cursor older than the retained history is answered `overflow`, never with an empty list. There is no replay log beyond `eventsJournalEvents`.
4. **Nothing at all when the target is replaced wholesale.** A mount swap, a re-created tree, a re-pointed `--root`: the watch held the **old** object, and the new one was never watched. This is a **resync event, not a change event** — it must surface as a tree-identity mismatch (+ a `tree` that differs per line, which the client already treats as a resync, `invalidate.go:227–231`) and must not be reported as a `watch_install_failed` or as a burst of per-path changes.
5. **No writer attribution.** The channel cannot say who wrote, or whether the write came through WebDAV, an editor, a build, `git checkout` or a cron job. R1 chose the filesystem as the source precisely because attribution is unavailable and undesirable there.
6. **No file-level watches; per-directory only, and renames cross two directories.** A rename within a directory arrives as two events on that one watch; a rename across directories needs both sides watched — which is one of the ways W-4's partial coverage arises. Symlinks are not followed by a watch: a change behind a symlink inside the tree is a change to the link's target, which may be outside the tree entirely.
7. **No atomicity with the poll ledger.** A `watch` subscriber and an `events` poller are two independent readers of the same tree; nothing makes their observations simultaneous. A client must not mix a cursor from one with the other (the `seq` spaces are per tree and per ledger instance; BFS-041 owns the consumer-side cursor rules).

---

## 7. THE REVISION CONTRACT

### 7.1 The derived artifacts, and which change moves each

| # | artifact | where it lives | a change moves it **today** if and only if… | after the watcher lands, it MUST also move when… |
|---|---|---|---|---|
D1 | `X-Bunker-Rev` for a git tree (`"git:<40 hex>"`) | `tree.go:281–292`, response headers + every envelope + `b:rev` property | `.git/HEAD`'s ref moves (commit, checkout, reset) — **not** an uncommitted working-tree write | a watched, vouched-for change touches the tree |
D2 | `X-Bunker-Rev` for a non-git tree (`"rev:<n>"`) | `tree.go:269–276` (`bumpRev`) | a mutation goes through **this surface** | an out-of-band change is watched |
D3 | `X-Bunker-Tree` (`"tree:<16 hex>"`) | `tree.go:303–310`, every response | the served root's path/dev/inode changes (re-created tree, mount swap) | *nothing new* — its stability is the contract; a swap **must** move it |
D4 | the server's hash cache, `(size, mtime)` per path | `tree.go:32–36, 165–199` (`hashFile`/`forget`/`remember`) | a write through this surface forgets the path; metadata differs on the next read | a watched change forgets the path (`forget(abs)`) — closing the ctime gap of §2.4 |
D5 | the E-6 event ledger (`observed` identities + journal) | `events.go` | its own next observation | a watched change (this is R2's second half: the **server's** derived view, not only the client's) |
D6 | the client's cache (the consumer of all the above) | `internal/fsclient/cache.go` | an applied drop/resync | nothing here — D6 is downstream of D1–D5 by choice |

### 7.2 The rules

- **R-V1 — the revision must move for a change nobody made through us.** After BFS-035, an out-of-band change that the watcher vouches for **must** advance the served revision, so that the one-call snapshot op's currency claim is aligned with reality. Today it is not (§2.4 blind spot 1). This is R2 of the PRD stated as a testable requirement.
- **R-V2 — and it must not cost a stat.** The revision must be advanced by the **watcher's own event**, never by a stat-per-file walk on a read. PRD §5 is explicit that the snapshot op is the reason this project exists and R2 "must not turn a stat-free snapshot into a stat-per-file one". A revision that only becomes correct when you measure the tree is not a revision; it is a rescan wearing its name.
- **R-V3 — the revision's kind is declared, and may be extended honestly.** `extensions.rev.kind` declares `git` | `counter` today (`ops.go:445`, `revKind()`). A composite token is admissible **only if declared under a new kind value** and if it remains a monotonic-per-tree opaque string. An old client treats any moved revision as a resync (`invalidate.go:544–546`) — correct, merely expensive, and only on the third-tier mechanism.
- **R-V4 — an artifact that cannot move must be reported as a gap, not left as a green check.** If a build cannot align D1/D2 with out-of-band change (e.g. the watcher is absent), the honest report is the absence (`watch_*` reason + `mode=poll`), **not** a revision that claims to be current. This is the BFS-032 lesson applied to a revision.
- **R-V5 — D3 must not be repurposed.** `X-Bunker-Tree` is an identity, not a change counter: it is opaque to clients and moves only on replace/recreate. Using it as a change signal would break AC-7 (a destroyed and re-created tree binding to a different token) and is forbidden.
- **R-V6 — the derived artifacts must agree about what a change is.** D4 (size+mtime) and D5 (size+mtime+ctime) currently disagree about a same-size, same-mtime edit (§2.4 blind spot 2). The watcher closes it by invalidating on the event; a build without a watcher must **not** silently claim D4 is as sharp as D5 — the `events` mechanism is the one that carries ctime, which is one more reason the client's preference for `events` over `rev` is load-bearing and not cosmetic.

---

## 8. CAPABILITY NEGOTIATION (normative)

### 8.1 The rule

**A client must never infer a watcher — or its absence — from a configuration flag, a build tag, an option value, or a negotiated HTTP version.** The only admissible sources are the three carriers below, all of which describe the **running** process. This is BFS-004 §4.1's own rule ("a client must never infer capability from the negotiated version") extended to the one capability that is easiest to fake.

### 8.2 The surface (additive; `document_version` stays 1)

The `capabilities` op's `extensions.watch` block gains fields and keeps its existing ones verbatim, so an old reader sees a document it already understands:

```json
"watch": {
  "name": "X-Bunker-Op: watch", "v": 1,
  "mode": "push|poll",
  "heartbeat_ms": 30000,
  "max_paths_per_event": 4096,
  "modes": {"push": "inotify→stream", "poll": "X-Bunker-Op: events"},
  "state": "watching|overflow|lost|absent",
  "reason": "<one of the seven>|null",
  "blocks_push": true,
  "backend": "inotify|kqueue|fen|none",
  "target": {
    "mount_type": "ext4", "mount_point": "/",
    "network_backed": false,
    "boundary_split": false
  },
  "coverage": {
    "directories_desired": 1200, "directories_watched": 1200,
    "complete": true, "headroom": 512
  },
  "limits": {
    "watching": {"limit_name": "fs.inotify.max_user_watches", "configured": 1048576,
                 "watches_held_by_this_process": 1200, "desired": 1200, "headroom": 512},
    "instances": {"limit_name": "fs.inotify.max_user_instances", "configured": 1024},
    "queued_events": {"limit_name": "fs.inotify.max_queued_events", "configured": 16384}
  },
  "counters": {
    "overflows_total": 0, "rescans_total": 0, "install_failures_total": 0,
    "overflow_dropped_events": null,
    "overflow_dropped_reason": "the kernel reports one overflow marker, not how many events it dropped",
    "last_event_age_ms": 412,
    "changed_since": "2026-09-27T14:02:00-05:00"
  }
}
```

Rules for this block:

- **It reports the running process, not the configuration** — the same rule the `transports` block already lives by (`ops.go:456–470`). A watcher configured but not established is `state:"absent"` with its `reason`, never `mode:"push"`.
- **Every reason in §4 appears in exactly one place here**: `reason` + `state` + `blocks_push`. Where a reason is `true` for `blocks_push`, the same reason also appears as the op's refusal (§4) and as a `degradations[]` entry. **W-7 is the exception**: `blocks_push:false`, a `degradations[]` entry for the *warning* with the mode actually in force, and no refusal.
- **`null` carries a reason.** Any field that cannot be measured is `null` with a sibling `*_reason` string (`overflow_dropped_events` is the worked example; `boundary_split:"unknown"` is the state-value form). A silent `false`/`0` for an unmeasurable fact is a fabricated measurement and is forbidden.
- **`counters` are the observability hand-off** (BFS-045): this contract fixes *which* counters must exist and what they mean; the record's layout, naming and drill-down are BFS-045's.

### 8.3 The client's decision table (the consumer's half, fixed here so §8.2 has a purpose)

| what the probe says | mechanism the client uses | what it must NOT conclude |
|---|---|---|
`state:"watching"` and the stream establishes | `watch` (push) | that silence means "no change" (§5.4 O-2) |
`state:"absent"` + any `reason` with `blocks_push:true` | poll: `events` if `X-Bunker-Op: events` is served, else `rev`; and report the reason | that the reason is transient, or that a retry loop will fix W-1/W-3 |
an `overflow` event | full resync on the **same** mechanism; record it | that the mode changed, or that the missing range should be re-requested |
`state:"lost"` mid-session | the stream ends; re-handshake; resync past the loss | that the interval before the loss is in doubt (it is not) |
`target.boundary_split:true` | nothing — keep the mechanism; surface the warning | that changing the mechanism can help (it cannot) |
`state` unreadable / old document (no `watch` block) | probe the op exactly as today (BFS-026 path) | that a config flag on either side implies a watcher |

**Compatibility invariant** (this is the release's own rule, applied here): for every one of these rows, a client that implements only today's behaviour takes today's branch, because the verdict, the status, `scope`, and `mode` are unchanged; the discriminator is a new field it never looks at.

---

## 9. Boundaries — what this document does NOT decide

| row | its surface | the interface this document hands it |
|---|---|---|
**BFS-041** (push wire form) | long-poll vs stream, the resume cursor, reconnect/backoff, the subscriber bound | The `overflow`/`heartbeat`/`invalidate` event semantics of §5; the forced-resync table (§5.2); "resync ≠ quiet" (§5.3). **Not decided here:** whether the transport is a stream or a long poll, the cursor's wire name, the reconnect schedule, and how a dead subscriber is bounded. (`events.go`'s header already notes the poll carries no stream; that note stays BFS-041's to act on.) |
**BFS-042** (hot-file policy) | popularity accounting, the size rule, queue stop semantics, the pool share | Only this: the hot-file refresh is a **performance** feature and must not become a correctness dependency (PRD §2.4). Nothing about scoring, sizes, or pool shares. |
**BFS-043** (server config) | watcher/push knobs, defaults, validation, the refusal path for an unhonourable value | The state/reason vocabulary (§3.2, §4) that any configured watcher must report through, and the `limits.*` + `coverage.headroom` values a config surface must be able to express. **Not decided here:** knob names, defaults, validation rules. |
**BFS-044** (client config) | mount flags, defaults, validation, reported bounds | §8.3's decision table (what a client does with each state) and the compatibility invariant. **Not decided here:** flag names or defaults. |
**BFS-045** (observability) | the status record: every bound counted and visible | The counter set of §8.2 (`overflows_total`, `rescans_total`, `install_failures_total`, `overflow_dropped_events` + reason, `last_event_age_ms`, `changed_since`) and the configured-vs-observed fields (§4.2). **Not decided here:** record layout, naming, aggregation, retention. |
**BFS-046** (tests) | smoke, integration, E2E, negative controls, coverage floor | §11's acceptance criteria, and the pinned reproducer of §5.4 as the first negative-control cell. **Not decided here:** the test program, harness, or floor. |
**BFS-026** (landed) | the poll form | Unchanged and re-used verbatim as the fallback (§2.2). This document mandates no change to it. |

**Explicitly out of scope:** the watcher's implementation, any config surface for it, any test, any board row (this row reports; it does not file), and any client-side change (R-2's fix, §10).

---

## 10. Residuals and holes found while writing this (reported, not smoothed over)

| # | finding | evidence | disposition |
|---|---|---|---|
R-1 | **The five-case matrix was missing a sixth.** A writer whose view of the tree resolves to a different superblock (container overlay upperdir; bind of a copy) writes bytes **no** mechanism can see — not the watcher, not the poll. | this host's own mount table carries `overlay` mounts with containerd snapshot layers (App. A.5) | Added as **W-7** with `blocks_push:false` (§4.8). Detection is partial by construction and `"unknown"` is a permitted value. Needs an owner for the *operational* control (topology check + operator rule); recommended: BFS-043 for the config-side check, BFS-046 for the cell. **Not filed as a row by this worker** (the brief forbids it). |
R-2 | **The client's revision-poll claim is false on a git tree.** `invalidate.go:508–513` says one `rev` poll answers "is my view still the tree's view?" for the whole tree; on a git tree `rev` is HEAD, so an uncommitted out-of-band write moves nothing (§2.4, §7.1). | source read: `tree.go:281–292` + `invalidate.go:544–546` | Fixed at the **contract** level by R-V1 (§7.2). The **code** fix (server advancing the revision on a watched change) is BFS-035's; if the revision cannot be advanced without a stat, the honest alternative is R-V4 (report the gap). Recommended owner if a code change is needed beyond BFS-035: a follow-up row. |
R-3 | **D4 and D5 disagree about what a change is** — the server hash cache keys on (size, mtime); the event ledger includes ctime because that is exactly the miss class it was built for. | `tree.go:32–36, 165–192` vs `events.go:31–36, 97–102` | Named in the revision contract (R-V6). The watcher-closed version is D4's row in §7.1. No separate row recommended — it is the same fix as R-2's. |
R-4 | **A netbacked target has a better answer than "degrade to poll".** Watch at the *writer* (the remote side) and relay. | PRD §2.3 asks only for identification | Reported as a design direction, **not** specced here: it crosses into the remote-agent surface. If it is ever wanted, it needs its own row. |
R-5 | **The per-user watch total is unobservable**, so "bounded with headroom" cannot be a pre-flight computation in general — only a pre-flight against the *configured ceiling* plus this process's own held set. | App. A.3 (procfs contains only the three ceilings; `/proc/self/fdinfo` gives this process's watches) | Folded into W-2's mandated probe (attempt + classify, with numbers reported). This is a **correction to the row's own framing** ("exceeding it fails quietly"): the failing add is loud (`ENOSPC`); what is quiet is the aggregate and the loop that ignores it. |
R-6 | **The library drops the overflow notice on the event channel.** `sendEvent` discards `Op == 0`, and an overflow record resolves to a zero `Event`. A watcher reading only `Events` gets **no signal at all**; a watcher that never drains `Errors` **blocks the reader** (unbuffered channel). | `backend_inotify.go:397–399`, `backend_inotify.go:428–431`, `shared.go:21–24`, `shared.go:34–45`; live repro App. A.4 | Turned into obligations O-1/O-2 (§5.4) and the first negative-control cell for BFS-046. |
R-7 | **The library's recursive watch is disabled** (`enableRecurse = false`, tests only), so partial coverage is the default failure shape rather than an edge case, and coverage must be maintained forward on `CREATE`/`MOVED_TO`. | `fsnotify.go:503–515` | Folded into W-4's probe and its forward-maintenance requirement. |

---

## 11. Acceptance criteria for BFS-035 (each with the control that proves it can fail)

| # | criterion | proof |
|---|---|---|
A-1 | every reason in §4 is reachable by its own probe, and each reports its **own** name | one cell per reason, each asserting `reason`, `scope`, `mode`, `blocks_push`; plus the control that a build *without* the probe reports no reason rather than a default one |
A-2 | the refusal shape is unchanged for an old client | the four-field assertion (`verdict`, `status`, `scope`, `mode`) byte-identical to §2.3; plus the stock-client arm from BFS-026 §7.2 re-run |
A-3 | `reason` reaches all three carriers | one cell per carrier (envelope `error.reason`, `X-Bunker-Capability` part, document `extensions.watch.reason`) + the mutation control that removing one carrier fails the cell |
A-4 | **an overflow forces a full rescan and is counted** | the §5.4 reproducer (`max_queued_events` reached ⇒ marker) drives one `overflow`; assert the counter moves **and** that a *partial* path list was never emitted; the control is a quiet interval, which must show counter-flat + empty answer |
A-5 | **the overflow notice cannot be lost by a reader that only watches events** | a cell that runs the watcher with the `Errors` drain disabled and asserts it **fails** (this is R-6 turned into a red arm) |
A-6 | a stalled reader is not reported as quiet | with the drain blocked, assert the state is never `watching` and no interval is reported as vouched; the control is the healthy path reporting `last_event_age_ms` |
A-7 | W-2 reports **configured vs observed**, and never keeps a partial set | a fixture ceiling small enough to exhaust (a per-instance/`RLIMIT` fixture, or an injected ceiling) asserting the five numbers + teardown; the control is a successful install reporting `directories_watched == directories_desired` |
A-8 | W-4's `missing[]` never lies about its bound | a tree with more missing directories than the cap ⇒ empty list + full `missing_count`; the control is a short list, verbatim |
A-9 | W-3 identifies the mount type and does not change verdict | a fixture root on a fuse/network mount type (or an injected fstype) asserting `mount_type`/`mount_point`; the control is a local ext4 root reporting `network_backed:false` |
A-10 | the revision contract holds without a stat-per-read | assert `X-Bunker-Rev` moves after a vouched-for out-of-band change (R-V1), and that the snapshot op's call count/stat count is unchanged (R-V2); the control is an unvouched interval, which must not move it |
A-11 | a wholesale replacement is a resync, not a change burst | swap the served root under the watcher; assert a tree-identity change and a resync, and that no per-path `invalidate` for the old tree is emitted |
A-12 | state is discoverable and never inferred | assert the document's `state`/`reason`/`blocks_push` for each §4 case, and that a client given no `watch` block behaves exactly as today (§8.3's last row) |

---

## Appendix A — every number and every measured fact used above, and how it was taken

| # | fact | value | how taken |
|---|---|---|---|
A.1 | the ceilings on a fleet host, as the **configured** side of configured-vs-observed | `fs.inotify.max_user_watches = 1048576`, `max_user_instances = 1024`, `max_queued_events = 16384` | `cat /proc/sys/fs/inotify/{max_user_watches,max_user_instances,max_queued_events}` |
A.2 | the upstream default the row names | "commonly 8192" | the row's own text; the host above is tuned upward, which is exactly why `limit_configured` must be reported rather than assumed |
A.3 | this process can count its **own** watches; the per-user **total** is not observable | `/proc/self/fdinfo/<fd>` returned one `inotify wd:N ino:… sdev:… mask:fff ignored_mask:0 …` line per watch (2 watches ⇒ 2 lines, plus `pos/flags/mnt_id/ino`); `/proc/sys/fs/inotify` lists exactly the three ceilings and no in-use counter | a `ctypes` `inotify_init1` + `inotify_add_watch` probe on a mktemp dir; `/proc` reads |
A.4 | `IN_Q_OVERFLOW` is reproducible, and the drop count is unknowable | 12 000 file creations on a watched directory, descriptor never read: **16 385** events read back, exactly **one** `IN_Q_OVERFLOW` (wd `-1`, mask `0x4000`) at event 16 385, ≈4 events per file ⇒ ≈31 600 dropped with no count reported. Runtime 1.1 s. | a `ctypes` inotify probe with `IN_NONBLOCK`; `max_queued_events` read first |
A.5 | mount-type detection by longest-mountpoint-prefix works, and this host has overlay/containerd layers | `/home/kara/bunker → ext4 @ /`, `/run/user/1000/gvfs → fuse.gvfsd-fuse`, `/mnt/passport → fuseblk`; `overlay` entries with `lowerdir=…snapshotter.v1.overlayfs…` present in `/proc/self/mountinfo` | `/proc/self/mountinfo` parse with longest-prefix matching |
A.6 | the poll form's bounds | path cap 4096, journal 256, scan limit 100000 | `internal/server/webdav/events.go:74–81` |
A.7 | the refusal as served today | §2.3 verbatim | `internal/server/webdav/ops.go:84–90`, `:456–461`, `:494–499` |
A.8 | the library facts (already in `go.mod`) | `fsnotify v1.10.1` indirect (`go.mod:21`); overflow ⇒ `ErrEventOverflow` on `Errors` (`backend_inotify.go:397–399`); zero `Event` for `wd=-1` (`:428–431`); `Op == 0` dropped (`shared.go:21–24`); `Errors` unbuffered and blocking (`fsnotify.go:280`, `shared.go:34–45`); `defaultBufferSize = 0` (`backend_inotify.go:145`); recursion disabled (`fsnotify.go:503–515`) | source read of the module cache copy of v1.10.1 |
A.9 | the revision/hash-cache facts | `rev = "git:"+HEAD` with a 500 ms memo (`tree.go:281–292`, `gitRevCacheTTL`); `bumpRev` advances only the counter (`tree.go:269–276`); `hashEntry` = (size, mtime) with no ctime (`tree.go:32–36, 165–192`); the event ledger's identity includes ctime (`events.go:97–102`) | source read |
A.10 | the client's vocabulary and branches | modes `push|poll`; mechanisms `watch|events|rev|none`; the poll branch keys on verdict/status only; the rev-poll resync reason text | `internal/fsclient/invalidate.go:44–62, 305–307, 508–547`; `capabilities.go:131–140` |

Probes A.1/A.3/A.4/A.5 were run in a `mktemp` directory outside the repository; nothing in the tree was written by them.

## Appendix B — how this document relates to the documents it inherits

- **`PRD-bunker-invalidation.md`** — the design authority. §1 (R1, R2) is realised here as §7; §2.1 as §5; §2.2 as §4.2/§4.4/§4.5; §2.3 as §4.3; §2.7 ("a bound the owner cannot see is not a bound") as §8.2's counter set and §4.2's numbers. This document does not re-argue any of those decisions.
- **`PRD-bunker-fs.md`** — AC-9 (a missing capability still mounts, names itself, degrades visibly) is the ancestor of §3/§4; the "capability variance is an ERROR, not a dynamic surface" paragraph is why a missing watcher is a refusal with a reason and never a silent absence.
- **`BFS-004-webdav-surface.md`** — §3 E-6 fixes the event names and the three declarations (heartbeat ≤ 30 s, ≤ 4096 paths per event, `tree` on every line), §4.2 the capability document, §5.1–§5.3 the refusal vocabulary and the `scope` field, §10.5 the bytes. This document **adds a field** (`reason`) and **adds no verdict code**, so §5.1's completeness claim survives.
- **`BFS-005-client-cache-and-diff.md`** — §4.1 (the resync closure), §4.2 (the ordered drop), §4.4 (the declared poll fallback) are the client's half; §8.3's decision table defers to them and adds nothing of its own.
- **BFS-026's evidence document** — the inventory in §2.2 is that row's landed behaviour, re-measured against the tree on this branch rather than quoted from its prose.
