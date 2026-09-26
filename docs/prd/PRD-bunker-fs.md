# PRD: `bunker-fs` — a bunker-native remote filesystem (WebDAV + HTTP/2/3 + our own FUSE client)

**Project:** bunker (owning) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Status:** proposed · **Effort:** L (five slices, two measurable gates before any build)
**Front door for:** the *filesystem* path. The editing/verb path has its own front door — see below.
**Inherits from:** `docs/prd/PRD-bunker-remote-editing.md` — the Path A/Path B split, the "mount gives edits, never execution" rule, the lease registry, the binding rules. Those are **not** restated here; they carry by reference.
**Supersedes:** the *Path B transport* only. Path B as a **delivery path** survives; what this document replaces is **SFTP/SSHFS as its transport**. `PRD-bunker-remote-editing.md` remains the front door for Path A (verbs) and for the delivery-path comparison; this document is the front door for the filesystem transport and adds a third path (Path C).

---

## The one-liner

A **bunker-native filesystem**: `bunkerd` serves a WebDAV surface over HTTP/2 (later HTTP/3), and our own FUSE client mounts it pipelined, cached, and hash-guarded — so whole-tree git operations stop being arithmetic at one round trip per file.

## The problem, measured

The existing filesystem path is **SSHFS over SFTP**, and its cost is not bandwidth — it is that SFTP has no request pipelining, so every metadata operation is one serialized round trip. Measured 2026-09-26 on this control host against two independent Hetzner datacenters:

| Fact | Value | How measured |
|---|---|---|
| RTT, control host → dedi-2 (Helsinki, public) | **185.24 ms** (mdev 0.16 ms) | `ping -c 20 95.216.12.55` |
| RTT, control host → bunker-mvp (Falkenstein, public) | **~198 ms** | `ping -c 20 78.46.173.180` |
| sshfs operations completing, dedi-2 | **5 of 14** | `probes/git-over-mount-probe.sh <mnt> --timeout 45` |
| sshfs operations completing, bunker-mvp | **5 of 14** | same probe, separate host |
| sshfs operations **stalling** at the 45 s timeout | **7 (dedi-2) / 8 (mvp)** | same |
| sshfs `rev-parse HEAD` | 4.52 s / 4.11 s | same |
| sshfs `log -1` | 10.32 s / 10.12 s | same |
| sshfs `log --oneline -20` | 7.93 s / 7.92 s | same |
| sshfs `ls-files` | 2.72 s / 2.41 s | same |
| sshfs `status --short`, `diff --stat`, `commit`, `checkout -b` | **STALL (rc=124)** on every run, both hosts | same |
| The same 14 operations with git running **on** the host | **14/14, 0.41 s flat, zero stalls** | `probe-native.sh dedi-2 /root/fs-fixture` |

**The cross-DC agreement is the finding.** Two different countries, different hardware, different datacenters — and the completed operations land within ~10% of each other (`rev-parse` 4.52 vs 4.11, `log -1` 10.32 vs 10.12, `log -20` 7.93 vs 7.92, `ls-files` 2.72 vs 2.41), stalling the same operations. This is not one bad host. **It is distance, and the operations that stall are exactly the ones that walk the tree.**

**And every existing mount alternative fails worse than sshfs:**

| Option | Result | Measured |
|---|---|---|
| sshfs (current default) | 7–8 of 14 operations stall | above |
| `rclone mount`, `--vfs-cache-mode off` | **WEDGE** — no result after **1 h 49 m** | zero rows; no git process alive; pipeline parked on an unflushed pipe |
| `rclone mount`, `--vfs-cache-mode full` (bounded 256 MB) | **WEDGE** — bounded at 900 s, **zero rows** | rclone log: `vfs cache: cleaned: objects 0` for 17 straight minutes — **the cache cached nothing** |
| `rclone mount` + `--sftp-concurrency 128` | **WEDGE** — bounded at 900 s, **zero rows** | same |
| NFSv4 (server on dedi-2, `nconnect=16`) | set up, not yet measured | — |

Two things follow, and both matter more than "sshfs is slow":

