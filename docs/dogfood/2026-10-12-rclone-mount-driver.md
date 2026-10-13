# 2026-10-12 — rclone SFTP mount driver (MOUNT-007): first real-use dogfood

**Angle:** every prior run used the default sshfs mount path (or the broken pre-DF-BUNKER-39
form). The rclone driver — registered 2026-10-07 (commit 10760912, MOUNT-007) — had never been
driven by a real user. This run exercised it end to end: spawn with `--mount-driver rclone`,
mount, two-way file round-trips, a compile-through-the-mount workflow, umount by id and by
path, plus a same-file exec-bit parity probe against sshfs.

## Promise under test

docs/mount-drivers.md §3.2: "rclone is OPT-IN … an agent spawned with `--mount-driver rclone`
mounts through the rclone arm; the agent needs NOTHING installed — only the client needs the
rclone binary." The flag is registered in the CLI at HEAD.

## What worked (evidence)

- **HEAD daemon accepts the driver.** Local scratch daemon (HEAD ae821785, isolated
  base_data_dir, ports 29090/28080, auth on): `bunker spawn dfrclone-a --ttl 3h
  --mount-driver rclone` → bundle carries the stored rclone command with server-local paths.
- **Client rewrite is correct.** `bunker mount dfrclone-a --server dfhead` rewrote the stored
  command: `--sftp-key-file /tmp/dfd012-loc/ssh/dfrclone-a` → client key
  `~/.config/bunker/keys/dfrclone-a`, host → the address the client actually uses, mountpoint
  → the server-namespaced private mountpoint. Mount in **0.60s** (vs 4.8s sshfs cold on the
  same client; 459.8ms ±55.9ms warm, hyperfine 3 runs — sshfs arm not benched warm because a
  sibling destroyed that agent mid-run).
- **Two-way data path works.** Client write → agent read (exact bytes), agent write → client
  read after mount refresh; gcc compile on the agent through a client-written source file;
  `bunker exec` docker version (29.1.3) and `env set/get` on the same agent both green.
- **umount both forms work** (by id and by explicit path), including the two-concurrent-mounts
  case (id unmounts only the default-root mount; explicit path clears the custom one). No
  rclone process survives a clean unmount. Umount-when-nothing-mounted exits 0 ("already
  clean" is SUCCESS per the help contract).
- **Fail-loud contracts hold:** unknown driver refused by the daemon with the registered set
  (`invalid_argument: unknown mount driver "rclone" (registered drivers: [bunkerfs sshfs])`,
  rc=1); unknown agent on mount → rc=1.

## What broke (filed as rows)

1. **Docs teach a flag the released daemon refuses** (DF-BUNKER-92): v0.2.0 (tag 149411c1,
   2026-10-01) predates the driver registration (10760912, 2026-10-07 — NOT in the tag).
   Following the documented quickstart verbatim against a released daemon: `spawn …
   --mount-driver rclone` → `invalid_argument: unknown mount driver "rclone"`. The client
   (0.2.0 installed) already knows the flag — pure daemon-side skew. README's own
   version-gating lesson (the /tmp-isolation note) was not applied to this feature.
2. **Readdir cache makes agent-side writes invisible for ~4-5 minutes** (DF-BUNKER-93):
   rclone's default `--dir-cache-time` (5m) is unpinned by the stored command. Agent-side
   file/dir creates are invisible to the client until the cached dir entry expires (measured:
   writes at 19:19-19:21, first visibility 19:25-19:29; a binary the agent compiled was
   invisible on the mount for >5 min). A developer writing code THROUGH the mount is fine
   (client writes appear agent-side immediately) but the reverse direction — build artifacts,
   logs — reads as silently missing. Fix direction: the server-side builder should pin
   `--dir-cache-time 10s` (or document the flag and its UX cost next to §3.2).
3. **Exec bits are lost through the rclone mount** (DF-BUNKER-94): a `chmod 755` file on the
   agent reads `-rw-rw-r--` through the rclone mount (rclone VFS default umask 002 masks the
   exec bits; the mode shown is 0666 & ~002), while the identical probe through the sshfs
   driver shows `-rwxr-xr-x`. Running a built binary straight off the mount fails with
   Permission denied. Parity is per-driver, and sshfs is the honest one. Fix direction:
   `--umask 022` (or a mount-driver parity test asserting mode round-trips).

## Process findings (dogfood-lane learnings, not product rows)

- A scratch daemon on a SHARED host must set `agent.base_data_dir` — it defaults to
  `/var/lib/bunkerd`, and the instance-identity file lives there: a second daemon silently
  inherits the first daemon's identity and classifies its live agents as ITS OWN orphans
  (sweep guard caught the wrong classification only by luck: archive-first, and the destroy
  was killed mid-flight). One config line (`base_data_dir: /root/…/data`) makes the foreign
  classification fire correctly. Filed as DF-BUNKER-95 with the full mechanism.
- A sibling automation on bunker-mvp (`wipe-df-scratch.sh`) hardcodes past scratch paths and
  deleted my first scratch setup 2 seconds after boot; the mvp E2E battery's monitor also
  destroyed my live 6h-TTL agent (dfsshfs-a) 20 minutes into the run (audit: caller=master
  from 100.97.236.14). Lesson for future ephemeral legs: use fresh per-run path prefixes.
- No install leg on an ephemeral bunker box: every bunker host except mvp was unreachable
  (TCP timeouts), and mvp was mid-E2E-battery and unsafe for a scratch daemon. The HEAD-daemon
  leg ran on the local control host instead — named as a substitute, not a pass.

## Verdict

🟡 PROMISING-BUT-ROUGH — the rclone driver delivers its promise (fast, configless, agent-side
nothing-to-install) and the CLI rewrite path is genuinely good, but a fresh user following the
docs against a released daemon hits a hard refusal, and the mount's read-back semantics (4-5
min staleness, exec-bit loss) will confuse the remote-build workflow it exists to serve.
