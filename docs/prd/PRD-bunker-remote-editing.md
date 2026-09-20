# PRD: `bunker-remote-editing` — make remote bunker file work feel local and never land in the wrong tree

**Project:** bunker (owning) · tools rows: coding-hermes-tools · shim row: hermes-agent
**Item:** GAP-092..GAP-099 (board rows filed below) · **Author:** Hermes (bunker thread 70902) · **Date:** 2026-09-20
**Status:** proposed · **Effort:** M (one S + three M items, one L) · **Depends on:** `toolsd` (coding-hermes-tools), bunker ⇄ Hermes exec path
**Feeds:** every Hermes session that edits code on a deployed bunker

---

## The one-liner

Give a Hermes session a **session-bound, stateless remote editing surface** onto a deployed bunker — so read/write/patch/apply/search/build/test behave like local tools on a remote tree, and a write can never land in another session's bunker or tree because routing is a value carried on every call rather than a mutable global.

## Why this PRD exists (not a task)

The request was one capability — "make my file tools work against the bunkers so the compute moves off this box". Writing it as a task would have shipped a shim. The narrow shape is what breaks: a shim that trusts an ambient "current server" is *the* wrong-bunker bug, and a lease scheme that isolates state per-agent defeats the collision protection it looks like it adds. This document is the whole circuit: state model, the two failure edges, the loops, and the proofs that close them.

---

## The core insight: three states, two dangerous edges

| State | What it is | Where it lives today | Incident that lived on this edge |
|---|---|---|---|
| **DECLARED** | what the session believes it is working on (repo, bunker, agent) | *nowhere* — today the session declares nothing | — |
| **BOUND** | what the CLI/transport actually resolves at call time | `~/.bunker/config.yaml` → `active_server` | **measured 2026-09-20:** that file reads `active_server: bunker-las-02` while its sibling entry carries `connected_at: 2026-09-20T03:05:06Z` — a *different* session changed the target at 22:05 local. Next mutating call from any other session goes to las-02. |
| **ACTUAL** | which tree the bytes actually landed in | the agent's home dir on the bunker host | an agent destroyed and re-spawned under the same name resolves to a **different, empty tree** while the session still believes it holds the old one |

The dangerous edges are **DECLARED→BOUND** (silent substitution by the global pointer) and **BOUND→ACTUAL** (the target moved underneath a stale handle).

What a naive implementation sees: `active_server` is set, the call succeeds, exit 0. No error, no signal — the write simply lands somewhere else, and the *first* evidence is a diff nobody expected. **22 mutating call sites** resolve their server implicitly today (`ActiveServer` appears at 22 non-test sites across `internal/cli/`), and only **19 of 38** `internal/cli/*.go` files even accept a `--server` flag.

---

## Taxonomy of the failure this must kill

| # | Class | One-line definition | Real example |
|---|---|---|---|
| 1 | **Ambiguous routing** (content) | a mutating call resolves its target from a shared mutable global | `active_server` rewritten mid-session above; 22 implicit sites |
| 2 | **Stale binding** (lifecycle) | the bound agent/tree was destroyed or rebuilt; the handle still looks valid | `agent_stopped` (GAP-071) covers *stopped*; nothing covers *replaced* |
| 3 | **Cross-session collision** (runtime) | two sessions edit one tree; the second silently overwrites | `toolsd` CHT-007 was filed for exactly this (*"merge 8b27266 wiped uncommitted hunks mid-guard"*) |
| 4 | **Registry fragmentation** (design) | the lease registry lives on the wrong side of the wire, so it cannot see the real contender | a registry on this box, or per-agent, with two sessions on one tree |
| 5 | **Identity collision** (credentials) | per-session tokens/keys resolve to one shared profile | `~/.bunker/config.yaml` carries tokens for 3+ servers in one file read by every session |
| 6 | **Attribution blindness** (forensics) | the audit chain records an action but not *which session* performed it | concurrent ticks appear as one actor |

Classes 1, 4 and 5 are the ones today's evidence puts live. Class 3 is why `toolsd` exists. Class 6 is what makes 1–3 hard to debug after the fact.