1. **rclone's cache never engaged.** Three variants, two cache modes, ~2.5 hours of wall clock, **zero cache objects and zero rows of data**. The wedge is not the mode I originally blamed — it reproduces identically with the cache off *and* on.
2. **The hang is worse than the stall.** sshfs returns `rc=124` — slow, but it *tells you*. rclone hangs on the first metadata read and never reports. A stall is a measurement you can act on; a hang is an unbounded wait with no signal, and an agent cannot recover from it.

**This fails an acceptance criterion the fleet already wrote.** `PRD-bunker-remote-editing.md` success criterion 19: *"Kill the SSH transport mid-edit … the failure surfaces as a bounded timeout with a named cause, **not** an indefinitely hanging tool."* We measured the indefinitely-hanging tool, three times, on the current path's most credible replacement. **Path B's transport is the thing that fails; Path C exists because of it.**

## The mechanism, proven

The entire design rests on one claim: **a multiplexed protocol makes per-operation cost stop scaling with latency.** So it was measured, not assumed, in a controlled A/B on the real link.

A single probe binary (`h2probe`) serves the same 4 KiB object two ways. The client is identical in both cases and **always sets `MaxConnsPerHost=1`** — one connection, forced. With a single connection, high concurrency is only possible if the protocol multiplexes. So the *only* variable is the protocol.

| Case | Negotiated | 100 sequential | **100 concurrent** | Multiplexing gain |
|---|---|---|---|---|
| **A** — HTTP/1.1, plain TCP, 1 conn | `HTTP/1.1` | 38.37 s (383.72 ms/req) | **38.47 s** (384.67 ms/req) | **1.0×** |
| **B** — HTTP/2 over TLS, 1 conn | `HTTP/2.0` | 19.66 s (196.60 ms/req) | **0.79 s** (7.88 ms/req) | **25.0×** |

Same host, same 185 ms link, same client, same one-connection constraint.

- **HTTP/1.1 buys nothing from concurrency.** 100 concurrent requests take exactly as long as 100 sequential ones — 1.0×. One connection serializes; that is the wall sshfs also hits, expressed as a protocol.
- **HTTP/2 drops effective per-request cost from 384.67 ms to 7.88 ms** — **2.4% of one round trip**, a **49× per-request improvement**, and 100 concurrent requests complete in **0.79 s**.

**Why this is specifically the fix for a filesystem, and not a general "HTTP is fast" observation.** `git status` and `git diff` over a tree issue *many independent* metadata calls — the calls have no ordering dependency on each other. A serialized transport pays **N × RTT** for them; a multiplexed one pays **~1 RTT**. That is precisely the gap between the measured 45 s stalls and the native 0.41 s. And it is precisely the lever sshfs *cannot* pull: upstream libfuse/sshfs #300 attributes sshfs's high-latency penalty to the absence of SFTP pipelining, with OpenSSH capping packets at 256 KiB, which disables TCP window scaling.

**Stated honestly:** this proves the *transport* mechanism. It does not yet prove a FUSE client that exploits it — that is what AC-1 and AC-2 below are for, and they are the first work items.

## User stories

1. **As a Hermes session** editing code on a remote bunker, I run `git status` / `git diff` through the mount and get an answer in **seconds, not a 45-second stall**, so I can use the same inspection commands remotely that I use locally.
2. **As a Hermes session**, when another writer changed a file I am about to overwrite, I get a **refusal naming the current hash** rather than silently losing their work — so a lost update is impossible by default rather than detected later.
3. **As the owner**, I get a filesystem whose **local disk cost is bounded and reported**, so growing projects does not mean growing my local storage — the constraint that ruled out full-copy sync tools.
4. **As an agent on a tick** (the machine customer), I can mount a tree, read and patch files with POSIX tools that already work, and know that a **build still never runs on my box** — the mount is edits, never execution.
5. **As the operator**, I can see and change the protocol in use (`h2`, later `h3`) and the cache bound per mount, and a stalled or failed transport surfaces as a **named error within a bounded time**, never an indefinite hang.

## Acceptance criteria

`AC-n: Given <state>, when <action>, then <observable result> — proof: <command / test / artifact / number>. (T|C|M)`

