# SPEC: SSHFS mount durability — no phantom spaces, no silent disappearance, auto-recovery

**Project:** bunker · **Rows:** GAP-103 (lifecycle), GAP-104 (namespace/workspace), **this spec adds durability** · **Author:** Hermes · **Date:** 2026-09-20
**Status:** proposed (spec of record for the durability half of Path B) · **Effort:** M · **Depends on:** GAP-103 · **Feeds:** GAP-105 (fidelity + do-not-build guard)
**PRD of record:** `docs/prd/PRD-bunker-remote-editing.md` (Path B / S9)

---

## The problem in one sentence

An SSHFS mount is a **network filesystem that pretends to be a local directory**, so when the link dies it does not crash — it *lingers*, and the operator keeps writing into something that is no longer connected.

## The six failure modes (each measured against the current code)

Checked on 2026-09-20 against `internal/cli/mount.go` (331 lines) and `sshfs` 3.7.3.

| # | Failure mode | What the operator sees | Status today |
|---|---|---|---|
| 1 | **Phantom space** — transport drops, FUSE session stays | `ls` returns **stale cached entries**; a write "succeeds" into a kernel buffer and is lost when the session finally dies | **LIVE.** `mount.go` passes only `StrictHostKeyChecking`, `UserKnownHostsFile`, `IdentitiesOnly`. No `-o reconnect`, no keepalive — a blip is permanent, and nothing detects it. |
| 2 | **Silent empty tree** | mount succeeds against an agent whose home is empty (re-created agent) — an editor writes into a fresh empty tree | **LIVE.** The mount replays the daemon's stored command; nothing verifies the remote path exists or is a directory. |
| 3 | **Stranded mountpoint** | process dies (crash, SIGKILL, reboot) and the mountpoint stays, blocking the next mount with `mountpoint is not empty` | **LIVE.** No `-o auto_unmount`; no cleanup command (`mount.go:311` only *prints* the `fusermount -u` hint). |
| 4 | **Unbounded hang** | a tool call blocks indefinitely instead of erroring — the worst failure for an agent, because it cannot tell "slow" from "dead" | **PARTIAL.** GAP-084 gave the *mount attempt* a bounded retry; the *mounted session* has no timeout, and no `-o ConnectTimeout` (unlike `cp`, `ssh`, `deploy`, `session_probe`, which all pass `-o ConnectTimeout=10`). |
| 5 | **Stale-after-reconnect confusion** | transport recovers but the tree on the far side changed (agent re-created, branch switched); the mount looks fine and serves a different reality | **UNHANDLED.** No state/identity check at any point after mount. |
| 6 | **Interleaved concurrent writers** | two sessions on one agent silently overwrite each other | **PARTIALLY GUARDED.** Leases are the designed guard (CHT-052) but are not yet wired to the mount path. |

## Design: the mount is a claim that must be continuously true

A mount is not an event — it is an **ongoing assertion** that "this local path is that remote tree, right now". Every failure above is the assertion going false while the mount keeps looking mounted. So the design makes the assertion explicit, checkable, and repairable.

### 1. Durability options that must be passed (currently absent)

| Option | Why | Today |
|---|---|---|
| `-o reconnect` | survive a transport blip instead of dying permanently (failure 1) | **absent** |
| `-o ServerAliveInterval=15 -o ServerAliveCountMax=3` | detect a dead peer within ~45s instead of hanging forever | **absent** |
| `-o ConnectTimeout=10` | bound the initial connect — every other ssh path in this repo already does this | **absent** |
| `-o auto_unmount` | release the mountpoint when the process dies (failure 3) | **absent** |
| `-o dir_cache=no` (editing profile) | avoid serving stale directory entries while editing (failure 1) | **absent** |
| `-o idmap=user` | uid/gid mapping so files read as the operator, not as nobody | varies by stored command |

**Rule:** the durability option set is **constructed by the client**, not inherited from the daemon-stored command string. The stored command is a starting point; durability flags are added explicitly and are asserted present by a test.

### 2. The health predicate (kills the phantom)

A mount is considered **healthy** only if a *stat of a known path on the remote side* succeeds within a bounded time, against a marker whose content is known.

