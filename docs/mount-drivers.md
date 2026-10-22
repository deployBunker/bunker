# Mount drivers — sshfs (default), rclone, and FUSE over io_uring

**Status:** sshfs default; rclone opt-in implemented (MOUNT-007);
`fuse-io_uring` implemented as a verified, opt-in FUSE *transport* (MOUNT-008) —
see §3.3, which also corrects the earlier measurement that wrongly reported the
packaged libfuse as lacking the transport. This document also records the sshfs
security issue that forces an upgrade, because the fix and the driver model
touch the same code. **§5 records the MOUNT-017 remote-filesystem latency
decision: git stays on the agent (SURF-018) and `bunker cp` carries bulk
transfer — measured on a verified direct tailnet path, not a relay.**

See also: [`both-ways.md`](both-ways.md) (when to use the mount vs the verb path),
[`performance.md`](performance.md) (what caps throughput on a long path: the socket-buffer
ceiling, and why HTTP/2 walks straight into it),
[`prd/ADR-toolsd-integration.md`](prd/ADR-toolsd-integration.md) (the verb path),
`SECURITY.md`,
[`mount-watchlist.md`](mount-watchlist.md) (mount technologies we are
*watching*, not building: what each is, why it is or is not relevant here, and
the named trigger that would make us pick it up).

---

## 1. The security issue that forces an sshfs upgrade (read this first)

Two vulnerabilities in **sshfs itself** were fixed in **3.7.6**:

| CVE | Kind | Severity |
|---|---|---|
| `CVE-2026-47187` | Symlink escape — a **rogue SFTP server** gets **local file read/write on the client** | 9.3 critical |
| `CVE-2026-48711` | Argument injection — local command execution | high |

- **Affected:** sshfs `<= 3.7.5`. **Fixed in:** `3.7.6`. Upstream: `oss-security`
  2026-05-30, release tag `sshfs-3.7.6`.
- **Our exposure (measured 2026-09-20):** the control host runs `3.7.3-1.1build5`
  and the daemon host `78.46.173.180` runs `3.7.3-1.1build3`. **Both are in the
  affected range.** The Ubuntu package has no fixed candidate, so an upgrade means
  building `3.7.6` (or taking a newer release) rather than `apt upgrade`.
- **Why it matters here specifically:** the mount **trusts the agent**, and the
  agent is the component we deliberately run untrusted code in. The failure
  direction is agent → **your workstation**, not the other way round.
- **The documented `transform_symlinks` mitigation does NOT cover it.**
  `transform_symlink()` returns early (`sshfs.c:2181`) while `sshfs_readlink()`
  otherwise copies the server-supplied relative link target to the kernel
  (`sshfs.c:2234`). Do not rely on that option as a workaround.

**Until the upgrade lands, treat `bunker mount` as write-capable-from-the-agent
and only mount agents you would let write to your local filesystem.**

**Mitigations now shipped (MOUNT-009):** sshfs 3.7.6 has been built from the
pinned upstream tag and deployed to the control host and bunker-mvp (see
`docs/sshfs-3.7.6-deployment.md`), and two code defenses exist:

1. **Install-time option** — `install.sh --sshfs[=min|package|source]`
   optionally hardens sshfs after the bunker install. Default (no flag): the
   installer never touches sshfs. `min` installs the distro's newest sshfs when
   missing or < 3.7.6 and warns if the result is still affected; `package`
   refuses (42) when the distro cannot provide >= 3.7.6; `source` builds the
   pinned `sshfs-3.7.6` tag (HEAD verified against the pinned commit, sha256
   recorded before install) with meson/ninja.
2. **Refuse-fast guard** — `bunker mount` probes `sshfs --version` once before
   any mount attempt. An sshfs < 3.7.6 (or an unprobeable/unparsable one)
   prints a warning naming both CVEs and continues; `--sshfs-require-patched`
   turns that into a refusal before any sshfs exec or mountpoint creation.