---

## The mechanism: Bane's rule, taken as design authority

The user's proposed mechanism was: *"make it stateless, or each agent in Hermes has isolated state of the tool so they are not fighting over a lease."*

That is **two rules**, and they are not both right. Both get a named commitment here.

### Rule 1 — Statelessness (accepted, verbatim in spirit)

> **The tool call carries its own routing. There is no ambient current-server.**

Every translation layer call takes an explicit target `(server, agent_id, repo_path)`; the session binds it once at start and every call repeats it. Concretely: **no mutating path may read `active_server`.** A session that has no explicit binding **refuses the operation and names the missing binding** — it never falls back to the global. Because the target is per-call data rather than process state, N sessions in N threads cannot interfere: there is nothing to interfere *with*.

This is what makes concurrency safe here — not locking, but the absence of shared mutable state on the routing path.

### Rule 2 — Lease isolation: **the stated mechanism is inverted, and this is the key correction**

> **Per-agent lease isolation would remove the protection, not add it. Leases must be isolated per *tree*, and shared by everyone touching that tree.**

The measured mechanics decide it:

- `toolsd lease` stores its registry at **`<git-common-dir>/agent-leases.json`** (`internal/lease/lease.go:53`, plus a `.lock` sibling) — deliberately shared across *linked worktrees* of one repo, verified live in CHT-007: *"the WORKTREE sees the same registry (commondir); 6 concurrent CLI processes give exactly 1 winner / 5 refusals / 1 record, conflict refused by holder name with empty stdout."*
- The correct home for that registry is **inside the remote repo on the bunker**, because that is where the contended tree lives.
- Therefore:
  - two sessions bound to *the same remote tree* → **same registry** → second one gets a **refusal naming the holder** instead of a silent overwrite ✅ (that is the protection)
  - two sessions bound to *different bunkers or different repos* → **different registries** → zero cross-talk ✅ (that is the isolation)

An isolated per-agent registry flips the first case into the worst outcome: **shared tree, separate registries, no refusal** — two writers who cannot see each other, which is strictly worse than today's accidental collision because it is now invisible *and* looks protected.

**Rule 2, restated as the design law:** *isolation is a property of trees, not of agents; leases follow the tree, and the tree is remote.*

### The placement corollary (what makes Rule 2 practical)

If the registry must live on the remote tree, then the lease verbs must be **executed remotely** — one `ExecAgent` call running `toolsd lease … --root <repo>` on the agent. No new protocol, no shared filesystem, no NFS assumption. The registry is already flock-guarded (`build-tagged lock.go`), so remote concurrency is handled by the same machinery that already passed the 6-process probe.

---

## The feedback loops

### Loop 1 — Binding (per call): resolve → verify → execute → record
1. Session holds `(server, agent_id, repo_path)` bound at start (explicit flag or session env, never the global).
2. **Verify before execute:** one cheap remote probe that the target exists, is reachable, and that `repo_path` is the tree the session expects (e.g. carries the expected commit or a marker file).
3. Execute the translated operation.
4. Record `(session_id, server, agent_id, repo, op, result)`.
- **Closure:** the call returns success *and* the post-verify matches the expected pre-state. A call whose target fails verification **closes as `refused`**, not as an error to retry.
- **Re-escalates:** 3 consecutive refusals for one binding → the binding is marked STALE and surfaced to the session as a decision (re-bind or abort), never auto-repaired.

### Loop 2 — Collision (per edit): acquire → refuse-or-proceed → release → learn
1. Before a mutating edit, acquire a lease on the changed paths **in the remote registry**.
2. A refusal names the live holder and its expiry (measured behaviour already: empty stdout + named conflict).
3. Proceed only on grant; release on completion (TTL 45m default, `--pid` liveness when the session is long-lived).
- **Closure:** the edit commits *and* the lease is released; a lease whose TTL expires without a release closes as `abandoned` and is reported, because abandonment is the signal that a session died mid-edit.
- **Feeds:** Loop 4's false-refusal metric; and the "two sessions on one tree" event is the trigger to prefer `toolsd session` worktrees.

