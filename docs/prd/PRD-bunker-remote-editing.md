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

## The surface is CONSTANT in SHAPE. Each session's ENABLED SET is its own.

Two rules that are easy to conflate, and conflating them is how this design goes wrong in either direction.

**Rule A — shape is fixed by verb count.** The *catalog* of verbs is independent of how many bunkers exist. Adding a twentieth bunker must not add a twentieth read tool.

**Rule B — the enabled set is session-mutable.** Which verbs *this session* has enabled is dynamic: a session can enable or disable a tool mid-life, and that changes the tool list that session is offered. Rule A does not forbid this; it only forbids the catalog from being a function of remote infrastructure.

Together: *one catalog, N independently mutable session views.*

### Why the two are different problems

| | Catalog (Rule A) | Enabled set (Rule B) |
|---|---|---|
| Depends on | the verb list — a code constant | session intent and task |
| Changes when | a verb is added to the code | a session enables/disables, at any time |
| Cardinality | **1**, fleet-wide | one row **per session** |
| Must not depend on | bunker count | any other session |

### The current isolation gap (measured)

Hermes today has both halves of this, but they are at different scopes:

- `hermes chat -t <toolsets>` selects toolsets for **one run** — genuinely per-session, but ephemeral and set only at start.
- `hermes tools enable|disable` is the **persistent** surface, and it writes to a single global config: `agent.disabled_toolsets` and `plugins.disabled` in one `config.yaml` read by **every** session on the box.

So enabling a tool for one session currently toggles a global — the same shared-mutable-pointer shape as `active_server` on the routing side (22 implicit sites, rewritten mid-session). A session that enables a verb for its own task silently changes what every other session is offered.

**Design rule: a session's enabled set is session state, never global config.** Enabling a verb for this session writes one row — this session's own — and re-projects the tool list for this session only. Nothing else on the box changes.

### Tool state: what is shared and what is never shared

The requirement is precise: a tool loaded for one session must not reuse another session's state. The clean split that satisfies it *and* keeps memory bounded:

| Layer | Sharing | Why |
|---|---|---|
| **Registration metadata** (name, JSON schema, description) | **shared, immutable** | it is identical for every session; duplicating it would cost memory × sessions for zero isolation benefit |
| **Enabled set** (which verbs this session sees) | **per session**, mutable | this is the isolation boundary that Rule B needs |
| **Execution state** (MCP client connection, plugin instance, LSP session, caches, lease handles, open file handles) | **per session, never shared** | this is where "reusing state from other sessions" actually happens, and it is the thing that must not leak |

**The rule:** *registrations are shared and immutable; execution state is per-session and never pooled across sessions.*

A pooled stateful client handed from session A to session B is exactly the leak to prevent — B would inherit A's subscriptions, working directory, half-open file, cached reads, or LSP document state. Envelope those per session; pool only what is provably stateless.

**Resource honesty:** per-session execution state costs memory in proportion to *concurrently active* sessions, not total sessions (1,264 starts/day, far fewer concurrent). Reclaim on session end; a session that dies must not strand its connections. If a verb's per-session state is expensive, the mitigation is to make the client stateless or lazy — not to share it.

### What the session tool list is, mechanically

1. **catalog** — a constant list of verb definitions (shared, immutable)
2. **session enabled set** — a per-session row, defaulted from a profile, mutable at runtime
3. **projection** — the intersection, rendered into that session's system prompt as the tool list the model sees
4. **dispatch** — a call resolves against **the session's own enabled set**, never a global; an unenabled verb is refused as `tool_not_enabled_in_session`, distinct from `capability_unavailable`
5. **state** — any instance the call creates is keyed `(session_id, tool)` and released at session end

Because the projection is per-session, enabling a tool mid-session is a local change that takes effect for that session on its next turn, and is invisible to every other session.

### Delivery with no per-bunker, and no per-session, registration step

- `toolsd mcp` already serves every verb as an MCP tool — the **catalog** is one registration with the bunker as an argument.
- The `bunker_*` shim is registered **once** in the Hermes agent project.
- What varies per session is only the enabled set and the execution state — both data, neither a registration.

So: **no per-bunker tools, no per-session re-registration, and still fully per-session dynamic enable/disable.**

## What Hermes already supports (audited 2026-09-20)

Nothing below is a proposal — it is what the running system does today, checked on this box. It resizes the build significantly, and it corrects one of my own assumptions.

