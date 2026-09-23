# Bunker Remote Dev Workflow — Dogfood Integration Report

**Date:** 2026-09-23
**Run:** bunker-dogfood lane, 16th run (continuation of bunker-dogfood-2026-09-22-23-11-04-nudge2)
**Angle:** Remote development workflow — deploy, cp, exec, run, env, tunnel, mount as a real user would use them to build and test a project on a remote agent
**Server:** bunker-las-03 (100.69.3.13, bunkerd 0.1.4/6a6ad20)
**Agent:** f0901fd3 (ephemeral, TTL 2h, destroyed after run)

## Promise Statement

"A user can deploy a project to a remote agent, build it, run tests, manage environment variables, and access the agent's Docker daemon — all from the local CLI, without SSHing into the box directly."

## What Was Tested

1. **bunker deploy** — deploy a local Go project to an agent
2. **bunker cp** — copy a single file to an agent
3. **bunker exec** — run commands on the agent (build, test, run binary)
4. **bunker exec --script** — upload and execute a local script
5. **bunker run** — alternative execution verb
6. **bunker run --detach** — background/detached execution
7. **bunker env set/get/list/unset** — persistent environment variables
8. **bunker tunnel** — SSH tunnel to the agent's Docker socket
9. **bunker mount** — SSHFS mount of the agent's home directory
10. **Install leg** — fresh-agent install via scripts/install.sh

## What Worked

| Operation | Status | Time | Notes |
|---|---|---|---|
| deploy (4 files) | OK | 6.4s | Files land with correct ownership |
| cp (single file) | OK | 4.4s | Ownership set to agent user |
| exec (simple cmd) | OK | 0.5s | Fast, SSH-based |
| exec --script | OK | 0.6s | Full build+test+run in one shot |
| run (simple cmd) | OK | 0.6s | Same speed as exec |
| run --detach | OK | <1s | Process started, survived session end |
| env set/get/list/unset | OK | <1s each | Persistence works across exec calls |
| tunnel + docker | OK | 5s connect | Full Docker access (version, ps, run hello-world) |
| mount (HEAD CLI) | OK | <1s | SSHFS mount, bidirectional file edits |
| install.sh | OK | 14s | SHA256-verified, smoke check passed |

## Findings

### DF-BUNKER-48 [P1] — env set PATH is silently overridden by exec/run

**Repro:** `bunker env set <agent> PATH=/home/bunker-<agent>/go/bin:...` then `bunker exec <agent> -- 'echo $PATH'` prints a DIFFERENT PATH (`/home/bunker-<agent>/bin:/usr/local/sbin:...`). The env file at `/run/bunker/<agent>/env` correctly contains the user-set PATH, and GOPATH/GOCACHE from the same env file ARE applied — but PATH is overridden by the SSH session's own PATH setup, which unconditionally sets `/home/bunker-<agent>/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`.

**Impact:** A user who installs a toolchain in the agent's home (Go, Rust, etc.) and sets PATH via `bunker env set` to make it available in exec/run will silently find their PATH ignored. The only workaround is using absolute paths or symlinking into `~/bin`. This is the first thing a remote developer hits after installing Go.

**Fix direction:** The exec/run SSH session should source the env file AFTER the default PATH is set, so user-set PATH appends or overrides. Alternatively, prepend the user's PATH to the session PATH.

### DF-BUNKER-49 [P1] — installed CLI binary (00c3555) predates the DF-BUNKER-39 mount fix; bunker mount fails

**Repro:** `bunker mount <agent> --server <srv>` with the installed CLI (commit 00c3555, 2026-09-20) fails with "mount preflight: no remote path to check" on every invocation. `bunker mount <agent> <mountpoint> --path /home/bunker-<agent>` fails with "remote path does not exist or is not a directory" — the preflight runs but the CLI version has a bug where the preflight's ssh connection fails silently.

Building from HEAD (945cd5c) and using the fresh binary: `bunker mount <agent> <mountpoint> --path /home/bunker-<agent>` succeeds immediately. The HEAD CLI's preflight correctly connects, verifies the path, and mounts.

**Impact:** Any user following the README quickstart with the latest release (0.1.4) cannot mount. The mount command is advertised as a headline feature. The fix (653d763) was merged after the release was cut.

**Fix direction:** Cut a new release that includes 653d763, or update the install script to build from HEAD.

