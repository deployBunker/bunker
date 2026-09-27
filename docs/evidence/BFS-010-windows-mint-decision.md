# BFS-010 — Windows and Linux Mint client support: the decision, and the seam that was not there

**Row:** BFS-010 (P1) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-26
**Owner's words:** *"have a feeling windows and Linux mint be needed."*
**Status:** decision row — **Windows: decided here (WinFsp via cgofuse, opt-in) with its costs named;
Mint: NO WORK, with the reasoning and the measurement that says so.** One code defect was found and
fixed because the row's own acceptance is "verify the `!linux` seam compiles and its refusal is honest":
**the seam did not compile.** Two further defects are reported, not fixed (§7).

| question | verdict | where |
|---|---|---|
| **PART 1 — Windows** | **WinFsp, driven from Go through `cgofuse` \| no new server-side requirement \| opt-in, sshfs stays the default.** The OS redirector is **rejected**, and the reason is this release's own finding: the redirector cannot express the client's concurrency knob and cannot carry `X-Bunker-Op: snapshot`, which are the two things that made the client fast. | §2 |
| **PART 2 — Linux Mint** | **No separate build, branch, or package.** Mint inherits Ubuntu's kernel FUSE support and its `fuse3` helper; the client is a static Go binary that does not link `libfuse` at all. BFS-003 §5's one Mint caveat (`user_allow_other` in `/etc/fuse.conf`) is **void for this driver**, which strips `allow_other`. One install-time line remains. | §3 |
| **the `!linux` seam (BFS-010's own file)** | **It did not compile — for any non-Linux platform.** Fixed: it now builds for windows/amd64, windows/arm64, windows/386, and type-checks for darwin/freebsd/openbsd/netbsd/solaris/illumos/plan9/js (once one unrelated darwin defect is fixed — §7). Its refusal is honest: a named error, no mount handle, no local side effect, never retried. | §2.4, §5 |
| **cross-compile** | **PASS for the fsmount seam on every target tried; `GOOS=windows GOARCH=amd64 go build ./...` still stops in `internal/cli` (a second, unrelated seam gap, §7).** The guard that catches this class of bug is committed and executable: `probes/cross-GOOS-build.sh`. | §5, §7 |

---

## 1. What this row was asked to do, and what it deliberately did not do

The row is a **decision** row and says so twice: its second half may correctly resolve to *no work at
all*, and manufacturing work to fill it is a failure. It was also told the hard fact of this
environment: **there is no Windows machine here.** No Windows FUSE binding can be built, mounted,
measured or even run in this session, so:

* **No Windows code was written.** A stub `fs_windows.go` that cannot be compiled against a real WinFsp
  install, cannot be mounted, and cannot be tested is worse than no file: it would make the repo *look*
  ported while nothing on Windows had ever run. The plug-in point is specified instead (§2.4), with the
  upstream API named to the method.
* **No Windows result is claimed.** Everything Windows-shaped below is labelled either *upstream
  fact with a citation*, *our measurement*, or *claim* — §6 is the ledger.
* **What could be verified was verified**: the seam compiles for the platforms it claims, its refusal is
  named and side-effect-free, the cross-compile was run, and the Linux client's actual runtime
  requirements were measured on this host to answer the Mint half (§3).

Two source facts that decide how much of this is new work:

* `internal/fsmount/options.go:4-21` — the package's own seam comment: everything platform-specific is in
  this package's per-OS files and **nowhere else**, and `internal/fsclient` has "no FUSE and no platform
  constant beyond the named errno table". The client half of that claim is real (§7 lists the one place
  it leaks, on darwin only).
* `internal/mountdriver/bunkerfs.go:50-62` — the driver already carries **its own option set** (no sshfs
  option appears in it), and `:67-77` already lists `"no fuse binding on this platform"` among the
  **PERMANENT** failure fragments — the class whose stated meaning is "retrying cannot help"
  (`mountdriver.go`: only `transient` is retried). So the "its own options and its own failure classifier"
  release rule is already satisfied for the unsupported case, and a Windows mount would inherit the same
  two tables rather than a second policy layer.

---

## 2. PART 1 — Windows

### 2.1 The four candidates against what this release measured

`docs/investigation/BFS-003-fuse-client-binding.md` §4 is this repo's own prior research (WinFsp vs
Dokany vs the redirector, with upstream file:line citations). This row does not re-fetch upstream: it
**re-decides against what BFS-011 and BFS-016 measured**, because those two rows changed what the
decision has to optimise for.

What the release actually established:

