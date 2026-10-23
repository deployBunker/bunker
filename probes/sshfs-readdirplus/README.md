# MOUNT-015 — readdirplus for sshfs (patch experiment)

Status: **verified on a live mount** (localhost SFTP over ssh, libfuse 3.18.2,
kernel FUSE interface 7.45, patched against upstream `sshfs-3.7.6-12-ga5d0c9b` =
`a5d0c9b3b66ae00c1b97c95dbc00789f77b89dd0`).

- Source: `vendor/sshfs/` (upstream tree at the pin, patched in place, `.git` removed
  so the patch is visible in this repo's diff).
- Patch as a file: `sshfs-readdirplus.patch` (applies to the pin; `git apply` clean).
- Diff size (**D4**): `sshfs.c` **+137 / −7** lines, `sshfs.rst` **+12 / −0**.
  Whole patch: 149 insertions, 7 deletions.

## What the patch actually is (and what the row got wrong)

The row's plan asked for three things; only one of them is what makes readdirplus work
on this codebase. Verified against libfuse 3.18.2 source, not from memory:

1. **`.readdirplus` in `fuse_operations` — impossible and unnecessary.** The high-level
   API (`struct fuse_operations` in `fuse.h`) has no `readdirplus` member. libfuse's
   `fuse.c` already registers `fuse_lib_readdirplus` as the low-level handler for every
   high-level filesystem, and `fuse_lowlevel.c` therefore already wants
   `FUSE_CAP_READDIRPLUS` by default (`LL_SET_DEFAULT(se->op.readdirplus, …)` inside
   `do_init`, which runs *before* the filesystem's `init()`). So on stock sshfs the
   kernel is **already** sending `FUSE_READDIRPLUS`; sshfs just ignored the flag and
   called the filler with flags `0`, which makes libfuse emit `ino = 0` — "the kernel
   should ignore the readdirplus stat info" — and the kernel then does one `getattr`
   per entry.
2. **The filler flag is the whole feature.** `sshfs_readdir()` now sets
   `FUSE_FILL_DIR_PLUS` when called with `FUSE_READDIR_PLUS`. The attributes are
   already in `struct dir_entry.stbuf`.
3. **`FUSE_CAP_READDIRPLUS` in `sshfs_init` is belt-and-braces, and
   `-o readdirplus=no` needed real work.** libfuse documents that option, but it is
   applied by `fuse_apply_conn_info_opts()`, which only the low-level path
   (`fuse_session_new` users, e.g. sshfs's `passive` mode) calls. `fuse_main_real()`
   never parses it, so on sshfs's normal high-level path `-o readdirplus=no` is a
   no-op. The patch therefore parses `readdirplus=no|yes|auto` itself and *clears*
   the wanted capability bits in `sshfs_init` (clearing sticks: libfuse converts
   `conn->want` to the extended want flags immediately after `init()`).

Staleness caveat (the one the source itself flags at `sshfs.c:196-206`): for an entry
whose path is in `sshfs.conntab` — i.e. a path with a possibly in-flight `write()` —
attributes are filled **without** `FUSE_FILL_DIR_PLUS`, so the kernel does a real
`getattr()` for that entry, and `get_conn()` routes it to the connection that owns the
write. `conntab` only exists with `max_conns > 1`; single-connection mounts serialize
everything over one SFTP channel, so listings cannot be stale there.

## Results

All numbers are **SFTP round trips** (`SSH_FXP_LSTAT` lines in the `-o sshfs_debug`
log), one operation per fresh mount (fresh FUSE superblock = cold caches), minus the
mount's own overhead. Fixture A: 165 files in 11 directories, 2 modified, 2 deleted,
1 untracked file, 1 untracked directory — `make-fixture.py` recreates it,
`reset-fixture.py` restores that state after a probe writes through the mount.

`per-op.sh` (one mount per row):

| operation | base | patched | patched `-o readdirplus=no` |
|---|---|---|---|
| `find -printf` metadata walk | 180 | **3** | 180 |
| `ls -lR` | 180 | **4** | 180 |
| `git status --porcelain` | 286–459 (noisy) | 458 | 459 |
| `git diff --stat HEAD` | 1556 | 1556 | 1556 |
| `git ls-files` | 20 | 20 | 20 |
| mount overhead (no-op) | 2 | 2 | 2 |

`repeat-op.sh` (same mount, repeated) shows why the `git status` row is noise and what
the steady state is:

| operation | arm | iteration 1 | 2 | 3 | 4 |
|---|---|---|---|---|---|
| `git status --porcelain` | base | 459 | 276 | 115 | 116 |
| `git status --porcelain` | patched | 284 | 114 | 114 | 114 |
| `find` metadata walk | base | 180 | 14 | 14 | — |
| `find` metadata walk | patched | **3** | **3** | **3** | — |

So: pure metadata walks (find/`ls -lR`/rsync/du-shaped scans) lose **98%** of their
per-file round trips, reproducibly, and the opt-out restores the old behaviour exactly
(180). `git status` gains only on the first invocation in a mount (459 → 284) and then
costs the same per-file `getattr`s as before (≈115 either way) — the row's claim that
git's per-file `getattr` storm collapses is **not** what this measurement shows. Wall
clock is useless as a metric on loopback (0.28 s for the whole battery); the value is
in round-trip count, which is what costs 292 ms/op on the real link.

Correctness (**D3**), from `readdirplus-ab.sh`: `git status --porcelain`,
`git status --short`, `git diff --stat HEAD` and the `find` walk are
**byte-for-byte identical** across base / patched / patched-with-opt-out
(108 / 108 / 151 / 7038 bytes), and the tree state after each arm is identical.

Freshness (the staleness caveat), from `freshness-probe.sh` on the patched build with
`max_conns=4`: 6/6 then 20/20 iterations where `stat()` while the file is still open,
`ls -l` of the containing directory, and `stat()` after close all report the written
length; the suppression counter fires exactly once per open file (6, 8, 20 hits), i.e.
the caveat path is exercised, not dead code. A **control build with the suppression
disabled** (`if (1) return 0;` in `dir_entry_may_have_pending_write`) also reported
consistent sizes in 20/20 iterations: on loopback writes complete in well under the
1 s attribute timeout, so a stale-attribute regression is not reproducible here. The
guard is preventive — justified by the source's own comment and by `get_conn()`
routing — and would matter on the real link, where a queued write can outlive the
attribute timeout.

Regression: upstream's own suite (`test/test_sshfs.py`, 16 combinations of
multiconn × sync_readdir × cache_timeout × debug) passes 16/16 on the patched build and
16/16 on the unpatched pin. It needs `--basetemp` under `/tmp`: with this host's
`TMPDIR=/var/tmp/hermes-scratch` the suite's mountpoints get `fusermount3: mount
failed: Permission denied` — an environment limit, not a patch failure.

## Scripts

| file | what it does |
|---|---|
| `build.sh <dir> [patched\|base]` | clone the pin, optionally apply the patch, meson+ninja, print the binary path |
| `make-fixture.py <dir>` | create Fixture A (165 files + known dirty state) |
| `reset-fixture.py <dir>` | undo what a probe wrote through the mount |
| `readdirplus-ab.sh` | 3-arm whole-tree battery + byte comparison of git outputs |
| `per-op.sh` | one row per (arm, operation), fresh mount each, round-trip counts |
| `repeat-op.sh` | repeat one operation in one mount; shows cold vs steady state |
| `freshness-probe.sh` / `.py` | staleness probe + suppression counter |

Example (what was run for this experiment):

```
bash build.sh /tmp/m015-base base          # -> /tmp/m015-base/sshfs
bash build.sh /tmp/m015-plus patched       # -> /tmp/m015-plus/sshfs
python3 make-fixture.py /tmp/fix
bash readdirplus-ab.sh /tmp/m015-base/sshfs /tmp/m015-plus/sshfs localhost /tmp/fix /tmp/mnt /tmp/out
bash per-op.sh base=/tmp/m015-base/sshfs plus=/tmp/m015-plus/sshfs \
     'plus-off=/tmp/m015-plus/sshfs|-o readdirplus=no' -- localhost /tmp/fix /tmp/mnt /tmp/out
bash freshness-probe.sh /tmp/m015-plus/sshfs localhost /tmp/fix /tmp/mnt /tmp/fresh
```
