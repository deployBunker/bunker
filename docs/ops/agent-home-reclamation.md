# Agent home reclamation policy

**Status:** policy + tooling in place (DF-BUNKER-55, GAP-080, INT-HOST-007). Reclaiming
is **operator-gated**: nothing deletes an agent home automatically, and the default of
every reclamation verb is a read-only report.

This page is the answer to one operator question: *"`/home` is full of `bunker-*`
directories nobody is using — what is allowed to remove them, and what proves it is
safe?"*

---

## 1. What counts as an orphan home

An **agent home** is `/home/bunker-<agent-id>`, created by `useradd -m` during spawn
(`internal/agent/manager_spawn.go`, Step 2). A home is **stale** — reclaimable — when
both hold:

1. its directory name matches `bunker-*` (the managed-name predicate, `internal/cli/homes.go`
   `managedHomeName`), and
2. **no system user with that exact name resolves** through `os/user.Lookup`
   (`homeUserExists`). The lookup uses the directory name verbatim, including the
   `bunker-` prefix: judging a home by an unrelated human account called `<id>` would
   classify a live agent's home as garbage.

Anything else is out of scope and is never classified, never counted as stale, and
never removed: human accounts' homes, `bunker` itself (no suffix), `bunker-`, dotfiles,
and unrelated directories.

An **inconclusive** lookup (NSS error, not "user absent") aborts the whole scan before
anything is removed. Absent is a fact; unreadable is not.

---

## 2. How an orphan home appears (and why `adopt` never cleans it)

The daemon's tracker knows about *agents*, not about *directories*. Every leak path
below leaves the directory without a live record, and `reconciliation.mode: adopt`
makes that state durable: adopt **re-registers** a record for an owner/user that still
exists, and it has nothing to re-register for a home whose user is gone. An orphan home
is therefore invisible to the tracker and can sit for weeks.

| Leak path | Mechanism | Where it is handled |
|---|---|---|
| Spawn cancelled past the client deadline, then rolled back | rollback stops at a stage that cannot finish (`userdel` without `-r`, a busy mount, a timed-out archive) and the home survives its user | detected by `bunker homes` / `bunker reap` |
| Rollback failure after any spawn stage error | the failure breadcrumb records the stage; the directory is not necessarily gone | `bunker homes` |
| Destroy whose archive fails | **fail-closed by design** (DF-BUNKER-33): the destroy answers `home_retained` and *nothing* is deleted — the home is deliberately left for the operator | `bunker homes`, then retry the destroy |
| A home **copied** instead of moved (migration, snapshot restore, manual `cp -a`) | two directories describe one agent; only the one the passwd entry names is live, the other is a full-size duplicate | `bunker homes` (the duplicate has no user) |
| `useradd -m` on an id whose home survived a previous `userdel` | useradd does **not** clobber an existing home: it warns, creates the user with a NEW uid, and leaves the tree owned by the OLD uid | see §6 — this is why the report names age *and* why destroy archives |

The duplicate-home case is the one that produced the 40 GB finding this policy exists
for (evidence below), and a copy is the mechanism that explains two near-identical trees
created two minutes apart.

**Measured example (read-only, cube-las-00, 2026-09-23):** `getent passwd` listed exactly
one agent user (`bunker-2cdce4d0`), while `/home` carried two agent homes:
`/home/bunker-2cdce4d0` (42 GB, live) and `/home/bunker-eduos-agent` (40 GB, **no user**,
last modified `2026-09-19 14:22` — two minutes after the live home was created). 40 GB of
that host's disk belonged to a directory nothing was tracking and nothing could reclaim.

---

## 3. Step 1 — report (read-only, no daemon required)

`bunker homes` classifies this host's agent homes and prints **one line per stale home
with its on-disk size and its age**, because reclaiming is a size decision:

```bash
bunker homes --dir /home
```

```text
homes status: /home
  entries scanned: 2
  stale (user gone): 2
  kept (live user): 0
  stale size: 3.0 MB
  unmanaged (ignored): 1
  stale entries:
    bunker-2cdce4d0  8.0 KB  age <1m (last modified 2026-10-25)
    bunker-eduos-agent  3.0 MB  age 36d (last modified 2026-09-19)
  run `bunker homes prune` to remove them (use --dry-run first)
```

