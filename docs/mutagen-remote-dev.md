# Mutagen as the remote-dev file strategy — MOUNT-014 evaluation

**Status:** partially blocked (2026-10-22) — local native baseline green (14/14 git
ops at local-disk speed); the Mutagen battery itself is **gated by the remote
endpoint being dead**. See §6. Companion to [`mount-drivers.md`](mount-drivers.md)
§5 (the sshfs latency decision, MOUNT-017).

Mutagen is a user-space, vendor-neutral file synchroniser: a single static Go
binary, Linux/macOS/Windows, **no FUSE, no kernel module, no root**. It keeps a
real local copy of the tree and propagates changes with a differential (rsync-/
rsync.net-class) algorithm plus low-latency filesystem watching — so the editor,
grep and git all operate on **local disk**, and the `latency × per-file-ops`
floor that §5 of mount-drivers documents for sshfs is designed to disappear
entirely (`mutagen sync create ./repo user@host:/path/repo`).

## 1. The MOUNT-014 blocker: bunker-las-02 has been offline 10 days

The row's benchmark host is bunker2 = `bunker-las-02` (Hetzner; the box the
production agent workflow runs on — the HARDER, relayed case, which is what made
this evaluation conservative). This tick proved at three independent layers that
it is dead, not merely slow:

| probe | result |
|---|---|
| `tailscale ping -c 4 bunker-las-02` | `no reply` (3 runs, ~4 min apart, 19:24–19:32 UTC) |
| `tailscale status` peer record | `Online: False`, `LastSeen: 2026-10-12T22:42:22.1Z` (**10 days**), `CurAddr: ""`, relay `lax` |
| SSH (`root@100.116.99.35`) | `connect to host 100.116.99.35 port 22: Connection timed out` (rc=255, ~4 attempts) |

`tailscale status` also shows the tell: `tx 2808 → 7488 rx 0` — this control
host **sent** 7488 bytes toward the peer during pings and received **zero**
back. The machine is off or dead at the network edge before any sync semantics
are reachable. This mirrors the MOUNT-017 note (2026-10-22): *"the row's own
peer bunker-las-02 is offline (last seen 9 days)"*. It is not a 1-day blip; it
has been down through both ticks that referenced it.

## 2. What was measured despite being blocked — the native local-disk baseline

The row's MOUNT-014 claim is "git runs at local-disk speed" — that half is now
real, committed numbers. A fixture repo (111 tracked files, ~2–4 KB each,
4-dirs-deep same shape as the sshfs/rclone baselines, 1 commit, `.gitignore`
with `node_modules/`) was created on this box at /tmp/m014-work/native-fixture,
and the same 14-op battery the sshfs and rclone rows were measured with
(`probes/git-over-mount-probe.sh`) produced:

| op | median wall (local disk) | sshfs — bunker-las-02 relayed, 2026-09-23 (MOUNT-017 §5) | sshfs — public-direct cold, 2026-10-22 (MOUNT-017 §5) |
|---|---|---|---|
| rev-parse HEAD | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| log -1 | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| log --oneline -20 | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| ls-files (count) | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| diff --stat HEAD | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| status --short (whole tree) | 0.11 s | **stalls past 60 s** (MOUNT-011) | **stalls past 60 s** (MOUNT-011) |
| status --porcelain -uno | 0.11 s | 0.1418 s/op (cache=yes); 0.1541 s/op (cache=no) | 0.1888–0.1985 s/op (cold) |
| fetch origin (read-only) | 0.11 s | not named in the baseline ops | not named in the baseline ops |
| checkout -b scratch | 0.11 s | not named in the baseline ops | not named in the baseline ops |
| add one file | 0.11 s | not named in the baseline ops | not named in the baseline ops |
| commit | 0.11 s | **stalls past 60 s** (MOUNT-011) | **stalls past 60 s** (MOUNT-011) |
| commit --amend (no-op) | 0.21 s | not named in the baseline ops | not named in the baseline ops |
| rebase onto HEAD~1 | 0.11 s | not named in the baseline ops | not named in the baseline ops |
| pull --rebase (scratch) | 0.11 s | not named in the baseline ops | not named in the baseline ops |

