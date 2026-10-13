# GAP-112 — Mount live proof: transport durability + fidelity matrix

**Task:** GAP-112 (from the GAP-103/-107 durability half; spec `docs/prd/SPEC-sshfs-mount-durability.md`)
**Date:** 2026-10-13 · **Host under test:** bunker-mvp (`78.46.173.180`, daemon `http://100.115.67.116:18080`, bunkerd 0.2.0)
**Agent under test:** `gap112-live` (spawned for this run, `--ttl 4h`, destroyed at the end — see criterion 5)
**Client:** karaHermes (`/tmp/gap112/bunker`, built from this worktree at `main @ a0e03ecd`; binary version stamp `819ab90`)
**Peer:** GAP-122 (preset E2E battery) was running against the same host on the same client during this run — see *Transport control* below for why that mattered.

---

## Verdict summary

| # | Criterion | Verdict | Measured |
|---|---|---|---|
| 1 | PHANTOM — write refused when the transport drops | **PARTIAL** | refused ✓, no partial file ✓, but **55.6 s** (not ≤45 s) and the cause is a bare `EIO`, not a named cause |
| 2 | RECONNECT — <15 s blip, no operator action, current data | **PASS** | 12 s blip; post-blip read returned data created out-of-band *during* the blip; ssh transport never even dropped |
| 3 | EMPTY TREE — empty tree / missing `--path` both refused | **FAIL (half)** | missing path and non-directory **refused** ✓; an **existing empty directory MOUNTS** ✗ |
| 4 | STRANDED — `kill -9` sshfs, then mount again | **FAIL** | `bunker mount` does **not** clear it (`mkdir … file exists`); `bunker umount <agent-id>` does (default layout only) |
| 5 | CLEANUP — destroy the agent ⇒ zero local mountpoints | **FAIL** | **both** mounts and **both** sshfs daemons survived the destroy |
| 6 | HANG-BOUNDED — bounded error, never an indefinite block | **PARTIAL** | bounded ✓ (57.9 / 20.0 / 10.0 s), never indefinite ✓, but the error is `EIO` (errno 5), not a named verdict; first op exceeds 45 s |
| 7 | IDEMPOTENCE — recovery/umount path run twice | **PASS** | both runs byte-identical output and identical state |
| F | FIDELITY MATRIX (read_file / patch / search_files ×3 / git diff / git log) | **PASS** | every comparison identical to a local checkout of the same commit; 3 documented divergences |
| G | GUARD end-to-end (local build refused, remote build works) | **PARTIAL** | local build **is** refused (exit 97, nothing runs) but the installed wrapper **prints nothing**; remote build works |

Ten findings fall out of this battery (see *Findings*). The three highest-value ones are not in the criteria text at all: **`--path` is advisory and does not change what is mounted**, **`destroy` leaves live mounts behind**, and **the installed guard refuses silently**.

---

## Transport control (how the failure was injected, and why this way)

The criterion says "kill the agent's sshd or firewall the host". Both are host-wide on bunker-mvp: every agent SSHes to the same `:22` on the same host, so a plain firewall rule (or killing sshd) would also have cut **GAP-122**, which was mid-battery on this same client.

What was done instead: a **per-identity blackhole**. A dedicated local identity (`gap112probe`) mounted the same agent, and the drop was installed on the client keyed to that identity's sockets:

```
iptables -I OUTPUT 1 -m owner --uid-owner 1001 -d 100.115.67.116 -j DROP
```

`xt_owner` keys on the socket creator's uid, so only that mount's ssh/sftp packets — including every reconnect attempt — are dropped.

**Scoping proof (raw):**

```
--- (b) install the scoped DROP ---
INSTALLED uid=1001 dst=100.115.67.116
Chain OUTPUT (policy ACCEPT)
num  target     prot opt source               destination
1    DROP       all  --  0.0.0.0/0            100.115.67.116       owner UID match 1001
--- (c) scoping proof: kara's traffic to the SAME host is unaffected ---
KARA_UNTOUCHED_OK
kara_ssh_exit=0
```

Every mount in this battery used the options the client constructs (`[02-mount-state]`):

```
sshfs -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes
      -o reconnect -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ConnectTimeout=10
      -o auto_unmount -o dir_cache=no -o IdentityFile=… -o idmap=user
      bunker-gap112-live@100.115.67.116:/home/bunker-gap112-live /tmp/gap112/mnt/live
```