- **AC-1** Given a `bunker-fs` mount of Fixture A (150 files / 6 dirs) over a ≥180 ms link, when the full 14-operation battery runs, then **0 operations stall** and `status --short`, `diff --stat`, `commit` and `checkout -b` all **complete** — proof: `probes/git-over-mount-probe.sh <mnt> --timeout 45` reports `STALLS (0)`. Baseline to beat, measured: **7 (dedi-2) / 8 (mvp)** stalls. **(T)**
- **AC-2** Given the same mount, when the battery runs, then total wall time for the 14 operations is **≤ 15 s** — proof: the probe's CSV summed; `awk -F, 'NR>1{s+=$2}END{print s}'`. Baseline measured: `rclone` produced **no rows at all**; sshfs's completing 5 ops alone take ≈ 25.6 s (dedi-2). **(T)**
- **AC-3** Given a file written by writer B after reader A cached it, when A writes with the **stale hash**, then the write is **refused** with a message naming the current hash, and the file's content is **unchanged** — proof: content sha256 before/after is identical, refusal message contains both hashes; the write is rejected, not merged. **(T)**
- **AC-4** Given an agent-side edit (a process on the agent appends to a mounted file), when the invalidation path fires, then the client's next read returns the **new** content within **≤ 2 s**, without a remount and without a manual cache clear — proof: timestamped read before/after, sha256 matches the agent's file. **(T)**
- **AC-5** Given a mount with `--cache-max-size 256M`, when a tree larger than the bound is read, then local cache disk usage **never exceeds 256 MB** and is **reported** by `bunker fs status` — proof: `du -sh` of the cache dir sampled during the run, plus the reported figure; both ≤ the bound. **(T)**
- **AC-6** Given a mount, when the transport is killed mid-operation (drop the server listener or the network), then every in-flight operation returns a **named error within ≤ 30 s**, no partial file is left behind, and the mountpoint is recoverable with **one command** — proof: the failure text names the cause; `ls` on the mountpoint after does not hang; recovery command documented and executed once. Replays `PRD-bunker-remote-editing.md` criterion 19, which the current path **fails**. **(T)**
- **AC-7** Given an agent that has been destroyed and re-created under the same name, when a mount bound to the old instance performs a read, then it reports **STALE** and refuses rather than serving the new empty tree — proof: tree-identity probe value differs; the refusal names both identities. **(T)**
- **AC-8** Given N agents (mount one, destroy another, mount a third), when the offered mount surface is enumerated, then the **tool count and names are byte-identical** before and after; the target is an **argument**, never a global — proof: diff the mounted-target list plus `bunker fs --help` before and after; `git diff` empty. **(T)**
- **AC-9** Given a capability the target lacks (an agent whose build has no inotify watcher), when the mount requests invalidation, then the mount **still mounts** and reports a structured `capability_unavailable` naming the missing piece, degrading to **poll-based** invalidation with the mode visible in `bunker fs status` — proof: mount succeeds; `bunker fs status --json` names the degradation. **(T)**
- **AC-10** Given the battery results, when the local control host's CPU time is measured across a full remote edit-build-test cycle, then the **build's CPU is not on the local box** and the mount contributes only its own I/O — proof: `/proc/<pid>/stat` utime+stime for the local side, plus proof the build ran on the agent. Extends the house PRD's criterion 9 (offload). **(T)**
- **AC-11** **(anti-gaming)** Given a mount that claims pipelining, when the concurrency is **forced to 1** (`MaxConnsPerHost=1` equivalent / `--max-concurrent-requests 1`), then the battery **degrades measurably** toward the serialized baseline — proof: the same battery at concurrency 1 vs N differs by ≥2× on the whole-tree operations. A green result that survives concurrency=1 proves nothing was actually pipelined. **(T)**
- **AC-12** Given the shipped client, when the same battery is re-run with the **exact invocation the design documents**, then the published numbers reproduce within 15% — proof: re-run command printed verbatim in the results file, before/after column. A number whose command is not quoted is unverifiable by construction. **(T)**

## The design

### Shape

```
  CONTROL HOST                                          BUNKER (agent)
  ┌───────────────────────────┐                         ┌────────────────────────────┐
  │  local tools / git / IDE  │                         │   the tree (.git etc.)      │
  │            │              │                         │            ▲               │
  │      POSIX VFS            │                         │       inotify watcher      │
  │            │              │                         │            │               │
  │   bunker-fs FUSE client   │  HTTP/2 (later HTTP/3)  │     bunkerd WebDAV layer    │
  │   ├ pipeline/coalesce  ───┼──── multiplexed ────────┼──▶  ├ WebDAV verbs          │
  │   ├ bounded LRU cache     │  streams, one conn      │     ├ X-Bunker-Hash/Rev    │
  │   └ hash-checked writes   │◀── invalidations ───────┼────  └ batch/op delegation  │
  └───────────────────────────┘                         └────────────────────────────┘
```