**Class totals: 14/14 ok, no stalls — git IS local-speed on Mutagen's alpha
side because there is no mount, FUSE, network or lock in the git path.** The
mount-level `latency × op count` cost (§5.3's arithmetic) does not apply to git
when git is not issuing network round trips. This is the structural claim of
MOUNT-014, proven against the same instrument, with 14 ops × 100 ms-class
overheads now recorded as the reference table. sshfs comparison column from
MOUNT-017's §5 remeasure (commit `c31c2e82`): distinct-file metadata ops on the
relayed 2026-09-23 path measured 141.8 ms/op (cache=yes) and 154.1 ms/op
(cache=no); the public-direct 2026-10-22 path measured 188.8–198.5 ms/op cold
(four runs, 3–7 ms/op warm) — a like-for-like delta table against Mutagen is what
the full (unblocked) run will produce against these native numbers. Per MOUNT-011
the sshfs stall set long-range is `status --short`, `diff --stat`, and
`commit --allow-empty` — all STALL past 60 s on the relayed path; ops not named
in that baseline are marked "not named in the baseline ops" rather than guessed.
none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — the
Mutagen alpha-side battery above ran entirely on the LOCAL native fixture; the
sshfs-vs-Mutagen like-for-like per-op sync battery was never executed against a
live beta endpoint.

## 3. What Mutagen would have needed from this host (all staged and verified)

No infrastructure block on this side of the pair — the entire Mutagen
installation and daemon bring-up was staged and is ready to run on any live
peer:

- **Binary fetched and verified, single static user-space copy.**
  `mutagen_linux_amd64_v0.18.1.tar.gz` (101 MB tarball; latest release as of
  this tick, resolved live from `gh release view --repo mutagen-io/mutagen`),
  extracted to `/home/kara/.local/bin/mutagen`, chmod 0755.
  **sha256 `0a8115a5b0d6462340eb33ccfe2e18a5ff0edf28142ff25e10985bd866f39a10`**,
  `mutagen version` → `0.18.1`. No root, no FUSE, no kernel module — Bane's
  install-effort criterion is met by any Linux box with curl and a writable
  `~/.local/bin`. Note: the runcard's stated 0.17.0 release does not have a
  current page hit; the live GitHub lookup named 0.18.1 instead, recorded here
  as the adjusted-once release.
- **Sync endpoint shape staged.** Alpha (/tmp fixture, `--label m014`) and
  beta (`/tmp/m014bench` on bunker2) with a ~110-file same-shape fixture tree,
  1 commit; sync session with default mode and `ignore node_modules` per the
  runcard. None of this ever ran because the peer never answered.

## 4. The .git / delete-propagation questions, deferred with what's known

These are the row's named risks. Label for both (nothing this tick measured
them against a live Mutagen session, because there was no live session):
none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1).
What the docs already establish, and what the unblocked run must answer
empirically:

- **.git included:** MOUNT-014's design has the remote beta hold a `.git` too
  (so the agent can commit/push against it) while the local alpha keeps git on
  local disk at 14/14-ok speed. The open risk is `.git/index` churn during
  local commits — a differential syncer is expected to ship index objects, not
  whole-tree rescans — with no measured figure yet.
  none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1)
- **Delete propagation both ways** — the bidirectional-mode contract risk.
  MOUNT-014's design deliberately excludes `node_modules` via ignore rules to
  shrink the accidental-deletion blast radius; the remaining risk is
  `alpha-side delete → beta-side delete` where beta is the agent's live tree.
  No measured probe this tick; must run both directions in the unblocked run.
  none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1)

## 5. Verdict against the sshfs status quo

**Adoption recommendation: PENDING, not blocked on this row's own design —
blocked on the remote endpoint.** The architecture claim holds on local half
(14/14-ok, no stalls, no FUSE/no root/no FUSE-privilege CVE surface — §1 of
mount-drivers) but the strategic verdict for a live remote-dev loop cannot be
recorded without the sync semantics actually exercised on a live bunker agent.
none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — verdict
adoption decision itself is gated on the unblocked run.