Baseline mount, as control:

```
Workspace: /home/bunker-gap112-live (not a git work tree)
bunker: note: dropped "allow_other" from the agent's stored SSHFS command — …
Mounted gap112-live at /tmp/gap112/mnt/live
real  0m4.908s        EXIT=0
```

---

## 1. PHANTOM — write attempted with the transport dropped

Transport blackholed, then a write through the mount, in the background so a `D`-state syscall could not wedge the harness:

```
--- (c) scoping proof: kara's traffic to the SAME host is unaffected ---
KARA_UNTOUCHED_OK
--- (d) the write, launched in the background at 2026-10-13T07:24:04Z ---
[2026-10-13T07:24:45Z] state: 4143039 S    do_wait    00:40
write_exit_code=1
write_elapsed_seconds=55.6
write_stderr_stdout:
bash: line 1: /home/gap112probe/mnt/live/gap112-phantom.txt: Input/output error
--- (e) the mount table while the transport is dead (is it still 'mounted'?) ---
bunker-gap112-live@100.115.67.116:/home/bunker-gap112-live on /home/gap112probe/mnt/live type fuse.sshfs (…)
```

Both sides checked immediately after the failure (`[07-phantom-followup]`, remote check out-of-band via `bunker exec`, i.e. not through the mount):

```
--- REMOTE side: was the phantom write ever applied? ---
ls: cannot access '/home/bunker-gap112-live/gap112-phantom.txt': No such file or directory
remote_ls_exit=2
--- LOCAL side (probe mount) ---
ls: cannot access '/home/gap112probe/mnt/live/gap112-phantom.txt': No such file or directory
local_stat_exit=2
```

**Verdict: PARTIAL.** No success was reported and no partial file exists on either side — the expensive phantom-space failure did *not* happen. Two deviations from the written criterion:

1. **55.6 s, not ≤45 s.** The 45 s figure is the `ServerAliveInterval=15 × ServerAliveCountMax=3` *detection* window; the caller's refusal lands after a bounded reconnect attempt behind it (`ConnectTimeout=10`). Re-measured under the clean criterion-6 harness: **57.9 s** for the first network-requiring operation.
2. **The cause is not named.** The operator gets the kernel's `EIO` (`os error 5`). There is no `mount_unreachable` verdict, because there is no write-time health predicate in the CLI path: `mount.go` mounts and returns, and the only guard it installs is the *build* guard (`.bunker-mount`). Spec §2 (health predicate) and §3 (write-time guard) are not implemented; the bound in practice comes entirely from the sshfs option set.

Post-recovery note: after the blackhole was cleared the mount served reads again with **no operator action**, and the sshfs child had re-spawned (`ssh …sftp` pid 4170935, created during the EIO) — so `-o reconnect` did re-establish the session.

---

## 2. RECONNECT — 12 s blip

A file was created **on the agent, out-of-band, while the transport was dark**, so "current data, not a cached stale entry" is a real question rather than a re-read:

```
--- (c) install the scoped DROP (blip starts) at 2026-10-13T07:27:57Z ---
--- (d) during the blip: create the file OUT-OF-BAND on the agent ---
GAP112-RECONNECT-CURRENT-DATA
--- (e) hold the blip to ~10s total, then clear it ---
blip_seconds=12
CLEARED rules_removed=1
--- (f) sshfs/ssh process identity AFTER the blip (same pid = transport absorbed it) ---
4133950 … sshfs -o … -o reconnect …  /home/gap112probe/mnt/live     # unchanged
4170935 … ssh … sftp                                                # unchanged
--- (g) NO OPERATOR ACTION: next read through the mount, immediately ---
GAP112-RECONNECT-CURRENT-DATA
read_exit=0
-rw-rw-r-- 1 gap112probe gap112probe 30 Oct 13 02:28 …/gap112-reconnect-new.txt
--- (h) is it the CURRENT data? compare with the agent's own view ---
GAP112-RECONNECT-CURRENT-DATA
--- (i) write through the recovered mount and verify out-of-band ---
write_exit=0 → agent side: GAP112-AFTER-BLIP-WRITE
```

