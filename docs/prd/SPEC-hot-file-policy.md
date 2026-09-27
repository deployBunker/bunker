# SPEC: the hot-file policy — accounting, the size rule, the queue, the pool, and promotion

**Project:** bunker (owning) · **Row:** BFS-042 (P1, complexity 3) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-27
**Status:** proposed — the contract an implementer builds BFS-037 against. **No product code changed by this row.**
**Deliverable:** this document. Nothing else — no code, no board row, no config surface.
**Design authority (inherited by reference, not restated):** `docs/prd/PRD-bunker-invalidation.md` §2.4 (the performance-only boundary), §2.6 (promotion must not double-fetch), §2.7 (a bound the owner cannot see is not a bound), §3 (the "hot" data type and its four properties) and R4–R9 (§1). That document is the *why*; this one is the *contract*.
**Implemented by:** BFS-037 (the hot-file refresh), which depends on BFS-035 (the watcher) and BFS-038 (atomic publish, which must land first — PRD §4).
**Consumed by:** BFS-044 (client config flags, defaults, validation), BFS-045 (the status record), BFS-046 (the test program).
**Boundaries (named, not crossed):** BFS-031 and BFS-032 (filed defects — referenced, not repaired), BFS-038 (the atomicity representation — **consumed**, not respecified), BFS-040 (the events contract — consumed), BFS-041 (the push channel — not touched), BFS-043 (server config), BFS-044 (client config surface), BFS-045 (record layout), BFS-046 (the test program). §11 states the exact interface handed to each.

**Why this is its own file.** Same three reasons as its sibling `SPEC-watcher-capability.md`: five sibling specs are being authored concurrently against a fleet committing to this repo, so every row needs its own path to keep commits path-limited and rebasable; the PRD argues a design while this fixes a contract an implementer is graded against; and the boundaries above are only expressible as a document of their own. It imports the PRD's decisions, re-argues none of them, and corrects two of them in §12 where they do not survive contact with BFS-031's finding.

---

## 0. Verdict in one screen

| # | what the row asked for | the answer this document fixes | where |
|---|---|---|---|
| 1 | **popularity accounting** — weights, decay, cold start, tie-break, what a full tracker does | Read weight **1**, edit weight **8**, decay **0.9 per 300 s step** on touch **and** on comparison (≈ **32.9 min** half-life). Bounded in **entries** (4096) *and* bytes (1 MiB), both reported. **Cold start is empty and does no walk** — on a 400k-file tree a fresh tracker costs zero reads, zero stats, zero requests. Tie-break: lowest effective score, then stalest touch, then lexicographically first path. Full tracker: a newcomer is admitted only if it outranks the current minimum, else it is refused and counted. | §3 |
| 2 | **the size rule** | `hot_max_file_bytes` = **8 MiB**, **inclusive** (`<=`), re-checked **after** the HEAD against the served size. Over the ceiling ⇒ never queued and never promoted, the read is served exactly as with the hot path compiled out, and **every skip is counted by reason** — never silent. Size rule above the cache bound, or above the per-entry cap, is a **mount-time refusal** naming both numbers (BFS-044 owns the message). | §4 |
| 3 | **the queue contract, and what STOP IN FULL means** | Depth **256**; per-path dedupe; replacement is **by score, not FIFO** (a cold 10k-path churn may never displace the file the user is editing). **STOP IN FULL is one thing and it is defined in one sentence** (§0.1(c), §5.5) — and the two other readings an implementer would otherwise pick are named and *refused*. Every abandoned item is provably safe to abandon, per state (§5.6). | §5 |
| 4 | **the pool policy** | Refreshes hold at most **`floor(Concurrency/8)` = 3** of the client's 25 slots (hard ceiling), governed day-to-day by **`hot_refresh_max_inflight` = 2**. Foreground **always** outranks a refresh: either can never be delayed by the other, and the arithmetic that guarantees it is stated. Backoff: **250 ms · 2ⁿ, cap 30 s, full jitter**. Hang-up: deadline **10 s**, reacquire window **30 s** — abandon rather than starve, and abandonment is safe by §5.6. | §6 |
| 5 | **promotion** | A queued refresh asked for by a real read becomes a **full foreground read**: it leaves the queue, takes a foreground slot, and does not sleep. Requires a **per-path single-flight map**, so the promotion **hands over the existing in-flight fetch** — §7 states the cancellation semantics for both the promoted fetch and the queue entry it replaced, and names the invariant that proves single-flight (**N concurrent readers ⇒ exactly 1 GET**). | §7 |
| 6 | **what it does not do** | Twelve prohibitions (§8), including the four the row names: nothing during the mount's first sync, nothing for a path the client has never read, nothing while stopped, and nothing that would evict a blob an in-flight reader holds (**BFS-038 owns that rule** — this spec consumes it and states the abandon-instead-of-evict obligation). | §8 |
| + | **a hole found while writing it (reported, not smoothed over)** | PRD §2.5's *"the size/eviction accounting must count published blobs, not in-flight ones"* and BFS-031's *"the bound must bound the whole directory, and the reported figure must agree with `du`"* **cannot both hold unamended** once a refresh writes bytes before publishing. Resolved in §5.4 by splitting **eviction accounting** (published only, as §2.5 says) from **admission reservation** (published **+ in-flight**, which is what bounds the directory), and by reporting the two figures separately. **F-1.** Two further corrections: decay-on-touch alone does **not** make an abandoned favourite fall out (**F-3**), and a second unaccounted file in the cache directory would repeat BFS-031's exact shape (**F-2**). | §12 |

### 0.1 The row's four evidence asks, answered

**(a) Every number, with its justification** — Appendix A is a single table of every number this document chooses, its default, its unit, why that value, and where it is reported. §9 is the same set as a bound census, so nothing in this spec is a number an implementer has to invent.

**(b) The exact sentence that makes the performance-only boundary load-bearing** — §1, **P-0**, verbatim:

> **The hot-file refresh is a PERFORMANCE-ONLY subsystem: correctness is supplied entirely by the invalidation channel plus read-path validation, so any state this subsystem holds — a score, a blob, a published pointer, an in-flight fetch, a stopped queue, or the whole tracker — may be discarded at any moment and without notice, and no read may return different bytes for it.**

Its falsifiable corollary is **P-1** (the subsystem must be *deletable*: a build with the hot path compiled out must pass every read-correctness cell of BFS-046 unchanged) and the acceptance cell that makes it load-bearing is **AC-14** — the cell that runs the correctness battery with every hot-path counter forced to its failure value and requires the battery to be unmoved.

**(c) STOP IN FULL, in one sentence** — §5.5, verbatim:

> **STOP IN FULL means no refresh is admitted, the queue is emptied at once, and every in-flight refresh is abandoned at its next yield checkpoint with its temporary blob deleted, its pool slot released and its reservation released — while a fetch that has already been promoted to foreground is untouched, because it is no longer the refresh's to abandon.**

**(d) The boundary this document did not cross** — §11, in one line each: **BFS-038** owns the atomic-swap representation and the refcount rule (this spec consumes both and only states what the refresh must *do* when the rule says it cannot publish); **BFS-040** owns the events contract and **BFS-041** the push channel (this spec reacts to an `invalidate` and says nothing about how it arrives); **BFS-031**, **BFS-032** are filed defects — they are used as *failure shapes to avoid* and are not repaired here; and no config flag, no counter *layout*, no test, and no board row is created.

### 0.2 What this document is not

It is not the hot-file *feature* spec. It fixes the policy numbers, the queue/pool contracts, the promotion rule and the prohibitions; BFS-037 implements them, BFS-044 names the flags, BFS-045 lays out the record, BFS-046 writes the cells. Every requirement below is stated so that those four rows can be graded against it without re-deciding anything.

### 0.3 Where this runs, and against what

The hot path is **client-side**. PRD R4 says to track "which files *this client* reads and edits most" and §3 says the tracker is "persisted per endpoint, keyed the same way the cache dir is keyed", so it lives in `internal/fsclient/` and is owned by the mount in `internal/fsmount/fs_linux.go`. The "shared global connection pool" of R7 is this client's **own** pool: `Client.sem` (`internal/fsclient/client.go:103,164`) sized by `--concurrency` (default `DefaultConcurrency = 25`, `client.go:26`) plus the transport's connection caps (`client.go:151–158`). There is no server-side component in this document, and **D-10** forbids inventing one.

---

## 1. The boundary, stated first, because every other rule is subordinate to it

> **P-0 — THIS SUBSYSTEM IS PERFORMANCE ONLY.** The hot-file refresh is a PERFORMANCE-ONLY subsystem: correctness is supplied entirely by the invalidation channel plus read-path validation, so any state this subsystem holds — a score, a blob, a published pointer, an in-flight fetch, a stopped queue, or the whole tracker — may be discarded at any moment and without notice, and no read may return different bytes for it.

This is PRD §2.4 stated as a requirement on the implementer rather than as an argument, and it is load-bearing in three concrete ways an implementer will otherwise violate:

1. **Discardable, not persistent.** Nothing in this subsystem may become a precondition for a correct read. A tracker file that cannot be parsed, a refresh that was killed mid-flight, a queue that was stopped at an arbitrary instant, and a tracker that was deleted entirely must all leave the mount functionally identical to a mount that never had a hot path. **No error from this subsystem may ever be surfaced to a reader as a read failure.**
2. **The hot cache is a HINT.** The bytes a read returns come from the cache only because the cache's own validity rules say so (the invalidation channel plus read-path validation — BFS-005, BFS-026, BFS-004 §6.1). The hot path *populates* that cache; it never extends what the cache is allowed to serve. In particular a refresh publishes a blob with the **same provenance and the same validity obligations** as a foreground read's blob — it does not get to skip validation because it was proactive. **If a refresh's blob is stale, the read path must reject it exactly as it would reject a stale foreground-fetch blob.**
3. **Never a gate, never a blocker, never a promise.** The hot path may not delay, veto, reorder or fail a foreground read or write (§6.2), may not hold a lock across a network wait (§6.5), and may not be reported as a guarantee of warm reads (a read may always go to the server; that is not an error, it is the design).

> **P-1 — the deletability test.** A build with the hot path compiled out (or a mount run with it force-disabled) MUST pass every read-correctness acceptance cell of BFS-046 unchanged. This is the operational form of P-0: it is the only test that can fail when a hot-path assumption has leaked into the read path.