Plan of record: build and ship sshfs `>= 3.7.6` for our deployments, offer it as
an **install-time option**, and keep this document as the record. Tracked as
`MOUNT-009` on the board — the install-time option and the mount-time guard
have landed (see above); deploying 3.7.6 to remaining hosts is operational work.

#### Comparative leg recorded (MOUNT-010): the unpatched client actually crosses the boundary

The MOUNT-002 close (2026-09-21) delivered the three-case escape battery on the
patched 3.7.6 client only; the promised comparative leg through an **unpatched**
client was still owed. Recorded 2026-10-22 (evidence logs `/tmp/m3t/`,
scratch state since removed): the §1 probe was run against a live sshfs mount
served by `/usr/bin/sshfs 3.7.3` (the distro build still on the control host),
with a same-topology **3.7.6 control** run back-to-back (`/usr/local/bin/sshfs`).

Topology deviation from MOUNT-002, on purpose and stated here: the served tree
was a self-contained local endpoint on the control host itself — a dedicated
scratch user `bunker-m3t` (uid 1001, locked password, key-only SSH) whose
`/home/bunker-m3t` was mounted locally over sshd — instead of a live bunkerd
agent on bunker-mvp. Same shape (untrusted user's home served over SFTP-FUSE,
client mounts with `idmap=user`, `allow_other`, `dir_cache=no`, no
trust-widening options), no live tenants involved or at risk.

**Result: 3.7.3 crosses the boundary in every direction where the target's own
Unix permissions allow it — the fix is not advisory, it is the only thing
standing there.**

- **(a) reads through escape symlinks — all SUCCEEDED under 3.7.3.**
  `escape-hostname -> /etc/hostname`, `escape-passwd -> /etc/passwd`,
  `escape-canary -> /srv/<outside-tree>` and `escape-client-key ->
  /home/kara/.ssh/id_ed25519_bunker` all read through the mount with byte-exact
  hashes matching the server files (e.g. canary sha256
  `c70202e5…f8ff8` == server `/etc/hostname`). `/etc/shadow` read only while it
  was briefly mis-permissioned 644 during setup; after restoring `640 root:shadow`
  the read through the mount is denied exactly like direct server-side access —
  the vulnerable client grants NO privilege escalation, it grants
  **tree escape**: outside-the-tree files appear in the mount.
- **(b) write-through — SUCCEEDED under 3.7.3.** A symlink to a
  user-writable file outside the tree (`/tmp/b3n3n3-witness`) was written
  through the mount (`rc=0`); the server-side witness picked up the exact
  payload (client and server sha256 both `5558351b…`). A root-owned target
  (`/etc/hostname`) correctly refused (`EPERM`) — ownership rules still apply.
  Under 3.7.6 on the identical tree, the same write returned
  `Operation not permitted` with the server witness byte-unchanged.
- **(c) readlink/follow-stat — all SUCCEEDED under 3.7.3.**
  `readlink` returned the full escape target (`'/etc/hostname'`,
  `'/home/kara/.ssh/id_ed25519_bunker'`), `lstat` returned the link
  (`0o120777`, target length as size), and follow-`stat` resolved the outside
  target (`size=22 uid=0`, `size=432 uid=1000`).
- **3.7.6 control, identical topology: every case refused with `EPERM`
  ("Operation not permitted")** — reads, write-through, readlink — and the
  server-side witness was byte-unchanged after the refused write; `lstat`
  still shows the link but never resolves it. This matches the MOUNT-002
  live-agent transcripts verbatim, now reproduced in the self-contained
  topology for direct A/B.

Consequence: the "treat `bunker mount` as write-capable-from-the-agent"
advisory above understates an *unpatched*-client mount — reading and following
escape links is equally exposed, and 3.7.3's early-return `transform_symlink()`
(sshfs.c:2181) never intervenes. The refuse-fast guard (`--sshfs-require-patched`)
is the load-bearing defense; `transform_symlinks` remains useless as a
workaround, as recorded in §1.