| Capability | State | Evidence | What it means for this design |
|---|---|---|---|
| **MCP per-session instance isolation** | **already true** | 17 `toolsd mcp` processes, **15 distinct parents** (Hermes session processes + `cron.scheduler`). Each session spawns its own stdio child. | The state-isolation requirement is **already satisfied for MCP tools**: no shared client to leak state. What is missing is only a *guarantee* + test, not new machinery. |
| **`toolsd` already registered as MCP** | **already true** | `config.yaml:1060` → `mcp_servers.toolsd` = `toolsd mcp`, `enabled: true` | The catalog registration exists. No new registration step is needed to expose the primitives. |
| **Per-run toolset projection** | **true, ephemeral** | `hermes chat -t <toolsets>` — "Comma-separated toolsets to enable", per run | Rule B's projection mechanism exists. It is start-time-only, and there is no mid-session toggle. |
| **Persistent enable/disable** | **global, not per-session** | `hermes tools enable\|disable` writes one `config.yaml` (`agent.disabled_toolsets`, `plugins.disabled`); `known_plugin_toolsets`, `platform_toolsets`, `toolsets:` sections are all global | **This is the one real gap for Rule B.** Enabling a tool toggles a global read by every session. |
| **Profiles exist but no per-session resolution** | **partial** | `hermes profile` has `list/use/create/…`; `use` sets a **sticky default**, not a per-session pick | There is machinery to hang a per-session profile on, but no resolution keyed by session today. |
| **Shell hooks can gate a tool call** | **available, unused** | `hermes hooks` (shell + outbound webhooks), with a first-use consent allowlist; none configured here | A hook is a viable interim enforcement point for "this verb is not enabled in this session" while the native enabled-set lands. |
| **`sshfs` on this box** | **present** | `/usr/bin/sshfs` (SSHFS 3.7.3), `fusermount3` 3.18.2 | The mount path needs no new dependency on the client side. |
| **`bunker mount`** | **exists, agent-scoped** | `internal/cli/mount.go`; wires `IdentityFile` + host via `resolveUserAtHost`, `rewriteSSHFSMount` (DF-BUNKER-14) | A working mount client already exists; DF-BUNKER-17/20 fixed its failure reporting. |

**Conclusion:** three of the seven things this design needed are already shipped, and one more (hooks) is an available interim. The build is smaller than the first draft implied and reduces to: **one real Hermes gap (session-scoped enabled set) + the remote verb layer + the mount path.**

## Two delivery paths (and why both, not one)

The review proposed a second route to the same goal: mount the bunker's tree locally over SSHFS, so file edits travel the mount and only *commands* run remotely. That is right, and it is the more stable path for some operations — but the two routes fail in opposite places, so the design keeps both and splits the work between them by what each is good at.

| | **Path A — remote verb layer** | **Path B — SSHFS mount + remote exec** |
|---|---|---|
| How an edit lands | `bunker_write` / `bunker_edit` / `bunker_apply` → exec on the agent | write the mounted local path; bytes go over SFTP |
| How a build runs | remote (`bunker_build`) | remote (`bunker_build`) — **never local** |
| Transport | one RPC per operation, explicit target | kernel VFS / SFTP, POSIX semantics |
| Tool behaviour | must be *made* equivalent to local tools (parameter parity, proofs 12) | **natively identical** — it is a real filesystem, so `read_file`, `patch`, `search_files`, `git diff` all work unchanged |
| Latency | one round-trip per call (~RPC) | per-syscall; fine for edits, **fatal for builds** |
| Git behaviour | remote by definition | local `git` reads/writes `.git` through the mount — convenient, but every object write is a round-trip |
| API keys / env | remote: the agent's own keys, never the local box's | remote for commands; local tooling does not need the repo's credentials |
| Failure mode | an RPC fails loudly and attributable | a stalled mount **hangs** the tool instead of erroring |
| Isolation | per-session binding, provable | **hard-linked mountpoint** — needs path-per-session isolation |

**The rule that makes Path B safe:** *the mount gives you file-level edits; it never gives you execution.* Anything that compiles, tests, installs or builds runs through `bunker_build`/`bunker_exec` **on the agent**. That single rule removes the objection that made me rule the mount out: local CPU is never spent traversing the tree, because no build ever walks the mount.

**Honest limitation, stated rather than discovered:** a mount cannot enforce binding. Two sessions mounting the same agent see the *same tree* through different mountpoints — correct for shared work, unguarded for concurrent writes. So Path B relies on the lease registry (which is per-tree and lives on the bunker) to turn a collision into a refusal. Path A gets binding as a first-class guarantee; Path B gets it from leases.

### What each path is best for