**Verdict: PASS.** No operator action, no stale entry: the file that appeared remotely *during* the blip was visible immediately after it. That is consistent with `dir_cache=no` — a cached directory listing would have missed a file created while the link was dark. Note precisely what was proven: a 12 s gap is *absorbed by the live TCP session* (below the 15 s `ServerAliveInterval`, so nothing has to recover — the transport pids are unchanged). The reconnect-after-detection path was exercised in criterion 1, where detection fired and a new ssh child was spawned.

---

## 3. EMPTY TREE — empty dir and missing `--path`

```
--- setup on the agent ---
empty_dir_entries=0
ls: cannot access '…/gap112-does-not-exist': No such file or directory     ← MISSING_OK
--- control: a FRESH agent's home is never empty ---
22                       ← spawn always seeds .bashrc/.bunker/bin/.docker/rootless-install.sh
--- ARM A: a remote path that exists but is EMPTY ---
Workspace: gap112-empty (not a git work tree)
Mounted gap112-live at /tmp/gap112/mnt/empty
arm_a_exit=0
mount_table_entry: bunker-…@100.115.67.116:/home/bunker-gap112-live on /tmp/gap112/mnt/empty type fuse.sshfs (…)
--- ARM B: --path that does not exist ---
bunker: mount preflight: remote path "gap112-does-not-exist" does not exist or is not a directory on bunker-gap112-live@100.115.67.116
arm_b_exit=1        mountpoint_after: No such file or directory     mount_table_entry: NONE
--- ARM B2: --path that exists but is a FILE ---
bunker: mount preflight: remote path "gap112-baseline.txt" does not exist or is not a directory on …
arm_b2_exit=1       mount_table_entry: NONE
```

**Verdict: FAIL on the empty-tree half, PASS on the missing-path half.** The preflight is `[ -d "$p" ]` and nothing else (`internal/cli/mount_preflight.go`, `remotePathExists`): a directory that exists but is empty mounts happily, which is exactly the "editor writes into a fresh empty tree" failure the criterion exists to prevent. The spec's own closure table says the mount-time preflight should check "path exists **+ non-empty** + identity".

A note on the literal wording: "an agent whose home is empty" is not producible by `bunker spawn` — a fresh agent's home always carries 22 entries of provisioning — so arm A (an empty directory as the resolved remote path) is the same code path exercised directly.

### 3b. Severity raised by a second live observation: `--path` does not change what is mounted

While checking arm A, the mount source turned out **not** to be the `--path`:

```
--- mount --path gap112-empty, then inspect what is really exposed ---
Workspace: gap112-empty (not a git work tree)
Mounted gap112-live at /tmp/gap112/mnt/pathtest
mount_exit=0
--- mount table SOURCE ---
bunker-…@100.115.67.116:/home/bunker-gap112-live on /tmp/gap112/mnt/pathtest type fuse.sshfs
--- is the named path a SUBDIRECTORY of the mount? ---
drwxrwxr-x 1 kara kara 4096 Oct 13 02:37 /tmp/gap112/mnt/pathtest/gap112-empty      ← yes
--- write through the mount, where does it land? ---
write_exit=0
--- agent side ---
-rw-r--r-- 1 bunker-gap112-live … /home/bunker-gap112-live/gap112-path-probe.txt     ← the HOME, not the named path
```

Confirmed in source: `rewriteSSHFSMount(mountCmd, serverHost, resolvedHost, keyPath)` rewrites only `IdentityFile` and the host; the source path is never touched, and `remotePath` is used *only* for `remotePathCheck` and `--expect-workspace`. So:

- the help text (`--path string  Remote path inside the agent to mount`) is false;
- the empty-tree predicate and `--expect-workspace` are evaluated against a tree that is **not** the tree that gets mounted — the workspace guarantee can be satisfied by a path the mount never exposes.

---

## 4. STRANDED — `kill -9` sshfs, then mount again

Live state, then the kill (the victim is selected by exact `comm`, see *Harness traps*):