## 2. The driver model

Today the mount command is **built server-side at spawn** and handed to the client
as an opaque string (`sshfs_mount` in the protobuf), which the CLI splits into
fields and executes. There is **no driver abstraction** — which is why adding a
driver is a seam change, not a one-line flag. That seam is tracked as `MOUNT-006`.

Design rules for every driver:

1. **No new server-side requirement.** A driver must work against the SSH service
   the agent already has. If it needs a daemon, a port, a credential or an
   installed package *on the agent*, it does not qualify.
2. **Not default.** sshfs stays the default; alternates are opt-in per mount.
3. **Extra options are the point.** Each driver exposes its own tuning knobs,
   and they are documented rather than hidden.
4. **Durability carries over.** The existing retry/classification behaviour
   (transient vs permanent failure, bounded attempts) is currently **sshfs-shaped**.
   A new driver must bring its own equivalent, or the durability guarantees
   silently do not apply to it.

To request a driver when creating an agent, set `SpawnAgentRequest.mount_driver`
(the CLI equivalent is `bunker spawn --mount-driver <name>`). An empty value
selects the server's `sshfs` default; a name that is not registered is refused
with `CodeInvalidArgument` rather than silently falling back. The selected
driver is persisted with the generated mount command and returned in
`MountSpec`, while the legacy `sshfs_mount` field remains available for older
clients.

## 3. Drivers

### 3.1 sshfs — default

- **Server-side requirement:** none beyond the `sshd`/`sftp-server` the agent already runs.
- **Transport:** SSH/SFTP, per-agent key.
- **Durability:** the implemented retry layer (bounded attempts, transient vs
  permanent classification, `-o reconnect`).
- **Known weakness:** one SFTP connection with no pipelining, so it is latency-bound
  and poor at many-small-file workloads; upstream issue #300 measures a large gap
  against rclone over high-latency links.
- **Security:** see §1 — upgrade required.

### 3.2 rclone mount, SFTP backend — opt-in

Speaks SFTP over the same SSH service, so rule 1 holds: **nothing to install on the
agent.** The `rclone` binary is a **client-side prerequisite** — it must be present
on the host that runs the mount, never on the agent. Select the driver per agent at
spawn with `--mount-driver rclone` (an agent spawned without it keeps the sshfs
default; an unknown name is refused by the server).

- **No config file required.** Use an inline backend spec so there is no
  `rclone.conf` and no credential file to manage:
  `rclone mount :sftp,host=<host>,user=<agent-user>,key_file=<key>,shell_type=unix:/ <mountpoint>`
- **Mode parity with sshfs.** The stored command pins `--file-perms 0777 --umask 022`,
  because rclone's FUSE layer reports FILES from `--file-perms` masked by `--umask`
  (its defaults, 0666 and 002, surface every file as `rw-rw-r--`) and never from the
  SFTP object's mode: without both options a `chmod 755` file loses its exec bit and
  `./built-binary` off the mount fails with `Permission denied`. The effective
  `0755 rwxr-xr-x` is what the sshfs driver reports for the same file.
- **Extra options it brings** (the reason to offer it):
  `--vfs-cache-mode off|minimal|writes|full`, `--dir-cache-time`,
  `--vfs-cache-max-size`, `--vfs-cache-max-age`, `--vfs-write-back`,
  `--vfs-read-chunk-size` / `--vfs-read-chunk-size-limit`, `--buffer-size`,
  `--transfers`, `--checkers`, `--umask`, `--sftp-set-modtime=false`,
  `--sftp-disable-hashcheck`, `--retries`, `--low-level-retries`.
- **Notes for the implementer:** the distro candidate is old (`1.60.1` on this
  host) — pin the version explicitly and prefer the current official static
  binary; rclone has its **own** retry/backoff, which overlaps the sshfs-shaped
  classifier and must be reconciled rather than stacked.