> **P-2 — the subsystem may never be the reason a bound moved.** No hot-path activity may increase `used_bytes` beyond what the published-blob accounting allows, nor make any other reported figure disagree with what is on disk. Where this spec requires an extra reservation to keep that true, §5.4 says so.

*On the requirement numbering:* **P** is one ordered series of policy requirements across this document — **P-0…P-2** are the boundary (§1) and **P-3…P-18** are the pool policy (§6). The prefix is shared so that a citation is unambiguous without a section number; the order is the order of appearance.

---

## 2. What is true today (measured, not assumed)

The policy below is written against what is actually in the tree on this branch. Everything here was read, not recalled.

| fact | value | where |
|---|---|---|
| the client's request pool | `sem chan struct{}` of `--concurrency` (default **25**); transport `MaxConnsPerHost`/`MaxIdleConnsPerHost` = the same number; `MaxIdleConns` = 2× | `internal/fsclient/client.go:26,103,147–158` |
| one operation's deadline | `DefaultOpTimeout = 30 s`; the mount-time probe is `DefaultBindTimeout = 5 s` | `internal/fsclient/errors.go:231–233` |
| the cache bound | `DefaultCacheMaxBytes = 268435456` (256 MiB); `DefaultCacheMaxEntryBytes = 67108864` (64 MiB, "one entry may not monopolise a quarter of the bound"); `DefaultCacheMaxAge = 1 h` | `internal/fsclient/cache.go:17–26` |
| what `used_bytes` counts | published blobs + serialized index bytes (`usedLocked`/`recountLocked`); **not** `status.json`, **not** `conflicts.jsonl` | `internal/fsclient/cache.go:592–623` |
| the cache directory's other occupants | `index.json`, `blobs/` (content-addressed; `writeBlob` publishes by temp+rename), `status.json`, `conflicts.jsonl` | `cache.go:27–30`, `cache.go:360–387`, `status.go:145–149` |
| the mount directory's key | `MountID(baseURL) = sha256(TrimRight(baseURL,"/"))[:12]`, and the dir is `$XDG_CACHE_HOME/bunker/fs/<mount-id>` | `internal/fsclient/status.go:131–143` |
| the cache's eviction order | unreferenced entries, least-recently-**hit** first; ties by **largest blob** first; remaining ties **lexicographic path**; pinned blobs are never candidates | `cache.go:405–450` |
| the read path's cache admission | a **pre-filter** compares `len(data)` against `MaxEntryBytes` before calling `Cache.Insert` — so `Insert`'s own oversize branch is unreachable from the live read path | `internal/fsmount/fs_linux.go:1093,1100` vs `internal/fsclient/cache.go:312` — **this is BFS-032** |
| the channel that will drive refreshes | `invalidate` (path list) / `heartbeat` / `overflow`; poll default `DefaultPollInterval = 2 s`; `MechanismWatch/Events/Rev` | `internal/fsclient/invalidate.go:27–66,91` |
| the per-event path bound | `max_paths_per_event = 4096` | `SPEC-watcher-capability.md` §2.2/§2.3, `internal/server/webdav/ops.go:456–461` |
| the mount's first sync | the bind-time baseline: capability handshake + `PinTree`, then the optional initial snapshot / PROPFIND path | `internal/fsmount/fs_linux.go:203–238` |
| the ops the refresh may use | `Client.Head(ctx,path) (*FileMeta, *OpError)` and `Client.Get(ctx,path,ifNoneMatch) ([]byte,*FileMeta,*OpError)` — no new server op is needed | `client.go:455,497` |
| **there is no hot-file code today** | zero hits for a hot tracker anywhere in the tree | `grep -rni 'hot.file' --include=*.go` ⇒ none |

**Read this table as the shape of the trap.** BFS-031 found a bound that named one thing and bounded another (reported 1 KiB, directory at 30,689 B, 29.97×). BFS-032 found a counter that exists, is displayed, and can never move because a pre-filter upstream of it makes its increment unreachable (`fs_linux.go:1093,1100` ⇒ `cache.go:312`). Both shapes are one decision away in every section below, which is why §4.5, §5.4 and §9 exist.

---

## 3. POPULARITY ACCOUNTING

The tracker is one map, one decay rule, and two bounds. Nothing else.

### 3.1 The score

```
score(p) ← score(p) · decay^(elapsed(p) / decay_step) + weight(op)
```