### Loop 3 — Reconcile (per session end): declared vs actual
1. At session end, compare the declared binding against the actual recorded writes.
2. Any write that landed outside the declared tree → **hard finding**, reported with the audit rows that prove it.
- **Closure:** zero out-of-binding writes for the session *and* the declared tree's HEAD provenance check.
- **Feeds:** the accuracy baseline (where the router actually sent traffic).

### Loop 4 — Accuracy (per week, compounding): measure → loosen or tighten
1. From the recorded calls: refusal rate, false-refusal rate (a refusal where no live conflict existed), and unattributed-call count (must be **0**).
2. A class with a precision floor that keeps refusing harmless operations gets **demoted** (warn instead of refuse) — one class at a time, with the measurement that licensed it.
3. Recurrence of a real collision class → **promote to a guard** (a rule, not a warning).
- **Closure:** every class is either refusing, warning, or retired; no class sits undefined.
- **Compounds:** each retired class shrinks the noise floor, which is what keeps operators from disabling the check.

### Loop 5 — Attribution (per call, for forensics)
Session id flows into the existing audit chain (GAP-047..050 already hash-chain every RPC; the gap is that the chain records *an* actor, not *which session*).
- **Closure:** an incident can be reconstructed as "session S, on bunker B, touched tree T at time X" without guessing.

---

## Lifecycle state machine

```
unbound ──bind(server,agent,repo)──> BOUND
BOUND ──verify(pre) ok──> VERIFIED
VERIFIED ──lease grant──> EDITING ──commit+release──> CLOSED(ok)
VERIFIED ──lease refuse──> REFUSED(named holder) ──holder releases──> retry | abort
VERIFIED ──verify fail──> STALE ──(3x)──> ESCALATED(session decision)
EDITING ──ttl expiry, no release──> ABANDONED ──> reported
any ──session end──> RECONCILE(declared vs actual) ──> CLOSED | FINDING
```

Every transition is an **append-only event**; nothing is mutated in place, so the trail survives a crash mid-loop.

---

## Scope

### In scope
| Component | What it does | Repo |
|---|---|---|
| **Fail-closed binding** | explicit target required on mutating paths; no ambient fallback | bunker (CLI) |
| **Exec `stdin` + base64 + output cap** | file bodies in, binary-safe bodies out, bounded responses | bunker (RPC) |
| **Session id passthrough** | attribution in the audit chain | bunker (daemon) |
| **`toolsd` on agents** | the edit primitives present remote | bunker (image-spec extras) |
| **Lease wrapper** | auto acquire/release around remote edits | coding-hermes-tools |
| **Hermes tool shim** | `bunker_read/write/patch/apply/search/exec/build/test` + per-session profile | hermes-agent |

### Per-source capability table (what each layer can and cannot answer)

| Layer | Can answer | Cannot answer | Visible state when absent |
|---|---|---|---|
| Remote `toolsd` | patch/apply/replace integrity, diff3, symbol queries, lease state | anything about *which* Hermes session is calling | n/a — always present once installed |
| Bunker RPC | exec results, agent lifecycle, resource/IPC limits, audit chain | the caller's intent or declared repo | `agent_stopped` / `not_found` (explicit codes) |
| Hermes shim | routing, binding, translation, result shaping | remote auth failures beyond the daemon's own codes | must render **"unbound — refused"**, never a silent default |
| Audit chain | what ran, when, exit code, hash-chained order | *why*, and (until Loop 5) *who* | absent session id renders as `not captured`, never `0` |

