# PRD: `bunker-remote-editing` — remote file editing for Hermes sessions

**Project:** bunker (owning) · **Rows:** GAP-092..GAP-098 (bunker), `CHT-052` (coding-hermes-tools) · **Author:** Hermes (bunker thread) · **Date:** 2026-09-20
**Status:** proposed · **Effort:** M (four S/M slices + one L shim) · **Depends on:** `toolsd` (coding-hermes-tools) · **Feeds:** every Hermes session that edits code

---

## The one-liner

A Hermes session edits code **on a deployed bunker** — read, write, patch, apply, search, build, test — so the compute runs on the bunker while session data and inference stay on the main box.

## The problem, measured

The main box carries both the thinking and the doing. Every build, test run, grep and compile competes with the Hermes loops themselves.

| Fact | Value | Source |
|---|---|---|
| Sessions started in the last 24h | **1,264** | `state.db` sessions |
| 10-minute buckets holding concurrently-active sessions | **145** (peak 51 in one bucket) | `state.db` messages |
| Agents live on bunker-las-02 today | **2** (677MB and 736MB of a 64GB cap) | `bunker list --server bunker-las-02` |

The bunkers already exist, are already spawned and destroyed by this fleet, and their disk/CPU are idle. What is missing is not capacity — it is that **the editing tools live on the wrong side of the wire**.

## What it looks like to use (the tool list)

This is the surface a Hermes session gets. Each verb maps to one existing bunker operation; none of them is a new protocol.

| Verb | What it does | Runs where | Backed by |
|---|---|---|---|
| `bunker_read <path>` | read a file with a size cap | agent | `ExecAgent` |
| `bunker_write <path> <body>` | write via temp file + atomic rename | agent | `ExecAgent` (stdin, GAP-094) |
| `bunker_patch <path> <diff>` | strict unified-diff apply, **no fuzz** — refuses rather than corrupting | agent | `toolsd patch` |
| `bunker_apply <multi-file>` | atomic all-or-nothing multi-file edit with rollback | agent | `toolsd apply` |
| `bunker_replace <path> <old> <new>` | literal replace, unique-match by default | agent | `toolsd replace` |
| `bunker_search <pattern> <tree>` | bounded search, capped output | agent | ripgrep/`ExecAgent` |
| `bunker_exec <cmd>` | plain shell | agent | `ExecAgent` |
| `bunker_build` / `bunker_test` | build or test; long runs detach | agent | `ExecAgent` / `RunAgent` |
| `bunker_lease <acquire\|release\|status>` | edit lease on the remote tree | agent | `toolsd lease` |
| `bunker_bind <server> <agent> <repo>` | bind this session to one target | session | CLI config |

All ten are thin wrappers. The point of the list is that **the tools that already make local editing safe — strict patch, atomic multi-file apply, leases — are the same binaries, just executing where the tree is.**

## Routing: always the right bunker

The correctness question is how N concurrent sessions never write to each other's bunker or tree.

**Design rule: routing is a value carried on every call, never an ambient global.**

A session binds `(server, agent, repo)` once at `bunker_bind` and every translated call repeats it explicitly. A session with no binding **refuses the operation and names the missing binding**; it never falls back to a default.

Why this rule and not a smarter default: today's mechanism is a shared mutable pointer, and it is measurably fragile.

- `~/.bunker/config.yaml` holds **`active_server`** — one value read by every session on this box. It currently reads `bunker-las-02`, and its sibling entry carries `connected_at: 2026-09-20T03:05:06Z` — a **different session changed the target at 22:05 local today.**
- **22** non-test sites in `internal/cli/` resolve the server implicitly from that global; only **19 of 38** cli files accept a `--server` flag at all.

So the first session to run `bunker use X` silently re-targets every other session. A wrong-bunker write produces no error, no signal — exit 0, and the first evidence is a diff nobody expected. Per-call binding removes the class rather than detecting it.

## Three states, two dangerous edges

| State | What it is | Today |
|---|---|---|
| **DECLARED** | what the session believes it is editing | nothing — sessions declare no target |
| **BOUND** | what the transport resolves at call time | the shared `active_server` global |
| **ACTUAL** | which tree the bytes land in | the agent's home dir on some host |