```
SSHFS_PID=69767 ; 69767 sshfs   sshfs -o … -o auto_unmount … /home/gap112probe/mnt/live
kill -9 → kill_exit=0 ;  sleep 4
--- kernel mount table after the kill ---
bunker-…:/home/bunker-gap112-live on /home/gap112probe/mnt/live type fuse.sshfs (…)
--- is the mountpoint still a mount, per the kernel? ---
ls: cannot open file '/home/gap112probe/mnt/live': Permission denied
/proc/self/mounts: bunker-…:/home/bunker-gap112-live /home/gap112probe/mnt/live fuse.sshfs … 0 0
--- MOUNT AGAIN, no manual fusermount ---
bunker: create mount point /home/gap112probe/mnt/live: mkdir /home/gap112probe/mnt/live: file exists
remount_exit=1
```

At the **default** layout (path component *is* the agent id), the same kill, then the two recovery paths:

```
--- (c) bunker umount <agent-id> on the stranded mount ---
Resolved /home/gap112probe/.config/bunker/mnt/bunker-mvp/gap112-live from the live mount table (not the default root)
Unmounted /home/gap112probe/.config/bunker/mnt/bunker-mvp/gap112-live
umount_by_agent_exit=0        PROC_MOUNTS: cleared
--- (d) bunker mount <agent-id> again ---
Mounted gap112-live at /home/gap112probe/.config/bunker/mnt/bunker-mvp/gap112-live   remount_exit=0
--- (e) SECOND stranded round: mount again while the stale mountpoint is present ---
bunker: mount: no writable mountpoint available (create mount point …: mkdir …: file exists). Set BUNKER_MOUNT_ROOT …
remount2_exit=1
--- and at a CUSTOM path (no agent id in the path), against the live kernel entry:
bunker: Nothing mounted for /home/gap112probe/mnt/live (mount table checked: /proc/self/mounts)
umount_by_path_exit=0        STILL MOUNTED
--> only fusermount3 -uz cleared it (manual_uz_exit=0)
```

**Verdict: FAIL.** Three separate defects, all reproducible:

1. **`bunker mount` never clears a stranded mountpoint.** The criterion's literal sequence — kill, mount again — fails, and the message does not name the real cause: `MkdirAll` swallows the `ENOTCONN` from `Stat`, retries `Mkdir`, gets `EEXIST`, and reports `mkdir … file exists`.
2. **`-o auto_unmount` did not release the mount when sshfs was SIGKILLed** — the entry was still in `/proc/self/mounts` after the kill, and `ls` reported `Transport endpoint is not connected`. Since the criterion's premise is "the stale mountpoint *is* present", that premise holds, but the option does not do what the spec's failure table credits it with.
3. **`bunker umount <path>` reports "already clean" (exit 0) over a live stranded mount.** With the mount stranded, `os.Stat(target)` fails (`ENOTCONN`), so `runUmount` never sets `explicitPath`, falls through to `agentMountpoints(target)`, and matches the mount **target as a whole path component** — which a custom path such as `…/mnt/live` is not. `mount.go`'s design allows the custom-path form; the docs' worked example (`/mnt/bunker/<agent>`) happens to carry the id. Same class as DF-BUNKER-50, now in the case where the stat() the resolver depends on is *by definition* broken.

`bunker umount <agent-id>` **does** clear a stranded mount, but only when the mountpoint path contains the agent id as a whole component. That is a real capability, and it is what criterion 7's "recovery path" was measured on.

---

## 5. CLEANUP — destroy the agent

Two mounts were live (two identities) when the agent was destroyed:

```
--- (a) mounts live immediately BEFORE the destroy ---
… /tmp/gap112/mnt/live                        type fuse.sshfs (…user_id=1000…)
… /home/gap112probe/…/gap112-live             type fuse.sshfs (…user_id=1001…)
sshfs processes: 2
--- (b) destroy ---
Destroying agent gap112-live (home 246.7 MB; archiving before delete, deadline 12m0s)…
Agent gap112-live destroyed.
Removed local SSH key /home/kara/.config/bunker/keys/gap112-live
destroy_exit=0
--- (c) immediately after: local mount table ---
… /tmp/gap112/mnt/live                        type fuse.sshfs (…)
… /home/gap112probe/…/gap112-live             type fuse.sshfs (…)
MOUNTS REMAIN
--- /proc/self/mounts entries mentioning the agent --- 2
--- surviving sshfs processes for this agent ---
153192 sshfs … /home/gap112probe/…/gap112-live
4046870 sshfs … /tmp/gap112/mnt/live
--- (d) what is left at each mountpoint path? ---
[kara mountpoint]  Input/output error (os error 5)
[probe mountpoint] Permission denied
--- (f) the umount path on an already-destroyed agent ---
Resolved … from the live mount table (not the default root)
bunker: mountpoint … is in use by pid Cannot, stat, …/gap112-live:, Permission, denied — close it first, or re-run with --force to detach lazily
umount_after_destroy_exit=1
```