| finding | number | source |
|---|---|---|
| **Concurrency is the lever; the protocol version is only the carrier.** HTTP/1.1 on one connection is 11.969 s at c=1 and 11.966 s at c=8 — *identical*, because it cannot pipeline; HTTP/2 on one connection drops 12.231 s → 2.008 s (**6.09×**); HTTP/1.1 with **eight connections** at the same concurrency reaches 1.847 s (**6.4777×**) | 6.48× | BFS-011 §0, §4.4 |
| **Through the mount, concurrency is reachable and it is our knob**: 8 parallel cold reads reach **8 requests in flight** at `--concurrency 8` (1 at c=1), 0.584 s → 0.162 s (**3.6×**) | 3.6× | BFS-016 §3 arms 3-4 |
| **A serial whole-tree walk does not use it** (132 requests either way, 1→2 in flight, 1.03×) — **the whole-tree win is the `X-Bunker-Op: snapshot`: 421 nodes in ONE call, 0.035 s, and ZERO requests during the walk** (232× the c=8 walk arm) | 232× | BFS-016 §3 arm 5, §2 |
| 0 stalls in the 35-cell battery; the worst case anywhere is a **bounded** 30 s per internal request against a frozen server, with a named errno and no hang | 0/35 | BFS-016 §2, §4 |

| | **(a) WinFsp via `cgofuse`** | **(b) OS WebDAV redirector (`WebClient`)** | Dokany | defer (no Windows client yet) |
|---|---|---|---|---|
| who owns the HTTP layer | **we do** — `fsclient` runs in our process, so `--concurrency`, the connection pool, retries and deadlines are ours | **the service does** — its own connections, its own schedule | we do | — |
| the concurrency lever | **expressible**: `Concurrency`/`MaxConnsPerHost` stay the client's (`options.go:64-69`) | **not expressible at all** — no knob, no extension point; the fast path measured in BFS-011/016 is simply gone | expressible | — |
| the snapshot op (`X-Bunker-Op`) | **our binding issues it** at mount through `fsclient` | **impossible** — the redirector sends standards-only WebDAV; no extension headers. The whole-tree read degrades to the PROPFIND walk: 132 requests / 8.2 s on BFS-016's own fixture instead of 1 call / 0.035 s | expressible | — |
| cache control | `SetDirectIO(true)` + `FileInfoTimeout` → our bounded content-addressed cache is the only byte cache, as on Linux (`fs_linux.go:228-249`) | **none** — the kernel service caches on its own terms with no policy API, so "ours is the only copy" cannot be stated, and a stale-content vs local-disk failure can no longer be *classified*, only observed | driver-level, coarser | — |
| invalidation | `FileSystemHost.Notify(path, action)` (WinFsp `FspFileSystemNotify`) | none | `FsRtlNotify*`, not surfaced to Go | — |
| Go binding | `github.com/winfsp/cgofuse` v1.6.0 (MIT; latest tag `b8358bc`, default-branch head `2fa812d`, 2026-05-31), cgo **and** `CGO_ENABLED=0` nocgo | n/a (no API) | **none maintained** — adopting it means hand-writing the bindings | — |
| install cost | WinFsp kernel driver, installed by its own signed installer (admin) | zero install — **but the service is deprecated and off by default** | Dokany driver installer (LGPL) | zero |
| other costs | our binary needs no signing beyond normal Windows practice; CI needs no mingw in nocgo mode | attribute budget (1 MB) and 50 MB file limit per MS Learn, cited in BFS-003 §4 | older FUSE 2.7-level wrapper; slower cadence | nothing ships; the need is unmet |

### 2.2 The deciding argument

**A binding that cannot express the client's concurrency knob throws away the thing that made the client
fast, and the redirector is exactly that binding.**

BFS-011's result is not "HTTP/2 is fast"; it is *concurrency is the cause and the version is the
carrier* — proven by the control arm (one connection, c=1 vs c=8, **1.0002×**: nothing to gain when the
transport serialises) and by the carrier arm (HTTP/1.1 with eight connections, **6.4777×**, the same win
without h2 at all). The redirector sits at the failing end of that control arm by construction: the
requests are made by a Windows service we do not configure, so the in-flight bound is not ours. It is
not that the redirector is slow; it is that **the lever does not exist inside it**, and the release's
central measurement — 8 requests in flight at `--concurrency 8`, measured at the socket — could never be
reproduced through it.

The second measurement closes it. The whole-tree read's win on this release is **not** concurrency: it is
the one-call `X-Bunker-Op: snapshot` (421 nodes, one call, **0.035 s**, zero requests during the walk),
which is why a release note attributing the walk win to concurrency would be wrong on our own evidence
(BFS-016 §3). The redirector **has no extension point** — it is a standards-only WebDAV client — so the
snapshot op cannot be expressed through it at all. It would take the slowest measured path in the whole
study (the 132-request walk) on every mount, forever.

That leaves (a) and Dokany. Dokany is rejected on the same grounds BFS-003 §4 recorded and this row
re-verified at the API level: **there is no maintained Go binding**, so adopting it means writing the
cgo bindings ourselves — precisely the cost that made WinFsp attractive; and its cache-control
granularity is coarser (no `FileInfoTimeout`-class policy), which matters because the cache policy is
one of the four mechanisms this release is *about*.

