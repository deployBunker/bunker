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

## The tool surface (and the parameters that make it feel local)

The surface is **derived from the tools I already have**, then extended with what `toolsd` adds. Parity is the requirement: if my local `read_file` takes an offset and a line limit, the remote one must too, or I will behave differently on remote trees and not notice.

### read — line windows, not whole files

Local `read_file(path, offset, limit)` returns line-numbered output, a total line count and a continuation hint. `toolsd`'s `fsops.Read` returns **whole file content only** — no window. So this cannot be a passthrough; it is a bounded window read with line numbers, which is a shell read pattern, not a toolsd call.

| Verb | Parameters | Returns | Backed by |
|---|---|---|---|
| `bunker_read` | `path`, `offset` (1-based), `limit`, `max_bytes` | line-numbered window, `total_lines`, `next_offset`, explicit truncation notice | bounded `sed -n 'START,ENDp'` + `wc -l` on the agent |

### search — modes, not one grep

Local `search_files` has three output modes, a context count, a file glob and a limit. A single "search" verb that only returns matches loses two thirds of the behaviour I rely on.

| Verb | Parameters | Returns | Backed by |
|---|---|---|---|
| `bunker_search` | `pattern`, `path`, `target` = `content` \| `files`, `output_mode` = `content` \| `files_only` \| `count`, `context`, `file_glob`, `limit` | matches with line numbers, or a file list, or per-file counts | `rg -n -C<context> -g<glob> -m<limit>` / `rg --files` / `rg -c` |

### write / edit — the three shapes are genuinely different

This is where the local and remote surfaces diverge most, and where a naive mapping breaks.

| Verb | Parameters | Semantics | Backed by |
|---|---|---|---|
| `bunker_write` | `path`, `content` | whole-file overwrite, parents created, atomic (temp + rename) | `toolsd apply` (one entry) or `fsops.Write` |
| `bunker_edit` | `path`, `old_string`, `new_string`, `replace_all` | literal replace; **unique match by default**, refuses naming the count | `toolsd replace` |
| `bunker_patch` | `diff` (unified diff text) | strict, **no fuzz** — refuses rather than corrupting | `toolsd patch -` (stdin) |
| `bunker_apply` | `edits[]` = `{path, content}` | atomic all-or-nothing across files, with rollback | `toolsd apply` |

**Two parity gaps to state honestly:**

1. **My local `patch` takes `old_string`/`new_string`; `toolsd patch` takes a diff.** So `bunker_patch` needs either a diff, or an adapter that synthesizes a unified diff from the two strings. `bunker_edit` is the honest home for the two-string case and is already backed by `toolsd replace` — whose unique-match default is *exactly* the "refuse instead of silently multi-hitting" behaviour my patch tool promises. So the two-string path maps to `toolsd replace`, and `bunker_patch` stays for genuine diffs.
2. **My local `patch` fuzzy-matches (9 strategies); `toolsd patch` refuses on inexact input.** Losing the fuzz is arguably the improvement — a refusal is visible, a fuzzy match is silent — but it *is* a behaviour difference and must be documented, not discovered.

### exec, alternatives, leases

| Verb | Parameters | Semantics | Backed by |
|---|---|---|---|
| `bunker_exec` | `command`, `timeout`, `cwd` | run to completion, capture stdout/stderr/exit | `ExecAgent` |
| `bunker_run` | `command`, `detach`, `timeout`, `name` | long jobs; detached returns a run id | `RunAgent` |
| `bunker_lsp` | `op` = `definition` \| `references` \| `check`, `file`, `line`+`character` or `offset`, `root` | symbol queries before an edit | `toolsd lsp` |
| `bunker_lease` | `action` = `acquire` \| `renew` \| `release` \| `status`, `holder`, `paths`, `ttl` | edit lease on the remote tree | `toolsd lease` |

**Note on optional verbs:** `bunker_lsp` needs a language server *on the agent*, so it is the one verb that may legitimately be unavailable. It answers `capability_unavailable` naming what is missing (see below) rather than disappearing from the surface — which is the design point of the next section.