**Verdict: FAIL.** Nothing in the destroy path touches the mounts: no teardown hook, no orphan sweep, no `mount status` surface (`bunker mount status` is not a subcommand — the invocation fell through to the session-binding guard). Both sshfs daemons are still alive against a destroyed agent, one of them with the key file `destroy` just deleted from under it. The deferred cleanup then has to be done by hand.

Secondary defect visible in the same output: the busy-holder check (`fuser -m` / `lsof -t`, falling back to parsing their output) produces `is in use by pid Cannot, stat, /home/…: Permission denied` — the *error text* of a failed probe is spliced in as the holder's name, and the command refuses without `--force` on a filesystem that cannot be reached at all.

Manual restoration of this host is recorded in `[restore]`: `fusermount3 -uz` on both mountpoints, `kill -9` the surviving sshfs daemons, iptables rule check (none left), probe identity and scoping group removed, kara's group list restored.

---

## 6. HANG-BOUNDED — stat with the peer blackholed

All operations run **as the mounting user** (that correction is itself a finding — see *Harness traps*). Healthy baseline first, then the blackhole, then each operation timed:

```
--- (0) HEALTHY baseline as the mounting user ---
healthy_read_exit=0 healthy_read_elapsed=0.5s
healthy_stat_root_exit=0 healthy_stat_root_elapsed=0.2s
healthy_stat_child_exit=0 healthy_stat_child_elapsed=0.2s
--- control: the SAME stat as root ---
stat: cannot stat '…/gap112-live': Permission denied (os error 13)     root_stat_exit=1
--- (1) blackhole installed at 2026-10-13T07:37:25Z ---
--- (2) stat the UNCACHED path (needs a real round trip) ---
stat: … Input/output error (os error 5)     uncached_stat_exit=1 uncached_stat_elapsed=57.9s
--- (3) stat the MOUNTPOINT ROOT (the criterion's literal target) ---
stat: … Input/output error (os error 5)     root_stat_exit=1 root_stat_elapsed=10.0s
--- (4) read a file whose attrs+data were warm ---
cat: … Input/output error                   warm_read_exit=1 warm_read_elapsed=20.0s
--- (5) write a NEW file ---
bash: … Input/output error                  new_write_exit=1 new_write_elapsed=10.0s
--- (6) write to an EXISTING warm file ---
bash: … Input/output error                  warm_write_exit=1 warm_write_elapsed=10.0s
--- (7) clear the blackhole; recovery with no operator action ---
GAP112-C6-CLEAN     recovered_read_exit=0 recovered_read_elapsed=2.2s
--- did any refused write land on the agent? ---
ls: cannot access '…/gap112-c6-write.txt': No such file or directory
GAP112-PROBE-CONTENT-26247              ← the warm file is intact, not truncated
```

**Verdict: PARTIAL.**

- **Never an indefinite block:** confirmed — the worst case is **57.9 s**, and every subsequent operation returns in 10–20 s. Nothing hung until the harness's own 150 s bound.
- **"Named error":** not met in the spec's sense. The caller sees `EIO (os error 5)` from the kernel. `mount_unreachable` / `mount_stale` exist only in the spec, not in the code path an operator or tool call traverses.
- **Bound:** the criterion puts the window at ≤45 s; the first operation after the drop took 57.9 s (45 s detection + bounded reconnect). Later operations are faster because the session is already marked dead.
- Data safety under the failure is good: the refused write left nothing behind and the warm file was not truncated.

---

## 7. IDEMPOTENCE — the recovery/umount path, twice

```
--- RUN 1: bunker umount gap112-live ---
Nothing mounted for gap112-live (mount table checked: /proc/self/mounts)      run1_exit=0
--- RUN 2: identical command again ---
Nothing mounted for gap112-live (mount table checked: /proc/self/mounts)      run2_exit=0
--- RUN 3 (extra): umount by explicit mountpoint path ---
Unmounted /home/gap112probe/mnt/live                                          run3_exit=0
--- REMOUNT ---
Mounted gap112-live at /home/gap112probe/mnt/live                             remount_exit=0
```