**Decision: WinFsp via `cgofuse`, opt-in, with the Linux binding untouched.** Same shape as the Linux
client: our process owns the transport, the cache, the invalidation channel, the write precondition and
the snapshot; the driver is a thin adapter.

### 2.3 Why the seam is the *right seam*, and what must be hoisted before it can be used

The seam's **kind** is right: platform-specific code lives in one build-tagged file inside
`internal/fsmount`, and the shared half (`Options`, the mountpoint posture, the mount identity, the
status document) is OS-neutral. What the seam could not do is what its own comment claimed — "callers
(the CLI) compile unchanged" — because it did not compile for any non-Linux platform and did not export
the function its callers call (§5, §7). That is now fixed and guarded.

The plug-in point, exactly:

| where | what happens there today | what the Windows binding does with it |
|---|---|---|
| `internal/fsmount/fs_unsupported.go` | `MountAt(Options) (*Mount, error)` returns the named refusal; the `*Mount` accessors exist only for signature parity | **the file a `fs_windows.go` replaces**, implementing `MountAt` against `cgofuse` |
| `internal/fsmount/fs_linux.go:136` | `MountAt` — normalize → prepare mountpoint → cache dir → `fsclient.NewClient` → bind preflight → cache → write path → snapshot → go-fuse mount | the same sequence, minus the go-fuse mount; a Windows `MountAt` is a sibling function, not a fork of the client |
| `internal/fsmount/options.go:48-97` | `Options` — the driver's own option set, already shared | used **verbatim**; a Windows option that is not here (e.g. WinFsp `FileInfoTimeout`) is added here as policy, not read ad hoc inside the binding |
| `internal/mountdriver/bunkerfs.go:50-62, :67-77` | driver option table + PERMANENT/TRANSIENT fragment tables | unchanged: the unsupported-platform refusal is *already* classified PERMANENT, and this row added the test that keeps it so (`internal/mountdriver/platform_refusal_test.go`) |
| `internal/fsclient/errno.go:36-39` | `portableErrno` — "the single seam where a named errno becomes the value the running platform reports" | the Windows binding maps the named errnos onto Win32 status codes here, not by re-deriving them |
| `internal/cli/fs.go:95-187` | the `bunker fs mount` command; it calls `MountAt` (`:143`) and the accessors; **no build tag** | **unchanged** — that is the point of the seam, and it is now proven by a windows cross-compile rather than asserted |
| `internal/fsmount/fs_linux.go:181` | `UserAgent: "bunker-fs/1 (Linux; go-fuse)"` | **must be hoisted** into `Options` (or a per-binding constant) first: a Windows client must not claim to be the Linux/go-fuse one |
| `internal/fsmount/fs_linux.go:228-249` | the mount-time policy literals (zero attr/entry timeouts, `ExplicitDataCacheControl`, `MaxBackground`/`MaxInflightRequestBytes` derived from `Concurrency`) | **must be hoisted** into one policy type both bindings translate from — the drift BFS-003 §4 warned about ("each binding must translate **one** policy type") |

The `cgofuse` surface a Windows binding implements, verified against upstream `fuse/fsop.go` and
`fuse/host.go` today (v1.6.0):

* `FileSystemInterface` is **path-keyed** — `Getattr(path, stat, fh)`, `Readdir(path, fill func(name string, stat *Stat_t, ofst int64) bool, ofst, fh)`, `Open(path, flags)`, `Read/Write(path, buff, ofst, fh)`, `CreateEx/OpenEx` (direct `FileInfo_t` control). **This is not go-fuse's `fs.Inode` tree**: the Windows binding is a second, thinner adapter over the same `fsclient` state (the snapshot is path-keyed — `Snapshot.Children/Known/Drop`), not a port of `fs_linux.go`.
* The knob map onto our Linux mechanisms: `SetDirectIO(true)` ↔ `FOPEN_DIRECT_IO` + `ExplicitDataCacheControl`; `Notify(path, action)` ↔ `InodeNotify`/`EntryNotify`/`DeleteNotify`; `SetCapReaddirPlus` ↔ the READDIRPLUS negotiation; `SetUseIno(true)` ↔ stable inode numbers.
* **Claim, not measurement:** whether `Notify` invalidates cached **file data** (not just directory contents) on WinFsp, and what `-o FileInfoTimeout=-1` means in practice, are open in BFS-003 §9 and stay open here — both need a Windows box. The invalidation channel must therefore be validated on hardware before the Windows mount is advertised as having parity.

### 2.4 What the decision requires that we do not have

Named, because "Windows support" is not one task:

1. **A Windows host.** Windows 10/11 x64 (and ARM64 if ARM64 is to be claimed). Nothing in this session
   can stand in for it.
2. **WinFsp installed** — a kernel-mode driver with its own installer and admin install step, plus its
   own signing (WinFsp ships signed; *our* signing requirement is a normal code-signing certificate for
   the binary, not a driver-signed package). This is a user-visible install requirement, which is exactly
   why the client must stay **opt-in** and sshfs stays the default.
3. **A release target that does not exist.** `Makefile:15` — `RELEASE_PLATFORMS := linux/amd64
   linux/arm64`, and `internal/releasecheck/releasecheck.go:335` asserts every published binary is a
   **linux** build. A Windows client needs: a new platform in that list, the installer/instructions, and
   the release-check rules widened deliberately rather than by accident.
4. **A new dependency.** `cgofuse` (MIT) is the first non-stdlib FUSE-adjacent dependency the Windows
   path needs; nocgo mode keeps mingw out of CI. Its licensing (WinFsp GPLv3 **with a FLOSS exception**,
   vs this repo's Apache-2.0) is an owner judgement, already flagged in BFS-003 §4 — not decided here.
5. **A CLI unmount path.** `internal/cli/umount.go` is POSIX-shaped (`fusermount3`/`umount`, plus
   `syscall.Stat_t`), which is why the whole-repo windows build still stops there (§7). A Windows unmount
   goes through `cgofuse`'s `FileSystemHost.Unmount`, i.e. the CLI's unmount path needs the same
   platform seam the mount path now has.

### 2.5 The verification plan (what would have to pass, in order)

Nothing below was run — **no Windows host** — and each step names the failure that would stop it:

| # | step | pass criterion |
|---|---|---|
| 1 | cross-GOOS guard green | `probes/cross-GOOS-build.sh` exits 0 with no unlisted gap |
| 2 | WinFsp installed on a real Windows box; `bunker fs mount <mp> --url …` mounts | mount succeeds, and `bunker fs status` reports the same cache/invalidation/snapshot/transport fields the Linux client does |
| 3 | **the concurrency arm** (BFS-016 §3 arms 3-4) through the Windows mount, with the counting relay | 8 parallel cold reads reach **≥ 8 in flight at `--concurrency 8`** (vs 1 at c=1), i.e. the lever exists on Windows |
| 4 | **the snapshot arm** (BFS-016 §3 arm 5) | a whole-tree walk = the snapshot's node count in **1 call**, **0 requests during the walk**; the walk time in the same order as the Linux arm |
| 5 | the 35-cell op battery (BFS-016 §2 instrument), inline timing (not the `timeout`-wrapped columns) | **0 stalls**; the same per-op shape; any deviation is a Windows-specific defect, filed |
| 6 | the not-hang arms (BFS-016 §4) | dead endpoint → named refusal fast; frozen server → bounded failure with a named cause, **mount stays mounted**, recovery without remount |
| 7 | the conflict cell | `412` + `X-Bunker-Verdict: hash_mismatch` + the hash pair; file bytes unchanged |
| 8 | the durability classifier matrix | **local-disk pressure** (write exceeds a local bound → `EFBIG`, local) and **stale content** (`ESTALE`/tree identity → re-read/re-bind) remain **distinguishable** on Windows, as they are on Linux (`internal/fsclient/errno.go:20-34`) — a Win32 mapping that collapses them into one code fails this step |
| 9 | invalidation parity (BFS-003 §9 item 3) | a server-side change is observed by the Windows mount; if `Notify` cannot invalidate file data, the release note says so **and the poll fallback is the declared mode** — never a silent stale read |

---

## 3. PART 2 — Linux Mint: **no work**, and here is what makes that a measurement

The owner's hunch was that Mint might need separate work. BFS-003 §5 already answered *no separate work
for the mount*; this row re-tested that answer against the **merged** client (BFS-003 was written before
the client existed) and against what the client actually does at mount time.

### 3.1 What the client requires at run time — measured on this host, from the merged tree

Raw transcript: `BFS-010-mint-mount-path.txt`.

```
execve("/usr/bin/fusermount3", ["/usr/bin/fusermount3", "/tmp/bfs010/mnt", "-o", "fsname=bunker-fs,subtype=bunker-"...]) = 0
openat(AT_FDCWD, "/dev/fuse", O_RDWR) = 4
mount("bunker-fs", ".", "fuse.bunker-fs", MS_NOSUID|MS_NODEV, "max_read=1048576,fd=4,rootmode=4"...) = -1 EPERM
```

and, with the release flags (`CGO_ENABLED=0`):

```
/tmp/bfs010/bin/bunker-static: ELF 64-bit LSB executable, x86-64, statically linked
not a dynamic executable
```

So the client's whole runtime requirement set is:

1. **kernel FUSE support** (`CONFIG_FUSE_FS`; `/dev/fuse` present) — go-fuse's floor is protocol
   **7.12**, which is Linux **2.6.31** (`fuse/request_linux.go`: `_FUSE_KERNEL_VERSION = 7`,
   `_MINIMUM_MINOR_VERSION = 12`, `_OUR_MINOR_VERSION = 28`; the tag mapping is BFS-003 §6);
2. **a `fusermount` mount helper** for an unprivileged mount — go-fuse looks for `fusermount3` then
   `fusermount` (`fuse/mount_linux.go` `fusermountBinary()`, `lookPathFallback(…, "/bin")`), or
   `DirectMount` with `CAP_SYS_ADMIN`;
3. **nothing else** — no `libfuse`, no glibc version coupling, no interpreter: the binary is static
   (`CGO_ENABLED=0`, the release shape at `Makefile`).

### 3.2 Against Mint's facts

| requirement | Mint (22.x = Ubuntu 24.04) | verdict |
|---|---|---|
| kernel FUSE + protocol ≥ 7.12 | ships LTS 6.8 / HWE 6.14 → protocol **39 / 42** (BFS-003 §5 table) — far above the 7.12 floor *and* above the 7.28 go-fuse prefers | satisfied with margin; no feature lost |
| `/dev/fuse` | from the same `fuse3` package Ubuntu ships (`fuse3 3.14.0-5build1` in noble) | satisfied |
| mount helper | `fusermount3` from that package (this host, Ubuntu-derived, measured: `/usr/bin/fusermount` → `fusermount3`, and the strace above shows the helper actually being exec'd) | satisfied |
| `libfuse` / ABI | **not involved** — static Go binary, no `libfuse` in `ldd` | the class of failure this repo *has* hit before (a cross-compiled `sshfs` failing on another box with `libfuse3.so.4` vs `.so.3`, `docs/sshfs-3.7.6-deployment.md:39`) **cannot happen here** |
| systemd / desktop / display server | not consulted by the client: no systemd unit, no tray, no X/Wayland dependency (repo has no tray target — BFS-003 §5 measured; `cmd/` is `bunker`, `bunkerd`, `docs-drift`) | irrelevant to this client |
| Mint 21.x (= Ubuntu 22.04) | kernel 5.15/6.2 → protocol 31/36, `fuse3 3.10.5` | also above the floor; nothing Mint-version-specific |

**Verdict: the existing linux/amd64 (and linux/arm64) build covers Mint. No separate build, no branch, no
package.** The one thing a *user* may have to do is install `fuse3` if their image lacks it — that is
true of Ubuntu, Debian and every derivative equally, it is a one-line install, and if it is missing the
failure is the kernel/helper error surfaced by go-fuse through `MountAt` (`bunker-fs: mount <mp>: …`),
not a silent wrong result. It belongs in an installation note, not in a Mint port.

### 3.3 BFS-003's one Mint caveat is void for *this* driver

BFS-003 §5 flagged: `-o allow_other` needs `user_allow_other` in `/etc/fuse.conf`, which Mint ships
commented out. **The bunker-fs driver never requests `allow_other`**: it is *stripped*, by design and for
every mount —

* `internal/fsmount/options.go:81-84` — the field exists "so the CLI can say it was stripped rather than silently ignoring the flag";
* `internal/fsmount/fs_linux.go:232` — `AllowOther: false, // STRIPPED: always private, whatever was requested`;
* BFS-016 §1.3 — the live mount table on a real mount carries **no `allow_other`**.

So there is no `/etc/fuse.conf` requirement, on Mint or anywhere else. The caveat is replaced by a
sentence the CLI already prints (and by a live mount's `/proc/mounts` line, the evidence above).

---

## 4. The three release constraints, satisfied

| constraint | how the decision satisfies it | evidence |
|---|---|---|
| **no new server-side requirement** | the Windows binding is a client of the surface `bunkerd` already serves: `OPTIONS` + standards WebDAV, every `X-Bunker-*` extension optional with a standard fallback, HTTP/1.1 fully supported | BFS-016 §1.4 preflight output (`proto: HTTP/1.1 … nothing here requires h2/h3`); `mountdriver/bunkerfs.go:11-17`; no server code in this row |
| **never default; opt-in; sshfs stays the default until replaced** | `DefaultDriver = "sshfs"`; the driver must be asked for by name; the CLI text says it is not the default | `mountdriver/mountdriver.go:25`; `mountdriver/bunkerfs.go:18`; `internal/cli/fs.go:38-55`; BFS-016 §1.1 |
| **its own options AND its own durability/failure classifier; local-disk vs stale-content not conflated** | the driver's option set is its own (`BunkerFSOptions` — no sshfs option appears) and its classifier reads bunker-fs's own named causes; the errno vocabulary keeps **local** (`EFBIG`, EPERM, capability) distinct from **stale** (`ESTALE` per-file, `EREMOTEIO` tree identity, `ENOTCONN` transport) | `mountdriver/bunkerfs.go:50-90`; `internal/fsclient/errno.go:20-34`; `errors.go:31-52`; and on Windows the mapping point is `portableErrno` (`errno.go:36-39`) — plan step 8 (§2.5) is the test that the mapping does not collapse them |

---

## 5. WHAT WAS PROVED (command → result)

Raw artifacts beside this file: `BFS-010-cross-GOOS-matrix.txt`, `BFS-010-mint-mount-path.txt`.

**5.1 The seam did not compile — and now does.** Before this row, every non-Linux build of
`internal/fsmount` failed:

```
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
internal/fsmount/fs_unsupported.go:19:28: Mount (function) is not a type
internal/fsmount/fs_unsupported.go:28:6: Mount redeclared in this block
internal/fsmount/fs_unsupported.go:31:10: Mount (function) is not a type
```

Three separate faults in one 31-line file: a function and a type sharing the name `Mount`; an exported
constructor `Mount(opts)` that no caller uses; and the absence of `MountAt`, which every caller *does*
use (`internal/cli/fs.go:143`), plus the absence of the accessors the CLI calls on the handle. The
file's comment claimed "the signature is the Linux one's, so callers (the CLI) compile unchanged" — that
was false for every platform but Linux, where the file is not compiled at all.

After the rewrite, the committed guard (three consecutive runs agree; transcript in
`BFS-010-cross-GOOS-matrix.txt`):

```
$ bash probes/cross-GOOS-build.sh
TARGET           BUILD      VET internal/fsmount     RESULT
---------------- ---------- ------------------------ ------
linux/amd64      PASS       PASS                     ok
linux/arm64      PASS       PASS                     ok
windows/amd64    GAP(cli)   PASS                     ok
windows/arm64    GAP(cli)   PASS                     ok
RESULT: PASS — every required target builds; the seam type-checks on all of them
```

`GAP(cli)` is §7.1 (`internal/cli/umount.go`), printed on every run with its owner, and it is the *only*
package that stops the windows build now. `VET ... PASS` is `GOOS=<target> go vet ./internal/fsmount`,
which **type-checks the `!linux` test file** (`internal/fsmount/platform_unsupported_test.go`) for each
target. That test can only ever be compiled in this environment — there is no non-Linux host — so vet is
the strongest check available, and it is enough to catch exactly the class of fault above. The same
convention is already used by `internal/hostsetup/owner_nonunix_test.go:14`.

**5.2 The refusal is honest.** Asserted by
`internal/fsmount/platform_unsupported_test.go` (compiled for the non-Linux targets above) and by
`internal/mountdriver/platform_refusal_test.go` (**executed on Linux**, `go test ./internal/mountdriver/`
→ `ok`):

* a refusal that wraps `ErrPlatformUnsupported`, naming `GOOS`/`GOARCH` — a sentence, not a hang;
* **no handle returned** beside the error, so a caller that checks only the handle cannot proceed;
* **no local side effect**: the refusal precedes `PrepareMountpoint`/`MountDir`, so nothing is created
  at the mountpoint (`os.Stat` → `fs.ErrNotExist`);
* the caller's own error is reported as itself — a missing `--url` is never dressed up as "unsupported
  platform";
* `Unmount` **refuses** rather than returning `nil`, so a caller that ignored the mount error cannot
  believe it detached something;
* the refusal text keeps the fragment `internal/mountdriver` classifies as **PERMANENT**, whose stated
  meaning is "retrying cannot help" — so the platform refusal carries an *answer* rather than falling
  through to the classifier's `unknown` default ("the output carried no recognised signal"). `unknown` is
  not retried either (only `transient` is), so the difference is not retry count: it is an attributed
  class versus an unattributed one. RED-proved by mutation in §5.6.

**5.3 The Linux client's runtime requirements are what §3.1 lists** — measured, not remembered: the
helper exec, the `/dev/fuse` open and the `mount(2)` call are all in the strace transcript, and the
static-linkage check is the release build shape (`CGO_ENABLED=0`).

**5.4 Where the "no FUSE binding off Linux" claim actually holds.** The guard's `--survey` arm builds
every platform `runtime.GOOS` can report, and the answer is worth stating precisely rather than
claiming the whole repo is now portable:

```
  darwin/amd64     rc=1 internal/fsclient
  darwin/arm64     rc=1 internal/fsclient
  freebsd/amd64    rc=1 internal/fsclient
  openbsd/amd64    rc=1 internal/fsclient internal/server
  netbsd/amd64     rc=1 internal/fsclient internal/server
  solaris/amd64    rc=1 internal/fsclient internal/registry
  illumos/amd64    rc=1 internal/fsclient internal/server
  plan9/amd64      rc=1 internal/fsclient internal/mountdriver internal/registry
  js/wasm          rc=1 internal/fsclient internal/registry internal/tunnel
```

For **windows**, exactly one package fails (`internal/cli`) and `internal/fsmount` is no longer among
them. For the other `!linux` platforms several packages fail, which is consistent with what this repo
is: `internal/releasecheck/releasecheck.go:335` asserts every published binary is a **linux** build.
The survey is printed as information, never as a pass/fail: it exists so a port's cost is measured
rather than guessed. `internal/fsmount`'s own claim — that platform-specific code lives in this package
and nowhere else — is the narrow claim, and it is now true for windows.

**5.5 Instrument finding (what this row had to fix to report honestly).** The guard's first version
reported the *same* target as `FAIL(cli)` and then as `GAP(cli)` twelve lines apart: its gap lookup was
`printf … | while … done | grep -q yes`, and `grep -q` exits at the first match, SIGPIPE-ing the upstream
stage, which `set -o pipefail` then reports as failure. The bug is fixed (here-string, no pipeline) and
the reason is commented in the script. The wrong first run is kept verbatim in
`BFS-010-cross-GOOS-matrix.txt` §1 rather than deleted, exactly as BFS-011 §6 keeps its own instrument
findings: a guard whose answer depends on which side of a SIGPIPE it lost is worse than no guard.

**5.6 The coupling test is RED-proved — and the RED proof corrected a claim.** `TestPlatformRefusalIsPermanent`
was run against a mutated sentinel: the fragment `no fuse binding on this platform` removed from
`internal/fsmount/options.go`, nothing else changed.

```
--- FAIL: TestPlatformRefusalIsPermanent (0.00s)
    --- FAIL: TestPlatformRefusalIsPermanent/bare_sentinel (0.00s)
        platform_refusal_test.go:59: classifyBunkerFS("bunker-fs: no binding here (…)") = unknown (""), want permanent
    --- FAIL: TestPlatformRefusalIsPermanent/seam_wrapping_with_the_platform (0.00s)
    --- FAIL: TestPlatformRefusalIsPermanent/cli-style_double_wrap (0.00s)
FAIL	github.com/deployBunker/bunker/internal/mountdriver	0.078s
```

Restored byte-for-byte (sha256 identical before and after: `6bd83fd0…9fe93a2`) and re-run green
(`ok … 0.518s`). **The mutation is what corrected a claim in this document's own first draft:** I had
written that the fragment stops the platform refusal being *retried in a loop*, but only `transient` is
retried (`mountdriver.go`), so a reworded sentence would not have been retried either — it would have
been classified `unknown`, "the output carried no recognised signal". What the test protects is an
**attributed class**, not a retry count, and §1, §4 and §5.2 now say that. The `!linux` half of the seam
(`internal/fsmount/platform_unsupported_test.go`) has **no** RED proof here: it can only be executed on a
non-Linux host, which this environment does not have, so its RED proof is one of the things §2.5's
verification plan buys.

---

## 6. WHAT WAS **NOT** PROVED (and is a claim, not a measurement)

* **Anything on Windows.** No mount, no driver, no `Notify`, no `FileInfoTimeout`, no performance number.
  The `cgofuse` API surface in §2.3 is *upstream source read today* (v1.6.0 files `fuse/fsop.go`,
  `fuse/host.go`) — not exercised. The knob equivalences are naming equivalences.
* **That the `!linux` seam test PASSES anywhere.** It is type-checked for every non-Linux target
  (`go vet`) and has no RED proof (§5.6): the environment has no non-Linux host to execute it on. What is
  proved is that it compiles and that its assertions are the ones written — not that they hold at run time.
* **A green `GOOS=windows go build ./...`.** It still stops in `internal/cli` (§7.1); the guard reports
  that as a listed GAP on every run rather than hiding it.
* **WinFsp's behaviour under our workload**, including the open BFS-003 §9 items: whether `Notify`
  invalidates cached file **data**, and the practical semantics of `FileInfoTimeout=-1`.
* **The Windows redirector was never run.** BFS-003 §4 recorded MS Learn's deprecation, the 1 MB
  attribute budget and the 50 MB file limit as *citations*; BFS-012 tested `curl`, not the redirector, so
  "what a stock Windows client can do against this surface" is still unmeasured. The decision above does
  not depend on it (the redirector is rejected as a *product* path either way), but the compatibility
  claim does.
* **A Mint machine.** No Mint host was booted. §3's verdict rests on (i) the client's measured runtime
  requirements, (ii) go-fuse's floor read from the vendored source, and (iii) Mint's base/version facts
  from BFS-003 §5, which cites Linux Mint release notes and the Ubuntu package index. Point (iii) is a
  citation, not a measurement made by this row.
* **A live mount from this session.** `mount(2)` returned `EPERM` here (§7.3) — an environmental block,
  not a client defect: the same mount path mounted successfully earlier today (BFS-016's transcripts show
  live mounts in `/proc/mounts`). This row therefore measured the *path* (exec/open/mount attempt) and
  the *refusal behaviour*, and relied on BFS-016 for the mounted case.
* **The `>50 MB` / attribute-budget limits of the OS redirector**, and everything else about a Windows
  run.
* **Performance parity on Windows.** `cgofuse` is a different binding with its own request loop; no
  claim is made that Windows would reproduce the Linux wall times, only that the *mechanisms* the Linux
  speed comes from (our transport, our cache, the one-call snapshot) are preserved.

---

## 7. Defects found and deliberately NOT fixed

The row's rules ask for a filing, not a silent fix. Both of these are outside `internal/fsmount` (the
seam this row owns) and both are reproduced with one command.

**7.1 `internal/cli/umount.go:272-279` — POSIX-only, blocks the Windows build (and the whole point of
the CLI seam).** `isMountPoint` compares `syscall.Stat_t.Dev` of a path and its parent:

```
$ GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
# github.com/deployBunker/bunker/internal/cli
internal/cli/umount.go:272:25: undefined: syscall.Stat_t
internal/cli/umount.go:273:20: undefined: syscall.Stat
```

Its own comment says it "behaves the same on every platform this CLI builds for" — the CLI builds for
Linux only, and a Windows unmount would go through `cgofuse`'s `FileSystemHost.Unmount`, not
`fusermount3`/`umount` (`umount.go:313-325`). This is the one site that stops
`GOOS=windows go build ./...` today, and it is in `KNOWN_GAPS` of the committed guard so it cannot rot
silently. **Fix owner: whichever row owns the CLI's platform seam** — the shape is `mountpoint_dev_unix.go`
/ `mountpoint_dev_windows.go` plus a Windows unmount branch, and it should not be written without a
Windows box to run it on.

**7.2 `internal/fsclient/errors.go:158` — `syscall.EREMOTEIO` is not defined off Linux for every GOOS.**

```
$ GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
# github.com/deployBunker/bunker/internal/fsclient
internal/fsclient/errors.go:158:15: undefined: syscall.EREMOTEIO
```

Windows *does* define it (windows builds reach `internal/cli`), darwin/freebsd do not. It is a
one-token fix that uses a mechanism the package already built for exactly this — the package defines
`ErrnoEREMOTEIO syscall.Errno = 121` (`errno.go:33`) and `portableErrno` precisely because "the Windows
syscall package does not define ESTALE, EREMOTEIO or ENOTCONN" (`errno.go:8-13`); the `ErrnoName` switch
just reaches for the platform constant instead. Not fixed here: it is a different package, a different
platform, and — per the row's rule — the fix is reported, not smuggled.

**7.3 This session cannot mount (environmental, unattributed).** `fusermount3` execs, opens `/dev/fuse`,
calls `mount(2)`, and gets `EPERM`; the client reports it and exits 1 within ~4 s, leaving nothing at the
mountpoint (empty listing, absent from `/proc/mounts`). Diagnosed as far as: `CapEff=0`, `Seccomp: 0`,
`NoNewPrivs: 0`, AppArmor `unconfined`, `/` not `nosuid` — and `unshare -rm` cannot write its uid map
either. BFS-016 mounted successfully on this host hours earlier, so this is a property of *this* worker
session's sandbox, not of the client. Recorded because it limits what this row could measure, not
because it is a client defect.

---

## 8. Reproducing this row

```bash
# the seam's compile surface, for every target in the required set, with the gaps printed
probes/cross-GOOS-build.sh            # add --survey for the wider platform list
go build ./... && go vet ./... && go test ./internal/mountdriver/ -count=1
GOOS=windows go vet ./internal/fsmount      # type-checks the !linux seam test

# the Mint half: what the mount path actually does
CGO_ENABLED=0 go build -o /tmp/b/bunker ./cmd/bunker && file /tmp/b/bunker   # static
strace -f -tt -e trace=execve,openat,mount -o /tmp/b/trace \
  /tmp/b/bunker fs mount /tmp/b/mnt --url http://127.0.0.1:18481/dav        # helper + /dev/fuse
```

**Files changed by this row:** `internal/fsmount/fs_unsupported.go` (rewritten), `internal/fsmount/options.go`
(seam doc + refusal text), `internal/fsmount/platform_unsupported_test.go` (new, `!linux`),
`internal/mountdriver/platform_refusal_test.go` (new, runs on Linux), `probes/cross-GOOS-build.sh` (new),
plus this file and its two raw transcripts. **No product behaviour changed on Linux**: the Linux binding,
the client, the server and the driver tables are untouched, and the CLI is unchanged — that is what the
windows cross-compile proves.
