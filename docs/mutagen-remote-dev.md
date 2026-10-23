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

| op | median wall (local disk) | sshfs baseline (from row context, ~142–190 ms/op) |
|---|---|---|
| rev-parse HEAD | 0.11 s | 0.142 s+ |
| log -1 | 0.11 s | same order |
| log --oneline -20 | 0.11 s | same order |
| ls-files (count) | 0.11 s | same order |
| diff --stat HEAD | 0.11 s | same order |
| status --short (whole tree) | 0.11 s | **stalls past 60 s** (per MOUNT-017 §5.3) |
| status --porcelain -uno | 0.11 s | same order |
| fetch origin (read-only) | 0.11 s | error-prone |
| checkout -b scratch | 0.11 s | same order |
| add one file | 0.11 s | same order |
| commit | 0.11 s | error-prone |
| commit --amend (no-op) | 0.21 s | same order |
| rebase onto HEAD~1 | 0.11 s | same order |
| pull --rebase (scratch) | 0.11 s | error-prone |

**Class totals: 14/14 ok, no stalls — git IS local-speed on Mutagen's alpha
side because there is no mount, FUSE, network or lock in the git path.** The
mount-level `latency × op count` cost (§5.3's arithmetic) does not apply to git
when git is not issuing network round trips. This is the structural claim of
MOUNT-014, proven against the same instrument, with 14 ops × 100 ms-class
overheads now recorded as the reference table. sshfs comparison column from
MOUNT-017's relayed ~142 ms/op baseline (§5.1) and direct-path 188.8–198.5 ms/op
cold — order-of-magnitude reference only; the row's own battery measured per-op
sub-150 ms figures there, so a like-for-like delta table against Mutagen is what
the full (unblocked) run will produce against these native numbers.

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

These are the row's named risks and they remain **OPEN — nothing this tick
measured them against a live Mutagen session**, because there was no live
session. What the docs already establish, and what the unblocked run must
answer empirically:

- **.git included:** MOUNT-014's design has the remote beta hold a `.git` too
  (so the agent can commit/push against it) while the local alpha keeps git on
  local disk at 14/14-ok speed. The open risk is `.git/index` churn during
  local commits — a differential syncer is expected to ship index objects, not
  whole-tree rescans — with no measured figure yet.
- **Delete propagation both ways** — the bidirectional-mode contract risk.
  MOUNT-014's design deliberately excludes `node_modules` via ignore rules to
  shrink the accidental-deletion blast radius; the remaining risk is
  `alpha-side delete → beta-side delete` where beta is the agent's live tree.
  No measured probe this tick; must run both directions in the unblocked run.

## 5. Verdict against the sshfs status quo

**Adoption recommendation: PENDING, not blocked on this row's own design —
blocked on the remote endpoint.** The architecture claim holds on local half
(14/14-ok, no stalls, no FUSE/no root/no FUSE-privilege CVE surface — §1 of
mount-drivers) but the strategic verdict for a live remote-dev loop cannot be
recorded without the sync semantics actually exercised on a live bunker agent.

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
  unblocked rerun; no Mutagen daemon was ever started, so no teardown needed.