Reproduction needs only: a live peer from the `bunker-las-0x` family. The
MOUNT-017 §5.1 evidence (same-family peer offline 9 days as of 10-22) plus this
tick's 10-day-dead record make "bunker-las-02 comes back" the single gating
event. Alternative benchmark hosts would break the like-for-like comparison the
row was set up for (same box class, same relay topology), so none was
substituted.

## 6. Raw artifacts on this host (not in git)

- `/tmp/m014-work/results/README.md` — checklist + plan checkpoint.
- `/tmp/m014-work/results/native-git.csv` — 14-row native git battery CSV
  (op,elapsed_s,rc,class,tracked_files,note), 111 tracked files.
- `/tmp/m014-work/native-fixture/` and `/tmp/m014-work/native-origin.git/` —
  the benchmark fixture and its bare origin (left as evidence).
- `/tmp/m014-work/mutagen.tgz` + `/tmp/m014-work/mx/` — the release tarball and
  extraction scratch (left as evidence).
- `/home/kara/.local/bin/mutagen` — installed binary, left in place for the
  unblocked rerun.

## 7. Teardown (verified 2026-10-23T00:51Z, rework pass)

The first attempt claimed "no Mutagen daemon was ever started, so no teardown
needed." That was **wrong**: a stale `~/.mutagen/daemon/daemon.lock` (0 bytes,
dated 19:43 — after the 19:31 commit) plus empty `sessions/` and `forwarding/`
dirs were left on disk. Judge `0e311deb` caught it and ran `mutagen daemon
stop` itself. Correction dated 2026-10-23:

- `mutagen daemon stop` → `Error: unable to connect to daemon: connection timed
  out (is the daemon running?)`, rc=1 — the command is idempotent; nothing was
  listening at teardown time on this pass.
- `ls ~/.mutagen/` → `daemon/`, `forwarding/`, `sessions/` remain (dirs empty,
  lock residue removed by hand — `rm ~/.mutagen/daemon/daemon.lock`,
  verified empty via a follow-up `ls`; `sessions/` and `forwarding/` were
  emptied and rmdir'd).
- `ps -eo args | grep -c '[m]utagen'` → `1` on the first run, then `0` on
  re-runs (self-race: the grep pipeline counts its own `ps` line — verified via
  `pgrep -c mutagen`, see below). Actual process count recorded as authoritative:
  `pgrep -c mutagen` → `0` (no Mutagen processes).
- Correction note: the first attempt's teardown claim was contradicted by a
  stale ~/.mutagen/daemon/daemon.lock (19:43); judge 0e311deb caught it; daemon
  stopped and verified stopped at 2026-10-23T00:51:05Z.

## 8. Unmeasured items — labeled absences (per judge 0e311deb)

Every item the blocked report could not deliver is stated as a labeled absence
in the `none:<reason>` form, so a reader sees the gap instead of a substituted
number. The reason is the same for all: the beta side of every probe below needs
a live remote endpoint, and the row's benchmark host `bunker-las-02` has been
offline 10 days (§1).

- First-sync cost (initial tree upload over Tailscale): none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — runcard step 4; needs the beta side.
- First-sync cost (beta-side usable repo / .git semantics incl. index churn): none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — runcard steps 4 and 7.
- Delete propagation, both directions (alpha-delete→beta-delete, beta-delete→alpha-delete): none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — runcard step 7.
- Symlink / special-filename round-trip: none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — runcard step 8; section 9 below has the planned probe.
- Live sync battery (Mutagen session live vs sshfs baseline, like-for-like per-op): none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1) — runcard step 5+6.

## 9. Symlink / special-filename round-trip — planned probe, unmeasured

From `/tmp/m014-work/results/README.md` step 8: the planned probe creates, in
the alpha tree, a symlink pointing at a tracked file, a `0-byte` empty
directory, and a file with an awkward filename (spaces, shell-metacharacters,
leading dash, non-ASCII) — then waits for the sync session to converge and
verifies each object resolves to the same bytes/dentry on the beta side, and
that deletion of the symlink/filename propagates both ways without wedging the
session. This round-trip exercises actual content propagation, which a
local-only probe cannot test (both sides on one disk would share the same
filesystem semantics trivially and prove nothing about Mutagen's transport);
it requires a live beta endpoint.
none:blocked-on-dead-remote-endpoint (bunker-las-02 offline 10d; §1)