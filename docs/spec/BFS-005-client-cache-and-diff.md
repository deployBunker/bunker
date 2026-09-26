# SPEC: the client's caching and diff feature set — bounded cache, invalidation channel with fallback, content-hash conflict refusal, and delegated diff/status

**Row:** `BFS-005` (P1, complexity 3) · **Type:** spec only — no product code changed
**Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Status:** proposed — design authority for the four client mechanisms below
**Owning PRD:** [`docs/prd/PRD-bunker-fs.md`](../prd/PRD-bunker-fs.md) (this spec pins what that PRD
schedules as slices `C3`/`C4`/`C5`, §288–298)
**Builds on:**
[`docs/investigation/BFS-003-fuse-client-binding.md`](../investigation/BFS-003-fuse-client-binding.md)
(the binding decision and the capability map — §3 is this spec's substrate) and
[`docs/investigation/BFS-002-multi-protocol-listener.md`](../investigation/BFS-002-multi-protocol-listener.md)
(the transport the event channel and the delegated calls ride on).
**Feeds:** `BFS-008` (client), `BFS-009` (implements this spec), `BFS-010` (Windows parity of the same
policy type), `BFS-012` (the proofs).
**Inherits by reference, does not restate:** `PRD-bunker-remote-editing.md` — the mount gives edits and
never execution, the lease registry, the anti-lying rule ("an operation is not reported successful without
a server ack").

---

## 0. What this document decides

Four mechanisms, one contract each. Every one of them is a decision with a number, a wire shape, an
owner in go-fuse's API surface, and a stated failure mode — not a restatement of the investigation.

| # | Mechanism | The decision in one line |
|---|---|---|
| 1 | **CACHE** | Content-addressed, whole-file entries under a hard cap of **268,435,456 bytes (256 MiB)**, LRU-evicted by *path entry*, with the kernel's own file cache switched **off** so ours is the only copy and the only figure that matters. Full ⇒ evict, and if nothing is evictable, **bypass** — never grow, never fail, never block. |
| 2 | **INVALIDATION** | A server-pushed, sequenced event stream drops **both** caches — ours *and* the kernel's, via `InodeNotify`/`EntryNotify`/`DeleteNotify`. A sequence gap is treated as a full resync. Absent watcher ⇒ **DECLARED poll mode** on a 2 s interval that is visible in `bunker fs status`, never a silent difference. |
| 3 | **CONFLICT** | Writes publish with `If-Match: sha256:<expected>` against the **content hash of the target file** — never mtime. Mismatch ⇒ **412**, target bytes unchanged, caller sees `ESTALE`, the current hash is named. The base hash is taken from the server, never assumed. |
| 4 | **DIFF / DELEGATION** | `status`, `diff`, `rev-parse`, `ls-files`, `log` and the tree **snapshot** are computed on the agent and returned in **one call** from a fixed allow-list — no shell. Measured ceiling of every transparent transport: whole-tree `diff --stat` is **32.74 s on NFS** and **STALL (45.09 s) on WebDAV**, against **0.41 s for all 14 operations run on the host**. |

**The spine of the design, stated once:** *one hash is the currency of reads, writes and invalidation.*
A cache entry is keyed by the sha256 of its bytes; a write carries the sha256 it expects to replace; an
invalidation names the sha256 that replaced it. The three mechanisms are one mechanism seen from three
sides, which is why they are specified together.

**What this spec is not:** it is not the server surface (`BFS-004` owns the verb/URI spelling and the
capability handshake), not the transport (`BFS-002`/`BFS-006`), not the client's mount plumbing
(`BFS-008`), and not execution of any kind — **nothing here runs a build, a test, or a shell on the
agent.** It is also not a second lock system: the conflict path backs onto the in-tree lease registry
(`<git-common-dir>/agent-leases.json`) exactly as the PRD's seed requires.

---

## 1. The measured premises (quoted, never estimated)

Every number in this spec is one of these, or is marked as a **chosen default** (a decision, not a
measurement). Provenance is `file:line` so any claim can be re-derived.

| # | Fact | Value | Provenance |
|---|---|---|---|
| M1 | RTT, control host → dedi-2 (Helsinki) | **185.24 ms** (mdev 0.16 ms) | `PRD-bunker-fs.md:21` |
| M2 | RTT, control host → bunker-mvp (Falkenstein) | **~198 ms** | `PRD-bunker-fs.md:22` |
| M3 | sshfs: operations completing / stalling at 45 s | **5 of 14 ok; 7 (dedi-2) / 8 (mvp) stall** | `PRD-bunker-fs.md:23–25` |
| M4 | sshfs: `status --short`, `diff --stat`, `commit`, `checkout -b` | **STALL (rc=124) on every run, both hosts** | `PRD-bunker-fs.md:30` |
| M5 | The same 14 operations with git **on** the host | **14/14, 0.41 s flat, zero stalls** | `PRD-bunker-fs.md:31` |
| M6 | WebDAV (rclone) totals; `diff --stat HEAD` | **11 ok / 1 stall**; `diff --stat` **STALL at 45.09 s** | `PRD-bunker-fs.md:85–86` |
| M7 | **NFS** `diff --stat HEAD` (the whole-tree walk) | **32.74 s** | `PRD-bunker-fs.md:85` |
| M8 | NFS other tree ops: `commit --amend` / `checkout -b` / `rebase HEAD~1` | **43.96 s / 18.83 s / 8.82 s** | `PRD-bunker-fs.md:81–84` |
| M9 | NFS fast ops: `rev-parse` / `ls-files` / `status` | 0.11 s / 0.11 s / 1.51 s | `PRD-bunker-fs.md:77–80` |
| M10 | HTTP/2 vs HTTP/1.1, 100 concurrent, one connection forced | **0.79 s (7.88 ms/req) vs 38.47 s (384.67 ms/req)** — **25.0×**, 49× per request, 2.4% of one RTT | `PRD-bunker-fs.md:60–61` |
| M11 | Connection setup, one conn vs multiplexed | **1589 ms vs 278 ms (5.7×)** | `PRD-bunker-fs.md:102` |
| M12 | rclone against the cache story: three variants, ~2.5 h | **wedge — zero rows; `vfs cache: cleaned: objects 0` for 17 straight minutes** | `PRD-bunker-fs.md:40–41` |
| M13 | Kernel READ ops for one 4 MiB file, **no OPEN flags**: 3 sequential opens | **32 READs per open, 96 total, 12,582,912 bytes — no reuse across opens** | `BFS-003 Appendix A.6` |
| M14 | Writes arrive wholly visible: 4,194,304 bytes written | **1023 WRITE ops totalling exactly 4,194,304 bytes**, per-op 94–8,106 bytes | `BFS-003 Appendix A.6` |
| M15 | Default INIT takes `AUTO_INVAL_DATA`, **not** `EXPLICIT_INVAL_DATA`, even though the kernel offered both | mtime-driven invalidation is the **default** | `BFS-003 Appendix A.5`, §3(a) |
| M16 | `WRITEBACK_CACHE` offered by the kernel and **not taken** | writes are never deferred behind a writeback cache | `BFS-003 §3(c)`, `Appendix A.5` |
| M17 | go-fuse protocol floor / speaks / kernel offered | **7.12 / 7.28 / 7.45** | `BFS-003 §6`, `Appendix A.1`, `A.5` |
| M18 | `NOTIFY_INVAL_INODE` + `NOTIFY_INVAL_ENTRY` floors | **7.12 = go-fuse's own minimum** (= Linux 2.6.31) | `BFS-003 §3(b)`, §6 |
| M19 | `NOTIFY_DELETE` floor | **7.18** (= Linux 3.3) | `BFS-003 §6` |
| M20 | Pure-Go client, `CGO_ENABLED=0`, statically linked | release matrix is `linux/amd64 linux/arm64` at `CGO_ENABLED=0` | `BFS-003 Appendix A.4`, `Makefile:14`, `Makefile:95–105` |
| M21 | `READDIRPLUS` negotiated by default; each entry resolved through our `Lookup` in-process | one kernel `READDIRPLUS` can be answered from an in-memory node tree | `BFS-003 §3(d)`, `Appendix A.5` |
| M22 | `PASSTHROUGH` negotiated and **not investigated** | may be a much cheaper read path; deserves its own look | `BFS-003 §9.10` |

### 1.1 One reconciliation the row needs (accepted criterion 4 names a number that does not exist)

The row's criterion 4 says "compare against the measured **33–36 s** NFS walk". **No 33–36 s figure exists
in either landed evidence document.** The NFS column of the PRD's own table carries exactly one whole-tree
walk number — `diff --stat HEAD` at **32.74 s** (`PRD-bunker-fs.md:85`) — and the other NFS tree ops are
43.96 s / 18.83 s / 8.82 s (M8). This spec therefore compares against **32.74 s** and records the
discrepancy instead of quietly inventing a range. If the row's author had a different measurement, that
measurement is not on the record and cannot be quoted; the *row's wording* is the thing to correct. (Also
see Open decision D-3.)

---

## 2. The four capabilities → concrete go-fuse operations

Per `BFS-003 §3`, with the anchors re-verified against `go-fuse v2.11.0` for this spec. Legend: **NATIVE**
= the binding provides the mechanism; **OURS** = our op handler / policy; **CANNOT** = impossible through
the binding.

| Capability | go-fuse surface we use (exact) | Verdict |
|---|---|---|
| **(a)** bounded local cache | `fs.NodeOpener.Open(ctx, flags) (FileHandle, fuseFlags, errno)` returns **`fuse.FOPEN_DIRECT_IO`** (`fuse/types.go:252`) so the kernel keeps **no** file data; `fs.NodeReader.Read` (`fs/api.go:376`) is where bytes enter; `fuse.MountOptions.ExplicitDataCacheControl = true` (`fuse/api.go:307`, applied at `fuse/opcode.go:126–133`); `fs.Options.AttrTimeout/EntryTimeout/NegativeTimeout` (`fs/api.go:769/774/779`) set to **0** at mount and per-reply where needed (per-reply override: `fs/bridge.go:255–283`) | **NATIVE** knobs, **OURS** policy + store |
| **(b)** push-driven invalidation | `Server.InodeNotify(ino, 0, 0)` (`fuse/server.go:592`, floor 7.12), `Server.EntryNotify(parent, name)` (`:790`, floor 7.12), `Server.DeleteNotify(parent, child, name)` (`:769`, floor 7.18); availability probed with `InitIn.SupportsNotify` (`fuse/server.go:814–825`); `Server` embeds `protocolServer` (`fuse/server.go:44–45`), which is what makes them callable | **NATIVE** primitive, **OURS** channel + inode map |
| **(c)** write precondition | `fs.NodeWriter.Write(ctx, f, data, off)` (`fs/api.go:383`) sees every chunk; publication at `fs.NodeFlusher.Flush` (`:398`) / `fs.NodeFsyncer.Fsync` (`:389`) / `fs.NodeReleaser.Release` (`:407`); the guarantee underneath is that **no go-fuse v2.11.0 option enables `CAP_WRITEBACK_CACHE`** — the bit is defined (`fuse/types.go:293`) and printed (`fuse/print.go:40`) but is absent from the accepted mask (`fuse/opcode.go:114–116`), so writes can never be deferred out of our sight (M14, M16) | **OURS**, native guarantee underneath |
| **(d)** whole-tree delegation | `fuse.NodeLookuper.Lookup` (`fs/api.go:517`), `fs.NodeReaddirer.Readdir` (`:565`), `fs.NodeGetattrer.Getattr` (`:317`), `fs.NodeStatfser.Statfs` (`:285`) all served from **one** snapshot; `READDIRPLUS` resolves each entry through our `Lookup` in-process (M21) | **NATIVE** substrate, **OURS** snapshot + delegated ops — with one **CANNOT** (§6.4) |

**Not used, deliberately:** `InodeNotifyStoreCache` (`fuse/server.go:625`, floor **7.15**) — pushing bytes
*into* the kernel cache contradicts decision C-2 (the kernel keeps no file data). It stays available and
unused; if the direct-I/O decision is ever reversed (Open decision D-1), it is the tool that fills the
kernel cache from an invalidation.

---

## 3. CACHE — capability (a)

**Contract:** the local copy is **bounded by a number, populated on access, and reported**. It is never a
mirror: nothing is fetched that nobody read.

**Decisions this section pins** (referenced as `C-n` below): **C-1** the bound is a *hard cap* over the
bytes actually on the client's disk; **C-2** the kernel keeps **no** file data — our cache is the only byte
cache, and therefore the only figure that can be compared with `du`; **C-3** when the cache is full the
client **evicts**, and if nothing is evictable it **bypasses** — it never grows, never fails a read, never
blocks; **C-4** the store is **content-addressed** with a path index over it.

### 3.1 The bound (accepted criterion 1)

| Parameter | Value | Kind |
|---|---|---|
| `--cache-max-size`, default | **268,435,456 bytes (256 MiB)** | chosen default (PRD `:203`, `AC-5` `:130`), the number the owner's decision recorded |
| Hard-or-soft | **HARD.** The client never exceeds it — the cap is enforced at insert, not sampled after | decision C-1 |
| What the cap counts | `cache_blobs_bytes` **+** `cache_index_bytes` — the bytes actually on the client's disk, so `du` can be compared to a single reported figure | decision C-1 |
| `--cache-max-entry-bytes`, default | **min(cache_max_size, 67,108,864) = 64 MiB**; a single file larger than this is **never cached** | chosen default |
| `--cache-max-age`, default | **1 h** — a backstop TTL, not the primary mechanism (invalidation is) | PRD `:204` |
| Cache dir | `$XDG_CACHE_HOME/bunker/fs/<mount-id>/` (default `~/.cache/bunker/fs/<mount-id>/`), mode 0700 | decision (this spec) |
| `--cache-max-size 0` | the cache is disabled: every read goes to the server, every figure reports 0 | decision C-1 |

### 3.2 The store, and why it is content-addressed

**Decision C-4.** An entry is a **whole file's bytes**, stored under the sha256 of those bytes, plus a **path
index** entry (path → blob, hash, size, last-hit, generation). Consequences, all deliberate:

- **The read that fills the cache is the read that decides the conflict.** The hash of the bytes we cached
  is the hash a later write carries as `If-Match` (§5). No second hashing pass, no drift between "what I
  served" and "what I expect".
- **Two paths with identical content cost one blob.** A git worktree with a checked-out duplicate, a
  `dist/` file copied into two places, a vendored file — one local copy. This is the mechanism that makes
  "local copies must not grow with the tree" true even for trees that are internally redundant.
- **`du` agreement is achievable.** Whole-file blobs (not 128 KiB chunks) means the reported
  `cache_blobs_bytes` matches `du -s --block-size=1` on the cache dir up to filesystem block rounding, which
  is what `AC-5` asks to be shown side by side.

**Numbers reported** by `bunker fs status --json` (all of them; a bound the owner cannot see is not a
bound):

```json
{
  "mount": "<mount-id>",
  "mode": "push",
  "cache": {
    "max_bytes": 268435456,
    "blobs_bytes": 0,
    "index_bytes": 0,
    "used_bytes": 0,
    "entries": 0,
    "blobs": 0,
    "evictions_total": 0,
    "bypass_events": 0,
    "oversize_bypasses": 0,
    "pinned_blobs": 0
  },
  "invalidation": { "mode": "push", "seq": 0, "last_event_age_ms": null, "poll_interval_ms": null },
  "conflicts": { "refusals_total": 0, "last": null },
  "transport": { "verdict": "healthy", "last_ok_age_ms": 0, "cause": null }
}
```

`used_bytes = blobs_bytes + index_bytes` is the figure `AC-5` compares with `du`; `max_bytes` is what it
must never exceed.

### 3.3 Eviction policy — what is evicted first (accepted criterion 1)

Eviction is triggered at insert when `used_bytes + new_entry > max_bytes`. Order, **first evicted first**:

1. **Unreferenced path entries, least-recently-used first.** "Unreferenced" = no open FUSE file handle ever
   served from that blob, and the path is not the target of an in-flight operation. LRU key is the
   monotonic timestamp of the last *hit* (read served), not the last insert.
2. **Entries whose blob is already dirty/being-published** are never candidates (they are the write path's
   data, not cache).
3. **Tie-break at identical timestamps:** largest blob first. Evicting one 64 MiB blob buys the room that
   evicting 400 small ones would, in one step, and keeps the eviction loop short.
4. **Ties on size:** lexicographic path order, so eviction is deterministic and reproducible in a test.

**Never evicted:** a blob **pinned** by a live open handle. A pinned blob stays until its last handle
releases; at `Release` the blob is dropped if its path entry was evicted while it was pinned. This is what
makes eviction safe under concurrent readers: the handle holds its own reference, so a read in flight can
never be reading reclaimed bytes.

Dropping a path entry drops the blob **only when its last path reference goes** (refcount), which is where
the content-addressing pays for itself.

### 3.4 What happens when the cache is full (accepted criterion 1)

**Evict, then fetch. If eviction cannot free enough, bypass.**

1. The miss fetches from the server and inserts. Insert first evicts per §3.3 until the entry fits.
2. If the entry cannot be made to fit — every remaining candidate is pinned, or the entry alone exceeds
   `--cache-max-entry-bytes` — the read is served **straight through**: the bytes go to the caller's buffer,
   no blob is written, `bypass_events` (or `oversize_bypasses`) increments, and the read **succeeds**.
3. It never grows past the bound. It never fails the read because of a local-capacity condition. It never
   **blocks** waiting for a pinned handle to close — a cache that blocks is the "indefinitely hanging tool"
   the house PRD's criterion 19 exists to forbid (`PRD-bunker-remote-editing.md:276`, `PRD-bunker-fs.md:50`).
4. Bypass is reported, so "the bound is being respected by not caching anything" is visible rather than
   disguised as a healthy bound.

### 3.5 The kernel half: the kernel keeps no file data (M13, M15)

`ExplicitDataCacheControl` is **set**, and every `Open` returns **`FOPEN_DIRECT_IO`**. Together these mean:

- The kernel does not cache file data, so the only bytes cached locally are ours — bounded, reported,
  hash-keyed, evictable. There is no second, unbounded, unreportable, mtime-driven cache in the picture.
  This matters for `AC-5` literally: a kernel page cache is not disk usage, so an unbounded kernel cache
  would not break the byte bound but *would* break the invalidation story (§4) and hide the reported figure.
- The measured cost of not having it is already paid today: with no OPEN flags the kernel re-issued **32
  READ requests per open, 96 total for three opens of one 4 MiB file, with no reuse across opens** (M13).
  Under this design those READ ops are served from our LRU at memory speed after **one** fetch per
  `(path, hash)` — i.e. the 32-op pattern stops being 32 opportunities for a network round trip.
- `AttrTimeout`, `EntryTimeout` and `NegativeTimeout` are set to **0** and each `Lookup`/`Getattr` reply
  carries its own timeout (`fs/bridge.go:255–283`). The default this replaces is not zero: with a nil
  `Options`, `fs.NewNodeFS` applies **1 second** entry *and* attribute timeouts (`fs/bridge.go:291–297`), so
  an unconfigured client would serve a stale `stat` for up to a second — long enough for a build to touch a
  file and for git to decide it did not change. A nonzero TTL would let the kernel's **mtime-driven**
  attribute cache serve that stale `stat`, and a stale `stat` is exactly how git decides a file did not
  change and skips re-reading it (`BFS-003 §3(d)`). Zeroing the TTL makes every attribute question come to
  us, where it is answered from the snapshot in-process with **zero** network round trips (M21).
- **This is a default that must be set deliberately, not inherited.** Measured: the default INIT takes
  `AUTO_INVAL_DATA` and not `EXPLICIT_INVAL_DATA` (M15). `BFS-008` must not accept the default.

**Cache population is on access.** There is no tree-wide prefetch and no mirror. The only bulk fetch in the
design is the **metadata snapshot** (§6.3), whose bytes are small and counted in `cache_index_bytes`; file
bytes are fetched only by a read that actually asked for them.

---

## 4. INVALIDATION — capability (b)

**Contract:** an agent-side watcher pushes invalidations; the client drops its entry **and** the kernel's;
the fallback when the watcher is absent is a **declared poll mode visible in status**, never a silent
difference (accepted criterion 2).

### 4.1 The server-pushed event

Channel: a long-lived stream on the same connection family as the mount —
`GET <mount-base>/fs/events?mount=<id>&since=<seq>` with `Accept: text/event-stream`. It rides the
h2/h3 connection the rest of the mount uses; on a clean close the client reconnects with `since=<last-seq>`.
*(The final URI/verb spelling is `BFS-004`'s to pin — the semantics below are this spec's.)*

One event = one JSON object, one line (SSE `data:`), sequenced **per mount**:

```json
{"v":1,"seq":41,"op":"inval","path":"src/main.go",
 "hash":"sha256:<64-lowercase-hex>","ino":1234,"ts":"2026-09-26T12:00:00.000Z"}
```

| `op` | Meaning | Emitted when |
|---|---|---|
| `inval` | file bytes changed (or the file's content hash is now known to differ) | watcher saw a write/close-write/attrib on a path |
| `entry` | a name appeared, disappeared by rename, or its metadata changed | create/rename/move where parent is known |
| `delete` | a name is gone | unlink/rmdir |
| `resync` | the client must drop **everything** and re-snapshot | watcher restart, mount re-bind, or `seq` gap detected by the server |
| `overflow` | kernel inotify queue overflowed — knowledge is lost | `IN_Q_OVERFLOW` from the agent watcher |

`seq` is monotonic per mount. **A gap in `seq` is treated as `resync`, not as a lost line.** That single
rule is the closure for the PRD's named risk "inotify misses events (overflow, unmounted, kernel limits)"
(`PRD-bunker-fs.md:282`): a missed event is not silently a stale byte, it is a full drop and re-snapshot.
`resync` and `overflow` are also the only events that may arrive without a `hash`.

### 4.2 What the client drops, in order (accepted criterion 2)

On `inval`/`entry`/`delete` for a path, the client drops, **in this order**, so that no observer can see a
half-invalidated entry:

1. **Our path entry** — the path index entry (and the blob only if refcount reaches zero).
2. **Our metadata** — the path's node-tree record (size, mtime, mode, hash); **and its parent directory's
   readdir snapshot**, because a new name must appear in the next `readdir`/`READDIRPLUS` answer.
3. **The kernel's copies** — one notify call per class:
   - data + attributes of the inode → `Server.InodeNotify(ino, 0, 0)` (`fuse/server.go:592`, floor **7.12**)
   - a directory entry (rename/unlink we cannot express as a delete) → `Server.EntryNotify(parentIno, name)`
     (`:790`, floor **7.12**)
   - a name that is provably gone → `Server.DeleteNotify(parentIno, childIno, name)` (`:769`, floor **7.18**)
   Availability is *runtime-probed*, never assumed: `InitIn.SupportsNotify` (`fuse/server.go:814–825`).
   Both primitives this design needs at its hottest sit at **7.12 — go-fuse's own minimum** (M18), so a
   kernel below 7.18 loses only the `DeleteNotify` refinement and falls back to `EntryNotify`, which is
   enough to stop the kernel serving the name.
4. **The write precondition's base hash is refreshed, never dropped.** If the event carries a `hash`, the
   path's base hash becomes that hash. If it does not, the base hash becomes **`unknown`** — which forces
   the fetch-then-check rule of §5.3 on the next write. *Deliberately not* "drop it and forget": forgetting
   the base hash is how a lost update gets silently allowed.

**Where the inode number comes from:** the node tree already holds `fuse.EntryOut.NodeId` /
`Attr.Ino` for every path the client has resolved (`server.InodeNotify` takes that number, not a path). A
path with no inode yet — never looked up — needs **no notify call**: there is nothing in the kernel to
invalidate, and step 2 removed our metadata. This is why the design never needs a wildcard invalidation.

### 4.3 Latency budget (measured premises, declared spend)

`AC-4` requires the agent-side edit to be visible within **≤ 2 s** with no remount (PRD `:129`).
The budget, from measured premises only:

| Component | Spend | Basis |
|---|---|---|
| agent watcher debounce | **≤ 250 ms** | chosen default (D-1: the end-to-end number is unmeasured) |
| event propagation to the client | **~1 RTT = 185.24 ms** + tiny payload; the stream is already open, so no connection setup is paid (setup is 1589 ms if it were not: M11) | M1 |
| client apply (drop + notify) | no round trip; notify calls are kernel writes | design |
| **Total budget** | **≤ 2 s**, i.e. ~10× the one-way cost | M1 |

`BFS-003 §9.1` records the honest gap: no agent-side watcher exists yet, so this end-to-end figure is
**not measured**. What this spec fixes is the *spend* (250 ms debounce + one RTT + apply), so that when
`BFS-009` measures it, the number either fits the budget or the debounce is the thing that moves. The check
is named in §8 (`V-2`).

### 4.4 The DECLARED poll fallback (accepted criterion 2)

**When poll mode is used** — any of these, and only these:

1. The bind-time capability handshake answers `capability_unavailable` naming the watcher (PRD `AC-9`,
   `:134`) — an agent image with no inotify watcher.
2. The event stream cannot be established at all (404/405/501 on the stream resource — an older agent
   build), or fails **3 consecutive times** with a non-transport error.
3. The event stream is idle for **90 s** with no heartbeat (the server sends a heartbeat every 30 s, so
   90 s of silence means the channel is dead) — the client switches to poll **and says so**, rather than
   pretending the push channel is alive.
4. `--invalidation=push|poll|auto` set explicitly (default `auto`).

Transient transport failures are **not** poll triggers: they are `unreachable` (§7) and the client
reconnects with `since=<seq>`, because a transport blip does not mean the events stopped being generated.

**What poll mode is, concretely:** a `changes` call on the delegation surface —
`POST <mount-base>/fs/op` with `X-Bunker-Op: changes`, body `{"since": <rev>}` — returning the **same event
list shape** as §4.1 plus the current tree revision. The client polls one call per interval and applies the
same §4.2 drop sequence.

| Poll parameter | Value | Kind |
|---|---|---|
| `--poll-interval`, default | **2 s** | chosen default |
| Cost | 1 request per 2 s = **0.5 req/s**, ~185 ms of one link's duty cycle per call | M1 |
| Scope | one call covers the whole tree revision, not one call per directory | decision (this spec) |
| Staleness window | **≤ poll interval (2 s)** by construction — the figure `status` reports as the client's guaranteed freshness, so the difference from push mode is a *number a caller can read*, not a behaviour nobody can see | decision (this spec) |
| Mode reporting | `bunker fs status --json` → `invalidation.mode` ∈ `push` \| `poll`, plus `poll_interval_ms` and `last_event_age_ms` | PRD `AC-9` `:134` |

Two modes, one vocabulary. `bunker fs status` never reports "invalidation: ok" without saying **which
mechanism** answered; a mount silently downgraded to polling would be the exact class of defect `AC-9`
exists to catch.

---

## 5. CONFLICT — capability (c): the write-precondition exchange

**Contract:** a write carries an expected content hash; a mismatch **refuses** and names the current hash.
**Content hash, never mtime** (accepted criterion 3).

### 5.1 Why the hash, and why mtime is only ever a cache key

Under an active build the tree is touched constantly with unchanged bytes. An mtime-keyed check refuses
safe writes until someone disables it — and the first thing anyone does with a nagging check is disable it
(`PRD-bunker-fs.md:235`). So:

- **The decision** ("may this write land?") is **always** a comparison of sha256-of-file-content.
- **mtime is allowed in exactly one place:** deciding whether the *server's hash cache* may be reused. The
  server keys its cached hash by `(dev, ino, size, mtime)`; if any of those moved, it **rehashes before
  deciding**. mtime can therefore cause extra work, never a wrong answer.

Hash form: `sha256:<64 lowercase hex>` over the **raw file bytes** (not a git blob hash) — the same digest
the cache is content-addressed by (§3.2) and the same one the invalidation event carries (§4.1). One hash
function, one representation, three uses.

### 5.2 The exchange (accepted criterion 3)

Publication is a single, whole-file, preconditioned call — never one call per FUSE WRITE op (there are
**1023** of those for a 4 MiB file: M14).

```
client                                                      agent / bunkerd
  │ FUSE WRITE ops (chunks)                                    │
  ├── PUT  /fs/stage/<token>          X-Bunker-Stage-Offset: N ───────▶  append to staging object
  │      (0 … 1023 chunks; strict offset continuity, 409 on gap/overlap)
  │                                                                    │
  │ FUSE Flush / Fsync / Release                                       │
  ├── POST /fs/publish  If-Match: "sha256:<expected>" ────────────────▶  compare against current content hash
  │      {stage:<token>, size:N, hash:"sha256:<new>"}                  │
  │                                                                    │
  │◀── 200 {etag:"sha256:<new>", rev:411}  ── write landed, revision bumped
  │        …or…
  │◀── 412 {"error":"hash_mismatch",                                   │
  │          "expected":"sha256:<expected>",                           │
  │          "current":"sha256:<current>",     ← NAMED, both hashes     │
  │          "etag":"sha256:<current>"}                                │
  │     staging object DELETED; target file byte-identical to before    │
```

| Case | Server | Client sees |
|---|---|---|
| precondition matches | writes, computes new content hash, returns `200` + `ETag: "sha256:<new>"` + new `X-Bunker-Rev`, and emits an `inval` event to **other** mounts of the same tree | success |
| precondition mismatches | **412**, no byte written, `current` names the server's hash | refusal: `ESTALE`, names both hashes |
| target path absent and `If-Match` present | **412** with `"current": null` — one refusal class for "the state I expected is not there", so a caller never has to branch on 404-vs-412 | refusal: `ESTALE` |
| target absent and `If-None-Match: *` | creates | success |
| staging offsets gap/overlap | **409**, staging object destroyed | `EIO`, named `stage_protocol` |
| write to a path outside the mount root | **403** | `EPERM` |

**What the client does on mismatch** (accepted criterion 3, both halves):

1. It **does not retry blindly, does not merge, does not overwrite.** The staged bytes are discarded.
2. The caller's write fails with **`ESTALE`** (§7.1); `fsync` fails with `ESTALE`; `Release` records the
   refusal. *(POSIX `close(2)` ignores errors — which is precisely why the refusal is also written to the
   mount's conflict log and surfaced in `bunker fs conflicts`, rather than relying on the syscall alone.)*
3. The path's base hash is **updated to the server's `current`**, so a caller that re-reads and retries
   starts from truth rather than from its stale belief.
4. The refusal is counted and listed: `conflicts.refusals_total`, `conflicts.last` = `{path, expected,
   current, ts}` in `bunker fs status --json`, and `bunker fs conflicts [--json]` for the full list.
5. Recovery is the caller's loop, per the PRD: re-read → merge → retry, **closed when a write lands with a
   matching hash** (`PRD-bunker-fs.md:190`). If the class *recurs on the same path*, the escalation is the
   in-tree **lease registry** — `LOCK`/`UNLOCK` back onto `<git-common-dir>/agent-leases.json`, keyed by
   tree, never by mount or agent (`PRD-bunker-fs.md:217, :248`). **This spec adds no second lock system**,
   and the client **never takes a lease implicitly**: an implicit lease turns one caller's refusal into
   every other writer's deadlock, so leasing is explicit (`bunker fs lock <path>`) or the escalation path.

`--on-conflict` decides only the narrow case the PRD names (`PRD-bunker-fs.md:207`):

| Value | Behaviour on mismatch |
|---|---|
| `refuse` (**default**) | 412 → `ESTALE` → caller re-reads. A silent overwrite is a lost update; a refusal is a recoverable event (`PRD-bunker-fs.md:319`). |
| `overwrite-if-unchanged` | If the server's `current` hash equals the hash the client **last served** for that path, the mismatch was *metadata-only* (our view of the server's content is exact) and the write is retried with the corrected base. If `current` differs from what we served, it is a **real concurrent edit** and the write is **still refused** with `ESTALE`. |

### 5.3 The base hash: where it comes from, and the case with no base

The precondition's value comes from **our cache, not from the caller** — a FUSE syscall cannot carry
"expected hash" (`BFS-003 §3(c)`). So:

| Situation | Base hash | Result |
|---|---|---|
| path was read (cache hit or miss) | the hash of the bytes we served | `If-Match: sha256:<that>` |
| path never read; target **exists** | **fetch-then-check**: `HEAD`/`GET`-metadata learns the current hash; adopt it as the base | `If-Match: sha256:<just-learned>` |
| path never read; target **absent** | none | `If-None-Match: *` |
| base invalidated without a hash in the event | marked `unknown` → same as "never read" | fetch-then-check |

**Fetch-then-check is a decision, and the reason is behavioural:** refusing any write to a path we never
read would break `cp`, `tar -x`, `sed -i` and every "create or replace" workflow that is perfectly safe.
Adopting the server's hash at write time means *"I intend to replace what is there now"* — a lost update is
still impossible, because the base is read from the server in the same exchange that decides the write. The
alternative (`PUT` unconditionally) **is** the lost update and is rejected.

### 5.4 What the binding cannot give us here, stated plainly

- **A caller-supplied precondition is impossible.** No FUSE syscall carries "expect hash H"; the base hash
  is always the client's own record. A tool that wants "only write if unchanged since *I* read it" must use
  the verb surface (Path A), not the mount.
- **The precondition is per file, not per chunk.** The 1023-op chunk stream (M14) is not 1023
  preconditions; staging + one publish is what makes a content hash meaningful.
- **mmap writers** produce page-granular WRITE ops at offsets we did not choose (`BFS-003 §9.5`, undecided
  there). This spec's recommendation: base hash adopted at the **first** write-open or first WRITE op,
  publication at `Release` — the same rule as §5.3, and the residual (whether a kernel can leave dirty
  mmap pages unpublished at Release) is carried as Open decision **D-4** with its check.
- **Client RAM is not the write buffer.** Chunks stream straight to the agent's staging object, so the
  client's per-write memory is one chunk (**≤ 8,106 bytes** measured, M14), not the file. An orphaned
  staging object (client died mid-write) is reaped by the agent after 10 min idle or on mount disconnect;
  the **target file is untouched until publish**, so an interrupted write can never leave a partial file
  (which is `AC-6`'s "no partial file is left behind", `PRD-bunker-fs.md:131`).

---

## 6. DIFF / DELEGATION — capability (d)

**Contract:** the operations whose *answer* is small and whose *cost* is a tree walk are computed on the
agent and returned in **one call**.

### 6.1 The delegated operations (accepted criterion 4)

| Op (`X-Bunker-Op:`) | What it returns | Why it is delegated |
|---|---|---|
| `status` | `git status --porcelain=v2` (also `--short`, `-uno`) | walks the tree; **STALL on sshfs** (M4), 1.51 s on NFS (M9) |
| `diff` | `git diff` / `--stat` / `--numstat` / `--cached`, over allow-listed refs | **the ceiling case**: 32.74 s on NFS (M7), **STALL 45.09 s on WebDAV** (M6) |
| `rev-parse` | `HEAD`, `HEAD~1`, branch names — allow-listed | 4.52 s / 4.11 s on sshfs (PRD `:26`); 0.11 s native-class |
| `ls-files` | tracked paths | 2.72 s / 2.41 s on sshfs (PRD `:29`) |
| `log` | **bounded** (`-1`, `--oneline -N`): 10.32 s / 10.12 s on sshfs (PRD `:27`) — unbounded log is refused rather than delegated | walks the commit graph |
| `snapshot` | the subtree metadata for `READDIRPLUS`/`Lookup` (§6.3) | the N-round-trips→1 call that makes the walk free (§6.2) |
| `changes` | the poll-mode event list + current tree revision (§4.4) | one call per interval instead of N stats |

**Never delegated, by rule:** anything that *executes*. The op list is a **fixed allow-list** executed by
`bunkerd` on the agent — the request body is a JSON object of structured fields
(`{"op":"diff","args":{"stat":true,"cached":false,"refs":["HEAD"]}}`), **never a shell command string**, so
the mount remains "edits, never execution" (`PRD-bunker-fs.md:260`, `PRD-bunker-remote-editing.md`).
Unknown ops, unknown fields and non-allow-listed refs are **400**, never a best-effort parse.

*(Wire spelling — verb, base path, header names — is `BFS-004`'s; the semantics and the op list are fixed
here. The `X-Bunker-*` header family is already this repo's convention: `X-Bunker-TLS-Unverified`
(`internal/audit/tls_unverified.go:43`), `X-Bunker-Chain-Head` (`internal/audit/ship.go:400`).)*

### 6.2 What it buys, against the measured ceiling (accepted criterion 4)

| Path | `git diff --stat HEAD` on a 150-file tree | Provenance |
|---|---|---|
| sshfs (today's default) | **STALL** (rc=124 at the 45 s probe timeout), both DCs | M4 |
| WebDAV transparent (rclone) — the best transparent result in the whole study | **STALL (45.09 s)**; 11 ok / 1 stall overall | M6 |
| NFSv4 (`nconnect=16`) | **32.74 s** | M7 |
| **git on the agent, all 14 operations** | **0.41 s flat, zero stalls** | M5 |
| **this design: one delegated call** | `0.41 s`-class compute **+ one RTT (185.24 ms)** ≈ **0.6 s**, and the walk is not attempted at the client at all | M5 + M1 |

**The honest arithmetic.** 0.41 s is the host-side figure for the whole 14-operation battery; the
*delegated single call* is not itself separately measured. What is measured is the two things it is made
of — the host's own cost (0.41 s, 14/14, zero stalls) and one round trip on this link (185.24 ms) — which is
why the claim is "compute on the host, pay one round trip", not "0.6 s proven". The end-to-end delegated
number is `BFS-009`/`BFS-011`'s measurement (`V-6` in §8), against the 32.74 s / 45.09 s / STALL baselines
above.

**Why no transport closes this gap** — and therefore why delegation is load-bearing rather than an
optimisation — is settled three times over: the whole-tree operations stall on sshfs (M4), stall at 45.09 s
even on the best transparent WebDAV result (M6), and take 32.74 s on the kernel's own NFS (M7). HTTP/2 does
not change it either: multiplexing removes the *per-operation* latency tax (25.0×, 7.88 ms/request, M10), it
does not remove the *work* — and `diff --stat HEAD` **STALLs even on WebDAV** (PRD `:104–106`). At 32.74 s
worst-measured, the walk is not a transport problem; it is a "do not ship the tree to the client" problem.

### 6.3 What that buys inside the filesystem: the snapshot kills the walk, not the reads

`READDIRPLUS` is negotiated by default (M21) and go-fuse resolves each entry through our own `Lookup`
in-process (`fs/bridge.go` `readDirMaybeLookup`, `:1087`) with per-entry timeouts applied
(`fs/bridge.go:255`). So **one** `snapshot` call populates the node tree, and then:

- `ls -l`, `stat`, and a `git status` walk answer from memory — **zero** further round trips.
- A **clean** tree costs one snapshot and **no** file reads: git re-reads only files whose stat moved, and
  the stat it gets comes from our snapshot (`BFS-003 §3(d)`).

### 6.4 The one CANNOT, and the alternative (accepted criterion 6)

**Cannot, through the binding alone:** `status`, `diff --stat`, `rev-parse` and `ls-files` **computed on
the agent** are *not* reachable by making git's syscalls smarter. git is not a client of our API; it calls
`lstat`/`open`/`read`, and we cannot know which tool is running or what it will ask for next. The mount can
make the **walk** one call and can make unchanged files free, but **git's own content reads for *changed*
files are N reads by construction, and no binding changes that** (`BFS-003 §3(d)`). This is a correction to
the PRD's phrasing, not a defect in it: *"`status` becomes a server-side operation behind a filesystem
interface"* is true **only for the metadata walk**.

**The alternative — what does close it:**

1. **The second surface.** Delegation keeps its own entry point — `bunker fs status` / `bunker fs diff`
   (CLI and verbs) invoking `X-Bunker-Op` — for the cases where the answer is computed remotely and never
   walked at all. That is the 0.41 s path (§6.2), and it is why the delegated ops ship in the same slice as
   the client rather than after it (`PRD-bunker-fs.md:280`).
2. **Inside the mount, the honest claim** is narrower and still large: the walk is one call, unchanged
   files are free, and only genuinely modified files cost N reads — the ones that would cost N reads
   anyway, now served from a bounded cache with hash-exact invalidation.
3. **The stated fallback if the client loses to delegation outright:** ship the delegated path alone
   (`PRD-bunker-fs.md:280`). This spec keeps that door open by specifying the delegated ops as a surface
   that does not depend on the mount being mounted.

---

## 7. Failure mode: the server is unreachable — FAIL LOUDLY, never hang (accepted criterion 7)

Replays the house PRD's criterion 19 — *"the failure surfaces as a bounded timeout with a named cause,
**not** an indefinitely hanging tool"* (`PRD-bunker-remote-editing.md:276`) — which the current sshfs path
**fails**, and which rclone failed worse than sshfs by hanging where sshfs at least returned `rc=124`
(`PRD-bunker-fs.md:48, :50`; `AC-6`, `:131`).

### 7.1 The errno table (one errno per recovery class; the cause is named in status)

| Condition | FUSE-visible error | Named cause (log / `status`) | Recovery |
|---|---|---|---|
| connect refused / TLS handshake failure | `ENOTCONN` | `unreachable_connect` | wait for `reconnect` window; a bound probe decides |
| in-flight request exceeded its deadline | `ENOTCONN` | `unreachable_deadline` | same; one bounded retry on a stream reset, **never** a second one in the read path |
| stream reset / GOAWAY mid-op | `ENOTCONN` | `unreachable_reset` | re-dial, then fail the op if the deadline passes |
| server unbound / destroyed / re-created (tree identity changed) | `EREMOTEIO` | `stale_identity` (names **both** identities) | **never auto-adopt a fresh tree**; requires `--recover` (`AC-7` `:132`) |
| write precondition mismatch | `ESTALE` | `conflict` (names both hashes) | re-read, merge, retry (§5.2) |
| malformed / 5xx response | `EIO` | `server_error` | report; do not retry blindly |
| local staging protocol violation | `EIO` | `stage_protocol` | caller retries the write |

`ENOTCONN` for all three transport causes is deliberate: **one errno per *recovery* class** (they recover
identically) with the *cause* named one level up, where a human and a test can both read it. `EREMOTEIO` is
kept distinct from `ESTALE` because a per-file conflict is recoverable by re-reading one file, while a tree
identity mismatch is not recoverable per file at all — and a tool that retries an `ESTALE` in a loop must
not do the same with a re-bound tree.

### 7.2 The deadlines (numbers)

| Deadline | Value | Kind |
|---|---|---|
| bind/connect at mount time | **5 s** | chosen default; failure ⇒ **the mount is refused**, nothing appears at the mountpoint |
| any in-flight operation (read, write chunk, publish, delegated op) | **30 s** | `AC-6`'s bound (`PRD-bunker-fs.md:131`) — adopted as the cap |
| event-stream heartbeat / idle switch to poll | 30 s / **90 s** | §4.4 |
| staging object reap (client gone) | 10 min idle | §5.4 |

At the measured 185.24 ms RTT (M1), 30 s is ~160× a round trip: the deadline is a ceiling on the
pathological case (a server that accepted the connection and then stopped answering — the shape that made
rclone unbounded), not a normal-case budget. **No retry may extend an operation past its deadline**
(bounded: at most one immediate retry on a stream reset).

### 7.3 Losing the server is loud, and the mount is never a phantom

1. **Every operation fails with a named errno within ≤ 30 s** — a bounded timeout, never an indefinite
   wait. A POSIX tool reports it verbatim ("Transport endpoint is not connected"), which is loud,
   standard, and diagnosable.
2. **`bunker fs status` says so**: `transport.verdict` = `healthy` | `unreachable` | `stale_identity`,
   with `cause`, and `last_ok_age_ms`. Three verdicts, never two — `unreachable` (transport) and
   `stale_identity` (wrong tree) demand **different** recovery, exactly as the durability spec argues
   (`docs/prd/SPEC-sshfs-mount-durability.md` §2).
3. **Nothing is reported successful without a server ack.** An operation that did not get one closes as
   `unverified`, never as success (the durability spec's rule 3, inherited).
4. **The mount is kept, not silently unmounted**, when the transport dies mid-session: unmounting under a
   live consumer is how a developer's editor loses a buffer. The mountpoint stays; **all** operations fail
   loudly; recovery is one documented command (`bunker fs mount --recover` → probe, remount once; or
   `bunker umount` then mount).
5. **At mount time, unreachable means refused:** the bind preflight probes the tree identity (the
   `AC-7` UUID probe) and the capability handshake with the 5 s deadline. Failure ⇒ exit non-zero with the
   named cause and **nothing at the mountpoint** — the "silent empty tree" failure mode the durability spec
   records as LIVE today (`mount_unreachable` / `workspace_invalid`).
6. **A hung server cannot hang the client**: the deadline timer lives on every request through
   `context.WithTimeout`, and the read path has no unbounded wait — its one escape hatch (cache bypass,
   §3.4) is a straight pass-through, not a wait.

---

## 8. What `BFS-009` must show (each check is observable, no criterion here is a claim)

| # | Check | Passes when |
|---|---|---|
| **V-1** | **Bound** (`AC-5`): read a tree larger than `--cache-max-size 256M`; sample `du -s --block-size=1` of the cache dir during the run and read `cache.used_bytes` from `bunker fs status --json` | both ≤ 268,435,456, and the two figures agree up to block rounding; `evictions_total > 0` |
| **V-2** | **Invalidation** (`AC-4`): append to a mounted file **on the agent**; timestamped read before/after, sha256 compared | new content within **≤ 2 s**, no remount, no manual cache clear |
| **V-3** | **Declared fallback** (`AC-9`): mount an agent with the watcher disabled | mount **succeeds**; `status --json` reports `capability_unavailable` naming the watcher and `invalidation.mode == "poll"` with a non-null `poll_interval_ms`; agent-side edits still visible within ≤ poll interval + RTT |
| **V-4** | **Refusal** (`AC-3`): two writers, second carries a stale hash | `ESTALE`; the 412 body names **both** hashes; content sha256 before/after identical; `conflicts.refusals_total == 1`; the path's base hash is now the server's `current` |
| **V-5** | **Kernel half of invalidation**: with a file open and read (so the kernel holds attributes/dentries), edit on the agent, then `stat` | the client's notify calls fire (`InodeNotify` for the changed inode, `EntryNotify` for a rename) and `stat` reflects the agent's values — **not** a TTL-delayed mtime |
| **V-6** | **Delegation** (`PRD` success 3): `bunker fs status --short` on the 150-file fixture with delegation on vs off | delegated path is measurably faster, output byte-identical; reported against the 32.74 s (NFS) / 45.09 s (WebDAV) / STALL (sshfs) baselines |
| **V-7** | **Transport kill** (`AC-6`): drop the listener mid-operation | every in-flight op returns `ENOTCONN` within ≤ 30 s with `cause` named; no partial file; recovery is one command |
| **V-8** | **Anti-gaming on the cache**: force bypass by shrinking `--cache-max-size` below one entry | reads still succeed, `oversize_bypasses > 0`, `used_bytes <= max_bytes`, no read ever fails for local-capacity reasons |
| **V-9** | **Direct-I/O decision** (D-1): run the read path with `FOPEN_DIRECT_IO` and, in a second arm, with `FOPEN_KEEP_CACHE` + `ExplicitDataCacheControl` | both arms correct; the numbers decide D-1, and the losing arm is reported, not buried |

---

## 9. Chosen defaults (decisions, **not** measurements)

Kept in one table so nothing in this spec can be read as a measured number that is not.

| Default | Value | Why this value |
|---|---|---|
| `--cache-max-size` | 268,435,456 B (256 MiB) | the PRD's owner-decision default (`:203`, `:318`) |
| `--cache-max-entry-bytes` | min(size, 64 MiB) | one entry must never be able to monopolise a quarter of the bound |
| `--cache-max-age` | 1 h | PRD `:204`; a backstop, not the mechanism |
| LRU key | last **hit**, not last insert | a hot path that is read often enough stays; a one-shot bulk read leaves |
| `--poll-interval` | 2 s | 0.5 req/s at 185 ms RTT is cheap, and 2 s is the `AC-4` budget itself |
| watcher debounce | 250 ms | ~1/8 of the 2 s budget, leaving 1.75 s for one RTT + apply |
| event heartbeat | 30 s; idle→poll at 90 s | three missed heartbeats is unambiguous silence |
| pool of staging objects on the agent | reaped at 10 min idle / disconnect | matches the mount lifetime, bounds agent disk |
| deadline: bind 5 s / op 30 s | see §7.2 | 30 s is AC-6's own number |
| `--on-conflict` | `refuse` | PRD `:319` |
| `--invalidation` | `auto` (`push` ⇒ push-only, fail loudly if the watcher is absent; `poll` ⇒ poll from the start) | `auto` is the useful default: prefer push, declare the downgrade |
| cache dir | `$XDG_CACHE_HOME/bunker/fs/<mount-id>/` | per-mount, per-user, 0700 |
| hash cache key on the agent | `(dev, ino, size, mtime)`; rehash when it moves | mtime may cost a hash, never decide a write |
| `snapshot` hashes | only for files ≤ 1 MiB; larger files report `hash: null` | keeps the snapshot in the 0.41 s class; a large file's hash is learned by GET on demand (D-5) |

---

## 10. Open decisions

Each with a recommendation and the reason. `D-1`–`D-4` are the ones the row's own investigation left open
(`BFS-003 §9`); `D-5`–`D-6` are created by this spec.

**D-1 — Is `FOPEN_DIRECT_IO` for every file the right read policy?**
→ **Recommend: yes, and measure the alternative in the same slice (V-9).** Reason: with direct I/O the
local byte cache is exactly one cache — ours, bounded, reported, hash-keyed, evictable — and the kernel's
mtime-driven invalidation cannot serve a byte we did not sanction. The measured cost of *not* having kernel
caching is already on the record: 32 READs per open, 96 total for three opens of a 4 MiB file, **no reuse
across opens** (M13). The alternative (`FOPEN_KEEP_CACHE` + `ExplicitDataCacheControl`, invalidating through
`InodeNotify` + `InodeNotifyStoreCache`, floor 7.15) is fully supported by the binding and is the losing arm
of V-9 if the numbers say so — at the cost of a second, unbounded, unreportable cache.

**D-2 — Event transport for the push channel: SSE on the mount's own connection, or a WebDAV
`REPORT`-style polling resource?**
→ **Recommend: SSE (`GET …/fs/events`, `text/event-stream`) over the same h2/h3 connection.** Reason: it is
the only shape here that is genuinely *push*, it inherits the connection's multiplexing (M10/M11: no second
setup cost), and its heartbeat gives the client an unambiguous liveness signal for §4.4's 90 s switch. It
is a **new** resource, and `BFS-004` owns its final spelling; a WebDAV-only client never touches it (the
`AC-12`/`BFS-012` old-client guarantee is unaffected — old clients poll or ignore).

**D-3 — The row's "33–36 s NFS walk" does not exist on the record.**
→ **Recommend: correct the row to the measured 32.74 s** (`PRD-bunker-fs.md:85`) and keep the comparison
there. Reason: §1.1 — a spec may not invent a range; the NFS column has one whole-tree number and it is
32.74 s. If the row's author had a different measurement, it needs a provenance line before it can be used.

**D-4 — mmap writers** (`BFS-003 §9.5`).
→ **Recommend: adopt the base hash at the first write-open or first WRITE op, and publish the whole file at
`Release`; verify on a real mmap workload whether the kernel can leave dirty pages unpublished at `Release`**
(check `V-4` extended with an `mmap` writer). Reason: staging makes a per-chunk precondition impossible
anyway (§5.4), so the mmap case collapses into the ordinary one; what remains genuinely unknown is the
kernel's flush timing, which is a measurement, not a design choice.

**D-5 — Snapshot hashing budget.**
→ **Recommend: hash only files ≤ 1 MiB in the `snapshot` response; report `hash: null` above that, and learn
the hash on demand by GET before a write.** Reason: the snapshot's value is that it is one cheap call in the
0.41 s class (M5); hashing an arbitrary tree's large files inside it would make the snapshot proportional to
bytes rather than entries, which is exactly the cost model this design refuses. The residual is measured in
V-6 — and if snapshots still run long, the threshold is the knob.

**D-6 — Kernel attribute TTLs of 0 cost one `Getattr`/`Lookup` per stat; is that acceptable?**
→ **Recommend: 0 (kernel never the authority), with the snapshot behind every answer.** Reason: each
Lookup/Getattr is answered in-process from the node tree (M21) — no network round trip — so the cost is
local CPU, while a nonzero TTL trades that away for *stale `stat` under an active build*, which is the one
thing that makes git skip a file it should re-read. If V-5's numbers show the local CPU is material, the
resolution is a per-node nonzero TTL for **directories and clean files under push mode only**, and never in
poll mode.

---

## Appendix A — every go-fuse anchor cited here, re-verified

Against `github.com/hanwen/go-fuse/v2@v2.11.0` (the version `BFS-003` measured, `BFS-003 Appendix A.1`).

| Anchor | What it is |
|---|---|
| `fs/api.go:285` / `:317` / `:322` | `NodeStatfser` / `NodeGetattrer` / `NodeSetattrer` |
| `fs/api.go:367` | `NodeOpener.Open` — where `FOPEN_DIRECT_IO` is returned |
| `fs/api.go:376` | `NodeReader.Read` — the read path our cache sits in |
| `fs/api.go:383` | `NodeWriter.Write` — every chunk visible to us |
| `fs/api.go:389` / `:398` / `:407` | `NodeFsyncer.Fsync` / `NodeFlusher.Flush` / `NodeReleaser.Release` — the publication points |
| `fs/api.go:517` / `:565` / `:571` | `NodeLookuper.Lookup` / `NodeReaddirer.Readdir` / `NodeMkdirer.Mkdir` |
| `fs/api.go:597` / `:604` / `:617` | `NodeCreater.Create` / `NodeUnlinker.Unlink` / `NodeRenamer.Rename` |
| `fs/api.go:765` / `:769` / `:774` / `:779` | embedded `fuse.MountOptions`; `EntryTimeout` / `AttrTimeout` / `NegativeTimeout` |
| `fuse/api.go:157` `:168` `:179` `:213` `:250` | `MountOptions`: struct / `MaxBackground` / `MaxInflightRequestBytes` / `MaxWrite` / `SingleThreaded` |
| `fuse/api.go:307` | `ExplicitDataCacheControl` |
| `fuse/types.go:252–255` | `FOPEN_DIRECT_IO` / `FOPEN_KEEP_CACHE` / `FOPEN_NONSEEKABLE` / `FOPEN_CACHE_DIR` |
| `fuse/types.go:262–266` | `OpenOut` (`Fh`, `OpenFlags`) |
| `fuse/types.go:293` | `CAP_WRITEBACK_CACHE` — defined, never accepted |
| `fuse/types.go:598–606` / `:634–639` | `EntryOut` (`NodeId`, `Generation`, timeouts) / `AttrOut` |
| `fuse/print.go:40` | the only other mention of `WRITEBACK_CACHE` (a printer) |
| `fuse/opcode.go:114–116` | the accepted capability mask — **no `CAP_WRITEBACK_CACHE`** |
| `fuse/opcode.go:126–133` | `ExplicitDataCacheControl` ⇒ `CAP_EXPLICIT_INVAL_DATA`, else `CAP_AUTO_INVAL_DATA` |
| `fuse/request_linux.go:8–12` | `7` / minimum minor `12` / our minor `28` |
| `fuse/server.go:44–45` | `Server` embeds `protocolServer` (why the notify methods are reachable) |
| `fuse/server.go:592` / `:625` / `:769` / `:790` | `InodeNotify` / `InodeNotifyStoreCache` / `DeleteNotify` / `EntryNotify` |
| `fuse/server.go:814–825` | `SupportsNotify` floors: 7.12 / 7.12 / 7.15 / 7.18 / 7.45 |
| `fs/bridge.go:255` | `setEntryOutTimeout` (per-reply override) |
| `fs/bridge.go:291–297` | `NewNodeFS` with nil `Options` ⇒ **1 second** entry and attribute timeouts (the default this spec replaces with 0) |
| `fs/bridge.go:1087` | `readDirMaybeLookup` (READDIRPLUS resolves through our `Lookup`) |

## Appendix B — repo facts cited

| Fact | Location |
|---|---|
| release matrix `linux/amd64 linux/arm64`, `CGO_ENABLED=0` | `Makefile:14`, `Makefile:95–105` |
| mount-driver registry seam (`Driver` type `:56`, `Register` `:112`, `Resolve` `:126`), driver identity on `proto MountSpec.driver` | `internal/mountdriver/mountdriver.go` |
| sshfs-shaped launch path this client replaces | `internal/cli/mount.go` (585 lines) |
| `X-Bunker-*` header precedent | `internal/audit/tls_unverified.go:43`, `internal/audit/ship.go:400` |
| house criterion 19 (bounded timeout, named cause, never a hang) | `docs/prd/PRD-bunker-remote-editing.md:276` |
| `AC-3`/`AC-4`/`AC-5`/`AC-6`/`AC-7`/`AC-9`/`AC-11` | `docs/prd/PRD-bunker-fs.md:128–136` |
| mount parameters (`--cache-max-size`, `--cache-max-age`, `--concurrency`, `--on-conflict`) | `docs/prd/PRD-bunker-fs.md:198–207` |
| server surface (`If-Match`, `ETag`, `X-Bunker-Hash`, `X-Bunker-Rev`, `X-Bunker-Op`) | `docs/prd/PRD-bunker-fs.md:211–218` |
| the 14-operation battery every number is comparable through | `probes/git-over-mount-probe.sh` |
| mount durability (three verdicts, no-success-without-ack, recovery may re-establish but never re-target) | `docs/prd/SPEC-sshfs-mount-durability.md` |

*No product code was changed by this row. Scratch go-fuse sources used to re-verify the anchors live
outside the repo; the only change in this row is this document.*