| id | rule | value |
|---|---|---|
| **H-1** | `hot_weight_read` | **1.0** — a read is a weak signal (a search, a scan and a build all read) |
| **H-2** | `hot_weight_edit` | **8.0** — an edit is a strong signal: the file was worth opening *and* will be revisited (PRD §3's own argument). 8 is the smallest power of two that outranks a burst of seven neighbouring reads, and it is one hex digit in a config |
| **H-3** | `hot_decay` / `hot_decay_step` | **0.9** per **300 s** step. Effective half-life **1973.6 s ≈ 32.9 min** (Appendix A.2). Retained: 90% at 5 min, 73% at 15 min, 53% at 30 min, 28% at 1 h, 8% at 2 h |
| **H-4** | when decay is applied | **On touch** (`elapsed(p)` = now − `touched_at(p)`, then `touched_at ← now`) — the PRD's decay-on-touch, so no sweeper exists. **And on comparison** (H-5, used only in §3.3) — see H-5 and F-3 |
| **H-5** | the comparison form | `effective(p, now) = score(p) · decay^((now − touched_at(p)) / decay_step)`. Used **only** for the eviction candidate and the admission test, so it costs no traversal and changes no stored state. This is what makes "an old favourite falls out with no sweeper" true — decay-on-touch alone cannot do it (**F-3**, §12) |
| **H-6** | score ceiling / renormalisation | `hot_score_ceiling = 10000`. When the maximum effective score exceeds it, every score is multiplied by 0.5 in one pass (`hot_renormalisations_total`). A uniform scale changes **no** ordering, so renormalisation is semantically a no-op — and that invariant is asserted, not assumed (AC-3) |

**Bounded, twice.** The tracker is bounded in **entries** *and* in **bytes**, and both are reported (PRD §3 asks for exactly this).

| id | bound | value | why this value |
|---|---|---|---|
| **H-7** | `hot_tracker_max_entries` | **4096** | The working set of an editing session, at three orders of magnitude below the 400k-file tree the PRD names. At ≈160 B/entry serialised that is ≈640 KiB — so the byte bound below is a backstop, not the binding one |
| **H-8** | `hot_tracker_max_bytes` | **1048576** (1 MiB) | 0.39% of the 256 MiB cache bound, and it is enforced on write rather than hoped for |

### 3.2 What a touch is, exactly

| id | rule |
|---|---|
| **H-9** | A **read touch** is recorded once per path per **`hot_read_touch_window = 5 s`**. Consecutive reads inside the window do not touch again (`hot_read_touches_total`, `hot_read_touches_deduped_total`). **Why a window:** a FUSE read of an 8 MiB file arrives as ~64 chunked calls; without the window one file's single access would score ~64 and the tracker would fill with one pattern (**F-6**). The window is a countable dedupe, not a heuristic |
| **H-10** | An **edit touch** is recorded for a write that **changed the content**. The `identical_content` no-op (`PutResult.Noop`, `client.go:90–91`) does **not** touch: it changed nothing, so it is not evidence of interest (`hot_edit_touches_total`) |
| **H-11** | **A refresh never touches.** A background refresh is derived from a read that already touched; counting it would make a hot file self-reinforcing and permanently undisplaceable. Asserted directly: N refreshes leave every score bit-identical (AC-1's third arm) |
| **H-12** | A failed read or a failed write does **not** touch. Popularity is evidence of demand, not of traffic |

### 3.3 Eviction, admission, tie-break

| id | rule |
|---|---|
| **H-13** | **Tie-break, total and deterministic.** Order entries by `effective(p, now)` **descending**; ties by `touched_at` **ascending** (the staler entry loses); remaining ties by **lexicographically first path** losing. This is the cache's own house pattern (`cache.go:420–450`) extended, and it is deterministic so a test can reproduce it (AC-3) |
| **H-14** | **Tracker full, new file arrives.** The newcomer is admitted **iff** its score (at admission, `weight(op)` with no decay term) is **greater than** the current minimum effective score. If yes: the minimum is dropped (`hot_tracker_evicted_total`) and the newcomer takes its place. If no: the newcomer is **not tracked**, and that refusal is counted (`hot_tracker_rejected_total`). A one-shot read must not evict a genuine favourite; a genuinely hot newcomer displaces the coldest entry immediately. **This is the LFU-with-decay admission rule, and it is the only place the tracker grows** — there is no other growth path |
| **H-15** | Note the asymmetry and keep it: **an untracked path is not a broken path.** It is read normally, forever; it is simply never *proactively* refreshed (D-2) |

### 3.4 Cold start — what a fresh tracker does on 400k files (the row's specific question)

| id | rule |
|---|---|
| **H-16** | **It does nothing, and that is the requirement.** A fresh tracker starts **empty** and learns only from accesses through *this* mount. It performs **no walk, no stat, no PROPFIND, no snapshot scan, no prewarm, and no catch-up refresh**. On a 400k-file tree the cost of a cold start is **zero requests and zero paths examined**; the observable is `tracker_entries: 0`, `tracker_bytes: <the empty document>`, `hot_refreshes_total: 0` (AC-2 pins it, with a prewarm-injection control that makes the cell fail) |
| **H-17** | Consequently the first refreshable path appears only after the client's first read or edit, and a mount that is never read never refreshes anything. This is not a gap to close later; it is the property that makes the feature safe to ship to a user with a 400k-file tree |
| **H-18** | **No entry is ever created except by H-9/H-10.** An invalidation of an unknown path does not create a tracker entry (that would be the tree teaching the tracker, i.e. a walk by another name); it is counted as a skip with reason `untracked` (§4.5) |

### 3.5 Persistence

| id | rule |
|---|---|
| **H-19** | **One file per mount: `<mount dir>/hot.json`**, next to `index.json`, `status.json`, `conflicts.jsonl`. The mount dir is `$XDG_CACHE_HOME/bunker/fs/<MountID>` — i.e. **keyed exactly the way the cache dir is keyed** (`status.go:131–143`), so it survives a remount and follows the endpoint rather than the mountpoint |
| **H-20** | The document carries `version`, `mount_id` (`sha256(trimmed baseURL)[:12]`) and `tree` (the pinned tree identity at write time). A `version` it does not know, a `mount_id` mismatch, a `tree` mismatch, or a parse error ⇒ **reset to empty and count the reason** (`hot_tracker_resets_total{reason}` with the closed set `missing | corrupt | version_mismatch | mount_key_mismatch | tree_mismatch`). **A bad tracker never fails a mount** (P-0 clause 1) |
| **H-21** | Written by temp+rename (the house pattern, `cache.go:625–642`), mode **0600** inside the 0700 mount dir, flushed every **`hot_flush_interval = 30 s`** when dirty and once at unmount. A hard kill therefore loses at most 30 s of scores — a stated, acceptable loss, because the tracker is a hint (P-0) |
| **H-22** | The file contains **paths, weights, timestamps and the three header fields — nothing else**. No content, no content hashes, no ETags, no credentials, no tokens, ever. It is a popularity record, and keeping it incapable of carrying a secret is a design property, not an omission |
| **H-23** | The tracker's bytes and entries are **counted and reported** (`tracker_bytes`, `tracker_max_bytes`, `tracker_entries`, `tracker_max_entries`), and the tracker file is part of the cache directory's reported footprint — see §5.4 and **F-2**. A bound the owner cannot see is not a bound, and a file in the bound's directory that the bound does not mention is BFS-031 |

---

## 4. THE SIZE RULE

R5: "Never proactively re-download a file above a limit. A 2 GB artefact must not be pulled because someone touched it." This section is that limit and every consequence of it.

### 4.1 The ceiling

| id | rule | value |
|---|---|---|
| **S-1** | `hot_max_file_bytes` — the ceiling above which a file is **never proactively re-read** | **8388608** (8 MiB). It must be `≤ cache_max_entry_bytes`, and it is **12.5%** of the 64 MiB default entry cap and **3.1%** of the 256 MiB bound. The hot files of a working tree are source files (KiB); 8 MiB sits an order of magnitude above them while still excluding a build artefact, an archive or a VM image. It is a knob (BFS-044 names it), and this is its default |
| **S-2** | **A file exactly AT the ceiling IS refreshable.** The test is `size <= hot_max_file_bytes` (inclusive). **Why inclusive is the specified answer:** an exclusive reading makes the boundary a silent one-byte difference that no user can see, and the whole point of naming a ceiling is that its edge is stated. Both sides are test cells: *at* the ceiling refreshes, ceiling **+1** skips (AC-5) |

### 4.2 The check is two checks, and the second one is the one that matters

| id | rule |
|---|---|
| **S-3** | **Pre-check (cheap):** against the size the client already knows (the cache index entry, or the snapshot node's size, or the last `FileMeta`). If that is over the ceiling, skip with reason `oversize_pre` — **no request at all** |
| **S-4** | **Re-check (authoritative):** after the refresh's `Head` (`client.go:497`) and **before** the byte fetch, against the **served** size. If the served size is over the ceiling, **abandon before fetching a single byte**, with reason `oversize_after_head`. **Why this is the requirement and not a nicety:** a file that *grew* past the ceiling, or whose local size was never known, would otherwise be pulled — and R5's sentence names exactly that case ("a 2 GB artefact must not be pulled because someone touched it"). A refresh that has already issued the GET has already failed R5. AC-5's control is this arm: remove the re-check and a grown-file fixture must be observed pulling bytes it may not |
| **S-5** | **The HEAD is the size authority, and the refresh never trusts a cached size for admission.** A stale size is a stale fact; the two-check shape costs one request when the path is definitely too big and two when it is not |

### 4.3 Interaction with the cache bound — a file too big to be re-read is not promoted either

| id | rule |
|---|---|
| **S-6** | A path over the ceiling is **never enqueued** and **never promoted**. There is no queued entry to promote (S-3/S-4 refuse admission), and the promotion path must re-run the same admission test, so a path that grew past the ceiling while queued is dropped rather than promoted (**Q-3**/*S-8* below) |
| **S-7** | **The size rule gates the hot path only; it never gates a read.** A real read of an over-ceiling path is served exactly as it would be with the hot path compiled out. The hot path's contribution is zero requests, and that is the correct contribution |
| **S-8** | **A tracked path that grows past the ceiling is untracked at its next touch** (`hot_untracked_oversize_total`), so it cannot be refreshed later by a stale score. The tracker does not carry a lie about a file's size |

### 4.4 The size rule above the bound is a configuration error (BFS-044 owns the message)

| id | rule |
|---|---|
| **S-9** | **`hot_max_file_bytes > cache_max_size` ⇒ refuse the mount**, before any request is issued, naming **both numbers**. A refresh of a file larger than the entire cache bound is not a policy, it is a misconfiguration: the refreshed bytes can never be the reason a later read is warm. The refusal shape, its field names and the flag spelling are **BFS-044's**; the *condition*, the two numbers and the mount-time timing are fixed here |
| **S-10** | **`hot_max_file_bytes > cache_max_entry_bytes` ⇒ refuse the mount**, naming both numbers. **This one is derived, not named in the row, and it is deliberate:** a file above the per-entry cap can never be stored (`cache.go:312`/`fs_linux.go:1093`), so a refresh of it would spend pool share, bandwidth and a reservation to fetch bytes the cache must discard on arrival. Refusing is the honest answer; silently warning would leave a knob whose middle range only burns bandwidth |
| **S-11** | **Neither refusal is a hot-path feature gate.** The mount still works; the hot path is inert and reports itself as such (`hot_state: "misconfigured"`, with the two numbers). P-0 clause 1: nothing here may turn a broken knob into a broken mount |

### 4.5 A skip is counted, never silent — and the counter must be reachable from the live path

| id | rule |
|---|---|
| **S-12** | Every decision **not** to refresh increments `hot_skips_total{reason}` from a **closed vocabulary**: `oversize_pre`, `oversize_after_head`, `untracked`, `disarmed`, `stopped`, `cache_disabled`, `no_room`, `pinned_eviction`, `queue_full`, `resync`, `tree_mismatch`, `not_found`, `replaced`. A refresh that *started* and gave up increments `hot_abandoned_total{reason}` instead (§5.6). There is no path in this subsystem that decides against a refresh without incrementing one of the two |
| **S-13** | **The increment must sit at the decision point, on the live path.** This is **BFS-032** turned into a design constraint: `fs_linux.go:1093,1100` pre-filter the case `cache.go:312` is supposed to count, so that counter is displayable and unreachable — a permanent 0 that reads as "this never happened". Therefore: **no pre-filter may sit above a hot-path counter**, and every counter in §9 must be drivable by driving the subsystem (**AC-6**), not by calling the increment's function directly. A skip decision that happens in a helper the live path calls on the way *past* its own check is the same defect one layer down |
| **S-14** | **Every reason in S-12 must have one test cell that makes it move** (AC-6, handed to BFS-046). A reason with no reachable trigger is not a reason; it is dead vocabulary and it must be removed rather than shipped as a zero |

---

## 5. THE QUEUE CONTRACT

### 5.1 Depth and identity

| id | rule | value |
|---|---|---|
| **Q-1** | `hot_queue_max_depth` | **256**. For scale: one `invalidate` event may carry up to `max_paths_per_event` = **4096** paths (`ops.go:456–461`), so depth 256 is **6.25%** of a single worst-case event. The depth is deliberately far below a worst-case burst, because the replacement policy (Q-3) — not the depth — is what protects the hot file |
| **Q-2** | **One entry per path.** A second invalidation of an already-queued path **updates** that entry (score, timestamp, generation) and does not add depth (`hot_queue_deduped_total`). Depth is a count of distinct pending paths, never a count of events |

### 5.2 Replacement when full — which item is dropped, and why

| id | rule |
|---|---|
| **Q-3** | **Replacement is by score, not FIFO.** When full and a newcomer arrives: compare the newcomer's effective score against the **lowest-scoring queued item**. If the newcomer is higher, that item is **dropped** (`hot_queue_displaced_total`) and the newcomer takes the slot. If it is not, the newcomer is **refused** (`hot_queue_rejected_total`) and nothing is queued |
| **Q-4** | **Why not FIFO — stated so the choice is not silent.** A `git checkout` of a large tree invalidates thousands of cold paths; under FIFO those thousands would occupy the whole queue and the one file the user is actively editing would wait behind all of them, which is the exact inversion of the feature's purpose. Under Q-3 a burst of cold churn can never displace a hot file, and a lone hot file is refreshed while 10,000 cold ones are dropped on the floor. **The cost of Q-3, named:** a cold path that a user reads exactly once immediately after an invalidation may lose its slot to a hotter path and receive no warm-up. That is a performance loss on a path the evidence says is not worth warming (H-14 is the same trade at the tracker) |
| **Q-5** | **Service order is the same order.** The refresher takes the item with the **highest** effective score first; ties by enqueue time ascending, then lexicographically first path (H-13's total order, so the queue is reproducible) |
| **Q-6** | **Anti-starvation bound.** A queued item that has been waiting longer than **`hot_queue_max_wait = 300 s`** without a start attempt is dropped (`hot_queue_expired_total`). 300 s is exactly one `hot_decay_step`, so an item that has waited a full decay step has lost 10% of its score and, by H-5's comparison, is no longer the most interesting thing in the queue. **Without this rule a mid-score item under sustained churn could linger indefinitely** — a bound nobody can see |

### 5.3 In-flight width

| id | rule | value |
|---|---|---|
| **Q-7** | `hot_refresh_max_inflight` — how many refreshes may be fetching at once | **2**, and the effective value is `min(this, hot_pool_slots)` reported as `refresh_max_inflight` (§6.1). **1** would serialise the queue (one slow file stalls all warming); **2** is the smallest width that keeps a slow transfer from blocking the queue while staying far below the pool share. At the default size rule it bounds in-flight bytes to **16 MiB** (2 × 8 MiB) — and therefore also bounds the refresh's **memory**, since a refresh may not buffer more than `hot_max_file_bytes` (P-8) |

### 5.4 Booking bytes: two accounts, not one (this is where F-1 lives)

The cache bound must remain honest in both directions at once: PRD §2.5 says the **size/eviction** accounting counts **published** blobs, not in-flight ones; BFS-031 says the **bound must bound the directory**, and the reported figure must agree with `du`. As written, those two cannot both hold once the refresh writes bytes before publishing. The resolution, and it is the implementer's obligation:

| id | rule |
|---|---|
| **Q-8** | **Eviction reasons about published blobs only** (PRD §2.5, unchanged). An unpublished blob is never a candidate to be evicted, and an in-flight refresh may **never** cause the eviction of a published blob. `used_bytes` stays exactly what it is today: published blobs + index bytes, comparable with `du` at rest |
| **Q-9** | **Admission reserves published + in-flight.** A refresh is admitted only if `used_bytes + tracker_bytes + index_growth + this_blob_bytes ≤ cache_max_size`. The enforced in-flight reservation is bounded by **`hot_refresh_max_inflight × hot_max_file_bytes` = 16 MiB** (with the share ceiling of §6.1 capping it at 24 MiB if the cap is ever raised to 3). **This is what bounds the directory**, and it is separate from what eviction reasons about |
| **Q-10** | **Both figures are reported**, so the owner can see the directory's real size: `used_bytes` (published, du-comparable **at rest**) and `reserved_bytes` (`used_bytes + in_flight_bytes`, the directory's peak). A refresh that cannot fit is skipped with reason `no_room` (S-12) — it is never a reason to evict a pinned blob, and never a reason to exceed the bound |
| **Q-11** | **The du-agreement assertion has a stated precondition: no refresh in flight.** At rest, `used_bytes` + the tracker file must agree with `du` of the cache directory up to block rounding; with a refresh running, `reserved_bytes` is the figure that must stay under the bound. A test that asserts `du` agreement while refreshes are in flight is asserting something the design deliberately does not promise — and would be flaky forever (AC-15) |

### 5.5 STOP IN FULL — defined once, precisely

> **STOP IN FULL means no refresh is admitted, the queue is emptied at once, and every in-flight refresh is abandoned at its next yield checkpoint with its temporary blob deleted, its pool slot released and its reservation released — while a fetch that has already been promoted to foreground is untouched, because it is no longer the refresh's to abandon.**

Unpacked, because "stop the queue" is ambiguous between three behaviours and an implementer will otherwise pick one silently:

| # | clause | precise answer | the reading it refuses |
|---|---|---|---|
| 1 | **are new refreshes refused?** | **Yes** — and refused *and counted* (`hot_refused_while_stopped_total`). The state is **level-triggered**: a stopped queue refuses for as long as it is stopped, not once | *"drain then resume"* — a stop that silently lifts is not a stop |
| 2 | **is the queue drained?** | **Emptied immediately.** Every queued-not-started item is discarded at once, each counted by reason `stopped` (S-12). It is *not* drained by letting items run (that is a slow stop, not a stop) and it is *not* held for later (a stop that keeps 256 items pending has stopped nothing) | *"drain-then-idle"* — the queue depth decays to 0 while stopped, but new work is still admitted while it drains |
| 3 | **are in-flight refreshes cancelled, or allowed to finish?** | **Cancelled at their next yield checkpoint** — abandoned, never published, temporary blob deleted, slot and reservation released | *"let in-flight finish"*: it makes the stop unbounded in time (a slow transfer holds the stop open for up to `hot_refresh_deadline`), and the reason the stop exists is usually that the client is under pressure *now* |
| 4 | **is every abandoned item provably safe to abandon?** | **Yes, per §5.6** — and the safety is not an argument, it is four obligations the implementation must satisfy and the tests must observe | — |
| 5 | **what is untouched?** | A **promoted** fetch (§7) is foreground work. STOP IN FULL does not cancel it, does not wait for it, and does not count it as an abandoned refresh. A stop that could cancel a foreground read would be a hang promoted into a feature | — |

| id | rule | value |
|---|---|---|
| **Q-12** | **`hot_stop_deadline`** — the time within which a stop must be fully in force | **1500 ms** = manager tick (1000 ms) + yield quantum (250 ms) + 250 ms slack (Appendix A.6). If any refresh is still running at that instant it is abandoned by the deadline and counted (`hot_stop_deadline_exceeded_total`) — **a counter that must be able to move**, and the arm that moves it is a stalled-server fixture (AC-8) |
| **Q-13** | **Re-arming is explicit, with one exception.** An **operator** stop (if BFS-044 exposes one — that surface is BFS-044's) is cleared only by an explicit re-arm: no timeout, no burst of successes, and no elapsed time may lift it. A **pool-pressure** stop (P-19) re-arms automatically when *both* hold: the queue is empty **and** no foreground request has waited on a slot for longer than `hot_yield_after`, checked on the manager's own tick. Both transitions are counted (`hot_stops_total{reason}`, `hot_resumes_total{reason}`) |
| **Q-14** | **The manager tick is `hot_tick_interval = 1 s`.** It is finer than the channel's own poll cadence (`DefaultPollInterval = 2 s`, `invalidate.go:91`), so the manager's own latency is never the bottleneck between an invalidation and a refresh decision; the tick performs the yield check, the expiry sweep (Q-6), and the auto-re-arm check (Q-13) |
| **Q-15** | **A stop is observable.** `hot_state ∈ {misconfigured, disarmed, armed, stopped}` plus `stop_reason` and `queue_depth` (which must read 0 while stopped) are in the status record. A stop the owner cannot see is a silent degradation of a performance feature — and, worse, indistinguishable from a hung queue |

### 5.6 Why every abandoned item is PROVABLY safe to abandon

Abandonment is safe **per state, with an obligation per state**. A refresh is in exactly one of three states:

| state | what exists | abandonment obligation | why it is safe |
|---|---|---|---|
| **QUEUED** | one queue record: a path, a score, a timestamp, a generation. **Nothing else** | drop the record | Nothing was acquired: no pool slot, no reservation, no temp file, no bytes, no pointer. There is nothing to release, so nothing can leak. The only consequence is that the file stays cold, and a cold file is served correctly by a normal read (P-0) |
| **FETCHING** | a pool slot, a cache reservation, a buffered/partial body, possibly a `.tmp-*` file inside `blobs/` | **(a)** delete the temporary blob; **(b)** close the response body so the connection returns to the pool rather than leaking out of it; **(c)** release the reservation (`in_flight_bytes` back to 0); **(d)** never attempt a publish — no index write, no pointer swap | Each of the four is a leak in the opposite direction if skipped: (a) unbounded disk growth = **BFS-031's class**; (b) pool exhaustion, which is precisely the starvation R7 forbids; (c) a permanent phantom reservation that eventually refuses all refreshes; (d) **corruption**, which is BFS-038's whole subject. §5.6 is not a reassurance — it is a checklist, and AC-13 fails the build if any of the four is missing |
| **PUBLISHING** | a complete blob, immediately before the atomic swap | **none — and this state is NOT a yield point.** The yield/cancel check must be structurally unable to fire between "blob complete" and "pointer swapped" | This is the **one window where abandonment would be corruption** rather than a lost optimisation (R8 / PRD §2.5, owned by **BFS-038**). The refresh state machine must place its cancel points at explicit checkpoints that exclude this window; a cancel check placed on a `for` loop over the body is compliant, a cancel check placed after the body is complete is not |

| id | rule |
|---|---|
| **Q-16** | **Abandonment is idempotent and silent to readers.** Abandoning a refresh that has nothing published must be unobservable to every reader — assert it: after any stop/abandon, a reader's bytes, the `used_bytes` figure and the cache's entry hash for the path are all bit-identical to before (AC-8 clause iii, AC-13) |
| **Q-17** | **The promoted case is excluded by construction, not by a flag.** Once a refresh is promoted, it is no longer a refresh: the queue entry it replaced is gone (§7.4) and the stop path has no handle on the fetch. This is why clause 5 of §5.5 can be stated as an absence rather than an exception list |

---

## 6. THE POOL POLICY

R7: "The refresh shares the global connection pool. It gets a slice of it, must back off in favour of other files, and may abandon a refresh entirely rather than starve foreground work."

### 6.1 The share

| id | rule | value |
|---|---|---|
| **P-3** | `hot_pool_share` — the maximum share of the client's request pool refreshes may hold | **1/8**, i.e. `hot_pool_slots = max(1, floor(Concurrency/8))` = **3** at the default `Concurrency = 25`. **Foreground's floor is therefore 22 of 25 slots (88%)**, and that floor is arithmetic, not a hope: the refresh budget is a **separate bounded budget**, so refreshes cannot consume a slot from the foreground semaphore at all |
| **P-4** | `hot_refresh_max_inflight` (Q-7) is clamped to the share | effective `refresh_max_inflight = min(hot_refresh_max_inflight, hot_pool_slots)` = **2** at defaults. If the configured cap exceeds the share, the value is **clamped and reported** (`refresh_max_inflight_clamped: true`) rather than refused — the house rule that a stripped request is strictly safer than a rejected one (`PrepareMountpoint`'s `--allow-other` precedent, `fsmount/options.go:186–190`). **The share is a hard ceiling; the cap is the ordinary governor**; both are reported as numbers |
| **P-5** | The reservation ceiling follows from the pair | `hot_refresh_max_inflight × hot_max_file_bytes` = **16 MiB**, and never more than `hot_pool_slots × hot_max_file_bytes` = **24 MiB** even if the cap is raised to the share (Q-9) |

### 6.2 The yield rule — a foreground read or write ALWAYS outranks a refresh

| id | rule |
|---|---|
| **P-6** | **Direction 1: a refresh must never block a foreground request.** Refreshes acquire from their own budget (P-3), never from `Client.sem`. A refresh may never hold a mutex, a lease or a cache lock across a network wait (P-10) |
| **P-7** | **Direction 2: a foreground request must never wait behind a queue of refreshes.** A refresh **holding** a slot must release it at its next yield checkpoint if any foreground request has been waiting for longer than **`hot_yield_after = 250 ms`**. A yielded refresh re-enters the queue at its score (not at the back, and not with a penalty) and counts as `hot_yields_total`. **Why 250 ms:** the measured concurrency study's whole 25× run over the same bytes took **0.79 s** (`client.go:21–24`), so a quarter second is under a third of an entire measured run — small enough that the yield decision is invisible beside the work it protects, and long enough that a refresh is not yielded for a transient waiter |
| **P-8** | **Consequence the implementer must accept: a refresh is preemptible, therefore none of its work is guaranteed.** The compliant shape is "release the slot, drop the partial buffer, re-queue at score". It is not permissible to hold the slot to finish (P-7 forbids it), and it is not permissible to keep fetching without a slot (that is a pool bypass and defeats P-3) |
| **P-9** | **The obligation is on latency, not on politeness.** The property to demonstrate is measurable: with a foreground stream injected against a throttled server, the foreground's latency distribution must be **unchanged** versus a build with the hot path compiled out (AC-9), and the row's own standard applies — a refresh starved/un-starved *count* is not the measurement; the foreground's own numbers are |

### 6.3 Backoff

| id | rule | value |
|---|---|---|
| **P-10** | Retry shape after a refresh failure or a yield | exponential, **base 250 ms, factor 2, cap 30 s, full jitter** (uniform in `[0, delay]`). Success resets it. Reported: `hot_backoff_current_ms`, `hot_backoff_max_ms`, `hot_refresh_retries_total`. **Why 30 s:** the client's own per-operation deadline is `DefaultOpTimeout = 30 s` (`errors.go:232–233`); a speculative fetch must never back off *longer* than the foreground is willing to wait for the same bytes, or the hot path has quietly become slower than the cold path |

### 6.4 The hang-up rule — abandoning is the correct answer, not the last resort

| id | rule | value |
|---|---|---|
| **P-11** | `hot_refresh_deadline` — an upper bound on one refresh's life | **10 s** = one third of `DefaultOpTimeout`. A speculative refresh is never worth a full foreground operation's deadline; a refresh still running at 10 s is abandoned with reason `deadline` |
| **P-12** | `hot_refresh_reacquire_window` — how long a yielded refresh may wait for its slot back | **30 s** = 15 channel poll opportunities at the 2 s default cadence (`invalidate.go:91`). An invalidation that has waited 15 cadences is stale; abandon it with reason `reacquire_window` |
| **P-13** | **Abandon reasons are a closed set** (`hot_abandoned_total{reason}`): `deadline`, `reacquire_window`, `stopped`, `pinned_eviction`, `no_room_after_start`, `tree_mismatch`, `not_found`, `replaced`, `shutdown`. Each is counted, and each has a cell that moves it (AC-10, AC-6) |
| **P-14** | **R7's sentence is the requirement, and it outranks the feature.** "may abandon a refresh entirely rather than starve foreground work" means: when the two conflict, the refresh loses **every time**, and choosing to complete a refresh instead is a defect even when the refresh would have succeeded |

### 6.5 Contention, stated for the implementer as two sentences and a table

| id | the situation | what must happen |
|---|---|---|
| **P-15** | A refresh is fetching; a foreground read or write wants to run | the foreground runs **now** — the refresh holds no resource the foreground needs, because its slot comes from its own budget (P-6). The refresh is yielded at its next checkpoint (P-7) |
| **P-16** | A queue of refreshes is pending; a foreground request arrives | the foreground runs **now** — at most `refresh_max_inflight` (2) refreshes can be in flight, out of at least 22 slots available to the foreground. There is no configuration in which a queue of refreshes delays a foreground request |
| **P-17** | Both are queued behind a slow server | refreshes consume their own budget only; the foreground's slots are never drained by refreshes, so the foreground's wait is bounded by the server, not by the hot path |
| **P-18** | The client is under memory or disk pressure | the hot path is **not** the mechanism that relieves it — the cache's own eviction is (BFS-005). The hot path's only responses to pressure are *yield* (P-7) and *abandon* (P-11/P-12), and neither may free a pinned blob (D-4) |

**P-19 — the pool-pressure stop, which is the trigger Q-13's automatic re-arm refers to.** If a foreground request has waited on a slot for longer than `hot_yield_after` on **five consecutive manager ticks** (5 s), the hot path stops *itself* in full (§5.5) with reason `pool_pressure` (`hot_stops_total{reason:pool_pressure}`). **Why a rule and not a per-refresh accident:** P-7 yields one refresh per offending tick, which is enough when pressure is transient and useless when it is sustained — under sustained pressure the queue would keep re-acquiring slots it must immediately hand back, and the honest answer is that warming is off until the foreground is quiet again. `hot_yield_after × 5` is long enough to distinguish pressure from a transient waiter, and short enough that a busy client is not warming through it. This is R7's *"may abandon a refresh entirely rather than starve foreground work"* raised from a per-refresh concession to a policy.

---

## 7. PROMOTION AND THE SINGLE-FLIGHT MAP

R9: "If a queued refresh is asked for by a real read, it promotes to full: it leaves the queue, does not sleep, because it is now on the critical path." PRD §2.6 adds the trap: without a single-flight map, promotion makes the stampede worse.

### 7.1 The promotion

| id | rule |
|---|---|
| **PR-1** | A real read (a FUSE read or a `Head`/`Get` through this client) of a path that has a **queued** refresh promotes it: the fetch becomes a **full foreground read** — it leaves the queue, it takes a **foreground** slot (not a refresh slot), and it **does not sleep**: no backoff, no waiting for a refresh slot, no queue ordering. It is on the critical path now |
| **PR-2** | **It does not promote a file the policy refuses.** The promotion re-runs the admission test: over `hot_max_file_bytes` ⇒ the queued entry is dropped with reason `oversize_pre`/`oversize_after_head` and the read proceeds as an ordinary read (S-7). A promotion is a *priority* change, never a *policy* bypass |
| **PR-3** | A promoted fetch's result is published by the same route as any foreground read's: through the cache's normal admission (which still applies its own per-entry cap), with BFS-038's atomicity. Promotion changes *who waits*, not *what is published* |

### 7.2 The single-flight map (the requirement that makes promotion an improvement)

| id | rule |
|---|---|
| **PR-4** | **There is one per-path single-flight map per mount**, keyed by the normalised path. Every fetch of a path that the hot path knows about — a refresh fetch, a promoted fetch, and a foreground read that would otherwise issue its own GET — goes through it. Its value is the in-flight fetch record: a result channel, a waiter count, and the fetch's owner |
| **PR-5** | **The promotion hands over the EXISTING fetch.** If a refresh for path *P* is already in flight (or an earlier foreground read is), the promoting reader **joins it as a waiter** (`singleflight_joins_total++`); it does **not** issue a second fetch. If no fetch exists, the promoting reader becomes the **leader** (`singleflight_leaders_total++`) and starts exactly one |
| **PR-6** | **The invariant, which is the whole point of the map:** for any path, the number of bytes-fetching requests issued concurrently is at most **1**. Operational form: **N concurrent readers of one cold tracked path with a queued refresh produce exactly 1 GET on the wire** — asserted against the server's own request count, not against a client-side counter (AC-11, with the map disabled as the control arm: N GETs) |
| **PR-7** | **Why this is not optional, in the PRD's own words:** without it, "a queued refresh and a direct read of the same path can both fetch it" and promotion "makes the stampede worse, not better" (PRD §2.6). A hot file is by definition one that many reads want; a promotion path that double-fetches would precisely anti-optimise the hottest paths |
| **PR-8** | **The map never leaks.** `singleflight_entries` is reported and must return to **0 at rest**; an entry is removed by its leader on completion, on error, and on abandon. A map that grows on errors is a slow leak in the component whose purpose is to bound concurrency |

### 7.3 What the promoted fetch's readers see

| id | rule |
|---|---|
| **PR-9** | All waiters of one fetch receive the same outcome — the same bytes, or the same error — so two readers of one path can never disagree about its content because they arrived at different moments. This is also what makes the map *correct*, not merely efficient: a shared fetch is a shared answer |
| **PR-10** | A waiter that is cancelled (FUSE interrupt) **detaches** and does not cancel the shared fetch for the others; whether the kernel-visible error is `EINTR` is **BFS-039's** contract, and this spec adds only that a detach must not remove an entry other waiters still hold |

### 7.4 Cancellation semantics — the promoted fetch, and the queue entry it replaced

| id | rule |
|---|---|
| **PR-11** | **From the instant of promotion, the fetch belongs to the foreground.** The refresh manager must not cancel it, must not count it as an abandoned refresh, and STOP IN FULL (§5.5 clause 5) must not touch it. It is completed or it fails on its own terms — that is what "it does not sleep" implies, and it is the only reading under which promotion is a latency win rather than a latency gamble |
| **PR-12** | **The queue entry it replaced is DELETED, never rescheduled, and counted with its own reason.** `hot_queue_promoted_total` is a distinct counter from `expired`/`displaced`/`stopped`: the three facts ("the queue dropped it because it was cold", "because it was old", "because the work was taken over") demand different readings of the record, and collapsing them would make promotion invisible in the queue's own accounting |
| **PR-13** | **If the promoted fetch is abandoned anyway** — its own deadline, a failed request — the promotion is **not** retried as a refresh. The path returns to the queue only on the next invalidation of that path, because re-queuing a path whose owner just gave up on it converts a foreground failure into an infinite background retry loop |
| **PR-14** | **A stop during a promotion is neither a race nor a lost read.** Ordering rule: STOP sets the state first, then sweeps the queue; a promotion that has already taken its queue entry off (§7.4) is untouched by the sweep; a promotion that arrives after the stop is refused a queue entry but **still serves the read normally** (S-7, D-3: the *read* is never refused, only the *speculative* fetch). The reader's outcome is identical in both orderings — which is the property the test asserts by running both (AC-8 clause iv) |

---

## 8. WHAT IT DOES NOT DO

Each line is a behaviour an implementer might otherwise assume, invent, or add for symmetry. Being explicit is what stops the assumption being built on. The first four are the row's list; the rest are the ones this policy would otherwise leave open.

| # | prohibition | why it is stated |
|---|---|---|
| **D-1** | **No refresh during a mount's first sync.** The hot path is `disarmed` until **both** (i) the bind-time baseline has completed (handshake + `PinTree` + the initial snapshot/PROPFIND path, `fs_linux.go:203–238`) and (ii) the invalidator has established its first observation (`head_seq` known, `SPEC-watcher-capability.md` §2.2). **Arming queues ZERO refreshes** — a full tracker restored by a remount must **not** produce a catch-up burst; the hot path reacts only to invalidations that arrive *after* arming. Counted: `hot_arms_total`, `hot_skips_total{reason:disarmed}` |
| **D-2** | **No refresh of a path the client has never read.** Equivalently: only tracker members (H-9/H-10) are ever refresh candidates. An invalidated path with no entry is skipped with reason `untracked` — the tree never teaches the tracker (H-18) |
| **D-3** | **No refresh while the queue is stopped.** Including: no refresh started at the instant of a stop, and no refresh resumed by a stop's own state change (Q-13). A *read* of that path is served normally — the prohibition is on speculation, never on service |
| **D-4** | **No refresh that would evict a blob an in-flight reader holds.** **BFS-038 owns that rule** (PRD §2.5's refcount corollary); this spec **consumes** it and fixes the hot path's side: when publishing would require evicting a pinned blob, the refresh **abandons** with reason `pinned_eviction` (S-12/P-13) rather than evicting. The hot path must never call the cache's eviction to make room for itself |
| **D-5** | **No refresh of a directory, no PROPFIND refresh, no tree walk.** Only whole-file paths in the tracker. One refresh = one path |
| **D-6** | **No refresh when the cache is disabled** (`MaxBytes == 0`): the cache cannot store, so warming is meaningless and would spend bandwidth for a guaranteed discard. The hot path reports `hot_state: "cache_disabled"` and counts `hot_skips_total{reason:cache_disabled}` |
| **D-7** | **No content, hashes, ETags or credentials in the tracker** (H-22) |
| **D-8** | **No correctness dependency, ever** (P-0/P-1). If any cell of BFS-046's read-correctness battery changes behaviour when the hot path is disabled, the hot path is wrong — not the cell |
| **D-9** | **No refresh of a path whose served tree identity differs from the pinned one.** A differing `X-Bunker-Tree` is `stale_identity` (a resync, `client.go:48–51`), never a refresh; the refresh abandons with reason `tree_mismatch` |
| **D-10** | **No new server-side requirement, and no new request type.** The refresh uses `Head` and `Get` and nothing else; a stock server that serves the existing surface serves this. (PRD §5: push is an upgrade, never a requirement — and the same applies to the hot path, which must work against the poll mechanism too, because most hosts will have no watcher) |
| **D-11** | **No refresh during or immediately after a resync.** A resync/overflow drops the cache's view (BFS-005 §4.1/§4.2); warming during it would race the drop and could publish a blob the drop is about to invalidate. Skips are counted with reason `resync` |
| **D-12** | **No operator-facing promise that a read is warm.** The status record may report warm hits; no user-visible document may state that a path *will* be cached. The mechanism degrades to a cold read by design, and that is not an error |

---

## 9. The bound census — every bound named, countable, and reported (handed to BFS-045)

BFS-045 owns the record's layout, naming and drill-down. This table is **which** counters must exist and **what each means**, because "a bound the owner cannot see is not a bound" (PRD §2.7) and because BFS-032's lesson is that a counter with no reachable trigger is worse than no counter. **Every row here must have a test cell that makes it move** (AC-6).

### 9.1 Bounds

| bound | default | unit | enforced by | counter / field |
|---|---|---|---|---|
| `hot_max_file_bytes` | 8388608 | bytes | refresh admission (S-3, S-4) | `hot_skips_total{oversize_pre,oversize_after_head}` |
| `cache_max_size` (existing) | 268435456 | bytes | cache admission (existing) | `used_bytes`, `max_bytes` |
| `cache_max_entry_bytes` (existing) | 67108864 | bytes | `Cache.Insert` (existing) | `oversize_bypasses` — **and see BFS-032: this one cannot move today; S-13 forbids reproducing that shape for any counter here** |
| `hot_tracker_max_entries` | 4096 | entries | tracker admission (H-14) | `tracker_entries`, `tracker_evicted_total`, `tracker_rejected_total` |
| `hot_tracker_max_bytes` | 1048576 | bytes | tracker write (H-8) | `tracker_bytes`, `tracker_max_bytes` |
| `hot_queue_max_depth` | 256 | entries | queue admission (Q-3) | `queue_depth`, `queue_max_depth`, `queue_displaced_total`, `queue_rejected_total` |
| `hot_queue_max_wait` | 300 s | seconds | expiry sweep (Q-6) | `queue_expired_total`, `queue_oldest_age_s` |
| `hot_refresh_max_inflight` | 2 (effective) | fetches | refresher (Q-7) | `refresh_inflight`, `refresh_inflight_max` |
| `hot_pool_share` / `hot_pool_slots` | 1/8 / 3 | slots | refresh budget (P-3) | `pool_slots_for_refresh`, `pool_slots_for_foreground`, `refresh_max_inflight_clamped` |
| `hot_refresh_deadline` | 10 s | seconds | P-11 | `abandoned_total{deadline}` |
| `hot_refresh_reacquire_window` | 30 s | seconds | P-12 | `abandoned_total{reacquire_window}`, `yields_total` |
| `hot_yield_after` | 250 ms | ms | P-7 | `yields_total`, `foreground_wait_ms_max` |
| `hot_stop_deadline` | 1500 ms | ms | Q-12 | `stop_deadline_exceeded_total` |
| `hot_tick_interval` | 1 s | seconds | Q-14 | (manager liveness) |
| `hot_decay` / `hot_decay_step` | 0.9 / 300 s | ratio / seconds | H-3/H-5 | `renormalisations_total` |
| `hot_score_ceiling` | 10000 | score | H-6 | `renormalisations_total` |
| `hot_read_touch_window` | 5 s | seconds | H-9 | `read_touches_total`, `read_touches_deduped_total` |
| `hot_flush_interval` | 30 s | seconds | H-21 | `flushes_total` |
| `hot_max_inflight_bytes` | 16 MiB (= 2×8) | bytes | Q-9 | `in_flight_bytes`, `reserved_bytes` |
| `hot_pool_pressure_ticks` | 5 (5 s) | ticks | P-19 | `hot_stops_total{pool_pressure}`, `hot_resumes_total{pool_pressure}` |

### 9.2 Flow and value counters

`hot_refreshes_total`, `hot_promotions_total`, `hot_fetches_total`, `hot_bytes_downloaded_total`, `hot_warm_hits_total` (reads whose bytes came from a blob the **hot path** published), `hot_warm_hit_bytes_total`, `hot_reads_total`, `hot_misses_total`, `singleflight_leaders_total`, `singleflight_joins_total`, `singleflight_entries`.

| id | rule |
|---|---|
| **O-1** | **The subsystem must be able to prove it earns its keep.** `hot_warm_hits_total` and `hot_bytes_downloaded_total` are required precisely so the owner can compute a hit rate and a cost. If over a full status window `hot_warm_hits_total == 0` while `hot_bytes_downloaded_total > 0`, the status record reports `hot_value: "no_measured_benefit"` — a **fact**, not an error. A hot path that cannot demonstrate a warm hit is spending bandwidth for nothing, and the record must say so rather than look healthy |
| **O-2** | **A performance-only subsystem's counters are still held to BFS-032's standard.** A counter that can never move is a gap, not a green check; a counter that *is* at zero must be provably reachable (AC-6). This is the arm BFS-046 must write for every reason in S-12 and P-13 |
| **O-3** | **No hot-path figure may be reported as a `used_bytes` substitute.** `reserved_bytes` is the directory's real peak; `used_bytes` is the published figure; they are never merged into one number (F-1) |

---

## 10. Acceptance criteria (handed to BFS-046; each with the control that proves it can fail)

| # | criterion | control that must make it fail |
|---|---|---|
| **AC-1** | **The score arithmetic is the specified arithmetic.** A deterministic sequence (read, read, edit, then a gap) produces the exact scores of H-1/H-2/H-3/H-5, asserted numerically (not "ordering held"). Second arm: an abandoned favourite's **effective** score decays by H-5, so a newcomer outranks it after the stated idle. Third arm: **N refreshes leave every score bit-identical** (H-11) | with `hot_decay = 1.0` the abandoned-favourite arm must turn red; a refresh that touches must turn the third arm red |
| **AC-2** | **Cold start does not walk.** On a fixture tree of 400k entries with an empty tracker: `tracker_entries == 0`, **zero** requests issued, zero paths stat'ed, `hot_refreshes_total == 0` (H-16) | a mutation that prewarms from the snapshot (or scans the tree) must turn the cell red — this is the cell that makes "must not walk them" a requirement rather than a hope |
| **AC-3** | **Tie-break is deterministic and renormalisation is order-preserving.** Two entries at equal effective score: repeated evict/insert cycles select the same survivor on every run; renormalisation changes no ordering | a random (or map-iteration-order) tie-break must fail; a renormalisation that changes an ordering must fail |
| **AC-4** | **A full tracker behaves as specified.** A lower-scored newcomer is rejected (`tracker_rejected_total` moves); a higher-scored newcomer displaces the minimum (`tracker_evicted_total` moves); the bounds hold (`tracker_entries ≤ 4096`, `tracker_bytes ≤ 1 MiB`) | an unbounded fixture must fail the byte/entry assertion; a rejection that silently creates an entry must fail |
| **AC-5** | **The size rule, at every edge.** A file **exactly at** `hot_max_file_bytes` **is** refreshed (S-2); ceiling **+1** is skipped with `oversize_pre` (S-3); a file whose local size is stale and whose **served** size is over the ceiling is abandoned **before any byte is pulled** with `oversize_after_head` (S-4); a tracked path that grows past the ceiling is untracked at its next touch (S-8); both misconfiguration arms refuse the mount naming both numbers (S-9, S-10) | **remove the post-HEAD re-check** and the grown-file arm must observe bytes being pulled (R5's exact failure); make the comparison exclusive and the at-ceiling cell must fail |
| **AC-6** | **Every skip and abandon reason is reachable from the live path.** One cell per reason in S-12 and P-13, each driving the **live** mount (a real read, a real invalidation, a real stop), each asserting the counter moved | **insert a pre-filter above any counter** and the affected cell must fail — this is BFS-032's shape reproduced as a regression arm, and it is the single most important control in this document |
| **AC-7** | **Replacement protects the hot file.** A 10,000-path cold invalidate burst with one genuinely hot queued path: the hot path is refreshed, the cold flood cannot displace it, and `queue_displaced_total`/`queue_rejected_total` account for the difference | switch the policy to FIFO and the hot-path arm must fail |
| **AC-8** | **STOP IN FULL does exactly §5.5 and nothing else.** (i) new work refused and counted while stopped; (ii) `queue_depth == 0` within `hot_stop_deadline`, with each dropped item counted by reason `stopped`; (iii) an in-flight refresh is abandoned with **no publish**: the path's cached hash, `used_bytes` and the reader's bytes are bit-identical before/after, `in_flight_bytes` returns to 0, no temp blob remains, no slot is held; (iv) **a promoted fetch SURVIVES the stop** and completes; (v) both stop/promotion orderings produce an identical reader outcome (PR-14) | *drain-then-idle* semantics must fail (ii); *let-in-flight-finish* must fail (iii)'s deadline arm; cancelling the promoted fetch must fail (iv) — that arm is what discriminates the correct definition from the plausible wrong one |
| **AC-9** | **The pool share and the yield rule hold under measurement.** Max concurrent refreshes never exceeds `min(refresh_max_inflight, floor(Concurrency/8))`; with a foreground stream against a throttled server, the foreground's latency is unchanged versus a hot-path-disabled build; under **sustained** pressure the hot path stops itself in full (P-19) rather than re-acquiring slots it must hand straight back, and re-arms only under Q-13's two conditions | let refreshes take slots from `Client.sem` and the share arm must fail; remove the yield check and the foreground-latency arm must fail; remove P-19 and the sustained-pressure arm must observe thrashing (a rising `yields_total` with the queue never stopping) |
| **AC-10** | **Hang-up beats the feature.** A throttled server: the refresh is abandoned at `hot_refresh_deadline` with `abandoned_total{deadline}` moving, and the foreground is never starved; a yielded refresh that cannot reacquire is abandoned inside `hot_refresh_reacquire_window` | remove the deadline and the foreground arm must fail; a refresh that completes instead of hanging up must fail |
| **AC-11** | **Promotion is single-flight.** 32 concurrent readers of one cold tracked path with a queued refresh: **exactly 1 GET on the wire** (asserted at the server), 1 leader + 31 joins, `singleflight_entries` back to 0 at rest | disable the map and the cell must observe 32 GETs (PR-7's exact trap) |
| **AC-12** | **Promotion is on the critical path.** The promoted fetch starts immediately (no backoff sleep, no refresh-slot wait) and takes a **foreground** slot; the queue entry is gone and counted under `queue_promoted_total` (never `expired`/`displaced`) | make promotion wait for a refresh slot and the latency arm must fail; count the removal as `expired` and the accounting arm must fail |
| **AC-13** | **Abandonment leaks nothing.** Kill the mount mid-refresh (and separately, mid-fetch): no reader ever observes a partial file; no unpublished blob is left behind; no connection is left out of the pool; no reservation survives (Q-16, §5.6's four obligations) | skip any one of the four obligations in §5.6 and the corresponding leak assertion must fail |
| **AC-14** | **THE BOUNDARY CELL (P-0).** Run BFS-046's read-correctness battery with the hot path force-disabled **and** with every hot-path counter forced to its failure value (a tracker that cannot be parsed, a stop in force, a refresh abandoned mid-flight, an oversize file, a disabled cache). Every read cell must pass **unchanged** | any read whose bytes or error change with the hot path disabled is a failure — this is the only cell that can catch a hot-path assumption that leaked into correctness |
| **AC-15** | **The two byte accounts are honest.** **At rest (no refresh in flight)**: `used_bytes` + the tracker file agrees with `du` of the cache directory up to block rounding. **Under load**: `reserved_bytes` never exceeds `cache_max_size`, and `in_flight_bytes` equals the sum of the in-flight refreshes' fetched bytes | count only published blobs during an in-flight refresh and the bound must be observed exceeded (F-1's proof); assert `du` agreement with a refresh in flight and the cell must be flaky — proving why Q-11's precondition is part of the specification, not a test convenience |
| **AC-16** | **The tracker is bounded and reported.** `tracker_bytes ≤ tracker_max_bytes`, `tracker_entries ≤ tracker_max_entries`, both visible; a `hot.json` written by a previous mount with a different `mount_id` is discarded and counted | an unbounded tracker fixture must fail; a silently inherited foreign tracker must fail |
| **AC-17** | **No new server requirement.** Every cell above runs against the **poll** mechanism (`events`/`rev`) as well as against `watch`, and the server sees no request type it did not already serve | a cell that needs a new op must fail (D-10) |

---

## 11. Boundaries — what this document does NOT decide

| row | its surface | the interface this document hands it | what stays its own |
|---|---|---|---|
| **BFS-038** | the atomicity representation: the immutable blob, the single pointer swap, refcounts, "eviction must respect in-flight reads", "size/eviction accounting counts published blobs" | **This spec CONSUMES it wholesale.** Its two obligations on the hot path are: **D-4** (abandon rather than evict a pinned blob) and §5.6's **PUBLISHING is not a yield point**. Promotion and refresh both publish through the same route as a foreground read (PR-3) | the representation itself, the swap, the refcount rule, the eviction interface. **This spec respecifies none of it** |
| **BFS-040** | the events contract: `invalidate`/`heartbeat`/`overflow`, the forced-resync table, the `reason` vocabulary | The hot path reacts to an `invalidate` (enqueue for tracked paths) and refuses to refresh during a resync (D-11). It consumes the contract and adds nothing to it | the wire vocabulary, the resync rules, the capability negotiation |
| **BFS-041** | the push channel: long-poll vs stream, the cursor, reconnect/backoff, the subscriber bound | **Nothing.** The hot path never sees the transport. It is driven by whatever mechanism the invalidator selected (`watch`, `events`, `rev`) — D-10/AC-17 make that explicit | everything about delivery |
| **BFS-031** | the cache-directory accounting defect (a bound that does not bound its directory; reported figure ≠ `du`) | Used as a **failure shape**. This spec's obligations: the tracker file must be inside the reported set (F-2), and the admission reservation must bound the directory (Q-8/Q-9) | the **repair** of the existing `status.json`/`conflicts.jsonl` accounting. **This document does not fix BFS-031** |
| **BFS-032** | the counter that can never move | Used as a **failure shape**, in both directions: S-13 forbids a pre-filter above any hot-path counter, and every reason in S-12/P-13 must be drivable (AC-6) | the repair of `cache.go:312` / `fs_linux.go:1093,1100` itself |
| **BFS-037** | the implementation | Every rule in §3–§8, the counter set of §9, and the cells of §10 | the code, the internal structure, the package layout |
| **BFS-043** | server-side config | **Nothing** — this policy is client-side (D-10). Named only so the boundary is explicit | server knobs, defaults, validation |
| **BFS-044** | the client-side config surface: flag names, defaults, validation, error messages | The **conditions** and the **numbers** that must be refusable (S-9, S-10), the clamp rule and its report (P-4), and the requirement that an operator stop (if exposed) is explicit-only (Q-13) | flag names, spelling, help text, the refusal's exact bytes |
| **BFS-045** | the status record: layout, naming, aggregation, retention | The counter set of §9 and the closed reason vocabularies of S-12/P-13 | the record's layout and naming |
| **BFS-046** | the test program: harness, floor, cells | §10's criteria and their controls | the harness, the coverage floor, the E2E shape |

**Explicitly out of scope:** any code change, any config surface, any test, any board row (this row reports; it does not file), any server-side change, and any change to the invalidator or the cache beyond what D-4/Q-8/Q-9 name.

---

## 12. Residuals and findings (reported, not smoothed over)

| # | finding | evidence | disposition |
|---|---|---|---|
| **F-1** | **Two inherited accounting rules cannot both hold unamended.** PRD §2.5 says "the size/eviction accounting must count published blobs, not in-flight ones"; BFS-031 says "the bound must bound the WHOLE cache directory" and "the reported figure must agree with `du`". Once a refresh writes bytes before publishing, counting only published blobs **cannot** bound the directory — a cache at 99% of its bound plus 2×8 MiB of in-flight blobs is 16 MiB over the bound while every reported figure stays "inside" it, which is BFS-031's defect **exactly**, one layer up | `cache.go:592–597` (`usedLocked` = published blobs + index) vs `cache.go:405–412` (`makeRoomLocked` bounds only that); BFS-031's reasoning ("a bound that is reported one way and enforced another"); arithmetic in App. A.5 | **Resolved here by splitting the two accounts**, not by choosing a side: **eviction** reasons about published blobs only (§2.5 kept verbatim), **admission** reserves published + in-flight (which is what bounds the directory), and both figures are reported (Q-8/Q-9/Q-10). This is a **real conflict between two documents**, and a reader of either alone would implement a bound that does not bound. It needs the PRD's owner to confirm the split; **not filed as a row by this worker** (the brief forbids filing) |
| **F-2** | **A second new file lands in the directory the cache bound names.** `hot.json` sits beside `status.json` and `conflicts.jsonl` — the two files BFS-031 identifies as outside `used_bytes`. Without an accounting decision, this spec would reproduce BFS-031's defect with a file of its own making | `status.go:145–149`, `cache.go:592–623`; BFS-031's 29.97× measurement | Requirement stated (H-8: bounded at 1 MiB and reported; Q-9: inside the admission reservation) and the test that proves it (AC-16, AC-15). The **general repair** of the directory accounting remains **BFS-031's** row; this document's obligation is only to not add a second unaccounted file. **BFS-031 ANSWERED IT BY DECISION (landed):** the cache directory is now `<mount dir>/cache` and holds the cache's own bytes only (`index.json`, `blobs/`), while `status.json` and `conflicts.jsonl` live in the mount directory under their own enforced bound (`StateMaxBytes()`). A tracker file the CACHE writes is therefore cache bytes and belongs in `<mount dir>/cache` — the directory Q-9's reservation bounds — and not beside the mount's state |
| **F-3** | **Decay-on-touch alone does not make an old favourite fall out.** PRD §3 says "decay-on-touch, so an old favourite falls out naturally and no sweeper is needed". True of the *mechanism*, insufficient as a *displacement* rule: an abandoned entry's score only decays if something touches it, and nothing does — so a file read 100 times last week keeps its score forever and a newcomer can never displace it on a full tracker. **This is a silent failure**: nothing looks broken, the tracker simply freezes on stale favourites | PRD §3's own wording vs H-14's admission rule; Steady-state arithmetic in App. A.2 | **Corrected by adding the comparison-time decay (H-5)** — still not a sweeper, still decay without a traversal, but it makes "an old favourite falls out" true. The PRD's sentence is a design intent this spec must *complete*, and an implementer reading only the PRD would build the frozen tracker. Flagged for the PRD's owner; not filed |
| **F-4** | **Queue replacement is a silent fork in the road.** FIFO and LFU-with-decay are both defensible, and an implementer picks one without noticing. Under FIFO a checkout of 10k cold files displaces the one file the user is editing — the feature inverted | Q-1's own arithmetic (depth 256 vs 4096 paths in one event) | Fixed by Q-3/Q-4, with the cost of the chosen policy stated as well as its benefit |
| **F-5** | **"Stop the queue" is ambiguous between three behaviours.** cancel-and-refuse; drain-then-idle; refuse-new-but-let-in-flight-finish. Each is a plausible reading, and the third makes a stop take up to `hot_refresh_deadline` to mean anything | R6's phrase "stoppable in full" | §5.5 defines one reading in one sentence and **names the other two as refused**, so the choice cannot be made silently. AC-8(iii)/(iv) are the arms that discriminate |
| **F-6** | **A per-call read touch inflates scores by two orders of magnitude.** A FUSE read of an 8 MiB file is ~64 chunked calls; without a dedupe window one file's single access scores ~64 and one access pattern can occupy the tracker | the mount's read path (chunked FUSE reads) vs H-1's weight of 1 | Fixed by H-9's `hot_read_touch_window = 5 s`, which is counted (`read_touches_deduped_total`) rather than heuristic |
| **F-7** | **Nothing else bounds the refresh's memory.** The size rule is the only bound on the bytes a refresh buffers; without it a refresh of a 2 GB path (R5's own example) buffers or streams 2 GB in a client that has no other memory bound | R5 vs H-11/S-1 | Stated as a second job of the size rule (P-6/P-8 and Q-7's arithmetic). Worth an owner's confirmation that the size rule is intended to bound client memory as well as bandwidth — the row names bandwidth only |

---

## Appendix A — every number this document chooses, and why

A.1 — **`hot_weight_read = 1.0` / `hot_weight_edit = 8.0`.** A read is a weak signal because a search, a scan and a build all read; an edit is a strong one because the file was worth opening *and* will be revisited (PRD §3). 8 is the smallest power of two that outranks a burst of seven neighbouring reads, and a power of two survives any future renormalisation exactly. Both must be positive and finite; neither may be zero (a zero weight would make a touch a pure decay step, which is a different feature).

A.2 — **`hot_decay = 0.9` per `hot_decay_step = 300 s`.** Computed, not asserted: half-life = `step · ln 0.5 / ln 0.9` = **1973.6 s = 32.9 min**; retained share = **90%** at 5 min, **72.9%** at 15 min, **53.1%** at 30 min, **28.2%** at 1 h, **8.0%** at 2 h. A five-minute step is long enough that a burst of chunked reads cannot advance the clock step by step, and a ~33-minute half-life matches the workload: "what I am working on now" is a session, not a week. Steady-state scores under continuous touch are ≈**570** at one read/5 s and ≈**10.0** at one read/5 min — both far under `hot_score_ceiling`, which is why the ceiling only ever fires under sustained edits.

A.3 — **`hot_tracker_max_entries = 4096`, `hot_tracker_max_bytes = 1048576` (1 MiB).** 4096 is an editing session's working set — three orders of magnitude below the 400k-file tree the PRD names — and ≈640 KiB serialised at ≈160 B/entry, so the byte bound is a backstop that a correct entry bound never touches. 1 MiB is **0.39%** of the 256 MiB cache bound.

A.4 — **`hot_max_file_bytes = 8388608` (8 MiB).** It must be `≤ cache_max_entry_bytes` (S-10), and it is **12.5%** of the 64 MiB default entry cap and **3.1%** of the 256 MiB bound. The hot files of a working tree are source files (KiB); 8 MiB is an order of magnitude above them and still excludes archives, images and build artefacts. **Inclusive** (S-2) so the edge is stated rather than implied.

A.5 — **`hot_refresh_max_inflight = 2`, `hot_pool_share = 1/8` ⇒ `hot_pool_slots = floor(25/8) = 3`, foreground floor = 22 slots (88%).** The reservation ceiling is `2 × 8 MiB = 16 MiB`, and the share ceiling caps it at `3 × 8 MiB = 24 MiB` — **9.38%** of the cache bound. 1 in flight serialises the queue; 2 keeps a slow transfer from stalling warming while staying below the share. The share is a hard ceiling; the cap is the governor.

A.6 — **`hot_stop_deadline = 1500 ms`** = manager tick (1000 ms) + yield quantum (250 ms) + 250 ms slack. `hot_tick_interval = 1 s` is finer than the channel's own poll cadence (2 s), so the manager is never the bottleneck between invalidation and decision.

A.7 — **`hot_yield_after = 250 ms`.** Under a third of the measured end-to-end cost of an entire 25×-concurrent run over the same bytes (**0.79 s**, `client.go:21–24`) — small enough to be invisible beside the work it protects, long enough not to yield to a transient waiter.

A.8 — **`hot_refresh_deadline = 10 s`** = one third of `DefaultOpTimeout` (30 s, `errors.go:232–233`): a speculative fetch is never worth a foreground operation's deadline. **`hot_refresh_reacquire_window = 30 s`** = the same op timeout, and 15 poll opportunities at the 2 s default cadence — an invalidation that has waited 15 cadences is stale.

A.9 — **`hot_queue_max_depth = 256`.** A single `invalidate` event may carry up to 4096 paths (`ops.go:456–461`); 256 is **6.25%** of one worst-case event, deliberately far below it because Q-3, not depth, is what protects the hot file. **`hot_queue_max_wait = 300 s`** = exactly one decay step, so an item that has waited a full step has already lost 10% of its score and is no longer the most interesting thing in the queue.

A.10 — **`hot_read_touch_window = 5 s`**, **`hot_flush_interval = 30 s`**, **`hot_score_ceiling = 10000`.** The touch window dedupes FUSE chunked reads (≈64 calls for an 8 MiB file); the flush interval bounds a hard kill's score loss to 30 s of a hint (acceptable, because P-0); the ceiling keeps the stored numbers small enough that renormalisation is a formality, and renormalisation is order-preserving by construction.

A.11 — **Backoff `250 ms · 2ⁿ`, cap 30 s, full jitter.** The cap equals `DefaultOpTimeout`: a speculative fetch must never back off longer than the foreground would wait for the same bytes, or the hot path is slower than the cold path.

A.12 — **Reason vocabularies.** Skips (S-12): `oversize_pre`, `oversize_after_head`, `untracked`, `disarmed`, `stopped`, `cache_disabled`, `no_room`, `pinned_eviction`, `queue_full`, `resync`, `tree_mismatch`, `not_found`, `replaced`. Abandons (P-13): `deadline`, `reacquire_window`, `stopped`, `pinned_eviction`, `no_room_after_start`, `tree_mismatch`, `not_found`, `replaced`, `shutdown`. Tracker resets (H-20): `missing`, `corrupt`, `version_mismatch`, `mount_key_mismatch`, `tree_mismatch`. All closed: an unlisted reason is a defect, and a listed reason with no reachable trigger is dead vocabulary that must be removed rather than shipped as a zero (S-14).

A.13 — **`hot_pool_pressure_ticks = 5`** (P-19). Five consecutive manager ticks = **5 s** of a foreground request waiting longer than `hot_yield_after`. Long enough to distinguish pressure from a transient waiter (a single tick would self-stop on any hiccup), short enough that a busy client does not warm through it. It is the trigger for the automatic stop, and therefore the only automatic re-arm in the document (Q-13).

A.14 — **Measured facts quoted above**, all read from this branch: `DefaultConcurrency = 25` (`client.go:26`); `DefaultOpTimeout = 30 s`, `DefaultBindTimeout = 5 s` (`errors.go:231–233`); `DefaultCacheMaxBytes = 268435456`, `DefaultCacheMaxEntryBytes = 67108864`, `DefaultCacheMaxAge = 1 h` (`cache.go:17–26`); `usedLocked` = blobs + index (`cache.go:592–597`); `MountID = sha256(trimmed base)/[:12]` (`status.go:131–143`); eviction order (`cache.go:405–450`); the oversize pre-filter (`fs_linux.go:1093,1100` vs `cache.go:312`); `DefaultPollInterval = 2 s` (`invalidate.go:91`); `max_paths_per_event = 4096` (`ops.go:456–461`); `Head`/`Get` (`client.go:455,497`); the bind-time baseline (`fs_linux.go:203–238`).

---

## Appendix B — how this document relates to the documents it inherits

- **`PRD-bunker-invalidation.md`** — the design authority. §2.4 becomes §1 (P-0/P-1); §2.6 becomes §7.2 (single-flight); §2.7 becomes §9; §3 becomes §3 (with H-5 completing it — F-3); R4 becomes §3, R5 becomes §4, R6 becomes §5, R7 becomes §6, R9 becomes §7. §2.5 is consumed and its one conflict with BFS-031 is reported as F-1. This document re-argues none of those decisions.
- **`SPEC-watcher-capability.md`** — the sibling contract. Its §9 hands BFS-042 exactly one interface: "the hot-file refresh is a **performance** feature and must not become a correctness dependency (PRD §2.4). Nothing about scoring, sizes, or pool shares." This document accepts that hand-off, adds nothing to the watcher's vocabulary, and consumes §2.2's `events` bounds (4096 paths, the first-observation rule) in D-1 and A.9.
- **BFS-038's spec** — not yet written; the representation this document consumes is named in D-4, §5.6 and PR-3, and every place this spec depends on it says so rather than restating it.
- **BFS-031 / BFS-032** — filed defects, used here as failure shapes (§2's table, S-13, Q-8/Q-9, F-1, F-2) and **not repaired**: their repairs are their own rows, and both need a code change this document deliberately does not specify.