- Implemented as `MOUNT-007` (commit `10760912`) — registered in
  `internal/mountdriver/rclone.go`, selected with the `--mount-driver rclone`
  spawn flag above.

### 3.3 FUSE over io_uring — opt-in, verify-then-enable (MOUNT-008)

A transport change for FUSE itself, not a filesystem: it moves the
kernel↔userspace request channel onto io_uring and cuts per-request overhead
(measured gains on the order of 1.4–2.8x on relevant workloads).

- **Requires all three:**
  1. a kernel with `CONFIG_FUSE_IO_URING` (>= 6.14);
  2. a **libfuse built with io_uring support**;
  3. the module parameter enabled: `echo 1 > /sys/module/fuse/parameters/enable_uring` (root).
- **Mount option:** `-o io_uring` — spelled with an **underscore**. It travels
  `sshfs → libfuse3 → fusermount3 → mount(2)`, which is where it is consumed;
  libfuse's own help text is `    -o io_uring            enable io-uring`.
  `--driver=fuse-iouring` adds exactly this option (and nothing else) to the
  argv.
- **Client-host property.** It applies where the mount happens, so the probe
  runs on the client, not on the agent.

**How to select it:**

```sh
bunker mount <agent-id>                      # --driver=auto (default)
bunker mount <agent-id> --driver=fuse        # plain FUSE transport, never probes
bunker mount <agent-id> --driver=fuse-iouring  # require it, refuse if unavailable
```

`auto` (the default) uses the io_uring transport **only** when all three
conditions hold, and otherwise mounts exactly as it did before this row — no
extra output, no argv change. `fuse-iouring` is a *requirement*, not a
preference: on a host that cannot provide the transport the command refuses
with a named error before it creates a mountpoint or executes anything.
Setting `BUNKER_FUSE_IOURING=off` pins the plain transport every time.

#### Corrected measurement — the earlier record in this section was wrong

The first version of this section (2026-09-20) claimed the packaged libfuse
"exposes **no** io_uring transport ... so it is not available in the current
build". **That reading was wrong**, and it was wrong because of a probe-path
bug rather than a missing feature: the check ran

```
nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3
```

and on this host that file **does not exist** — the SONAME here is
`libfuse3.so.4` (real file `libfuse3.so.3.18.2`, package version 3.18.2). `nm`
failed with `No such file`, grep found nothing, and "no output" was read as "no
support". Re-measured 2026-10-14 on the control host (kernel
`7.0.0-31-generic`):

| check | command | result |
|---|---|---|
| libfuse3 transport | `nm -D /usr/lib/x86_64-linux-gnu/libfuse3.so.3.18.2` | `T fuse_req_is_uring@@FUSE_3.18` — **present** |
| libfuse3 option table | `strings …/libfuse3.so.3.18.2 \| grep -i uring` | `io_uring`, `io_uring_q_depth=%u`, `-o io_uring            enable io-uring` — **present** |
| kernel | `uname -r` + `grep CONFIG_FUSE_IO_URING /boot/config-$(uname -r)` | `7.0.0-31-generic`, `CONFIG_FUSE_IO_URING=y` — ready |
| kernel switch | `cat /sys/module/fuse/parameters/enable_uring` | **`N`** — the transport is DISABLED |
| sshfs | `strings $(which sshfs) \| grep -i uring` | none — expected: sshfs is the filesystem, the option belongs to libfuse |
| fusermount3 | `strings $(command -v fusermount3) \| grep -i uring` | none — it forwards mount options opaquely, so its silence is not evidence |

**Finding:** on the control host the library and the kernel are both ready and
**the only unmet condition is the runtime switch**:

```sh
# root
echo 1 > /sys/module/fuse/parameters/enable_uring
bunker mount <agent-id> --driver=fuse-iouring
```

