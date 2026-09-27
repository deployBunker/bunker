# SPEC — maximum Linux I/O throughput through FUSE, and the configurable surface

Status: **design, for Bane · authored 2026-09-27**
Measured on this host: kernel **7.0.0-31-generic**, go-fuse **v2.11.0**, client `bunker-fs`.
Rows: BFS-052 (capability negotiation) · BFS-053 (the config surface) · BFS-054 (kernel cache decision)
· BFS-055 (transport tuning) · BFS-056 (passthrough spike) · BFS-057 (the measurement harness)
· BFS-058 (io_uring spike)

---

## 0. The one-screen answer

The wins are not one thing. They fall into **four layers**, and two of them are *currently switched off
on purpose* for correctness — so the honest plan is not "turn everything on", it is **make each lever
reachable, measurable and reversible**, and decide the two correctness trades explicitly instead of by
default.

| Layer | Biggest available win | Status today |
|---|---|---|
| **1. Kernel↔userspace (FUSE)** | **FUSE passthrough** — the kernel reads/writes the backing file with **userspace out of the path entirely** | kernel 7.0 has it; go-fuse v2.11 does **not** expose it |
| **2. The kernel's own caches** | **readdirplus + attr/entry timeouts** — kill the stat storm on walk/ls/find/git | **all zero today, deliberately** |
| **3. The transport (TCP/QUIC)** | **BBR + buffer sizing** — the send-buffer ceiling is a hard arithmetic cap on the WAN path | cubic, installed BBR unused, `tcp_wmem` max **4 MiB** |
| **4. The client's own scheduler** | concurrency + the snapshot + range pipelining | concurrency and snapshot **already landed**; pipelining not |

## 1. What is measured false, right now

These are facts from this host, not generalities.

**1.1 The send-buffer ceiling is arithmetic, not speculation.** `net.ipv4.tcp_wmem = 4096 16384
4194304`. The third number is the ceiling TCP auto-tuning may grow the **send** buffer to: **4 MiB**. On
the Helsinki path at **192 ms RTT**, a single connection therefore cannot exceed roughly
`4 MiB / 0.192 s ≈ 21.8 MB/s ≈ 175 Mbps` **no matter how much bandwidth the link has**, because a full
BDP must be outstanding to fill the pipe. *(This is arithmetic on a measured RTT and a measured sysctl —
the throughput claim itself is NOT yet measured, and BFS-057 is what would measure it. Stated that way
deliberately.)* **This matters most for HTTP/2**, because h2 multiplexes every stream onto **one** TCP
connection: the client's 25 concurrent requests share one 4 MiB send buffer. h3/QUIC is bounded by
`net.core.wmem_max` (also 4 MiB) for its UDP socket, plus its own flow-control windows.

**1.2 BBR is installed but unused.** `/lib/modules/7.0.0-31-generic/kernel/net/ipv4/tcp_bbr.ko.zst`
exists; the running algorithm is **cubic**, and `tcp_available_congestion_control` lists only `reno
cubic`. On a 192 ms path with real loss and bufferbloat (measured earlier: N100 idle 5.8/7.9/14.2 ms →
loaded 27.3/55.1/78.0 ms), CUBIC's loss-based back-off is exactly the wrong strategy. `tcp_bic`,
`tcp_cdg`, `tcp_dctcp` are also present.

**1.3 Two of our own levers are off by design.** From `internal/fsmount/fs_linux.go`:
- **`ExplicitDataCacheControl: true`** — "the kernel keeps no file data; our cache is the only one";
- **`AttrTimeout`/`EntryTimeout`/`NegativeTimeout` all zero** — every lookup and stat reaches userspace.

Both are **correctness decisions, not oversights**, and they are the reason the client is slow on
metadata-heavy work (a walk, `ls -l`, `find`, `git status`) while being fast on whole-tree reads (the
snapshot). **They are also a third cache we do not currently reason about** — see §3.

**1.4 FUSE has a kernel-side io_uring transport, disabled.** `/sys/module/fuse/parameters/enable_uring
= N`, described as *"Enable userspace communication through io-uring"*. Note carefully: **flipping this
does nothing on its own** — the daemon must also speak io_uring, and go-fuse v2.11 does not. It is a
lever that requires a client-side implementation, not a sysctl.

**1.5 Concurrency is negotiated, and we already tuned it.** `max_background = max(32, concurrency*2)`,
`MaxInflightRequestBytes = max(32, concurrency*2) * 2 MiB`, `MaxWrite = 1 MiB`. Live connection table
shows other mounts on this box at 12/9, 32/24, 50/37 — so these are real, per-mount negotiated values.

