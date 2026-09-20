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
- Any future proposal to daemonise `toolsd` or link it into `bunkerd` must argue against this record, naming the trigger condition it satisfies.