**Daemon host `78.46.173.180` (re-measured 2026-10-14):** kernel
`6.8.0-142-generic` — below the 6.14 floor and with no
`enable_uring` parameter at all — and libfuse3 `3.14.0` (no `uring` symbol or
string in it). Both halves are absent there, so its verdict is a plain
`unsupported` (no io_uring FUSE), not `unknown`.

#### What the probe does (and why it is three-valued)

`internal/cli/mount_iouring.go` implements a runtime probe that answers "can
someone here mount over io_uring?" by checking the same three conditions, then
classifying each as `supported` / `unsupported` / **`unknown`**:

1. **libfuse3 transport** — the library is *resolved* at runtime (`ldconfig
   -p`, then the standard lib directories globbed, then `$BUNKER_LIBFUSE3`)
   rather than hard-coded, and surveyed with `nm -D` + `strings`.
2. **kernel release** >= 6.14.
3. **`enable_uring`** = enabled.

The third value matters: a *failed read* (missing file, missing tool, unreadable
`/sys`) is `unknown`, never `unsupported`. Collapsing those two is exactly the
bug that produced the wrong record above. The probe is also bounded
(`iouringProbeCommandTimeout`) so a wedged survey tool cannot stall a mount, and
a failed attempt is bounded separately by the named
`iouringAcceptanceProbeTimeout` — the transport's live acceptance probe is the
first mount attempt that carries `-o io_uring`, not a separate throwaway mount.

Tracked as `MOUNT-008`.

## 4. The write shape of a `bunker fs mount` (DF-BUNKER-100)

A bunker-fs mount is **not** an sshfs mount, and the difference that costs an
afternoon is its write shape. Until this section existed, the mount's pitch could
not be exercised as documented: a repo was cloned onto the mount, `git init` was
run there, and it failed with an error nothing in the docs explained.

Stated once, in the words the CLI prints (`bunker fs mount` help), repeated by the
mount itself at the moment of mount, and carried on every refusal:

> **WRITE SHAPE CONTRACT — the mount publishes WHOLE FILES only.**
>
> - A file is **created or replaced by ONE whole-file `PUT` ("publish")**.
>   `cp FILE <mount>/path`, `tee <mount>/path` and an editor's save all work, and
>   the bytes read back are byte-identical to the bytes written (measured: dogfood
>   run 27, `docs/dogfood/2026-10-16b-bunker-fs-surface.md` — 82 KB and 287 KB
>   copies verified byte-for-byte).
> - **Shell redirection and in-place edits are NOT supported.** `>` and `>>` onto
>   a path that **already exists** are refused with `EOPNOTSUPP` — the shell
>   prints `Operation not supported` — and the file that was there is left
>   byte-identical. There is no in-place write; an append is at best a bounded
>   whole-file re-publication (BFS-021, `--write-buffer-max-bytes`), never an
>   ordinary append, so no workload should be built on one.
> - **git does NOT work on the mount.** git writes through lockfiles it chmods
>   and renames into place, and this surface carries neither mode changes nor
>   those rename shapes: `git init` fails with `chmod on .git/config.lock failed:
>   Operation not supported`. A working copy cannot live under the mount.
> - **Remediation.** Run git IN the agent (`bunker exec <agent> -- git …`) or
>   clone there, then publish results onto the mount as whole files; for a git
>   *question* that needs no working copy, use the mount's delegated verbs
>   (`bunker fs op` — `status`, `diff`, `rev-parse`, `ls-files`; the served-vs-
>   build matrix is BFS-005 §6.1).
>
> The sshfs mount (`bunker mount`, §3.1) is unaffected by any of this: it is a
> real POSIX filesystem and supports in-place writes.

Where the contract is enforced and reported, so a reader can check it rather than
trust this page:

- `node.Write` (in-place write through a handle that is not a create) and
  `node.truncate` (a resize with a live write-intent handle) both refuse with
  `EOPNOTSUPP` and the named cause `write_shape_unsupported`, counted in the
  `write_shape` block of the mount's `status.json`
  (`refusals_total` + the last refused `<path>`), and logged with the contract
  and the remediation.