and, at the default layout, the recovery pair run against a stranded mount:

```
umount_by_agent_exit=0  →  remount_exit=0   (both runs byte-identical except the path)
```

**Verdict: PASS** for what the criterion asks (repeated runs leave identical state, and cleanup is safe to re-run). Scope, precisely: (i) two consecutive identical `umount` invocations produced byte-identical output and identical state, exit 0 both times; (ii) unmounting something already unmounted is success, not an error; (iii) the unmount→remount cycle ran twice at the same layout with identical outcomes for the unmount half (`[09d]`). What was **not** measured is a second *stranded*-clear cycle (kill → clear → kill → clear); the stranded-clear path was exercised once (`[09d]`), and its re-run arm is the same code path as the documented no-op case.

---

## FIDELITY MATRIX

Corpus: a 10-file git repo (Go sources with numbered lines, unicode incl. CJK + emoji + a combining accent, a TSV, a nested dir, a 1 KiB binary, a symlink), committed once and deployed to the agent by `bunker cp` + `tar`. Same commit on both sides (`d037cca`).

**Tree identity** (`[12]`):

```
git HEAD   mounted=d037cca204b77a95d2ce82581169937c84b1f4ac  local=d037cca204b77a95d2ce82581169937c84b1f4ac
git log    mounted == local                 git status --porcelain  mounted == local (both clean)
diff -r --brief --exclude=.git  →  diff_rc=0
md5 of every tracked file → MD5: ALL TRACKED FILES IDENTICAL   (10/10, binary + symlink included)
```

**`read_file` line window + line numbers** — identical to the local file, byte for byte including `total_lines`, `file_size` and the unicode:

```
mounted  /tmp/gap112/mnt/live/fid-repo/src/main.go  offset=40 limit=12
40|// line 038 of main.go — MARKER-C038      …      51|// line 049 …
"total_lines": 84, "file_size": 3150
local    (identical output)
mounted  data/unicode.txt → "1|acentos: café naïve über / 2|CJK: 中文测试 / 3|emoji: 🚀💡"
local    (identical output)
```

**`patch` on a known edit** (`[13]`) — written *through* the mount, then verified without the mount:

```
patch tool → /tmp/gap112/mnt/live/fid-repo/docs/notes.md
-SEARCH-HIT-TWO is unique
+SEARCH-HIT-TWO is unique — PATCHED-THROUGH-MOUNT
--- through the mount: git diff ---            diff --git a/docs/notes.md b/docs/notes.md @@ -1,4 +1,4 @@ … (same hunk)
--- OUT-OF-BAND on the agent (bunker exec) ---
3:SEARCH-HIT-TWO is unique — PATCHED-THROUGH-MOUNT
 M docs/notes.md
--- (d) the LOCAL checkout is untouched ---   [end local porcelain]   local_grep_rc=1
--- (e) collateral check: exactly one file's md5 changed (notes.md)
```

**`search_files`, all three output modes** — identical (only the path prefix differs, as expected):

| mode | mounted tree | local tree |
|---|---|---|
| content (`SEARCH-HIT`) | 3 matches, `docs/notes.md` lines 1–3 | same 3 matches, same lines |
| files_only (`MARKER-C0`) | 1 file: `src/main.go` | same 1 file |
| count (`MARKER`) | 82 total: `README.md` 2, `src/main.go` 80 | same counts |

**`git diff` and `git log` diffed against a local checkout of the same commit** (`[17]`): the same edit was applied to a control copy; the two `git diff` outputs are byte-identical (`GIT DIFF OUTPUT IDENTICAL`), as are the `git log` outputs (`GIT LOG OUTPUT IDENTICAL`).

**Specials** (`[17]`): symlink readable through the mount with its target preserved; `blob.bin` md5 identical.

### Documented mount divergences (limitations)