The dangerous edges are **DECLARED→BOUND** (silent substitution by the global) and **BOUND→ACTUAL** (the agent was re-created and the handle now points at a different, empty tree). Bind-time verification closes both: one cheap probe confirming the target exists, is reachable, and that `repo` is the tree expected.

## Concurrency: how two sessions on one tree stay safe

Routing stops a session reaching the *wrong* bunker. It does not stop two sessions correctly bound to the **same** tree from overwriting each other — that needs locking, and the locking design has one non-obvious constraint.

**Design rule: leases follow the TREE, and the tree is remote.**

`toolsd` stores its lease registry at **`<git-common-dir>/agent-leases.json`**, deliberately shared across linked worktrees of one repo. CHT-007 proved the semantics with a live probe: *6 concurrent CLI processes → exactly 1 grant / 5 refusals naming the holder; the worktree sees the same registry (commondir).*

Consequences, which are the whole design:

- Two sessions bound to **one remote tree** → same registry → the second gets a **refusal naming the holder**, not a silent overwrite. That is the protection.
- Two sessions on **different bunkers or repos** → different registries → zero cross-talk. That is the isolation.
- The registry must therefore live **inside the remote repo**, queried by executing `toolsd` on the agent in one exec call. No shared mount, no NFS assumption — the existing flock handles concurrency.

The failure to avoid, stated plainly: a registry kept **locally**, or **per-agent**, while two sessions edit one shared tree. That produces *separate registries over one tree* — the two writers cannot see each other, get no refusal, and the collision is now invisible while appearing protected. Lease state is isolated **by tree**, never by agent.

Per-session isolation does apply to one thing — credentials and config. `BUNKER_HOME` already relocates the CLI config and agent keys (`internal/cli/paths.go`), so `BUNKER_HOME=$HERMES_SESSION_DIR/bunker` gives each session its own server list and keys. `HERMES_SESSION_ID` exists to key it.

## What already exists vs what is missing

| Already shipped | Missing |
|---|---|
| `ExecAgent` / `RunAgent` with script upload | explicit-target enforcement (GAP-093) |
| Per-agent Linux users, cgroups, resource limits | exec stdin + binary-safe bounded responses (GAP-094) |
| Durable agent registry surviving restarts (GAP-070) | session id in the audit chain (GAP-095) |
| Lifecycle RPCs incl. `agent_stopped` (GAP-071) | `toolsd` installed on agents by design (GAP-096) |
| Tamper-evident audit chain (GAP-047..050) | the Hermes-side shim + per-session profile (GAP-097) |
| `BUNKER_HOME` per-session profiles (DF-BUNKER-16) | lease wrapper + accuracy loops (CHT-052, GAP-098) |
| `toolsd` primitives incl. lease registry | remote-invocation contract for leases (CHT-052) |

Six of the seven gaps are small. The shim is the only large piece, and it cannot start until binding and bodies land.

## Scope

**In scope:** explicit-target binding; exec stdin/base64/output cap; session-id attribution; `toolsd` on agents; lease wrapper for remote trees; the Hermes tool surface; reconcile + accuracy loops.

**Out of scope, with reasons:**

- **A remote filesystem mount (`sshfs`).** `bunker mount` exists for humans; routing *tool calls* through a FUSE mount reintroduces a shared mutable view and pays local CPU for every metadata op — the opposite of offloading.
- **A new RPC family for file verbs.** Everything reduces to exec/run; new RPCs fork the surface for no gain.
- **Cross-bunker distributed locking.** Different bunkers are different trees; there is nothing to lock across them.
- **Auto-recreating a lost agent.** Re-binding is a session decision — silently adopting a fresh empty tree is itself a failure mode.

## Success criteria (replayable proofs)