- `Setattr` refuses `chmod`/`chown`/`utimes` (mode, ownership and timestamps are
  not carried by the surface) — this is the syscall git dies on. FUSE carries no
  text back to the caller, so the explanation goes to the mount log and to the
  contract above; the errno the kernel hands out is necessarily the bare
  `EOPNOTSUPP`.
- The refusal's `Detail`/`Error()` text in `internal/fsclient/errors.go` appends
  the contract's compact form to **every** `write_shape_unsupported` refusal, so
  `bunker fs status`, the conflict log and the mount log all name the shape that
  works and the way out.

## 5. Remote-filesystem latency — the direct-path decision (MOUNT-017)

**Decision (2026-10-22): git stays ON the agent (SURF-018).** The mount remains a
fidelity surface — inspect, grep, read, diff, small whole-file edits — and the one
thing that crosses the wire in bulk is file transfer, which is `bunker cp`
(SURF-014). This section records why that is structural rather than a preference:
even on a **verified direct** tailnet path, one *uncached* sshfs metadata
operation costs one round trip (~190 ms to the agent measured here), so whole-tree
git ops are bounded by `op count × RTT`, and RTT is geography — not the relay.

### 5.1 What the row claimed, and what was re-measured

MOUNT-017 (2026-09-23) attributed the sshfs/git slowness to the agent-host path
being a **Tailscale DERP relay** with no port mapping — ~142 ms per metadata op
with ~91 ms of jitter — and set a two-armed PASS: a direct path with a material
drop in the per-op figure, **or** a written decision. Re-probed 2026-10-22 on
this control host (kernel `7.0.0-38-generic`, Tailscale `1.102.3`) against a live
mount of `bunker-mvp:/root/mount017-target` on `/tmp/mount017-mnt` — sshfs
`3.7.6` (`/usr/local/bin/sshfs`, the MOUNT-009 patched build) over
`fusermount3 3.18.2`, mounted with `-o IdentityFile=… -o ServerAliveInterval=15
-o reconnect`, created 14:41 by an earlier tick. **Probing was read-only: no
mount was created, dropped or reconfigured, and the peer was not touched.**

The row's own peer `bunker-las-02` is **offline (last seen 9 days)** and
`tailscale ping bunker-las-02` returns `no reply`, so it could not be
re-measured. Its 2026-09-23 numbers are retained unchanged as the baseline:

| quantity | 2026-09-23 baseline (bunker-las-02, relayed) | 2026-10-22 probe (bunker-mvp, direct) |
|---|---|---|
| path | DERP(`lax`) relay, `PortMapping:` EMPTY | **direct** `78.46.173.180:41641` |
| RTT min/avg/max/mdev | 128.6 / 179.7 / 382.2 / **91.1 ms** | 167.7 / 168.1 / 169.1 / **0.30 ms** |
| sshfs metadata, distinct files | 141.8 ms/op (cache=yes), 154.1 ms/op (cache=no) | **188.8–198.5 ms/op cold** (four runs); 3–7 ms/op warm (cached) |
| sshfs bulk write | 2.04 MB/s (raw pipe 1.97 MB/s) | not re-measured — row finding stands |

The path verdict is not an inference, it is what the tools printed:
`tailscale status` → `active; direct 78.46.173.180:41641`, and
`tailscale ping -c 20 bunker-mvp` → `pong from bunker-mvp (100.115.67.116) via
78.46.173.180:41641 in 168ms` — an endpoint pair, not `via DERP(...)`. So **a
direct path is attainable today** for a peer with a public address, and that half
of the row's FIX needs no owner action.