## 2. The reachable lever set (what go-fuse v2.11 actually exposes)

**Everything below is a field we can set without forking** (`fuse.MountOptions`, module cache, verified):

| Lever | Default today | What it buys |
|---|---|---|
| `MaxReadAhead` | unset (~128 KiB) | **bigger kernel readahead** → fewer, larger reads on sequential access |
| `CongestionThreshold` | unset | how much async writeback before the kernel throttles writers |
| `RememberInodes` | unset | keeps the inode tree, fewer FORGET-driven lookups |
| `EnableLocks` | **unset** | kernel-side flock/POSIX locks → no round trip per lock |
| `EnableSymlinkCaching` | **unset** | caches symlink targets (also the honest half of **BFS-018**) |
| `DisableReadDirPlus` | unset | **verify whether readdirplus is actually in use** — if it is, the kernel gets attrs with the listing instead of a stat per entry |
| `DisableSplice` | unset → **splice is ON** | zero-copy pipe transport (and the reason BFS-025's splice bug existed) |
| `SyncRead` | false | leave false; async reads are what allow concurrency |
| `EnableAcl`, `IgnoreSecurityLabels`, `IDMappedMount` | off | ACL handling; **ID-mapped mounts are directly relevant to the `bunker-<agent>` container topology** |
| `DirectMount` / `DirectMountStrict` | off | mount without the fusermount3 helper (needs privilege) |
| `Options []string` | — | **arbitrary mount-option passthrough** — the escape hatch for anything not given a field (e.g. `max_readahead=`, `writeback_cache`, `max_pages=`) |
| `MaxStackDepth`, `SingleThreaded`, `Debug` | — | not perf levers; `SingleThreaded` is a pessimisation, `Debug` worse |

**Not reachable without a fork or raw `/dev/fuse`:** FUSE passthrough, io_uring transport.

## 3. The decision I will not make silently: the kernel's caches are a THIRD cache

Right now we have **two** caches: the client's user-space cache (256 MiB, LRU-by-hit) and the server's
derived tree view. The kernel maintains **two more that we have switched off**:

- **data page cache** — off via `ExplicitDataCacheControl` (no auto-inval) and, per the code comment,
  by not keeping file data;
- **dentry/inode (metadata) cache** — off via zero timeouts.

Turning the metadata cache on is the **single cheapest large win** for `ls -l`/`find`/`git status` —
and it **directly contradicts the invalidation work now in flight**, because it creates a cache whose
staleness our channel (BFS-035/036) does not currently reach: the kernel would serve a stale `stat` or
dentry *before* userspace is ever consulted. So this is not a tuning knob, it is a **correctness
decision**, and it must be made with BFS-045 (observability) and the invalidation specs in view, not
before them. **My recommendation: NOT until the channel lands and can invalidate kernel-side
attributes — otherwise we add a silent-stale path of exactly the kind this project keeps finding.**

If we do turn it on, it must be **opt-in, per-mount, with the window reported** — same law as
everything else: a bound the owner cannot see is not a bound.

## 4. The levers, ranked by expected payoff per unit of risk

**Tier 1 — cheap, safe, reversible, measurable now**

1. **Raise `MaxReadAhead`** (e.g. 128 KiB → 1 MiB) and measure sequential read throughput. Pure win on
   large reads; no correctness exposure (the kernel is only reading *more* of the same file per request).
2. **`net.ipv4.tcp_wmem` / `net.core.wmem_max` → 16–32 MiB.** Lifts the arithmetic ceiling in §1.1.
   Trivial, reversible, and the whole point of "make buffers configurable".
3. **`tcp_notsent_lowat` → 64–256 KiB.** Directly attacks the measured bufferbloat (27.3/55.1/78.0 ms
   loaded), i.e. **interactive latency while a bulk transfer runs** — which matters more than peak
   throughput for a working tree.
4. **`EnableLocks`**, **`RememberInodes`**, **`EnableSymlinkCaching`** — each removes round trips; the
   last also improves the symlink story.
5. **Verify readdirplus is on**, and if not, turn it on: it is the difference between one request per
   directory and one request per entry in a walk.

**Tier 2 — a real upgrade, needs a decision**

6. **`modprobe tcp_bbr` + `tcp_congestion_control=bbr`.** Biggest WAN win available with no code
   change. Risks: it is a kernel-level change to a **shared** box (it affects other traffic), and BBR
   needs pacing (`fq` rather than `fq_codel` — currently fq_codel).
7. **h2/h3 flow-control windows and QUIC buffer sizing** (Go `http2.Transport` initial stream/conn
   windows; quic-go receive windows + `SO_RCVBUF`). These are currently defaults and are almost
   certainly too small for a 192 ms path.
8. **Range-pipelined large reads** — issue N parallel HTTP range GETs for one large file read and
   assemble. This is a **client-side** win the kernel cannot give us, and it composes with the
   concurrency lever that already measured 7.74×.

**Tier 3 — high ceiling, real work, needs a spike first**

9. **FUSE passthrough (BFS-056).** The kernel redirects I/O to a backing fd, **bypassing userspace
   entirely**. For a cache hit, the backing fd is our local blob — so this is potentially the
   difference between "fast for a network filesystem" and "near-native". Requires raw `/dev/fuse`
   ioctls (go-fuse v2.11 has no API), and the correctness question is sharp: **the kernel serves those
   reads without consulting us**, so invalidation must reach the kernel's view. Spike it, measure it,
   then decide.
10. **io_uring FUSE transport (BFS-058).** Removes per-request syscall overhead. Requires client-side
    io_uring support; go-fuse does not have it. Lower ceiling than passthrough, less invasive.

## 5. What FUSE cannot do — stated so we stop looking for it

- **There is no way to avoid a kernel↔userspace round trip per operation**, except passthrough (Tier 3)
  and except by making the *requests larger and fewer* — which is what `max_readahead`, `MaxWrite` and
  readdirplus do, and what our one-call **snapshot** does at the protocol level.
- **No DAX**, no direct-mapping of remote storage.
- **You cannot `mmap` efficiently through FUSE** in the way a local fs can; writes via mmap are
  expensive regardless of tuning. (Worth measuring rather than assuming — BFS-057.)
- **A single syscall cannot serve a whole tree.** That is precisely why the snapshot op exists, and it
  is the largest single win we have — **232×** on loopback, and the only thing that short-circuits a
  whole-tree walk (NFS, kernel-parallel, still stalls 33–36 s).
- **Kernel capability support is version-dependent and must be probed, never assumed.** A requested cap
  the kernel lacks fails or silently no-ops. Same doctrine as the watcher contract: **probe, report the
  refusal with a reason, degrade honestly.**

## 6. The configurable surface (Bane's explicit ask)

Three places, one schema, all reported:

- **Mount flags** (`bunker fs mount`): `--fuse-max-readahead`, `--fuse-max-write`, `--fuse-max-background`,
  `--fuse-congestion-threshold`, `--fuse-attr-timeout`, `--fuse-entry-timeout`, `--fuse-negative-timeout`,
  `--fuse-locks`, `--fuse-symlink-cache`, `--fuse-readdirplus`, `--fuse-remember-inodes`,
  `--fuse-splice`, `--fuse-direct-mount`, `--fuse-idmap`, plus `--fuse-option=KEY=VAL` passthrough.
- **Transport**: `--conn-max-idle`, `--conn-max-per-host`, `--http2-stream-window`, `--http2-conn-window`,
  `--h3-stream-window`, `--h3-conn-window`, `--udp-buffer`, `--prefer-protocol=auto|h2|h3`,
  `--tcp-notsent-lowat-hint`.
- **Sizing**: `--cache-max-bytes`, `--cache-max-entry-bytes`, `--cache-max-entries`, `--concurrency`,
  `--readahead-blocks`, `--range-pipeline-depth`.

Rules that make this safe rather than a wall of knobs: **every knob has a documented default and range**;
**an invalid value fails the mount loudly** (never a silent fallback); **a knob the kernel cannot honour
is reported as a negotiated-value mismatch, with requested and effective values both printed**; and the
**effective values appear in the mount's status output** — the same law as everything else.

## 7. Measurement first (BFS-057)

No lever goes in without a before/after number on **this** hardware and **both** DCs, because that is
the only way this project has ever settled anything:
- **throughput** — single-stream and 25-way concurrent, h1/h2/h3, loopback and both DCs;
- **latency under load** — the bufferbloat number, since a working tree is interactive;
- **metadata ops/sec** — `find`, `ls -l`, `git status`, the workload that readdirplus and timeouts target;
- **IOPS** — small random reads/writes;
- **CPU per byte** — to prove a lever moves the cost rather than hiding it.

And the negative control the whole project now runs on: **a lever whose absence cannot be detected in
the numbers is not a lever**, and the harness must be able to show a sizeable regression when a lever is
turned off. Fuse passthrough and BBR are the two where a wrong measurement would be easiest to fool
ourselves on.
