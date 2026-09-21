# sshfs 3.7.6 deployment record — CVE-2026-47187 / CVE-2026-48711 remediation (MOUNT-001)

Status: **deployed to both verified-affected hosts** (2026-09-21, tick #516).
This is the execution record behind `docs/mount-drivers.md` §1 and the
`SECURITY.md` control table. MOUNT-002 (adversarial escape proof against a
real agent) remains open and is NOT satisfied by this deployment.

## Affected range and fix

- Affected: sshfs `<= 3.7.5` (symlink escape CVE-2026-47187, CVSS 9.3;
  argument injection CVE-2026-48711).
- Fix: upstream release tag `sshfs-3.7.6`, pinned at commit
  `7a2d988775446ebe7af9b01c99b3b8e86bddb05a` (verified via
  `git rev-parse HEAD` after `git clone --depth 1 --branch sshfs-3.7.6
  https://github.com/libfuse/sshfs.git` on BOTH hosts).

## Hosts

### Control host (kara workstation, x86_64, Ubuntu, libfuse3 3.18.2 / .so.4)

| Item | Value |
|---|---|
| Before | `3.7.3-1.1build5` (apt, `/usr/bin/sshfs`) |
| After | `3.7.6` at `/usr/local/bin/sshfs` (PATH precedence: `/usr/local/bin` before `/usr/bin`; `command -v sshfs` → `/usr/local/bin/sshfs`) |
| Build | `meson setup build --buildtype release && ninja -C build` (native) |
| sha256 | `ff4e202f71887065d92da933dcf84026d1f990e166f5cf7efe85c960e481fd33` |
| Linked | `libfuse3.so.4` |
| Rollback | apt package `3.7.3-1.1build5` still installed at `/usr/bin/sshfs`; delete `/usr/local/bin/sshfs` to fall back |

### Mount host / demo daemon (`bunker-mvp`, x86_64, libfuse3 3.14.0 / .so.3)

| Item | Value |
|---|---|
| Before | `3.7.3-1.1build3` (apt, `/usr/bin/sshfs`) |
| After | `3.7.6` at `/usr/local/bin/sshfs` (`command -v sshfs` → `/usr/local/bin/sshfs`) |
| Build | NATIVE build on the host: same clone/tag/sha, `meson setup build --buildtype release && ninja -C build` |
| sha256 | `855326bddf0f16cb88b052e0ae2f23e1fcff93a4af8e559ecf97b83527df52dd` |
| Linked | `libfuse3.so.3` |
| Note | A cross-compiled copy from the control host does NOT run here (control links libfuse3.so.4; the demo only ships .so.3 → `error while loading shared libraries`). Build on the target host or a matching sysroot. |
| Build deps installed | `meson ninja-build libfuse3-dev libglib2.0-dev` (apt) |

## Functional smoke (criterion evidence, both directions)

Mount (control host, unprivileged user): `sshfs -o ConnectTimeout=10
bunker-mvp:/tmp /tmp/bunker-t516-mnt` → FUSE mount live
(`type fuse.sshfs (rw,nosuid,nodev,...)`). 64 KiB random sentinel written
through the mount; readback byte-identical on both ends
(md5 `a523a5f7a6e53e2d1ed190d040f33edb` on client and server); the binary
seen THROUGH the mount was the new build (sha matches the table above).
Clean unmount: `fusermount3 -u` → rc 0, mount table count 0 after.

## Fleet state

- `bunker-las-01`, `bunker-las-02`, `bunker-las-03`, `bunker-las-04`: **NOT
  remediated by this record** — these hosts were re-probed as part of
  MOUNT-001's detection scope only for the two hosts above; upgrading the
  remaining fleet members is owed under MOUNT-009 (install-time option +
  full fleet sweep + refuse-fast guard in `bunker mount`).
- `bunker mount` option surface verified at HEAD: `durableSSHFSArgs()`
  carries `reconnect`, keepalive bounds, `ConnectTimeout`, `auto_unmount`,
  `dir_cache=no` — no `transform_symlinks`, no `allow_*` (stripped/refused
  in `internal/cli/mount.go`). The client never enables a trust-widening
  option; the CVE exposure was the client binary version itself, now fixed.
