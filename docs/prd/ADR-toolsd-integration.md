# ADR: toolsd integration point for bunker's remote file tools

**Status:** accepted · **Date:** 2026-09-20 · **Owner:** Hermes
**Consumer rows:** GAP-092 (zero-code proof), GAP-094 (exec bodies), GAP-095 (attribution), GAP-096 (toolsd on agents), GAP-097 (shim), GAP-101 (mount as editing surface), GAP-102 (Hermes-side gap)
**Related:** `SPEC-bunker-native-file-tools.md`, `PRD-bunker-remote-editing.md`, `docs/both-ways.md`

---

## The question

How does bunker's remote editing surface get the `toolsd` primitives — and where does the integration point belong? Three shapes were on the table:

- **(a) daemonise** — add an HTTP server to `toolsd`, have `bunkerd` proxy to it
- **(b) library** — treat `toolsd` as a Go library and call it in-process from `bunkerd`
- **(c) deliver** — ship the `toolsd` binary to the agent and invoke it through the exec path

## Decision

**(c), consumed as a CLI + JSON-descriptor protocol — not as a linked library.**

The `toolsd` binary is delivered agent-side by design (GAP-096, via the existing image-spec package-add / host-provision extras path), and every verb rides the exec path that already exists and is already authenticated, attributed and audited (GAP-094 supplies the stdin and bounded binary-safe output it needs).

Concretely: `bunker` does **not** gain a compile-time dependency on `coding-hermes-tools`. It depends on a **contract** — the CLI verb surface plus `toolsd describe --json` — and the version of that contract is pinned and verified per agent.

## Why not (b) — the library option is wrong, and it is wrong in a way worth naming

**(b) puts the file write in the wrong privilege domain.** `bunkerd` runs as **root on the host**. The agent's tree is an unprivileged user's home (`/home/bunker-<id>/`). If `bunkerd` imports `fsops` and performs the operation itself, every edit lands as **root**, bypassing the exact controls this project exists to provide: the agent's uid, its per-session private `/tmp` (GAP-075), its systemd scope and cgroup limits, and its network policy. The agent would be sandboxed against *itself* while the daemon reached into its tree with full privilege. That is the shape of a privilege-escalation bug, not an optimisation.

**(b) also cannot serve a remote instance.** With more than one host in the fleet, `bunkerd` on host A cannot `open()` a path on host B. The library model silently assumes a single-host, daemon-co-located tree — an assumption this fleet already violates (`bunker-las-01/02/03`, plus the local daemon).

**(b) creates a version lock between two independently deployed repos.** `coding-hermes-tools` is stdlib-only and independently versioned and deployed. Linking it into `bunkerd` means a `toolsd` change can require a daemon rebuild and redeploy across the fleet, and a version skew becomes a build error instead of a reported fact.

**The `internal/` rule already enforces the boundary.** Every `toolsd` package lives under `internal/` (`internal/fsops`, `internal/patch`, `internal/multifile`, …), which Go forbids importing across modules. Using it as a library would require promoting those packages to public API — widening a deliberate boundary to solve a problem that does not exist. This is not an interpretation of the rule; it was measured. A throwaway module with a `replace` directive to the tools repo was built against it:

```
$ go build ./...
main.go:6:2: use of internal package
  github.com/totalwindupflightsystems/coding-hermes-tools/internal/fsops not allowed
```

The control (a trivial `main` in the same module) built clean, so the rejection is the `internal` rule and not a resolution failure. Adopting (b) therefore requires *first* refactoring `coding-hermes-tools` to widen its public API.

## Why not (a) — the daemon option buys nothing and costs a transport

An HTTP server inside the agent would duplicate the transport we already have and already trust:

- **New attack surface in the untrusted domain.** A long-running network daemon inside an agent is reachable *by the agent*, and by anything that escapes into it. Today an agent has no listening service of its own.
- **New auth and lifecycle.** A second credential path, a second thing to start, restart, health-check and reap — and who owns that lifecycle inside an agent's systemd scope?
- **It bypasses the audit chain.** GAP-047..050 and GAP-095 give every exec an attributed audit row. A side-channel HTTP write is invisible to that chain unless we re-implement attribution inside it.
- **It needs a port per agent**, entangling the port allocator (QA-BUNKER-4) with the editing surface.