### Out of scope (with reasons)
- **A remote filesystem mount / sshfs for tool calls.** `bunker mount` exists for humans; routing tool calls through a FUSE mount reintroduces a shared mutable view and local compute for every metadata op — the opposite of the goal.
- **A second protocol.** Everything reduces to `ExecAgent` + `RunAgent`; adding RPCs for file verbs would fork the surface for no gain.
- **Making the shim a new agent harness.** It is a translation layer over existing tool semantics.
- **Cross-bunker distributed locking.** Different bunkers are different trees; there is nothing to lock across them, and pretending otherwise invents a coordination problem.
- **Automatic agent re-creation on STALE.** Re-binding is a session decision (Loop 1's escalation), because silently adopting a fresh empty tree is failure class 2.

---

## User stories

- **As Bane**, I add a feature from chat and the build/test compute happens on a bunker, while my session history stays in one place here — I get the same edit loop with the CPU bill moved.
- **As a foreman tick**, I edit a repo on a remote bunker and my file operations look identical to local ones, so my briefs and verification recipes don't fork per target.
- **As a concurrent session**, I try to edit a tree another session holds and I get a **refusal naming the holder** — I do not get a silently overwritten file.
- **As an operator**, a session dies mid-edit; the abandoned lease expires and is reported, and the next session can proceed without me hand-clearing anything.
- **As an auditor**, I ask "which session wrote this file" and the answer is a row, not a guess.
- **As a new joiner**, I read this doc and can execute the replayable proofs below without tribal knowledge.

---

## Success criteria — replayable proofs, not adjectives

1. **Replay today's incident.** Two sessions, different servers, neither passing an explicit target, one of them with `active_server` set to the other's bunker. *Proof:* both mutating calls refuse with a named missing binding; **zero** bytes land in either tree. (Today this class is live: config shows the global was rewritten by another session at 22:05 local.)
2. **Replay the CHT-007 concurrency probe.** Six concurrent remote edit calls on one tree. *Proof:* exactly **1 grant / 5 refusals naming the holder**, one registry record, registry inside the remote `.git`. (Local equivalent already passes; the proof is the same result *over the wire*.)
3. **Inverse proof (the correction).** Two sessions on **different** bunkers edit concurrently. *Proof:* zero refusals, zero cross-talk, two independent registries — demonstrating isolation is by tree.
4. **Stale-binding proof.** Destroy and re-create the bound agent under the same name; issue an edit. *Proof:* the session reports STALE and refuses; it does not write into the new empty tree.
5. **Binary-safety proof.** Read a file containing NUL bytes and non-UTF8 sequences through the shim. *Proof:* byte-identical round-trip (sha256 match), no truncation, no corruption.
6. **Cap proof.** Search a tree producing >50MB of output. *Proof:* the response is capped with an explicit truncation notice naming the cap and how to narrow the search — never a silent partial result.
7. **Attribution proof.** Perform one edit from each of two sessions. *Proof:* the daemon audit chain shows two distinct session ids on two rows.
8. **Anti-lying proof.** For every criterion above, the shim may not report success without a post-verify read-back; a call whose verify cannot run closes as `unverified`, and `unverified` is never rendered as success.
9. **Budget proof.** Measure the per-call overhead of the verify step on the live path and publish the number in the row rather than assuming it is negligible; if it exceeds the local-equivalent latency by more than a stated factor, Loop 4 demotes the check to per-session instead of per-call.

---

## Risks & failure modes

| Risk | Mechanism | Mitigation (design rule, not intention) |
|---|---|---|
| The shim recreates the exact bug it replaces | a "default server" creeps back for convenience | mutating paths **must not read** `active_server`; a test asserts the symbol is unreachable from the mutating code path |
| Verify step adds latency to every call | remote round-trip per operation | verification is *one* cheap probe per binding, re-used until the tree's state marker changes; Loop 4 measures it (criterion 9) |
| Leases become nagging and get disabled | false refusals on stale/abandoned leases | TTL + `--pid` liveness already exist; Loop 4's precision floor demotes noisy classes one at a time |
| Two sessions legitimately share one tree | long-running pair work | that is what `toolsd session` worktrees are for; the refusal message must suggest it |
| Remote `toolsd` version drifts from local | two binaries, two behaviours | install version is pinned and reported in the binding verify; a mismatch is a named warning |
| Session ids leak into shared artifacts | attribution added to audit rows | session id is an opaque identifier only; never a token, never a filesystem path |
| The whole feature is built before it is proven useful | full shim written up front | the zero-code proof ships first: install `toolsd` on one agent and hand-drive the tool table before any shim code lands |

---

## Cost & shape

| Slice | Effort | What ships | Depends on |
|---|---|---|---|
| **S1 — zero-code proof** | **S** | `toolsd` installed on one agent; the tool table hand-driven against a real remote tree; results recorded | nothing |
| **S2 — fail-closed binding** | **M** | explicit-target requirement on mutating paths + the refusal message | S1 |
| **S3 — exec stdin/base64/cap** | **M** | file bodies in, binary-safe bounded bodies out | S1 |
| **S4 — attribution** | **S** | session id through the audit chain | S1 |
| **S5 — lease wrapper (tools)** | **M** | auto acquire/release around remote edits | S1 |
| **S6 — Hermes shim** | **L** | the `bunker_*` tool surface + per-session profile | S2, S3, S4, S5 |

**Sequencing note:** S2 is the correctness keystone — nothing else should be relied on for safety until it lands. S6 must not start before S2, S3 and S5, or the shim ships with the wrong-bunker hole open.

**Repo split & durability:** PRD lives in `bunker` (has a remote: `deployBunker/bunker`). S2/S3/S4 rows go on the bunker board. S5 goes to `coding-hermes-tools`, which **currently has no git remote** (CHT-013 owns that decision) — so S5's work exists only on this box until that is resolved; flagged here rather than discovered later. S6 belongs to the Hermes agent project.

## Board rows filed with this PRD

| Row | Repo | Depends on |
|---|---|---|
| GAP-092 | bunker | — (zero-code proof; blocks all below) |
| GAP-093 | bunker | GAP-092 |
| GAP-094 | bunker | GAP-092 |
| GAP-095 | bunker | GAP-092 |
| GAP-096 | bunker | GAP-092 |
| GAP-097 | bunker | GAP-092 |
| CHT-052 | coding-hermes-tools | GAP-092 |
| GAP-098 | bunker | GAP-093 |

---

## What it is NOT

- **Not a remote filesystem.** No mount, no FUSE, no shared view — calls are discrete and explicitly targeted.
- **Not a new protocol or a new RPC family** for file verbs; it is `ExecAgent`/`RunAgent` plus convention.
- **Not a per-agent lock manager.** Leases belong to trees; per-agent isolation is the anti-pattern this doc exists to prevent.
- **Not a scheduler or a dispatcher.** It moves *where work executes*, not *when* or *who*.
- **Not a replacement for the existing edit tools.** `toolsd` primitives are the same binaries doing the same job, just executing where the tree is.
- **Not a credentials manager.** Per-session profiles *partition* credentials; they never store, rotate, or mint them.

---

## The critique, taken seriously

**The objection (raised as the feature was being framed):** *"make it stateless or give each agent isolated tool state so they are not fighting over a lease."*

**Failure modes behind it, and the rule that kills each:**

1. *Two sessions silently write the same file* → killed by **Rule 1** (explicit per-call target, no ambient global) **plus shared-per-tree leases**. Statelessness alone does not stop two sessions targeting the same tree; the refusal does.
2. *Lease state becomes a shared mutable resource that itself races* → killed by keeping the registry **inside the remote repo**, where `toolsd`'s existing flock + atomic replace already made 6 concurrent writers produce exactly one winner.
3. *Isolating leases per agent to avoid the race* → **explicitly rejected.** It converts a visible collision into an invisible one (shared tree, separate registries). Isolation is per-tree by law.
4. *Session state leaking across concurrent work* → killed by the per-session profile (`BUNKER_HOME` already relocates config + keys, measured in `internal/cli/paths.go`), which is the part of the objection that is fully correct and is adopted as-is.

**What I did not verify, stated plainly:** the per-call latency of the binding verify step (criterion 9 measures it before relying on it), and the behaviour of remote lease TTL expiry under a hard agent kill (Loop 2 treats it as `abandoned` and reports; it is not claimed as handled until probed on a real host).