(output shape verified against a local fixture; sizes/ages come from a real directory
walk and the home directory's mtime — the "last modified" an `ls -ld` shows)

Degradations are explicit, never silent zeros: a home the caller cannot read prints
`size unavailable` on its line, the total is labelled a lower bound, and its age is
still reported.

`bunker reap` gives the same answer for the **whole residue plane** in one pass —
orphan AppArmor profiles, orphan homes, stale systemd linger entries and leftover
`/run/bunker` directories — and is also a dry run by default:

```bash
bunker reap
```

Both commands are local-only: run them **on the host that owns `/home`**, as root, so a
stale home the daemon left behind is actually removable. They are not addressable over
the agent wire protocol.

**Continuous detection:** `bunker status` already folds the same signals into its
`Residue:` line — orphan users, **orphan homes**, orphan keys (the inventory probe).
A duplicate home therefore shows up on a surface operators already look at, without
waiting for someone to run a scan; the reclamation verbs above are what turn that count
into an actionable list with sizes and ages.

---

## 4. Step 2 — reclaim (opt-in, explicit, never automatic)

Removal is a separate, explicitly named action. There is no flag on the report that
deletes; there is no daemon timer that deletes orphan homes; there is no `adopt`
behavior that deletes.

```bash
# 1. see exactly what would go (nothing changes)
bunker homes prune --dir /home --dry-run

# 2. remove exactly the stale entries
bunker homes prune --dir /home

# or the wider residue plane, once an operator has decided
bunker reap --apply
```

Safety properties of the prune path (pinned by `internal/cli/homes_test.go`):

- only entries whose **user is gone** are removed — a live agent's home never is;
- only **managed names** (`bunker-*`) are considered at all;
- an **inconclusive lookup removes nothing** and fails loudly;
- a home whose **size cannot be measured is still removable** (it is the residue an
  operator most wants gone; the measurement is report-only);
- a removal failure stops the run and reports how many entries went, naming the entry
  and the cause — no silent partial success;
- a missing home root is a clean no-op with exit 0.

**Before reclaiming a 40 GB home, confirm it holds nothing you need** — `bunker homes`
tells you *which* and *how big*, not *what*. Copy anything worth keeping out first
(`bunker cp`, a `tar` of the tree) or reclaim from a filesystem snapshot.

---

## 5. The normal path is already covered: `destroy` removes the home

The reclamation verbs exist for the homes whose destroy **never completed**. A destroy
that does complete has always removed the home, and that is unchanged:

1. the home is **archived** to the daemon's `destroy_archive_dir` and the archive is
   verified **before** anything is deleted (default policy);
2. `userdel -r` then removes the user **and** the home;
3. if archiving fails, the destroy is **refused** and answers `home_retained` — nothing
   is deleted (DF-BUNKER-33, fail-closed);
4. `--archive=false` (or `--purge`) is the per-destroy opt-out that deletes the home
   with no copy;
5. the live E2E battery asserts the outcome directly: `e2e-full-battery.sh` checks
   `/home/bunker-e2e-main` exists after spawn and **fails if the directory is still
   there after destroy**.

So the policy is not "make destroy delete homes" — it does. The gap this page closes is
the **homes no destroy will ever visit**, plus the report an operator needs to decide
about them.

---

## 6. What is deliberately NOT automated

- **No automatic deletion of orphan homes.** A directory with no passwd entry can still
  hold unmerged work. Deletion requires an operator decision, recorded in the run.
- **No `--keep-home` on destroy.** The archive *is* the forensic copy: it is taken first,
  verified, and stored under the daemon's archive directory. Keeping the home directory
  instead would leave files owned by a uid that no longer exists, and the next spawn of
  that id would silently inherit them (`useradd -m` does not clobber an existing home) —
  which is exactly the uid-mismatched duplicate this policy exists to prevent. For a
  targeted forensic copy of a live agent, use `bunker cp` **before** destroy.
- **No daemon-side reclamation of `adopt`ed records.** Adopt re-registers a record for a
  user that exists; it does not authorize removing anything on the host.

---

## 7. Operator checklist

1. `bunker homes` (or `bunker reap`) on the host — record the list, the sizes and the
   ages in the ticket/run log.
2. For anything large: inspect what it holds, copy out what matters.
3. `bunker homes prune --dry-run` — confirm the list is the one you just reviewed.
4. `bunker homes prune` — record the `stale removed` count and the resulting free space
   (`df -h /home` before and after).
5. If a home was retained by a **failed destroy** (`home_retained`), prefer retrying the
   destroy (`bunker destroy <id> --archive=false` once the archive is the blocker) so the
   user record goes too, rather than pruning the directory from under a registered agent.

---

## 8. References

- `internal/cli/homes.go` — classification, size/age report, prune (GAP-080, DF-BUNKER-55).
- `internal/cli/reap.go` — one-pass residue reaper, dry-run default, `--apply` opt-in
  (INT-HOST-007 / INT-CI-081).
- `internal/agent/manager_destroy.go` — archive-then-delete, fail-closed `home_retained`
  (DF-BUNKER-33, DF-BUNKER-81).
- `internal/agent/manager_spawn.go` — `useradd -m` home creation and the
  "user already exists ⇒ re-register" reuse path.
- `e2e-full-battery.sh` — spawn/destroy home assertions.
- `docs/dogfood/2026-08-03-integration.md` — end-to-end walkthrough, with the
  reclamation addendum.
- `docs/ops/fleet-inventory.md` — fleet-wide host inventory (where to run this at scale).
