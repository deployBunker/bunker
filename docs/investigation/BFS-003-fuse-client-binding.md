# BFS-003 — FUSE client implementation: which binding can carry our cache and diff features?

**Row:** `BFS-003` (P1, complexity 2) · **Type:** investigation only — no product code changed
**Date:** 2026-09-26 · **Host of every "measured here" number:** control host, kernel `7.0.0-31-generic`,
Ubuntu `resolute` (the same host the PRD's sshfs/WebDAV baselines were taken on)
**Owning PRD:** [`docs/prd/PRD-bunker-fs.md`](../prd/PRD-bunker-fs.md)
**Supersedes nothing.** Feeds `BFS-008` (client), `BFS-009` (cache + diff features), `BFS-010` (Windows + Mint).

---

## 0. The question, stated so it can be answered

The client is **not a transparent proxy**. Four capabilities are ours to own, and the binding has to expose
enough control to carry all four:

| # | Capability | What "carrying it" means |
|---|---|---|
| **a** | serve reads from a **size-bounded local cache** | the fs, not the library, decides what bytes are cached and when the kernel may keep its own copy |
| **b** | **invalidate on a server-pushed event**, no polling | one push → the client drops its entry **and** the kernel stops serving the stale copy |
| **c** | **attach a precondition to a write** | a write is intercepted with an expected content hash and can be refused |
| **d** | **short-circuit a whole-tree op to one server call** | a tree walk becomes one remote call instead of N round trips |

A binding that only proxies syscalls fails all four. That is the filter this document applies.

---

## 1. Method — what is measured, and by whom

Two classes of number appear below, and they are labelled:

- **measured here** — re-measured on this host on 2026-09-26 for this row. Every command is quoted verbatim
  in [Appendix A](#appendix-a--measurement-provenance-verbatim).
- **inherited** — recorded in `PRD-bunker-fs.md` with its own provenance (lines 19–31, 37–43, 75–86, 241–248).
  Not re-run for this row: the 185 ms RTT, the 5-of-14 sshfs result, the rclone wedges, the WebDAV 11-ok/1-stall
  result and the 0.41 s native delegation. They are used as premises, not as new findings.

Nothing here is estimated. Anything not determined is in [§9 Open questions](#9-open-questions).

---

## 2. Linux: the two candidates

Both candidates are real, both are buildable on this host, and the difference is not "which works" — it is
**which one can be shipped as the artifact this repo already produces**.

| | **`github.com/hanwen/go-fuse/v2`** (pure Go) | **cgo → libfuse3** |
|---|---|---|
| Version tested / present | `v2.11.0`, released 2026-07-20 (module proxy `@latest`) | `libfuse3-dev 3.18.2-1`, `pkg-config --modversion fuse3` → `3.18.2` (installed) |
| How it talks to the kernel | implements the FUSE wire protocol itself; opens `/dev/fuse`, dispatches requests to our goroutines | links `libfuse3.so.3` and calls into it |
| Build | `CGO_ENABLED=0 go build` → **statically linked**, `ldd: not a dynamic executable` (measured here) | requires `CGO_ENABLED=1` + a C toolchain + libfuse3 headers **per build target** |
| Repo's release matrix | matches it exactly: `RELEASE_PLATFORMS := linux/amd64 linux/arm64`, `CGO_ENABLED=0` (`Makefile:14`, `Makefile:95–105`) | **breaks it**: no cross-compile without a per-arch C toolchain; the two released artifacts stop being "one `go build`" |
| Runtime dependency on the operator box | `/dev/fuse` + a `fusermount` helper (or `DirectMount` as root) — both stock on every distro | the **library** `libfuse3.so.3` must be installed, not just the helper |
| Request scheduling | ours: `MaxBackground`, `CongestionThreshold`, `SingleThreaded`, `MaxInflightRequestBytes` (`fuse/api.go` `MountOptions`) — this is the pipelining bet of slice C2, so it must be ours | libfuse's session loop, with our own threading policy bolted on |
| Data-cache policy | per-open `FOPEN_DIRECT_IO` / `FOPEN_KEEP_CACHE` (`fuse/types.go:252–259`) + `MountOptions.ExplicitDataCacheControl` + per-reply attr/entry timeouts | same concepts, but the whole-file cache control lives in the **low-level** API (`fuse_lowlevel_notify_*`), not the high-level `fuse_operations` most examples use |
| Maturity / third-party evidence | BSD-3, used widely | the reference implementation (libfuse 3.18.2 ships minor 45) |
| Windows reuse | **none** — it speaks the Linux FUSE protocol, which does not exist on Windows | real: WinFsp implements the FUSE (2.8) high-level C API and its own port of SSHFS used exactly that path |

### Decision: **go-fuse (pure Go), for the Linux client.**

The reason is not taste, it is a hard coupling in this repo: **the shipped artifact is produced by
`make release-binaries`, which builds `linux/amd64` and `linux/arm64` with `CGO_ENABLED=0`**
(`Makefile:95–105`, verified in `.github/workflows/release.yml:162–163`). A cgo libfuse client cannot be built
by that target at all, and would put a C toolchain, a per-arch sysroot and a `libfuse3.so.3` runtime
requirement in the path of every operator who today installs one static binary. Measured here, the pure-Go
choice keeps that promise: the same client source built with the repo's own switch is `statically linked`.

Second reason, and it is the one that matters for this specific product: **capabilities (a)–(d) are all
"our code decides"**. go-fuse hands us the op handlers, the per-open data-cache decision, the per-reply
attribute/entry timeouts, the kernel-invalidation primitives and the capability mask — all as ordinary Go
surface on the `Server` we construct. With libfuse, the equivalent control (file-cache invalidation) sits in
the **low-level** API, so a libfuse client would have to give up the high-level API that makes the port cheap
in the first place.

**What we give up, stated honestly:** cgo/libfuse is the mature reference implementation and it is the shape
that ports to Windows most cheaply (WinFsp's FUSE layer is the C high-level API 2.8 — see §4). Choosing
go-fuse therefore commits us to **two thin bindings over one shared core** rather than one C-shaped core.
§4 argues that cost is smaller than it looks, but it is a real cost and it is the strongest argument the
losing side has.

---

## 3. The four capabilities, mapped onto go-fuse

Legend: **NATIVE** = the binding provides the mechanism; **HAND-WRITTEN** = we implement it in our own op
handler / service; **CANNOT** = the binding or the FUSE contract makes it impossible.

| Capability | Binding provides | We write | Verdict |
|---|---|---|---|
| **(a)** reads from a bounded local cache | per-open `FOPEN_DIRECT_IO`/`FOPEN_KEEP_CACHE`; `ExplicitDataCacheControl`; `fs.Options.AttrTimeout/EntryTimeout/NegativeTimeout` with per-reply override | the bounded LRU, the fetch path, the eviction, the cache-bound reporting | **HAND-WRITTEN** op, on native knobs |
| **(b)** invalidate on a server-pushed event | `Server.InodeNotify` / `EntryNotify` / `DeleteNotify` / `InodeNotifyStoreCache` (`fuse/server.go:592,769,790,625`); floor is protocol 7.12 | the push channel (agent inotify → `bunkerd` → h2 stream → client) and the inode mapping | **NATIVE** primitive + **HAND-WRITTEN** channel |
| **(c)** precondition on a write | `NodeWriter.Write` is ours; writes are **not** deferred (writeback cache is never negotiated — measured in the live INIT) | the expected-hash attach, the 412 → errno mapping, the refusal message | **HAND-WRITTEN** op, with a native guarantee underneath |
| **(d)** short-circuit a whole-tree op | `READDIRPLUS` (negotiated) + per-entry `Lookup` served in-process + per-reply timeouts; `DisableReadDirPlus` if we want the other trade | the one-call tree snapshot, the node-tree population, the delegated ops themselves | **NATIVE** substrate + **HAND-WRITTEN** snapshot; one honest **CANNOT** for git's own content reads |

### (a) serve reads from the local cache — HAND-WRITTEN, on native knobs

`NodeReader` (`fs/api.go:376`) is our code: the bytes come from wherever we say, and the bounded LRU of
`--cache-max-size` lives there. What the binding must supply is the *kernel-side* half, and it does:

- **Per-open decision.** `FOPEN_DIRECT_IO` bypasses the kernel page cache entirely; `FOPEN_KEEP_CACHE`
  asks the kernel to keep it across opens (`fuse/types.go:252–259`). **This control is real and observable.**
  Measured here: with an OPEN reply carrying no flags, the kernel re-issued **32 read requests (4 MiB) on
  every one of three sequential opens** — 96 READ ops / 12,582,912 bytes total, no reuse (Appendix A.6).
  The client, not the kernel, decides whether the second open is free.
- **Stop the kernel's automatic invalidation.** `MountOptions.ExplicitDataCacheControl` asks for
  `CAP_EXPLICIT_INVAL_DATA` instead of `CAP_AUTO_INVAL_DATA` (`fuse/opcode.go`, `doInit`). This is the
  switch that has to be on for the PRD's rule — *"content hash decides, never mtime"* — to hold, because the
  default is mtime-driven invalidation.
  **Measured here, the default takes `AUTO_INVAL_DATA` and not `EXPLICIT_INVAL_DATA`** even though the kernel
  offered both (Appendix A.5). Defaults must not be inherited; the option must be set deliberately.
- **Attribute/entry timeouts.** `fs.Options.AttrTimeout/EntryTimeout/NegativeTimeout` are applied only when
  the reply's own timeout is zero (`fs/bridge.go:255–283`), so each `Lookup`/`Getattr` can set its own TTL
  through `*fuse.EntryOut`/`*fuse.AttrOut` — per-node policy, which is what a snapshot-backed tree wants.

**Nothing about (a) is a library feature we lack.** The cache is ours because it has to be.

### (b) invalidate on a server-pushed event, without polling — NATIVE primitive, hand-written channel

Two caches have to be dropped on a push, and only one of them is ours:

1. **our** cache entry — trivial once the push channel exists;
2. **the kernel's** cached data/attributes for that inode — this is where a binding either helps or blocks us.

go-fuse exposes the kernel half directly on the `Server` it returns (`type Server struct { protocolServer … }`,
`fuse/server.go`, so the notify methods are exported):

| call | effect | protocol floor (`fuse/server.go:814–824`) |
|---|---|---|
| `Server.InodeNotify(ino, off, len)` | drops the inode's cached **data + attributes** | `NOTIFY_INVAL_INODE` → 7.12 |
| `Server.EntryNotify(parent, name)` | drops a **directory entry** (rename/unlink without a delete event) | `NOTIFY_INVAL_ENTRY` → 7.12 |
| `Server.DeleteNotify(parent, child, name)` | tells the kernel a name is gone | `NOTIFY_DELETE` → 7.18 |
| `Server.InodeNotifyStoreCache(ino, off, data)` | pushes **content into** the kernel cache proactively | `NOTIFY_STORE_CACHE` → 7.15 |

The floor for the two we need most (`INVAL_INODE`, `INVAL_ENTRY`) is **7.12, which is exactly go-fuse's own
minimum** (`fuse/request_linux.go:11`) — so there is no kernel on which this client mounts but cannot
invalidate. Nothing is lost at the version floor.

The channel itself (agent-side inotify → `bunkerd` → a server-pushed stream → this client) is our code and is
BFS-009's problem, not a binding question. The PRD's poll fallback (AC-9) stays, because the *push channel*
can be absent even when the binding is capable.

### (c) attach a precondition to a write — HAND-WRITTEN op, with a native guarantee underneath

The interception point is `NodeWriter.Write` (`fs/api.go:383`): we see the target inode, the offset and the
bytes before anything leaves the host, so we can `PUT … If-Match: <hash>` and turn a server `412` into a
refusal.

Two facts make this enforcement real rather than nominal:

- **Writes are not batched behind a writeback cache.** The kernel offered `WRITEBACK_CACHE` in this host's
  INIT; go-fuse's accepted capability set does not include it and the negotiated reply omits it
  (`fuse/opcode.go` `doInit` — accepted set is `ASYNC_READ|BIG_WRITES|FILE_OPS|READDIRPLUS|NO_OPEN_SUPPORT|
  PARALLEL_DIROPS|MAX_PAGES|RENAME_SWAP|PASSTHROUGH|ALLOW_IDMAP` + `ExtraCapabilities`). Measured in the live
  session: a 4,194,304-byte file arrived as **1023 discrete WRITE ops totalling exactly 4,194,304 bytes**
  with per-op sizes from 94 to 8,106 bytes (Appendix A.6). Every byte is inspectable at our op boundary.
- **The precondition's value comes from our cache, not from the caller.** This is a FUSE contract limit, not a
  go-fuse limit: a syscall cannot carry "expected hash". The client attaches the hash it last served for that
  inode. A write to a path never read has no base hash — that case must either fetch-then-check or refuse, and
  that is a design decision for BFS-009, recorded here so it is not discovered late.

**Verdict: hand-written, supported, with one inherent limit named above** (no caller-supplied precondition;
mmap-granular writes see [§9](#9-open-questions)).

### (d) short-circuit a whole-tree operation to a single server call — NATIVE substrate + hand-written snapshot, and one CANNOT

This is where the PRD's language and the filesystem's reality need to be separated, because they are not the
same claim.

**What the binding makes possible (native, verified in the live session):** `CAP_READDIRPLUS` is negotiated by
default (measured: present in the INIT reply, Appendix A.5), and go-fuse's `READDIRPLUS` handler resolves each
entry through the filesystem's own `Lookup` in-process (`fs/bridge.go`, `readDirMaybeLookup(..., lookup=true)`,
calling `b.lookup`/`FileLookuper.Lookup` per entry) with per-entry timeouts applied
(`b.setEntryOutTimeout`). So **one kernel `READDIRPLUS` op can be answered entirely from an in-memory node
tree**: if our `Readdir`/`Lookup` are backed by a single server call that returned the subtree's metadata
(one `PROPFIND` with `Depth: infinity`, or the PRD's `X-Bunker-Op`), then `ls -l`, `stat`, and a `git status`
walk issue **zero** further network round trips. That is exactly "N round trips → 1 server call", and it is
the capability the row is asking about.

**What cannot happen inside FUSE:** the delegated ops of PRD slice C5 (`status`, `diff --stat`, `rev-parse`,
`ls-files` computed on the agent) are **not** reachable by making git's syscalls smarter. git is not a client
of our API; it calls `lstat`/`open`/`read` and we cannot know which tool is running or what it will ask for
next. The mount can make the **walk** one call, and can make unchanged files free (git re-reads only files
whose stat moved — and our metadata comes from the server, so a clean tree costs one snapshot and no reads).
But git's own content reads for *changed* files are N reads by construction, and no binding changes that.
Delegation therefore keeps a **second surface** (`bunker fs status`, `X-Bunker-Op` invoked by our CLI/verbs)
for the cases where the answer is computed remotely and never walked at all.

**This is a correction to the PRD's phrasing, not a defect in it:** C5's delegation stays load-bearing, but
"`status` becomes a server-side operation *behind a filesystem interface*" is only true for the metadata
walk. The whole-op delegation serves our own surfaces. BFS-005/BFS-008 should state it that way.

---

## 4. Windows: WinFsp vs Dokan vs the OS redirector

| | **WinFsp** | **Dokany** | **OS WebDAV redirector (`WebClient`)** |
|---|---|---|---|
| Shape | kernel-mode FSD + user-mode DLL; APIs: **Native, FUSE2, FUSE3, .NET** (project README) | kernel driver + `dokan2.dll` + **FUSE wrapper** (`dokanfuse2.dll`) | none — a Windows service, no extension point |
| FUSE API level exposed | `2.8` in the FUSE headers (`inc/fuse/fuse_common.h:37–38`), FUSE3 available for C | `2.7` (`dokan_fuse/include/fuse_common.h:24–27`) | n/a |
| Platform | Windows 7–11, x86 / x64 / ARM64 (README) | modern Windows | client SKUs |
| Licensing | GPLv3 **with an exception for FLOSS**, or commercial (README) | **LGPL** for driver + `dokan2.dll` + `dokannp2.dll` + `dokanfuse2.dll` + installer; MIT for `dokanctl`/samples (README §Licensing) | OS component |
| Cadence | active (repo pushed 2026) | less active: latest CHANGELOG entry `2.3.1.1000`, 2025-09-28 | **deprecated** |
| Go client | **`github.com/winfsp/cgofuse`** — MIT, 644 stars, pushed 2026-05-31; Windows works **both cgo and nocgo** (`README` support matrix; `fuse/host_nocgo_windows.go` loads the WinFsp DLLs via `syscall`) | **no maintained Go binding found** (a GitHub repository search for Go+Dokan returns only unrelated 0-star projects; `dokan-dev/dokan-go` does not resolve) — adopting Dokan means writing the cgo bindings ourselves | none possible |
| Cache control | `-o FileInfoTimeout=N` (metadata, millis, **`-1` for data caching**), `DirInfoTimeout`, `EaTimeout`, `VolumeInfoTimeout`; default `FileInfoTimeout = 1000` (`src/dll/fuse/fuse.c:92–99`, help text `:650–653`, default `:805`); FSD-side fields and per-class overrides in `inc/winfsp/fsctl.h:206,249–253` | driver-level caching, no FUSE-visible knobs of this granularity | **none** — only limits, not policy |
| Change notification | `FspFileSystemNotifyBegin/Notify/End` (`inc/winfsp/winfsp.h:1262,1277,1306`), exposed in Go as `FileSystemHost.Notify(path, action)` (`cgofuse fuse/host.go:833–845`) | `FsRtlNotify*` internals, not surfaced to a Go client | none |
| Measured/interop note | — | — | MS Learn: *"The Webclient (WebDAV) service is deprecated. The Webclient service isn't started by default in Windows."* Attribute budget: the attributes returned by a WebDAV server are capped at **1 MB** by default *"for security reasons"*, and the KB linked from the same page is about files **larger than 50000000 bytes** |

### Decision: **WinFsp, driven from Go through `cgofuse`.**

1. It is the only one of the three with a **maintained Go client** that works on Windows in both build modes —
   and the `nocgo` mode (Windows DLL binding via `syscall`) matters, because it means a Windows build does not
   depend on mingw being present in CI.
2. It exposes the two knobs our features need, on the surface our Go code can reach: **metadata/data caching
   policy** (`FileInfoTimeout`, `SetDirectIO`) and a **change-notification path** (`Notify` → `FspFileSystemNotify`).
3. Dokany is a licensing and maintenance downgrade for this repo (LGPL kernel driver, an older 2.7-level FUSE
   wrapper, slower cadence) **and** has no Go client we can adopt — we would hand-write the bindings, i.e. take
   on the one cost that made WinFsp attractive in the first place.
4. The OS redirector is **rejected as a product path** — it is deprecated and off by default, and it has no
   cache policy and no extension point, so it cannot carry (a), (b) or (c) at all. It stays relevant in one
   way only: **it is a compatibility target for third-party and stock WebDAV clients** (which is what
   `BFS-012` should test). That test needs a caveat the PRD does not have yet: "old clients keep working"
   is true for WebDAV clients in general, but the *OS-native Windows* one is a deprecated component the
   operator must now enable by hand, and it is bounded by the 1 MB attribute budget and the 50 MB file limit.
   For a 150-file tree carrying our extension attributes, that attribute budget is a real ceiling.

**Does the Windows client share the Linux core? Partly, and the boundary is measurable.**

- **Shared:** everything above the fs glue — the HTTP/2 client, the bounded cache, the invalidation logic, the
  hash-precondition rule, the snapshot/delegation RPCs, and the *policy* layer that decides cache behaviour and
  timeouts. That is one Go package, and it is most of BFS-009.
- **Not shared:** the fs glue itself. go-fuse speaks the Linux FUSE protocol over `/dev/fuse`, which does not
  exist on Windows; WinFsp is reached through C DLLs (`cgofuse`) or its own API. A Windows client is therefore
  **shared core + a second thin binding**, not a recompile.
- **The consequence that must be designed for (BFS-010):** the Linux knobs and the Windows knobs are different
  names for the same policies (`FOPEN_DIRECT_IO`/`ExplicitDataCacheControl` vs `SetDirectIO`/`FileInfoTimeout`;
  `InodeNotify` vs `Notify`). Each binding must translate **one** policy type, or the two platforms will drift
  in behaviour while claiming the same feature set.
- **One licensing note for the owner:** WinFsp is GPLv3 with a FLOSS exception (commercial licence otherwise).
  This repo is **Apache-2.0** (`LICENSE`), so the FLOSS exception appears to apply — but that is a licensing
  judgement for the owner, not a measurement, and it is listed in §9.

---

## 5. The Linux Mint case

**Verdict: no separate work for the mount. One documentation caveat.**

Bane's hunch was that Mint would need something. Followed to the measurable facts, the mount surface is
kernel + distro-package shaped, and Mint inherits both from Ubuntu:

| Fact | Value | Source |
|---|---|---|
| Mint base | "Linux Mint 22.x is based on Ubuntu 24.04." | Linux Mint release notes (22.1 / 22.3) |
| Kernels it ships | LTS 6.8; HWE **6.14** from 22.2 onward | same |
| Kernel FUSE protocol those kernels speak | 6.8 → **minor 39**; 6.11 → 40; 6.14 → **42** | measured here by tag scan ([Appendix A.7](#appendix-a--measurement-provenance-verbatim)) |
| go-fuse's floor / preferred | 12 / **28** | `fuse/request_linux.go:8–12` |
| `fuse3` package | Ubuntu `noble` ships `fuse3 3.14.0-5build1` (so `/dev/fuse` + `fusermount3` come from the distro) | packages.ubuntu.com/noble/fuse3 |
| The client itself | a statically linked Go binary (`CGO_ENABLED=0`), nothing else to install | measured here, [Appendix A.4](#appendix-a--measurement-provenance-verbatim) |

Every Mint kernel in support is far above both the binding floor (7.12) and the version go-fuse prefers
(7.28), so **no feature is lost and nothing has to be built for Mint**. The only Mint-specific surface left
would be desktop integration (the tray), and **this repo has no such target today** — a repository-wide grep
for `tray` matches only unrelated prose in tests and audit notes, not a command, target or package
(measured here; `cmd/` holds `bunker`, `bunkerd` and `docs-drift` only).

**The caveat, because it is real and cheap to document:** mounting with `-o allow_other` requires
`user_allow_other` in `/etc/fuse.conf`. On this host it is enabled (`/etc/fuse.conf`, measured); Mint ships
that file with the line commented out, so a mount that requests `allow_other` will fail until the operator
uncomments it. That is an install-time note, not a Mint port.

---

## 6. The FUSE protocol-version floor, and what it implies

| Binding | Enforced floor | Speaks | Evidence |
|---|---|---|---|
| **go-fuse v2.11.0** | **major 7 exactly**, **minor ≥ 12** — below that `doInit` answers `EIO` and the mount fails | 7.28 | `fuse/request_linux.go:8–12`; `fuse/opcode.go` `doInit` |
| libfuse 3.18.2 (the cgo alternative) | major **< 7** rejected (`EPROTO`), major > 7 handled by re-INIT; compat reply shapes for minor < 5 / < 23 | 7.45 | `lib/fuse_lowlevel.c:2671`, `:2947–2950`; `include/fuse_kernel.h` |

**What 7.12 means for how old a kernel can mount this:** protocol 7.12 is the minor reported by **Linux
2.6.31** (measured by tag scan: `v2.6.31 → 12`, `v2.6.32 → 13`). So the client's kernel floor is
**Linux ≥ 2.6.31 (September 2009)** — effectively "any kernel anyone runs". The features we depend on sit at
or just above that same line, all measured the same way: `NOTIFY_INVAL_INODE`/`INVAL_ENTRY` at 7.12 (the
floor itself), `NOTIFY_STORE_CACHE` at 7.15 and `NOTIFY_DELETE` at 7.18, both inside the 3.0–3.3 kernel line
(boundaries measured: `v3.0 → 16`, `v3.1 → 17`, `v3.3 → 18`).

**The practical floor is not the kernel.** It is the **mount helper**: an unprivileged mount needs a
`fusermount` binary that is setuid (measured on this host: `/usr/bin/fusermount` → symlink to the setuid
`fusermount3`, `3.18.2`), or the process must hold `CAP_SYS_ADMIN` and use go-fuse's `DirectMount`
(`fuse/mount_linux.go` `mountDirect`, which opens `/dev/fuse` and calls `syscall.Mount` itself). The live
probe here used the helper path, unprivileged, as uid 1000.

**Windows has no equivalent floor to state:** the "protocol" is WinFsp's own, and the platform floor is
WinFsp's (Windows 7–11, x86/x64/ARM64).

---

## 7. Live probe — what was actually executed on this host

Everything below is a re-measurement for this row. The full command transcript is
[Appendix A](#appendix-a--measurement-provenance-verbatim).

1. **A pure-Go go-fuse client builds and mounts here, unprivileged, with no libfuse involved.**
   `go-fuse v2.11.0` example filesystem built with `CGO_ENABLED=0` (`statically linked`), mounted at
   `/tmp/bfs003-mnt2` as uid 1000 through `/usr/bin/fusermount3`, and served a 4 MiB write + read + `stat`.
   Mount line, verbatim:
   `fuse.nodefs.memNode … rw,user_id=1000,group_id=1000,max_read=131072`.
2. **The kernel offered protocol 7.45 with full capability advertisement; go-fuse answered 7.28 and selected
   exactly the capabilities it wants.** Verbatim from the session (abridged to the capability lists):
   - `rx 2: INIT n0 {7.45 … AUTO_INVAL_DATA,READDIRPLUS,…,WRITEBACK_CACHE,…,EXPLICIT_INVAL_DATA,…,PASSTHROUGH,…}`
   - `tx 2: OK, {7.28 … ASYNC_READ,BIG_WRITES,AUTO_INVAL_DATA,READDIRPLUS,NO_OPEN_SUPPORT,PARALLEL_DIROPS,MAX_PAGES,INIT_EXT,PASSTHROUGH … Wr 131072 …}`
   **`WRITEBACK_CACHE` and `EXPLICIT_INVAL_DATA` were offered and not taken** — which is precisely why both
   switches must be set deliberately by us, and why writes stay inspectable at our op boundary.
3. **The per-open data-cache decision is observable and it is the filesystem's.** With OPEN replies carrying
   no flags: 3 sequential opens of the same 4 MiB file → **32 READ requests each, 96 total, 12,582,912 bytes**.
   No reuse across opens. The OPEN reply is where `FOPEN_KEEP_CACHE`/`FOPEN_DIRECT_IO` would change that, and
   it is our reply to write.
4. **Writes arrive as discrete, wholly-visible ops.** 4,194,304 bytes written → **1023 WRITE ops totalling
   exactly 4,194,304 bytes**, per-op sizes 94–8,106 bytes. Nothing was coalesced out of our sight.
5. **The host's FUSE stack is otherwise already proven** (the seed's claim, re-checked): `/dev/fuse`
   `crw-rw-rw- 10,229`; `fusermount3 3.18.2`; `sshfs 3.7.6`; `CONFIG_FUSE_FS=y`, `CONFIG_FUSE_IO_URING=y`,
   `CONFIG_FUSE_PASSTHROUGH=y`; and eight live FUSE mounts already exist on the box (`fuseblk`, `fuse.portal`,
   `fuse.gvfsd-fuse`).
6. **The repo has no FUSE dependency today** (`go.mod`: no go-fuse, no fuse3 bindings) and the mount seam is
   a *registry*, not a launcher: `internal/mountdriver` holds `Driver{Name, Classify, NoClassifier}` with
   `Register`/`Resolve` (`internal/mountdriver/mountdriver.go:56–88`), while the launch path is sshfs-shaped in
   `internal/cli/mount.go` (585 lines) and the spawn-built `sshfs_mount` string
   (`docs/mount-drivers.md:63–66`). A new driver therefore lands as **registry entry + launch path + (new)
   in-process client**, which is the work `BFS-008` measures.

---

## 8. What this decides for the downstream rows (no code changed by this row)

- **BFS-008 (client, Linux first):** build on `github.com/hanwen/go-fuse/v2` in `internal/fs/`, keeping
  `CGO_ENABLED=0` a hard release constraint; set `ExplicitDataCacheControl`, the attr/entry timeouts and the
  per-open flags deliberately rather than accepting defaults (measured defaults violate the content-hash rule).
- **BFS-009 (cache + diff):** the four capabilities are all implementable in our op handlers; the two design
  points this row surfaces are (i) the base hash for a write to a path never read, and (ii) the fact that a
  whole-tree snapshot collapses the *walk* while git's content reads for changed files remain N reads.
- **BFS-010 (Windows + Mint):** Windows = WinFsp via `cgofuse`, shared core + second binding + one policy type;
  Mint = no port, one `/etc/fuse.conf` note.
- **BFS-005 (client feature spec):** should pin the policy knobs per platform (§4 table) so the two bindings
  cannot drift.
- **BFS-012 (old-client compatibility):** stock WebDAV clients remain the compatibility target; the OS-native
  Windows client is deprecated and off by default, and has a 1 MB attribute / 50 MB file budget.

---

## 9. Open questions

Honest unknowns, each one a thing this row could not determine from here:

1. **Whether the push channel can meet AC-4's 2 s budget on a 185 ms link.** No agent-side watcher exists yet
   (PRD: "Agent-side inotify watcher — MISSING"), so the end-to-end invalidation latency is unmeasured.
2. **Whether the kernel's own attribute cache can be kept honest against mtime.** Our cache will key on content
   hashes, but the kernel's attr cache is mtime-driven unless we zero the timeouts and drive notifications. The
   live experiment (a build on the agent touching files with unchanged bytes, under a real git tree) was not
   run here.
3. **Windows invalidation semantics.** `FspFileSystemNotify` is documented as telling Windows about file
   changes, and `Notify` is exposed in `cgofuse`; whether that invalidates cached **file data** (as opposed to
   directory-change notifications) is not verified — there is no Windows host in this environment.
4. **Windows cache-policy parity.** `FileInfoTimeout=-1` is documented ("-1 for data caching") but its exact
   semantics were not exercised; the Windows policy layer can only be pinned after a Windows run.
5. **mmap writers.** A process that mmaps a mounted file and dirties pages produces writes our handler still
   sees, but at page granularity and with offsets we did not choose; how a whole-file hash precondition should
   behave there is undecided.
6. **Dokan's Go story.** "No maintained Go binding found" is a search result, not proof of absence — a binding
   may exist under a name that search did not surface.
7. **WinFsp licensing in our distribution.** The FLOSS exception appears to cover an Apache-2.0 client; that
   judgement belongs to the owner.
8. **Mint package pre-installation.** `fuse3` exists in Ubuntu `noble`; whether every Mint edition pre-installs
   it (as opposed to only shipping it in the archive) was not verified. Without it, an unprivileged mount has
   no `fusermount3` helper.
9. **Whether a whole-tree snapshot response fits third-party clients' budgets** (e.g. the redirector's 1 MB
   attribute cap) for trees larger than the 150-file fixture.
10. **`PASSTHROUGH` capability.** go-fuse negotiated `PASSTHROUGH` (kernel `CONFIG_FUSE_PASSTHROUGH=y`), which
    can hand reads/writes straight to another file descriptor. That may be a much cheaper read path than
    anything designed in this document — it was not investigated here and deserves its own look before
    BFS-008 picks a caching design.

---

## Appendix A — measurement provenance (verbatim)

Executed 2026-09-26 on the control host unless stated otherwise. Results are the literal outputs used above.

**A.1 — go-fuse version and its declared protocol floor**

```
curl -sS "https://proxy.golang.org/github.com/hanwen/go-fuse/v2/@latest"
  → {"Version":"v2.11.0","Time":"2026-07-20T07:08:06Z", …}
# source, from the v2.11.0 module zip
fuse/request_linux.go:8–12  →  _FUSE_KERNEL_VERSION = 7
                                _MINIMUM_MINOR_VERSION = 12
                                _OUR_MINOR_VERSION = 28
```
`doInit` (`fuse/opcode.go`) rejects `input.Major != 7` and `input.Minor < 12` with `EIO`.

**A.2 — this host's FUSE stack**

```
fusermount3 --version            → fusermount3 version: 3.18.2
sshfs --version                  → SSHFS version 3.7.6 / FUSE library version 3.18.2
ls -l /dev/fuse                  → crw-rw-rw- 1 root root 10, 229
grep -i fuse /proc/filesystems   → nodev fuse, nodev fusectl, fuseblk
grep FUSE_KERNEL_* /usr/include/linux/fuse.h → FUSE_KERNEL_VERSION 7 / MINOR_VERSION 45
ls -l /usr/bin/fusermount        → fusermount -> fusermount3   (fusermount3: -rwsr-xr-x, setuid)
grep -E '^CONFIG_FUSE' /boot/config-7.0.0-31-generic
  → CONFIG_FUSE_FS=y, CONFIG_FUSE_DAX=y, CONFIG_FUSE_PASSTHROUGH=y, CONFIG_FUSE_IO_URING=y
grep user_allow_other /etc/fuse.conf → present (enabled) on this host
grep -c fuse /proc/self/mountinfo    → 8 live FUSE mounts (fuseblk, fuse.portal, fuse.gvfsd-fuse)
pkg-config --modversion fuse3        → 3.18.2 ; dpkg: libfuse3-dev 3.18.2-1 installed
```

**A.3 — the repo's own release constraint**

```
Makefile:14   RELEASE_PLATFORMS := linux/amd64 linux/arm64
Makefile:95–105  release-binaries: … GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -o $$out ./cmd/$$name
.github/workflows/release.yml:162–163 verifies GOOS=linux / GOARCH=<platform> on the published asset
LICENSE → Apache License 2.0
go.mod  → no FUSE dependency of any kind
grep -rn 'tray' (repo) → no tray/desktop target
```

**A.4 — pure-Go build, statically linked**

```
cp -r <go-fuse v2.11.0 module zip> /tmp/bfs003-gofuse2
CGO_ENABLED=0 GOFLAGS=-mod=mod go build -o /tmp/bfs003-memfs-nocgo ./example/memfs
  → only external module downloaded: golang.org/x/sys v0.28.0 ; binary 3,888,215 bytes
ldd /tmp/bfs003-memfs-nocgo → "not a dynamic executable"
file …                      → "statically linked"
```

**A.5 — live mount + INIT negotiation**

```
/tmp/bfs003-memfs-nocgo -debug /tmp/bfs003-mnt2 /tmp/bfs003-backing   (background, as uid 1000)
grep bfs003-mnt2 /proc/self/mountinfo
 → … /tmp/bfs003-mnt2 rw,nosuid,nodev,relatime … - fuse.nodefs.memNode nodefs.memNode
   rw,user_id=1000,group_id=1000,max_read=131072
log line 1 (debug): callFusermount: executing ["/usr/bin/fusermount3" "/tmp/bfs003-mnt2" "-o" "subtype=nodefs.memNode,max_read=131072"]
log: rx 2: INIT n0 {7.45 Ra 131072 ASYNC_READ,…AUTO_INVAL_DATA,READDIRPLUS,…WRITEBACK_CACHE,…EXPLICIT_INVAL_DATA,…PASSTHROUGH,…}
log: tx 2:     OK, {7.28 Ra 131072 ASYNC_READ,BIG_WRITES,AUTO_INVAL_DATA,READDIRPLUS,NO_OPEN_SUPPORT,PARALLEL_DIROPS,MAX_PAGES,INIT_EXT,PASSTHROUGH 0/0 Wr 131072 Tg 0 MaxPages 32 MaxStack 1}
```

**A.6 — read/write accounting on that mount**

```
head -c 4194304 /dev/urandom > /tmp/bfs003-mnt2/big.bin      # 4 MiB written through the mount
cat big.bin > /dev/null   (×3, each in its own process)
grep -c ': READ n'  log  → 96           (32 per open × 3 opens)
sum of read lengths → 12582912 bytes    (exactly 3 × 4 MiB)
grep -c ': WRITE n' log  → 1023 ; sum of write lengths → 4194304 bytes (exactly the file size)
WRITE request sizes observed: 94 … 8106 bytes
OPEN replies: tx: OK, {Fh 2 }   (no FOPEN_* flags → no kernel-side reuse, as measured)
```

**A.7 — kernel protocol-version timeline (tag scan of `include/linux/fuse.h`, later `include/uapi/linux/fuse.h`)**

```
v2.6.31 → 12      v2.6.32 → 13      v2.6.34 → 13      v2.6.35 → 14     v2.6.36 → 15
v2.6.38 → 16      v2.6.39 → 16      v3.0 → 16         v3.1 → 17        v3.2 → 17      v3.3 → 18
v3.5 → 19         v3.6 → 20         v6.1 → 37         v6.8 → 39        v6.11 → 40     v6.14 → 42
```

**A.8 — libfuse 3.18.2 floor (the losing candidate, for fairness)**

```
include/fuse_kernel.h      → FUSE_KERNEL_VERSION 7 / FUSE_KERNEL_MINOR_VERSION 45
lib/fuse_lowlevel.c:2671   → arg->major < 7  ⇒ "unsupported protocol version" + EPROTO
lib/fuse_lowlevel.c:2947   → compat reply sizes for minor < 5 and minor < 23
```

**A.9 — Windows-side sources** (fetched from upstream on 2026-09-26; `master`)

```
winfsp README              → kernel FSD + user DLL; "Windows 7 to Windows 11 and the x86, x64 and ARM64
                             architectures"; "Includes Native, FUSE2, FUSE3 and .NET API's";
                             "GPLv3 license with a special exception for Free/Libre and Open Source Software.
                              A commercial license is also available."
winfsp inc/fuse/fuse_common.h:37–38 → FUSE_MAJOR_VERSION 2 / FUSE_MINOR_VERSION 8
winfsp src/dll/fuse/fuse.c:92–99, 650–653, 805
                           → -o FileInfoTimeout=/DirInfoTimeout=/EaTimeout=/VolumeInfoTimeout=,
                             "FileInfoTimeout=N  metadata timeout (millis, -1 for data caching)",
                             default FileInfoTimeout = 1000
winfsp inc/winfsp/fsctl.h:206, 249–253 → FileInfoTimeout + per-class overrides and *Valid bits
winfsp inc/winfsp/winfsp.h:1262, 1277, 1306 → FspFileSystemNotifyBegin/End/Notify
                             ("Begin notifying Windows that the file system has file changes.")
cgofuse README             → MIT; 644 stars; support matrix: Windows cgo ✓, !cgo ✓, FUSE3 ✗;
                             Linux cgo ✓; prerequisites on Windows = WinFsp + gcc (or CGO_ENABLED=0)
cgofuse fuse/host.go:663–697 → SetCapCaseInsensitive / SetCapReaddirPlus / SetCapDeleteAccess /
                             SetCapOpenTrunc / SetDirectIO / SetUseIno
cgofuse fuse/host.go:833–845 → FileSystemHost.Notify(path, action)
dokany README §Licensing   → dokan2.dll / dokan2.sys / dokannp2.dll / dokanfuse2.dll / installer = LGPL;
                             dokanctl.exe and samples = MIT
dokany dokan_fuse/include/fuse_common.h:24–27 → FUSE_MAJOR_VERSION 2 / FUSE_MINOR_VERSION 7
dokany CHANGELOG.md        → newest entry 2.3.1.1000, 2025-09-28
learn.microsoft.com/en-us/windows/whats-new/deprecated-features
                           → "The Webclient (WebDAV) service is deprecated. The Webclient service
                              isn't started by default in Windows."
learn.microsoft.com/en-us/troubleshoot/windows-client/networking/cannot-access-webdav-web-folder
                           → attribute budget "limited to 1 MB. This limit is for security reasons." and a
                             link to the KB about files "larger than 50000000 bytes" from a Web folder
github search (Go + Dokan) → no maintained Go binding; unrelated 0-star repositories only
```

**A.10 — Mint**

```
linuxmint.com release notes (22.1 / 22.3) → "Linux Mint 22.x is based on Ubuntu 24.04."; LTS kernel 6.8,
                                            HWE 6.14 from 22.2
packages.ubuntu.com/noble/fuse3           → fuse3 3.14.0-5build1 (HTTP 200, package present in the base)
```

---

## Appendix B — where the code facts live

**In this repo**

| Fact | Location |
|---|---|
| release matrix: `linux/amd64`, `linux/arm64`, `CGO_ENABLED=0` | `Makefile:14`, `Makefile:95–105` |
| driver registry (`Register`/`Resolve`/`Validate`) | `internal/mountdriver/mountdriver.go:56–129` |
| sshfs-shaped launch path, retry/classifier, version guard | `internal/cli/mount.go` (585 lines) |
| driver design rules + the missing abstraction (MOUNT-006) | `docs/mount-drivers.md:61–79` |
| the PRD's client requirements, AC-1…AC-12, slices C0–C6 | `docs/prd/PRD-bunker-fs.md:122–137`, `:288–302` |
| inherited baselines (RTT, 5/14, rclone wedges, WebDAV 11/1, delegation 0.41 s) | `docs/prd/PRD-bunker-fs.md:19–31`, `:37–43`, `:75–86` |
| the 14-op battery used for all baselines | `probes/git-over-mount-probe.sh` |

**Upstream**

| Fact | Location |
|---|---|
| protocol floor 7.12 / speaks 7.28 / major-7 check | `go-fuse/v2@v2.11.0` `fuse/request_linux.go:8–12`, `fuse/opcode.go` `doInit` |
| capability set accepted at INIT (no `WRITEBACK_CACHE`) | same `doInit` |
| notify primitives + their protocol floors | `fuse/server.go:592, 625, 769, 790, 814–824` |
| data-cache flags / mount options | `fuse/types.go:252–259`, `fuse/api.go` `MountOptions` |
| fs timeout options + per-reply override | `fs/api.go` `Options`, `fs/bridge.go:255–283` |
| readdirplus resolves entries through our Lookup | `fs/bridge.go` `readDirMaybeLookup` |
| mount implementation (helper vs direct syscall) | `fuse/mount_linux.go` |
| libfuse floor / minor | `libfuse` `lib/fuse_lowlevel.c:2671,2947`, `include/fuse_kernel.h` |