There is a real trigger for (a) later — a *subscription* need (the agent pushing change events to the client, rather than the client polling), or enough small-operation chattiness that SSH round-trips become the bottleneck. That is exactly the trigger already named in MOUNT-004 for a server-in-the-agent design. It is not the trigger today.

## The boundary that makes (c) coherent

Layering, so each piece keeps one job:

| Layer | Runs where | Job |
|---|---|---|
| `bunker-shim` (GAP-097) | Hermes session | the **constant** tool surface; translates an MCP call into a targeted invocation |
| `bunker` CLI | client | resolution (namespace/workspace), fail-closed binding (GAP-093) |
| `bunkerd` | daemon host | transport, auth, audit, agent lifecycle — nothing else |
| `toolsd` **binary** | **inside the agent** | the primitives: strict patch, unique-match replace, atomic multi-file apply, diff3, lease, lsp, and `fsops` confinement |

The decisive property: **the primitives execute in the agent's own privilege and filesystem context**, so uid confinement, private `/tmp`, cgroups and the real workspace all apply by construction — and the mount path (sshfs) and the verb path then read and write **one filesystem view of truth**. Had we done the writes on the daemon host, we would have two processes mutating one tree through two different mechanisms: the sibling-edit-loss class this fleet has already been bitten by.

### Structured payloads ride stdin, not argv

Where a verb carries user content, the payload goes over **stdin as JSON** (`bunker exec --stdin` → `toolsd apply -`), not as quoted argv. Shell quoting over a remote exec boundary is the defect class that produced DF-BUNKER-27 (response corruption on interleaved output) and DF-BUNKER-8 (flag grammar drift). `bunker_edit` wrapping `toolsd replace` may use argv for the needle, but any multi-file edit-set or diff body is stdin data.

### The contract is pinned and verified

GAP-096's own PASS criterion is the right one: the installed `toolsd` version is reported in the binding verify, and a local/remote mismatch is a **named warning**, not a silent behaviour split. `toolsd describe --json` is the machine-readable surface; the shim's catalog is checked against it rather than assumed to match.

## What this explicitly does NOT do

- Does **not** add a compile-time dependency from `bunker` to `coding-hermes-tools`.
- Does **not** promote `toolsd`'s `internal/` packages to public API. No such promotion is needed for this architecture.
- Does **not** add a daemon, port, or credential path to agents.
- Does **not** replace the mount. The mount is the high-fidelity, whole-toolchain editing path; the verbs are the targeted, attributed, confined write path — `docs/both-ways.md`.

## Honest limitations

1. **The verb path's workspace confinement is cooperative, not enforced by the OS.** `fsops` refuses escaping paths *inside toolsd*; a raw `bunker exec 'rm -rf ...'` still runs whatever the agent's own uid permits. The hard boundary is the agent's uid + cgroup + namespace, not the workspace argument. Stated here so it is never mistaken for a sandbox.
2. **One extra process per verb call.** Each invocation pays a process start in the agent (stdlib-only static Go, so this is small but not zero). Only measured chattiness justifies revisiting (a).
3. **Confinement semantics are shared by contract, not by code.** If `fsops` gains a rule, `toolsd` on the agent carries it; nothing forces a client-side reimplementation to keep up — because there is no client-side reimplementation, by design.
4. **Offline/flaky agents** are the transport's problem (GAP-107 durability, GAP-112 live proof), not the primitives'.

## Consequences

- **GAP-092** (install `toolsd` on an agent, hand-drive the whole table, record the contract) is the correct next step and blocks the line — it validates this decision empirically before more code is built on it.
- **GAP-096** is a delivery problem, exactly as its row already says.
- **GAP-094** (stdin + bounded binary-safe output) is the enabling work for stdin-carried payloads and has landed.
- Any future proposal to daemonise `toolsd` or link it into `bunkerd` must argue against this record, naming which of the two falsifiable revival triggers below it satisfies — a subscription-shaped workload (Trigger 1) or per-op transport overhead × call volume above the service's carry cost (Trigger 2) — with numbers from the CHT-038 invocation ledger, not adjectives.

## Revival triggers (falsifiable) — INTEG-003