- **Path B (mount)** for reading, grepping, diffing, small edits and anything where "exactly like local" matters most — it is the highest-fidelity option because it *is* a filesystem. It also makes `git` behave normally for inspection.
- **Path A (verbs)** for the operations that must be explicitly targeted, attributed, and refuse-on-unbound: writes that matter, atomic multi-file edits, leases, and anything that must appear in the audit chain with a session id.
- **Both** for builds: always remote, either transport.

## The design, restated

Goal: **file-edit tools as good as the local ones, so projects on remote servers are managed as if local, without copying data around.**

1. **Hermes side (one real gap).** Make the enabled set session-scoped instead of global, keep MCP instances per session (already true — pin it with a test), and expose a mid-session enable/disable. *Everything else on the Hermes side already exists.*
2. **Verb layer.** The `bunker_*` verbs with true parameter parity, constant catalog, target-as-argument.
3. **Mount path.** Ship `bunker mount` as a supported editing surface with the `DO NOT BUILD LOCALLY` rule, per-session mountpoints, and a lease-backed write guard.
4. **Never copy data.** Neither path mirrors or syncs the repo; the tree stays on the bunker and session data stays local, which is the original goal.

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

## Routing: always the right bunker

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

- **A new RPC family for file verbs.** Everything reduces to exec/run; new RPCs fork the surface for no gain.
- **Cross-bunker distributed locking.** Different bunkers are different trees; there is nothing to lock across them.
- **Auto-recreating a lost agent.** Re-binding is a session decision — silently adopting a fresh empty tree is itself a failure mode.

> **Superseded:** an earlier draft listed the SSHFS mount as out of scope. That was wrong, and the reason it was wrong is worth keeping. The objection was that routing tool calls through a mount "reintroduces a shared mutable view and pays local CPU for every metadata op." The first half is true but irrelevant to editing, and the second half only bites if builds traverse the mount. The review reframed it correctly: **mount for edits, remote exec for builds.** With builds kept off the mount, the objection dissolves. See "Two delivery paths" below.

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
14. **Session-isolation proof (the enable/disable requirement).** Two concurrent sessions. Session A disables a tool and Session B leaves it enabled; then both enable/disable a different tool in the same minute. → Each session's offered tool list is exactly what *that* session set; B's list is unchanged by A's toggles in both directions, and the global config's `disabled_toolsets` / `plugins.disabled` are byte-identical before and after. Neither session's change is visible in the other.
15. **No-state-reuse proof.** Session A loads a stateful tool (MCP client, LSP session, or a cached read) and leaves it warm; Session B then loads the same tool. → B receives a fresh instance with no A-derived state: no inherited working directory, cached read, open handle, subscription or document version. Prove it by having A leave a distinguishable artifact (a cached value or an open path) and asserting B's instance does not contain it.
16. **No-stranding proof.** Kill a session holding per-session tool state. → Its connections/instances are reclaimed on session end rather than stranded, and a subsequent session's memory footprint is unaffected by the dead session's prior state.
17. **Mount-fidelity proof (Path B).** Mount a real agent's tree and run the local tools against the mountpoint: `read_file` on a line window, `patch` on a known edit, `search_files` on a pattern, `git diff` and `git log`. → Byte-identical results to running the same operations against a local checkout of the same commit. Any divergence is recorded as a documented mount limitation.
18. **Never-build-locally proof.** With the mount active, run a build/test through the local toolchain by mistake (or by a naive `make`). → The design must make this either impossible or loudly wrong: the mount ships a guard (a repo-root marker plus a wrapper/hook that refuses and names `bunker_build`), and the proof shows the refusal firing rather than a silent 8-hour local build.
19. **Mount-failure proof.** Kill the SSH transport mid-edit (drop the agent's sshd or the network). → The failure surfaces as a bounded timeout with a named cause, **not** an indefinitely hanging tool; no partial write is left behind, and the mountpoint is recoverable with one command.
20. **Mount-isolation proof.** Two sessions mount the same agent. → Each gets its own mountpoint path (never a shared one), a write by one is visible to the other through the filesystem, and a conflicting concurrent edit is refused by the **lease registry** (the documented Path B guard) rather than silently interleaved.

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
| **S8** per-session tool enable/disable + state isolation | **M** | enables Rule B: session-scoped enabled set, per-session execution state | S6 (Hermes agent project) |
| **S9** mount path as a supported editing surface | **M** | Path B: `bunker mount` hardening, per-session mountpoints, DO-NOT-BUILD guard, lease-backed writes | S1 |
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