1. **Sub-second timestamps are lost.** Through the mount `/tmp/…/unicode.txt` reads `2026-10-13 02:24:16.000000000 -0500`; the same file locally reads `… 02:24:16.787016982 -0500`. SFTP carries whole seconds, so every mtime through a bunker mount is truncated. Build systems and tools that compare nanosecond mtimes (or `git checkout` racing a same-second edit) see a coarser clock through the mount than on disk.
2. **The mount is private to the mounting user — root included.** `stat` of the mountpoint as root returns `Permission denied (os error 13)` in 0.1 s while the mounting user reads it in 0.2 s. That is the deliberate consequence of dropping `allow_other` and creating the mountpoint 0700, but it means any root-run tool (a backup, a root shell, a container) sees EACCES, not the tree.
3. **Same-name files resolve differently after a failed write.** After the criterion-1 EIO, a *nonexistent* path resolved to `Permission denied` rather than `No such file or directory` for the next moments, and a stranded mount answers lookups with `ENOTCONN` (`Transport endpoint is not connected`). Error codes from a mount under stress are not the local-filesystem codes; tooling that branches on `ENOENT` vs `EACCES` should not trust them across a transport failure.

Everything else in the matrix was identical: content, line windows, search results, git objects, binary bytes, symlinks.

---

## GUARD end-to-end (GAP-105)

Marker present at the mountpoint root:

```
-rw-r--r-- 1 kara kara 218 … /tmp/gap112/mnt/live/.bunker-mount
This directory is an SSHFS mount of a bunker agent workspace. …
```

**Raw check — the refusal text exists and is good:**

```
( cd /tmp/gap112/mnt/live/fid-repo && bunker guard check go )
refusing: go is running inside a bunker SSHFS mount (/tmp/gap112/mnt/live/.bunker-mount).
A local build over SFTP is pathologically slow and wastes the local machine.
Run it remotely instead: bunker exec <agent-id> -- go ...
(or: bunker build <agent-id> -- ./...)
raw_check_exit=97
```

**Installed form — a real build is refused, but the operator is told nothing:**

```
( cd …/fid-repo && eval "$(bunker guard install)" )
--- go build ./... (stdout+stderr captured below) ---
local_build_exit=97                      ← NO OUTPUT AT ALL
--- make (another intercepted tool) ---
make_exit=97                             ← NO OUTPUT AT ALL
--- non-intercepted tools inside the mount pass through ---
cat README.md → …                 cat_exit=0 ;  git status --porcelain →  M docs/notes.md / ?? go.mod ; git_status_exit=0
--- documented bypass ---        command go version → go1.26.5 linux/amd64 ; command_go_exit=0
--- outside the mount the wrapper is a pure pass-through ---  go version → go1.26.5 ; outside_mount_go_exit=0
--- REMOTE build via bunker exec, with the guard installed in this shell ---
REMOTE_BUILD_OK ; go version → go1.22.2 linux/amd64 ; remote_build_exit=0
agent side: -rwxrwxr-x … 1376569 … /tmp/fid-bin      ← real remote artifact
```

**Verdict: PARTIAL.** The guard's *blocking* half is solid: the intercepted tool never runs inside the mount (exit 97, no build output on disk), non-build tools pass through untouched, the pass-through outside a mount is clean, and a remote build via `bunker exec` is unaffected. Two gaps:

1. **The installed wrappers swallow the refusal.** `bunker guard install` emits `if ! bunker guard check go >/dev/null 2>&1; then return 97; fi` — both streams of the check are discarded, so the carefully written refusal (which names the marker and the remote command form) is unreachable in the form operators actually use. `go build` inside a mount looks like a silent, failed shell function.
2. **The message names the local marker path, not the remote path/agent** (`/tmp/gap112/mnt/live/.bunker-mount`), and the remote hint keeps the literal `<agent-id>` placeholder. The task asked for a message naming the remote path.

---

## Findings (proposed rows)