The paragraph in "Why not (a)" names the revival triggers qualitatively — a *subscription* need,
or "enough" chattiness — and a rejected option whose revival condition cannot fail is a rejected
option that gets re-litigated. Both triggers are therefore pinned here as measurable conditions,
extending that record without changing the decision. A future proposal passes one or it does not;
there is no third reading.

### First: what SURF-001 already took off the table

This is stated first so the triggers are not misread as applying to the MCP surface. SURF-001
proved end-to-end, on a live agent, that the MCP-over-unix-socket surface needs **no new toolsd
code**: systemd `Accept=yes` socket activation turns the already-shipped stdio `toolsd mcp`
surface into a socket-served one, and the entire marginal cost is **two unit files** in the
agent's own `~/.config/systemd/user` (`evidence-surfaces-mechanism.md`,
`RECIPE-toolsd-socket-service.md`). For that surface, chattiness only ever decided *whether to
bother* — and the bother measured as nearly free. **The triggers below remain load-bearing for
the HTTP-shaped surfaces (SURF-007, CHT-055/056), which DO need toolsd code** and therefore carry
a real build plus the audit-continuity obligation of `SPEC-toolsd-agent-surfaces.md` §3.4. A
proposal that only re-serves the MCP surface does not need to revive (a) at all; it needs to
point at the recipe.

### Trigger 1 — a subscription-shaped workload (push beats poll)

The condition: real sessions need to be **pushed** change events. The current model is the
client polling the tree over the exec path.

- **WHO measures:** a real workload audit of Hermes session tool calls against bunker agents,
  run by whoever owns the revival proposal and attached to it as numbers.
- **WHAT counts:** either (i) **per-session polling volume** — `list`/`read` invocations whose
  only purpose is detecting a change the session did not cause — or (ii) **at least one
  missed-update incident**: a session acted on a stale view because the change only became
  visible at its next poll.
- **Threshold semantics:** polling's cost appears as a multiplier on call volume (calls ×
  polls-per-call). A revival must show that polling — or lowering the poll interval — is
  measurably worse than push **on the audited workload**. "Agents feel chatty" does not pass;
  the audit numbers pass or fail it.
- **Measurement source:** the **CHT-038 invocation ledger** — the instrument this fleet already
  owns — records every invocation with a caller tag and outcome, so call frequencies and caller
  tags are queryable today, before anything is built. The **CHT-037 chain** resolves the ledger's
  sink (where its records land). This is the measurement-first ordering
  `SPEC-toolsd-agent-surfaces.md` §3.6 already prescribes: the cheap half of the decision.

### Trigger 2 — per-op transport overhead dominating a real editing workload

The condition: the per-operation cost of exec-delivered verbs dominates a session's editing
time. Two costs must be measured and kept **separate**, because a persistent service removes
only one of them:

1. **Through-`bunker exec`, per verb, wall clock INCLUDING the SSH round trip and process
   start** — the real cost paid today.
2. **Local, no-SSH invocation of the same verb** on the agent host — same binary, no transport.

The difference isolates the **transport cost** a socket removes; the local number exposes the
**process cost** it does not (with `Accept=yes`, each new connection still spawns a fresh
`toolsd mcp`).

**Decision rule:** a service pays for its fixed carry cost only when

```
measured per-session call volume × measured saved per-op overhead
    > the service's fixed carry cost
```

and the carry cost is not zero — it is the two unit files **plus** a daemon lifecycle the agent
host must own: connection supervision, failure and restart semantics, the audit-continuity work
of SPEC §3.4, and contract version pinning, which the exec path re-verifies for free on every
invocation today. The record's existing bound still holds: a session making five calls fails
this rule at any realistic per-call saving. Only a measured N × S above the carry cost revives
(a) — and only for HTTP-shaped verbs; for the MCP surface the carry cost was measured at two
unit files and the decision is already made.

### What revives (a), precisely

A future proposal passes when it attaches, from the CHT-038 ledger and a measured workload,
either a subscription-shaped need (missed-update incidents or polling volume — Trigger 1) or a
measured per-session volume × saved overhead above the stated carry cost for an HTTP-shaped
surface (Trigger 2). Everything else is a taste argument, and this record rejects taste as a
trigger.