## The surface is CONSTANT. The bunker is an argument.

This is the rule that keeps the design from exploding, and it is worth stating as a hard constraint because the obvious wrong turn is very inviting.

**The wrong turn:** register tools *per bunker*. N verbs × M bunkers. With this fleet's real numbers that is not hypothetical — 2 agents live today, a fleet that runs to dozens, and a verb set of ten. Twenty bunkers would mean **twenty read tools, twenty search tools, two hundred registrations**, plus a discovery and sync problem: every spawn, destroy and restart changes the tool list, so the agent's tool surface becomes a function of remote infrastructure state.

**The rule:** *the tool count is fixed by the number of verbs and is completely independent of how many bunkers exist.* The target is **data**, resolved in this order:

1. **explicit argument** on the call — `bunker_read(target, ...)`
2. **the session's binding** — `bunker_bind(server, agent, repo)` once at session start, then every call uses it
3. **refusal** naming the missing binding — never a global fallback

So "which bunker" is a *value*, and the tool list is a constant. Nothing registers per-bunker; nothing has to be confirmed per agent; spawning or destroying a bunker does not change the surface I am offered.

### What "tool state" actually is

The state this design needs is small and session-scoped, not per-bunker:

| State | Cardinality | Where |
|---|---|---|
| session → bound target | one row per session | session profile (`BUNKER_HOME`), keyed by `HERMES_SESSION_ID` |
| session → server list + token | one file per session | `$BUNKER_HOME/config.yaml` |
| which bunkers exist | fleet-wide inventory | the scheduler / `bunker list` — **not a tool-registration concern** |

There is deliberately **no** per-bunker tool table, no capability sync, and no dynamic registration to reconcile.

### Capability variance is an ERROR, not a dynamic list

Bunkers differ — an older daemon lacks a verb, an agent has no language server, an agent is stopped. The tempting answer is to reflect that in the tool list. The rule is the opposite:

- the verb stays on the surface
- the call returns an explicit, structured failure naming what is missing and what to do — `capability_unavailable: toolsd not found on agent` / `agent_stopped` / `predates capability reporting`
- the binding verify (below) reports the *known* capability set once per binding, so the failure is predictable rather than surprising

That keeps the surface constant and fail-visible. A dynamic tool list would make the same information arrive as a silently missing tool, which is strictly worse: the agent cannot distinguish "not supported" from "not loaded" from "I forgot".

### How this is delivered without a registration step

Nothing here requires a new tool registration per bunker, and in the common case it requires **none at all**:

- **`toolsd` already serves itself as MCP** (`toolsd mcp` — every verb as a tool over stdio). The edit primitives can be exposed **once**, with the bunker as the execution target rather than a tool dimension.
- **The Hermes shim** (S6) adds the `bunker_*` verbs **once** in the Hermes agent project, with `target` as an argument.
- The long-term shape: my existing editing tools (`read_file`, `patch`, `search_files`) keep their names and parameters, and the shim routes them to the bound target — so the surface I see does not change at all as bunkers come and go.

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
11. **Constant-surface proof (the anti-explosion criterion).** Spawn a fourth bunker and destroy a fifth, then enumerate the offered tool list. → The count and names are **byte-identical** before and after; nothing registered, nothing removed. Repeat with two sessions bound to different bunkers in the same moment: each sees the same constant list, and only the *target* differs.
12. **Parameter-parity proof.** For each of `read`, `search`, `write`, `edit`, `patch`, `apply`, run the same arguments locally and remotely on the same file and diff the results. → Identical output modulo the tree, including `read`'s `offset`/`limit` windowing and line numbering, `search`'s three `output_mode` shapes and `context`, and `edit`'s unique-match refusal naming the occurrence count. Any difference is recorded as a documented divergence, not left implicit.
13. **Capability-error proof.** Call a verb the target genuinely lacks (e.g. `bunker_lsp` on an agent with no language server). → The verb is still on the surface and returns a structured `capability_unavailable` naming the missing piece; the tool list did not change.

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