Three components, and the third is what makes it more than "a better sshfs":

1. **Server: WebDAV in `bunkerd`.** Standard verbs (`GET/PUT/DELETE/MKCOL/PROPFIND/LOCK`) so existing clients work, plus bunker extension headers. This is the piece you proposed building directly into bunker, and it is the right call: we need to add semantics a generic WebDAV server cannot provide.
2. **Client: our own FUSE mount.** One binary, target is an argument. Its whole reason to exist is **pipelining** — issuing independent metadata calls as concurrent HTTP/2 streams — plus a bounded cache and hash-checked writes.
3. **Delegation (the extension that beats every transparent proxy).** Because we own both ends, whole-tree operations whose *results* are small can be computed **on the agent** and returned in one call: `status`, `diff --stat`, `rev-parse`, `ls-files` become server-side operations rather than N file reads. This is what the measured 0.41 s native path already demonstrates — the difference is it happens *behind a filesystem interface*, so unchanged local tools benefit.

### State model — three states, two dangerous edges

| State | What it is | Where it lives |
|---|---|---|
| **DECLARED** | what the session believes the tree contains | session |
| **CACHED** | what the client holds locally | control host cache dir |
| **ACTUAL** | what is on the agent's disk right now | the agent |

- **Edge ACTUAL → CACHED** (the cache is stale): another process edited the tree. **Closed by inotify-driven invalidation** on the agent, with poll-based fallback on agents lacking the watcher (AC-9).
- **Edge CACHED → ACTUAL** (a write lands on a tree that moved): the agent was destroyed/re-created, or another session wrote the same file first. **Closed by tree-identity probe at bind** (AC-7) and **hash-checked writes** (AC-3).

A filesystem that reads only CACHED and calls it truth is the narrow shape this document exists to avoid.

### Taxonomy — the staleness/conflict classes, one real case each

| Class | Real case | Mechanism |
|---|---|---|
| **Foreign edit, content changed** | a build on the agent writes `dist/` while a session has `src/` cached | inotify → targeted invalidation |
| **Concurrent writer, same file** | two sessions patch one file; the second has a stale hash | hash-checked write **refuses**, names the current hash (AC-3) |
| **Tree identity changed** | agent destroyed and re-created under the same name | bind-time identity probe → STALE (AC-7) |
| **Cache eviction** | tree larger than the cache bound | bounded LRU; a miss re-fetches; bound is **reported** (AC-5) |
| **Touch-without-change** | a tool rewrites a file with identical bytes | **content hash decides, never mtime** — mtime-based invalidation would invalidate constantly under a build |
| **Capability absent** | agent image has no inotify watcher | `capability_unavailable` + visible poll fallback (AC-9) |

### The loops, each with its closure

1. **Invalidation loop.** agent inotify → event → client drops the entry → next read re-fetches → **CLOSED when the client's hash equals the agent's hash** for that path.
2. **Conflict loop.** write carries `expected-hash` → server compares → mismatch → **refuse naming the current hash** → caller re-reads, merges, retries → **CLOSED when a write lands with a matching hash.** Recurrence escalates to the lease registry (lease before write) so the class stops recurring rather than nagging.
3. **Identity loop.** bind → probe tree UUID → match → proceed | mismatch → **refuse + require re-bind** → **CLOSED when the UUID matches.** Never auto-adopt a fresh tree.
4. **Cache-bound loop.** sample cache size → evict LRU → **CLOSED when size ≤ bound**, and the figure is reported so the bound is a fact, not a claim.

### Interface and parameters

**Mount:**

| Parameter | Meaning | Default |
|---|---|---|
| `target` | `agent[:path]` — **always an argument**, never a global | *required* |
| `mountpoint` | local path, created 0700 | *required* |
| `--protocol` | `h2` \| `h3` (h3 gated on AC-13) | `h2` |
| `--cache-max-size` | hard local bound, evicting | `256M` |
| `--cache-max-age` | TTL for entries with no invalidation channel | `1h` |
| `--mode` | `ro` \| `rw` | `rw` |
| `--concurrency` | in-flight streams per connection | `64` |
| `--on-conflict` | `refuse` \| `overwrite-if-unchanged` | `refuse` |