| ID | Severity | Finding |
|---|---|---|
| **F1** | P1 | **`destroy` leaves live mounts and sshfs daemons behind.** Criterion 5 in full: both mounts stayed in the mount table, both daemons survived, one with its key file deleted by the destroy itself. No teardown hook, no orphan sweep, no `mount status` surface. |
| **F2** | P1 | **`--path` is advisory: the mount always exposes the agent home.** The empty-tree predicate and `--expect-workspace` are evaluated against a tree that is not mounted; help text and `docs/both-ways.md` describe a capability that does not exist. |
| **F3** | P1 | **A stranded mountpoint is not cleared by `bunker mount`** (`mkdir … file exists`), `-o auto_unmount` does not release the mount on SIGKILL, and `bunker umount <path>` reports `already clean` (exit 0) over a live stranded mount for any mountpoint whose path components do not include the agent id — the DF-BUNKER-50 false-clean class, in the one case where the resolver's own `stat()` cannot succeed. |
| **F4** | P2 | **The installed guard refuses silently** (exit 97, message discarded), and its message names the local marker path plus a literal `<agent-id>` rather than the remote path. |
| **F5** | P2 | **Empty-tree preflight is incomplete:** `[ -d ]` only. An existing-but-empty tree mounts and looks correct (spec §closure table requires non-empty). |
| **F6** | P2 | **No named failure verdict on transport dead.** The caller gets the kernel's `EIO (os error 5)`; `mount_unreachable`/`mount_stale` (spec §2/§3) do not exist in any traversable path, and there is no write-time guard despite `docs/both-ways.md` claiming "the write guard refuses first". |
| **F7** | P2 | **The ≤45 s bound is not met end-to-end:** 55.6 s (criterion 1) and 57.9 s (criterion 6) on the first network-requiring operation after the drop — detection (45 s) plus a bounded reconnect attempt. |
| **F8** | P3 | **Garbled busy-holder message on the umount path:** `is in use by pid Cannot, stat, …: Permission denied` — a failed probe's error text is spliced in as the holder, and the command refuses without `--force` on an unreachable filesystem. |
| **F9** | P3 | **Doc claims in `docs/both-ways.md` are not true as written** ("a crashed mount leaves a stranded mountpoint that the next `bunker mount`/`bunker umount` clears automatically"; "the write guard refuses first"). |
| **F10** | P3 | **Operator-facing traps found while writing the battery** (worth a runbook line each): `pgrep -f "sshfs .*<mountpoint>"` also matches the `fusermount3` helper (its `fsname` contains `subtype=sshfs`) — a "kill sshfs to test the stranded case" recipe kills the wrong process; a bunker mount is inaccessible to every user but the mounter **including root**; and the CLI silently adopts `~/.bunker/config.yaml` into `~/.config/bunker/config.yaml`, moving the local SSH key path to `~/.config/bunker/keys/<agent-id>`. |

---

## Harness notes (so the numbers can be re-derived)

- **Per-identity blackhole**, not a host firewall: GAP-122 was mid-battery on the same client against the same host, and all agents share the host's `:22`. `xt_owner` scoping left kara's traffic untouched (proven in-arm).
- **Harness bug found and corrected mid-run, recorded because it changes the reading:** the first criterion-6 pass timed `stat` as **root**, which measured FUSE's access policy (`EACCES` in 0.1 s for a non-mounting user), not the transport. The corrected pass runs every operation as the mounting user. The first pass also killed `fusermount3` instead of `sshfs` because of the `fsname` match; the corrected pass selects by exact `comm`.
- **Writes and first-touch stats were launched in the background and polled**, so a `D`-state FUSE wait could not wedge the harness; all waits carried their own bound.
- **Out-of-band verification** of everything that "landed" was done with `bunker exec` (a different transport than the mount), never by reading back through the mount under test.
- **Ephemeral by construction:** one dedicated agent (`gap112-live`, spawned for this run), one dedicated local identity for the scoped drop, both removed at the end; the box was returned to its pre-run state (0 mounts, 0 sshfs, no iptables rules, probe user/group deleted).

## What was not proven

- Criterion 3's literal arm ("an agent whose home is empty") is not producible by `bunker spawn`; the equivalent resolved-empty-path arm was used.
- Criterion 2 measured a blip *inside* the `ServerAliveInterval` window (12 s < 15 s), which the live session absorbs. The reconnect-after-detection path was exercised only incidentally (criterion 1). A deliberate 20–60 s blip followed by restore is a distinct test and was not run.
- Criterion 4's manual-recovery control used `fusermount3 -uz` as root; the operator-facing equivalent on a machine without root is untested.
- The `stale` identity verdict (spec criterion 4 of the durability set: destroy + re-create the same agent name) was not run — GAP-112's list does not include it and the agent was destroyed at the end of this battery.