**The path is not sticky.** In a later run of the same probe the tunnel was
momentarily back on the relay — `pong from bunker-mvp … via DERP(nue) in 174ms`,
with the RTT series going 179.4 / 208.2 / 616.3 ms, mdev **97.8 ms** — and
`tailscale status` returned to `active; direct 78.46.173.180:41641` seconds later.
So the direct path is upgraded on demand and falls back to DERP when the link is
idle; a single "is it direct?" check at the top of a run does not describe the
whole run. `probes/mount017-path-probe.sh` records the path state **paired with
each cold op** for that reason.

What did **not** follow was a lower per-op metadata cost. Cold operations — the
ones no cache can answer — five samples per run, four runs over ~10 minutes
(188.8–198.5 ms across every sample):

| cold op (guaranteed wire) | mean per run (ms) | notes |
|---|---|---|
| `stat -f` (statfs, one round trip) | **193.3 / 190.4 / 191.7 / 189.4** | sample spread 188.8–198.5; the last two runs were paired with `tailscale status`, **direct at every sample** |
| `stat` on a never-seen name (negative lookup) | **195.6 / 190.6 / 189.9** | one 559.8 ms outlier landed on a relay transition |
| `cat` a 4 KB file (open + read + close) | **381.8 / 379.4 / 375.9** | ≈ 2 RTT, as FUSE implies |

That is **one uncached metadata op = 1 RTT + ≲25 ms of FUSE/sshfs userspace
overhead** (measured pairs: 190.4 − 168.1 = 22 ms; 189.4 − 179.9 = 10 ms — the
direct-path RTT itself drifted 168 → 180 ms across the hour, and the per-op cost
moved with it). Directness removed the relay's jitter and its bandwidth cap; it
cannot remove the round trip, because FUSE issues one request per operation and
the agent is ~168–180 ms away on a direct path too.

### 5.2 The measurement trap, in both directions

The row warned that re-`stat`ing the **same** file looks ~16x faster because it
is cached. This probe found the mirror-image trap: **distinct files are not
enough — the directory cache has to be cold too.** A 30-distinct-file loop
measured **2.95 ms/op**, which is impossible on a 168 ms link; those entries had
been warmed by an earlier `ls -la` on the same mount. Re-checked after 100 s
idle they were still **4.7–7.5 ms/op**, so on this build the userspace stat cache
outlives the documented `-o dcache_stat_timeout` default (20 s) — a "distinct
files" loop over an already-walked tree under-reports by ~30x.

Every figure quoted in §5.1 therefore comes only from operations that cannot be
cached (`statfs`, a never-before-seen path name, a first content read). Any
future probe of this row must do the same and must say which class it reports;
`probes/mount017-path-probe.sh` measures both and labels them.

### 5.3 Why a direct path does not rescue git-over-mount

A 60 s command budget divided by 190 ms/op is **~315 uncached metadata ops**.
`git status` on the SURF-018 repo — 106 tracked files, i.e. a *small* repo — reads
the index, walks the tree and stats every entry: thousands of ops, which is
exactly why it stalled past 60 s there. The same arithmetic bounds it on the
direct path, because the cost is `op count × RTT`. The relay was never the whole
story: it added **jitter** and a throughput cap, but the floor was always the
round trip.

Relayed peers measured the same day, for the jitter contrast (20 ICMP samples
each over the tailnet):

| peer | path | RTT min/avg/max/mdev |
|---|---|---|
| `bunker-mvp` | **direct** `78.46.173.180:41641` | 167.7 / 168.1 / 169.1 / **0.30 ms** |
| `bunker-mvp` | momentarily relay `nue` (idle fallback) | 179.4 / 208.2 / 616.3 / **97.8 ms** |
| `cube-las-00` | relay `lax` | 142.0 / 150.4 / 193.5 / **13.3 ms** |
| `master001` | relay `lax` | 144.3 / 170.1 / 467.5 / **69.0 ms** |

Same peer, same day: the relay state carries **0.3 ms → 97.8 ms** of mdev and a
616 ms tail. Note also that a relay's *average* can look fine when the peer sits
near the relay (`cube-las-00`): the cost shows up in the tail — which is
precisely what a synchronous FUSE filesystem waits on — and in bandwidth.