- Cheap probe: `stat` the workspace marker (`.bunker-mount` or the repo's `.git/HEAD`) with a hard timeout.
- **Cheap + truthful:** the marker's *content* is compared to a value captured at mount time. A mount whose marker vanished or changed is **not the tree we mounted** (failure 5) and is reported as `stale`, distinct from `unreachable`.
- Health is checked: before each tool call that writes, on a timer for long sessions, and on demand (`bunker mount status`).

Three verdicts, never two:
- **healthy** — probe succeeded and the identity matches
- **unreachable** — probe timed out or failed (transport dead)
- **stale** — probe succeeded but the tree is not the one mounted (agent re-created / path replaced)

`unreachable` and `stale` demand *different* recovery, which is why they are not collapsed into "broken".

### 3. Write-time guard (no silent loss)

A write through a phantom mount is the expensive failure — the operator believes the edit landed. So:

- **No write is reported successful without a read-back** through the same path, or a successful health probe immediately prior.
- If the health probe fails, the write is **refused** with a named cause (`mount_unreachable` / `mount_stale`), not attempted-and-lost.
- Where a write cannot be verified, it closes as `unverified` — never as success. (Same anti-lying rule as the verb layer, proof 8.)

This is the difference between "the mount dropped and my edit is gone" and "the mount dropped and my tool told me".

### 4. Auto-recovery, with an explicit policy

Not "retry forever" — a **bounded, idempotent recovery ladder** whose steps are chosen by the verdict:

| Verdict | Recovery |
|---|---|
| `unreachable` (transport) | wait for the built-in `reconnect` window; if the health probe recovers, continue with **no operator action**; if not, attempt unmount + remount **once**, then stop and report |
| `stale` (wrong tree) | **never auto-remount silently** — a stale mount may hold unflushed writes. Report, refuse writes, and require an explicit `bunker mount --recover` |
| `stranded` mountpoint (no live FUSE) | clear it automatically (normal unmount, falling back to lazy) then remount — bounded to one attempt |
| `busy` mountpoint (live consumer) | **never** touch it; report the holder (pid/process) by name |

**The rule that keeps recovery safe:** recovery may **re-establish** a mount, never **re-target** one. Recovery is idempotent — running it twice leaves the same state as running it once — and every step is logged, because a silent auto-repair is indistinguishable from the bug it is repairing.

### 5. Cleanup that cannot be forgotten

- `bunker umount <agent|mountpoint>` — idempotent (already gone = success), clears dead sessions via lazy unmount when a normal unmount fails, reports exactly what it did.
- `bunker mount status` / `--json` — every live mount as data: agent, mountpoint, remote path, namespace, pid, health verdict, age, last successful probe.
- **Teardown hooks:** agent destroy, session end, and daemon shutdown each release that session's mounts. A destroyed agent must leave **zero** local mountpoints.
- **Orphan sweep:** a mount whose owning pid no longer exists is reported (and cleaned on request) — the mount-side twin of the agent registry reconciliation in GAP-070.

### 6. Where the state lives

| State | Cardinality | Location |
|---|---|---|
| live mounts (agent, mountpoint, remote path, namespace, pid, health) | one row per live mount | `$XDG_RUNTIME_DIR/bunker/mounts.json` (per user, per boot) |
| mount identity marker (content hash captured at mount) | one per mount | recorded with the row |
| durability option assertions | constant | asserted by test |

Per-session, never a global registry, and never shared with another user.

## Failure-mode → mechanism matrix (the closure table)

| Failure | Detected by | Recovered by | Closes as |
|---|---|---|---|
| Phantom space | health predicate (`unreachable`) | `reconnect` window → remount once | `mount_unreachable` on refusal |
| Silent empty tree | mount-time preflight (path exists + non-empty + identity) | none — refused before mount | `workspace_invalid` |
| Stranded mountpoint | no live FUSE session for a recorded row | auto-clear (normal → lazy) + remount once | `mount_recovered` |
| Unbounded hang | `ConnectTimeout` + `ServerAlive*` + hard probe timeout | n/a — bounded by construction | `mount_unreachable` within ≤45s |
| Stale identity | marker content mismatch | **manual** `--recover` only | `mount_stale` |
| Concurrent writers | lease registry (per tree, on the bunker) | refusal naming the holder | `lease_conflict` |
| Forgotten cleanup | status surface + orphan sweep | umount / teardown hooks | `mount_removed` |

## Acceptance criteria (observable, each independently checkable)

1. **Phantom-space proof.** Mount a real agent; drop the transport (`iptables` drop or kill the agent's sshd); attempt a write. → The write is **refused within ≤45s** with `mount_unreachable`; no success is reported; no partial file remains on either side.
2. **Reconnect proof.** Blip the transport for <15s and restore. → The mount recovers with **no operator action** and the next read is correct (not a cached stale entry).
3. **Empty-tree proof.** Mount an agent whose home is empty (or a `--path` that does not exist). → **Refused before mounting** with a named cause; nothing appears at the mountpoint.
4. **Stale-identity proof.** Mount; destroy and re-create the agent under the same name; probe. → Reported `stale` (**not** healthy, **not** `unreachable`), writes refused, and no automatic remount without `--recover`.
5. **Stranded-mountpoint proof.** `kill -9` the sshfs process; attempt to mount again. → The stale mountpoint is cleared automatically and the remount succeeds, without the operator running `fusermount` by hand.
6. **Idempotent-recovery proof.** Run the recovery path twice. → State after the second run is identical to after the first; one mount, one registry row.
7. **Cleanup proof.** Destroy the agent (and separately, end the session). → Zero local mountpoints remain; `bunker mount status` lists none; an orphan sweep finds nothing.
8. **Hang-bounded proof.** With the peer firewalled, `stat` the mountpoint. → Returns a named error within the bounded window; the tool never blocks indefinitely.
9. **Busy-mount proof.** With a live consumer holding the mountpoint, request unmount. → Refused, reporting the holder by pid; the live mount is not disturbed.
10. **Anti-lying proof.** For every criterion above: no success is reported without a prior successful health probe or a read-back; `unverified` is never rendered as success.

## Boundaries — what this spec does NOT do

- **Does not make a mount safe for builds.** It stays editing-only; the do-not-build guard is GAP-105.
- **Does not enforce session binding.** A mount cannot; concurrent writes are guarded by the per-tree lease registry.
- **Does not silently repair a stale tree.** Auto-recovery re-establishes, never re-targets.
- **Does not pool mounts across sessions.** Per-session mountpoints, per-user state.
- **Does not replace the verb layer.** It is the high-fidelity editing path; targeted writes and attribution remain the verbs' job.