1. **Wrong-bunker replay.** Two sessions, different servers, neither passing an explicit target, one with `active_server` pointing at the other's bunker. → Both mutating calls refuse with a named missing binding; **zero bytes land in either tree**.
2. **Collision replay (CHT-007 over the wire).** Six concurrent remote edit calls on one tree. → Exactly **1 grant / 5 refusals naming the holder**, one registry record, registry inside the **remote** `.git`.
3. **Inverse proof.** Two sessions on **different** bunkers edit concurrently. → Zero refusals, zero cross-talk, two independent registries.
4. **Stale-binding proof.** Destroy and re-create the bound agent under the same name, then edit. → The session reports STALE and refuses; it does not write into the new empty tree.
5. **Binary-safety proof.** Read a file with NUL bytes and non-UTF8 sequences. → sha256-identical round trip, no truncation.
6. **Cap proof.** Search producing >50MB. → Capped response with an explicit truncation notice naming the cap and how to narrow it, never a silent partial.
7. **Attribution proof.** One edit from each of two sessions. → Two distinct session ids on two audit rows; `bunker audit verify` still passes.
8. **Anti-lying proof.** No verb reports success without a post-verify read-back; a call whose verify cannot run closes as `unverified`, and `unverified` is never rendered as success.
9. **Offload proof.** A real edit-build-test cycle on a remote tree, with the local box's CPU measured during it. → The work demonstrably ran remotely; the local cost is the loop only.
10. **Overhead proof.** Measure per-call verify latency and publish the number. If it exceeds local-equivalent latency beyond a stated factor, demote verification from per-call to per-session rather than assuming it is free.

## Risks

| Risk | Mitigation (design rule) |
|---|---|
| A "default server" creeps back for convenience | mutating paths must not read `active_server`; a test asserts the symbol is unreachable from that call graph |
| Verify latency taxes every call | one cheap probe per binding, re-used until the tree's state marker changes; criterion 10 measures it |
| Leases nag and get disabled | TTL + `--pid` liveness exist; noisy classes are demoted one at a time with the measurement that licensed it |
| Two sessions legitimately share one tree | that is what `toolsd session` worktrees are for; the refusal message must suggest it |
| Remote `toolsd` version drifts from local | version pinned and reported at bind time; mismatch is a named warning, not a silent behaviour split |
| Built before it is proven useful | zero-code proof ships first: `toolsd` on one agent, the tool table hand-driven, results recorded |

## Cost & sequencing

| Slice | Effort | Ships | Depends |
|---|---|---|---|
| **S1** zero-code proof — toolsd on one agent, hand-drive the table | **S** | the contract, demonstrated | — |
| **S2** fail-closed binding | **M** | routing correctness (keystone) | S1 |
| **S3** exec stdin / base64 / cap | **M** | file bodies + binaries | S1 |
| **S4** session-id attribution | **S** | attributed audits | S1 |
| **S5** `toolsd` on agents via image-spec | **M** | reproducibility | S1 |
| **S6** Hermes shim + per-session profile | **L** | the surface above | S2, S3, CHT-052 |
| **S7** reconcile + accuracy loops | **M** | keeps refusals honest | S1 |
| **CHT-052** lease wrapper (remote trees) | **M** | leases not skipped | S1 |

**S2 is the keystone** — nothing else should be relied on for safety until it lands. S6 must not start before S2, S3 and CHT-052, or the shim ships with the wrong-bunker hole open.

**Repo split:** PRD and S1–S4, S7 live in `bunker` (remote: `deployBunker/bunker`). CHT-052 lives in `coding-hermes-tools` (remote: `totalwindupflightsystems/coding-hermes-tools`). S6 belongs to the Hermes agent project.

## What it is NOT

- Not a remote filesystem, mount or FUSE layer — calls are discrete and explicitly targeted.
- Not a new protocol — it is `ExecAgent`/`RunAgent` plus convention.
- Not a per-agent lock manager — leases belong to trees.
- Not a scheduler or dispatcher — it moves *where* work executes, not *when* or *who*.
- Not a replacement for the edit tools — the same binaries, running where the tree is.
- Not a credentials manager — per-session profiles partition credentials; they never store, rotate or mint them.

## Open questions

1. **Per-call verify latency** — unmeasured until S1's proof runs; criterion 10 decides whether verification stays per-call.
2. **Remote lease TTL under a hard agent kill** — treated as `abandoned` and reported, but not claimed handled until probed on a real host.
3. **Which targets get a standing binding** — whether the fleet's own foremen bind by project, or each tick binds explicitly.