**Server surface (WebDAV verbs + extensions):**

| Verb | Extension headers | Notes |
|---|---|---|
| `GET` / `HEAD` | `ETag`, `X-Bunker-Hash` | hash is content-addressed, not mtime |
| `PUT` | `If-Match: <hash>` | **mismatch ⇒ 412 refused**, names the current hash |
| `DELETE` / `MKCOL` | — | — |
| `PROPFIND` | `X-Bunker-Rev` | directory listing **with attributes** (the readdirplus case) |
| `LOCK` / `UNLOCK` | — | backs onto the **existing lease registry** in the tree — not a second lock system |
| `POST` (bunker) | `X-Bunker-Op: status\|diff\|rev-parse\|ls-files` | **delegated whole-tree ops**, computed on the agent |

**Cardinality rule (hard constraint).** The tool count is fixed by the verb count; **the target is always an argument.** Mounting 20 agents must not create 20 tools. A per-instance design means N × M registrations and a surface that mutates every time an agent spawns — explicitly rejected.

**Capability variance is an ERROR, not a dynamic surface.** An agent missing the inotify watcher or on an older build keeps the full surface; the call returns a structured `capability_unavailable` naming what is absent and the mode in force. A silently missing capability is strictly worse — the caller cannot distinguish "not supported" from "not loaded" from "forgotten."

**Session-scoped mutability is a separate axis.** The catalog is constant; which verbs *this mount* exposes (`ro` vs `rw`, delegation on/off) is **per-mount state**, never a global config write. A mount's mode change must not alter any other mount's behaviour.

### Your mechanisms, as design authority

Four of these are your calls, and they are specified rather than paraphrased:

1. **WebDAV as the surface.** Adopted, and for the reason you gave — it is a protocol with enough surface area to build on, with clients that already exist (davfs2, rclone, Finder, Windows) and a place to hang extension headers. Interop is free; the extensions are where the speed comes from.
2. **HTTP/2 now; HTTP/3/QUIC as a measured follow-on.** HTTP/2 is **proven here** (25.0×, 7.88 ms effective). HTTP/3 adds 0-RTT setup and removes TCP-level head-of-line blocking — genuinely attractive on a *lossy* path. **Two honest caveats:** Go has **no stdlib HTTP/3** (it needs `quic-go`, a new dependency; `go.mod` today carries no QUIC at all), and QUIC's benefit on a 185 ms path with mdev **0.16 ms** is `AC-13` — a measurement, not an assumption. Your instinct that HTTP-family protocols are latency-tolerant is confirmed; which HTTP version is not yet.
3. **inotify-driven invalidation.** Adopted as the primary invalidation channel (AC-4), with poll as the declared fallback.
4. **Hash-based write rejection.** Adopted as the conflict rule (AC-3). This is the mechanism that turns a silent lost update into a loud refusal, and it maps onto the lease registry the house PRD already specifies.

**One refinement on hashes, because it changes behaviour:** compare **content hashes, never mtimes**. Under an active build the tree is touched constantly with unchanged bytes; an mtime-keyed check would refuse writes that are actually safe, and the first thing anyone does with a nagging check is disable it.

## What already exists vs what is missing (audited 2026-09-26)

| Capability | State | Evidence | What it means |
|---|---|---|---|
| `bunkerd` HTTP REST on `:18080` | **HTTP/1.1 only** | `curl` → `http_version=1.1`; `--http2-prior-knowledge` → code `000`; `/healthz` → 200 | **The existing REST listener cannot multiplex.** A WebDAV surface needs h2 (TLS or h2c) as new work. |
| gRPC listener on `:19090` | **HTTP/2 present** | gRPC mandates h2; listener serves | An h2 stack is already in the process — the substrate is not entirely new. |
| Go toolchain | **1.26.5** | `go version` | Modern; h2 via stdlib with TLS, no new dep. |
| `quic-go` / HTTP/3 | **MISSING** | `go.mod`: only `golang.org/x/net` (indirect); no QUIC anywhere | HTTP/3 is a new dependency and its own slice (AC-13). |
| WebDAV server | **MISSING** | repo grep: zero `webdav` hits in non-test code | New work — the core of the server slice. |
| FUSE client | **present** (sshfs 3.7.6) | `/usr/local/bin/sshfs`, FUSE 3.18.2, `/dev/fuse` | The mount *mechanism* exists; we replace the client, not the OS support. |
| Agent-side inotify watcher | **MISSING** | no inotify code in the repo | New work; AC-9 covers agents that lack it. |
| Lease registry in the tree | **exists** | house PRD; `toolsd` `<git-common-dir>/agent-leases.json`, CHT-007 live probe (6 procs → 1 grant / 5 refusals) | **Reuse — do not build a second lock system.** `LOCK`/`UNLOCK` backs onto this. |

