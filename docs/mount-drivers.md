# Mount drivers — sshfs (default), rclone, and FUSE over io_uring

**Status:** sshfs is the default and only implemented driver today. `rclone` and
`fuse-io_uring` are planned **opt-in** drivers. This document also records the
sshfs security issue that forces an upgrade, because the fix and the driver model
touch the same code.

See also: [`both-ways.md`](both-ways.md) (when to use the mount vs the verb path),
[`prd/ADR-toolsd-integration.md`](prd/ADR-toolsd-integration.md) (the verb path),
`SECURITY.md`.

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
agent.** The client needs the `rclone` binary.

- **No config file required.** Use an inline backend spec so there is no
  `rclone.conf` and no credential file to manage:
  `rclone mount :sftp,host=<host>,user=<agent-user>,key_file=<key>,shell_type=unix:/ <mountpoint>`
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
- Tracked as `MOUNT-007`.

### 3.3 FUSE over io_uring — opt-in

A transport change for FUSE itself, not a filesystem: it moves the
kernel↔userspace channel onto io_uring and cuts per-request overhead (measured
gains on the order of 1.4–2.8x on relevant workloads).

- **Requires all three:** a kernel with `CONFIG_FUSE_IO_URING` (>= 6.14), a
  **libfuse built with io_uring support**, and the module parameter enabled.
- **Enablement:** `echo 1 > /sys/module/fuse/parameters/enableuring`, then mount
  with the `-o iouring` option.
- **Measured state on our hosts (2026-09-20):**
  - control host: kernel `7.0.0-30-generic` with `CONFIG_FUSE_IO_URING=y` — kernel side **ready**;
  - **but** the packaged libfuse `3.18.2` exposes **no** io_uring transport (no
    `io_uring`/`iouring` strings in `libfuse3.so.3`, `fusermount3`, `mount.fuse3`
    or `sshfs`), so it is **not available in the current build**;
  - daemon host `78.46.173.180`: kernel `6.8.0-117-generic` — no io_uring FUSE.
- **Therefore this driver starts with a verification step, not an enablement:**
  probe the libfuse build for the option, and if it is absent, **fail with a named
  reason** rather than silently mounting without it. It applies where the mount
  happens (the client), so it is a client-host property.
- Tracked as `MOUNT-008`.