### DF-BUNKER-50 [P2] — bunker umount checks the wrong path when mountpoint is custom

**Repro:** `bunker mount <agent> /mnt/bunker/<agent> --server <srv>` succeeds and mounts at `/mnt/bunker/<agent>`. Then `bunker umount <agent> --server <srv>` prints "Nothing mounted at /run/user/1000/bunker/mnt/<agent> (already clean)" while the mount is still active at `/mnt/bunker/<agent>`. The umount command looks for the mount at a default path (`/run/user/<uid>/bunker/mnt/<agent>`) instead of checking where the mount actually is.

**Impact:** A user who mounts at a custom path must unmount manually with `fusermount -u <path>`. The umount command appears to succeed ("already clean") while leaving the mount active — a silent false positive.

**Fix direction:** umount should check `mount | grep <agent>` or track the actual mountpoint from the mount command, not assume the default path.

### DF-BUNKER-51 [P2] — run --detach prints a unit name but the process is not in a systemd unit

**Repro:** `bunker run <agent> --detach --name testbg -- sleep 300` prints "Run ID: ... Unit: bunker-run-<agent>-<uuid>". Checking via SSH: `systemctl --user list-units | grep bunker-run` shows no matching unit, but `ps aux | grep "sleep 300"` shows the process running with PPID=1 (init), in cgroup `session-<n>.scope` (not a `bunker-run-*.service` unit).

**Impact:** The CLI claims the detached run is a systemd transient unit, but it's actually just an orphaned process. There's no way to check its status, stop it via systemctl, or manage its lifecycle. A user who relies on the unit name for management will find nothing.

**Fix direction:** Either make the detached run actually use `systemd-run --user --unit=<name>` so the unit exists, or change the output to not claim it's a systemd unit.

### DF-BUNKER-52 [P2] — agent image lacks Go (and other common toolchains)

**Repro:** Fresh agent on las-03 has gcc, python3, make, git, jq, curl but NO Go. `make build` in a cloned repo fails with a helpful message pointing to the install script. A user who wants to build Go projects must install Go manually (~50s to download and extract).

**Impact:** The agent image is designed for running containers, not for building projects directly. But `bunker exec`/`run` are SSH-based (not container-based on las-03), so a user naturally expects to build on the agent. The missing toolchain is a friction point for the "remote dev box" use case.

**Note:** This is a known trade-off documented in SURF-013. The image spec can be customized per-agent. Filing as P2 because the default image should include at least Go (or the docs should make the "install Go first" step explicit in the remote dev workflow).

## Install Leg

**PASS** — fresh agent `e419d763` on bunker-las-03 (bare Debian 13, non-root).
- `git clone` succeeded in ~3s (945cd5c)
- `make build` gracefully refused (no Go) with a helpful message
- `scripts/install.sh` succeeded in 14s: SHA256-verified both binaries, installed to `~/.local/bin` (non-root fallback), smoke check `bunker --version` -> 0.1.4 OK
- Agent destroyed after test

## Performance Summary

| Operation | Time | Notes |
|---|---|---|
| deploy (4 files, ~1KB) | 6.4s | SCP recursive |
| cp (single file) | 4.4s | SCP single |
| exec (simple cmd) | 0.5s | SSH-based, fast |
| exec --script (build+test+run) | 0.6s | Cached build, very fast |
| run (simple cmd) | 0.6s | Same as exec |
| env set/get | <1s each | API-based |
| tunnel connect | ~5s | SSH tunnel setup |
| install.sh | 14s | Download + verify + install |

Nothing here is slow enough to warrant a PERF row. The SSH-based exec/run at 0.5s is excellent. Deploy at 6.4s for 4 files is SCP overhead — acceptable for the file count.

## Verdict

**PROMISING-BUT-ROUGH** — The remote dev workflow is functional and fast for the core operations (deploy, exec, run, env, tunnel). The mount command works with a HEAD build but is broken in the released binary. The env PATH override is the biggest usability friction — a user installing a toolchain and setting PATH will be confused. The detached run's fake unit name is misleading. But the core loop (deploy → build → test → run) works end-to-end in under 10s, and the Docker tunnel provides full container access.

## Cleanup

- Agent f0901fd3 destroyed via CLI (key removed)
- Agent e419d763 destroyed via CLI (key removed)
- Local mount point /mnt/bunker/f0901fd3 cleaned up
- No repo visibility/permission changes
- No scheduler/cooldown changes