**Resized scope in one line:** the transport substrate and the mount mechanism exist; the build is **a WebDAV layer + a pipelining FUSE client + an invalidation channel**, reusing the lease registry rather than rebuilding it.

## Scope

**In scope:** WebDAV surface in `bunkerd`; h2 transport with pipelining; the FUSE client with bounded cache; inotify invalidation + poll fallback; hash-checked writes; delegated whole-tree operations; lease-backed `LOCK`; capability reporting; the measurement harness.

**Out of scope, with reasons:**

- **HTTP/3 as a v1 gate.** Proven-in-principle but unmeasured here, and it costs a new dependency. It is `AC-13`, gated on the HTTP/2 path landing.
- **Full local mirroring / sync tools (Mutagen-style).** Ruled out by the standing storage constraint: a mirror's local cost grows with the tree, including the mass nobody reads. This design caches what is touched and bounds it (AC-5).
- **Execution through the mount.** Unchanged from the house PRD: **the mount gives file-level edits, never execution.** Builds run on the agent.
- **A second lock manager.** Leases stay in the tree, keyed by tree, never by mount or agent.
- **Windows/macOS clients as a v1 requirement.** WebDAV keeps the door open; v1 targets Linux FUSE.

## Success criteria (replayable proofs)

1. **Battery replay.** Run the same 14-operation battery used for the baseline through the new mount on both dedi-2 and bunker-mvp. → **0 stalls on both**; before/after published side by side against the measured 7/8.
2. **Multiplexing replay.** Re-run the `h2probe` A/B on the shipped transport. → HTTP/2 concurrent remains ≥10× the HTTP/1.1 control at `MaxConnsPerHost=1`; the 25.0× figure is re-derived, not inherited.
3. **Delegation replay.** `git status --short` on a 150-file tree through the mount with delegation on vs off. → the delegated path is measurably faster and returns byte-identical output.
4. **Conflict replay.** Two writers, one stale hash. → refusal naming the current hash; the file's bytes are unchanged (AC-3).
5. **Invalidation replay.** An agent-side append is visible in the client within 2 s with no remount (AC-4).
6. **Bound replay.** A tree larger than `--cache-max-size`. → local cache never exceeds the bound, and the reported figure agrees with `du` (AC-5).
7. **Transport-kill replay.** Kill the listener mid-operation. → named error within 30 s, no partial file, one-command recovery. **This is the criterion the current path fails** — the replay is the point.
8. **Constant-surface replay.** Mount one agent, destroy another, mount a third. → identical mount surface; the target stayed an argument (AC-8).
9. **Anti-gaming replay.** The battery at `--concurrency 1`. → it must degrade toward the serialized baseline. A pass that survives concurrency=1 is a false pass (AC-11).

## Risks

| Risk | Mitigation (design rule) |
|---|---|
| We build a FUSE client and it is **slower** than native delegation | Delegation lands **in the same slice** as the client, not after it; if the client cannot beat "compute it on the agent", say so and ship the delegation path alone |
| The cache silently grows on the owner's box | Hard `--cache-max-size` with eviction, and the figure is **reported** — a bound that is not visible is not a bound |
| inotify **misses** events (overflow, unmounted, kernel limits) | Watcher reports overflow explicitly; the client falls back to poll and **says which mode is in force** |
| Hash checks refuse safe writes under an active build | Compare **content hashes, not mtimes**; a refusal names both hashes |
| A "default mount" creeps in and replaces sshfs unasked | New drivers are **opt-in**; sshfs stays the default until the battery shows 0 stalls on two DCs, and the switch is an owner decision |
| h2 over a middlebox that breaks it | `--protocol` is explicit and reports what was negotiated; a downgrade is a visible error, never silent |
| Scope creep into HTTP/3 before v1 works | AC-13 gates h3 on the h2 path landing |