### 5.4 Why the `bunker-las-0x` peers still cannot go direct

`tailscale netcheck` on this host, 2026-10-22 (two runs, 17:32 and 17:38):
`UDP: true`, `IPv4: yes, 181.129.43.218:11174`, `IPv6: no`,
`MappingVariesByDestIP: true`, **`PortMapping:` EMPTY**, `Nearest DERP: Miami`
(mia 35.4–51.1 ms, lax 94.5–109.5 ms, nue 169–190 ms). An empty port mapping plus
a mapping that varies by destination IP means the local router
(`192.168.123.1`) is a **hard NAT**: it offers Tailscale no endpoint-independent
mapping, so hole punching to a peer that is itself behind a hard NAT fails and
the pair falls back to DERP. That is the mechanism behind the row's `DERP(lax)`
observation — a router fact, not an sshfs or SSH fact.

- **Owner action that would change it:** enable UPnP/NAT-PMP on the local router,
  or configure a **static UDP 41641 forward** to this host, so our side presents
  an endpoint-independent mapping; peers behind a cone NAT then punch through and
  the DERP hop disappears.
- **Per-peer relay preference:** none is exposed in this Tailscale version —
  `tailscale debug prefs` (1.102.3) carries no per-peer relay/region key, so the
  relay is chosen by the shared nearest-DERP heuristic. What *is* actionable is
  the observed gap: **mia 35.4–51.1 ms vs lax 94.5–109.5 ms**, i.e. 43–74 ms per
  relayed op if the relay assignment can be influenced at all.
- Neither measure changes §5.3: they remove jitter and recover bandwidth, they do
  not remove the round trip.

### 5.5 The decision, and what it does not change

1. **git runs on the agent** — `bunker exec <agent> -- git …`, or clone there.
   The SURF-018 stalls are structural (`latency × per-file ops`), and this probe
   shows that structural cost survives a direct path.
2. **The mount stays a fidelity surface** — inspect, grep, read, diff, small
   whole-file edits; nothing git-shaped runs on it.
3. **Bulk transfer crosses the wire with `bunker cp`** (SURF-014), never through
   the mount and never as a git working copy over FUSE.
4. **No product behaviour changed by this row** — no driver, flag, default or
   mount option was modified; this is a documentation/evidence decision backed by
   a read-only probe.
5. **A direct path is still worth having** (jitter, throughput, interactive
   latency) and is already available for public-address peers; the `bunker-las-0x`
   family needs the router change in §5.4 — an owner action, not a code change.

Reproduce: `probes/mount017-path-probe.sh <mountpoint> <peer> [samples]`
(read-only — tailscale status/netcheck/ping, ICMP RTT, cold `statfs` /
negative-lookup / cold-read timing, a warm-loop control, and a local baseline).

---
ch:trace row=MOUNT-017 spec=docs/mount-watchlist.md#remote-fs-latency test=docs/mount-drivers.md::MOUNT-017 doc=docs/mount-drivers.md prompt=/tmp/bunker-foreman-2026-10-22-22-13-04-MOUNT-017-brief.txt evidence=docs/mount-drivers.md witness=docs/mount-drivers.md verdict=none:pending-foreman-judge commit=none:pending-foreman-trace memory=none:not-applicable
STATUS: complete
TIER: T3 — invoked and observed live, read-only: `tailscale status --json` (per-peer Relay/CurAddr), `tailscale status`, `tailscale netcheck`, `tailscale ping -c 20 bunker-mvp` (DIRECT via 78.46.173.180:41641), `ping` RTT series on three peers, cold-op timing on the live mount `/tmp/mount017-mnt` (statfs ×5, negative lookup ×5, cold read ×5), warm distinct-file control after 100 s idle, and `go test ./internal/mountdriver/ -count=1`
EVIDENCE: docs/mount-drivers.md