## Cost & sequencing

| Slice | Effort | Ships | Depends | Gate |
|---|---|---|---|---|
| **C0** re-run the harness on the shipped invocation; publish the baseline file | **S** | the number everything is graded against | — | — |
| **C1** h2/h2c WebDAV surface in `bunkerd` (`GET`/`HEAD`/`PROPFIND` first, read-only) | **M** | server half; provable with `curl --http2` | C0 | AC-8, AC-12 |
| **C2** pipelining FUSE client, read-only mount | **M** | **the core bet** — AC-1, AC-2, AC-11 | C1 | **0 stalls** |
| **C3** bounded cache + inotify invalidation + poll fallback | **M** | AC-4, AC-5, AC-9 | C2 | cache bound held |
| **C4** hash-checked writes + `LOCK` onto the lease registry | **M** | AC-3, AC-6, AC-7 | C3 | refusal proven |
| **C5** delegated whole-tree ops (`X-Bunker-Op`) | **M** | AC-10 + success 3 | C2 | delegated ≥ proxy |
| **C6** HTTP/3 via `quic-go` | **M** | AC-13, only if h3 wins on this path | C5 | measured, not assumed |

**C2 is the bet.** If a pipelining FUSE client does not beat the serialized baseline on the same host, the design is wrong and C5 (delegation, which is *not* a filesystem) is the fallback. That is why C0 and C1 come first and why AC-11 exists.

**Repo home:** `deployBunker/bunker` — `internal/fs/` for the client and `internal/server/webdav/` for the surface; the harness belongs in `probes/` beside the existing probe.

## What it is NOT

- **Not a replacement for the verb layer.** `bunker_*` verbs stay the path for explicitly-targeted, attributed, refuse-on-unbound writes. A mount cannot enforce per-session binding; Path A is where that guarantee lives.
- **Not an execution path.** Builds, tests and installs run on the agent. Always.
- **Not a synchronizer.** No mirror, no full copy, no growing local footprint.
- **Not a new lock manager.** Leases stay in the tree.
- **Not a credentials store.** The mount uses the agent's own access; per-session credential partitioning stays where the house PRD put it.
- **Not a general-purpose WebDAV server.** The WebDAV surface exists to serve this filesystem; it is not offered as a standalone file-sharing product.

## Open questions — the owner's decisions, with my recommendation

1. **WebDAV-compatible, or our own protocol with WebDAV as an adapter?** → **Recommend: WebDAV core + bunker extension headers.** Interop costs nothing and the extensions are where the speed is. Revisit only if extension headers prove insufficient.
2. **HTTP/3 now or after HTTP/2 proves out?** → **Recommend: HTTP/2 first.** It is proven here at 25.0×; HTTP/3 is unmeasured on this path (mdev is 0.16 ms — this link is *not* lossy, which is exactly where QUIC's advantage is smallest) and costs a new dependency. Make h3 an owner call *after* AC-13's measurement, not a v1 assumption.
3. **Does `bunker-fs` replace sshfs as the default edit transport?** → **Recommend: no default change** until the battery shows 0 stalls on both DCs. New drivers opt-in; sshfs stays default until a measured win exists.
4. **Default cache bound?** → **Recommend: 256 MB, reported.** A bound the owner cannot see is not a bound. Per-agent override for big trees.
5. **Conflict default: refuse or last-write-wins?** → **Recommend: refuse**, with an explicit `--on-conflict` override. A silent overwrite is a lost update; a refusal is a recoverable event.
6. **Working name and repo home.** → `bunker-fs` is a working name. `internal/fs/` + `internal/server/webdav/` in `deployBunker/bunker` unless you want it separate — your call.

## AC-13 (blocked)

**AC-13** Given the working h2 mount, when the same battery runs over HTTP/3, then h3 is **kept only if** it beats h2 on wall time on this path — proof: same battery, both protocols, side-by-side numbers. **BLOCKED-ON:** C6 (needs `quic-go` vendored and the h2 path landed). Filed as a dependency, not deleted.
