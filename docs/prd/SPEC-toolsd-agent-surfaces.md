# SPEC: toolsd surfaces on the agent (per-session service over an SSH-forwarded unix socket)

**Status:** proposed — for decision · **Date:** 2026-09-20 · **Author:** Hermes
**Proposed by:** Bane (shape: login-triggered per-session daemon as the bunker user, unix socket, SSH as the transport, multiple protocol surfaces)
**Related:** `ADR-toolsd-integration.md`, `SPEC-bunker-native-file-tools.md`, `docs/mount-drivers.md`, `docs/both-ways.md`
**Board:** SURF-001..005 (bunker), CHT-055/056 (coding-hermes-tools)

---

## 1. The proposal

1. On SSH login to a bunker agent, run a `toolsd` daemon **as that agent's user**, so uid context is never broken.
2. Treat the open SSH connection as giving us a **remote unix socket**.
3. Expect **multiple** such sockets, one per connection (the connection is the lifetime).
4. Over that socket, serve **several surfaces** — REST, HTTP/2, WebSocket, QUIC, gRPC/RPC.
5. `toolsd` gains tasks to support the extra surfaces.
6. The client attaches the forwarded socket locally and drives file operations through it; other tools can borrow it.
7. Write proper specs, store them in DuckBrain, and maintain generated **OpenAPI** for the surfaces.

## 2. What is VERIFIED (measured on this fleet, 2026-09-20)

The load-bearing mechanism works. Forwarding a **remote** unix socket to a **local** one over SSH, using the daemon host's docker socket as a known-good remote socket:

```
remote sshd : allowstreamlocalforwarding yes · streamlocalbindmask 0177 · streamlocalbindunlink no

$ ssh -f -N -o ExitOnForwardFailure=yes -L /tmp/probe.sock:/var/run/docker.sock root@78.46.173.180
$ ls -l /tmp/probe.sock
srw------- 1 kara kara 0 ... /tmp/probe.sock                     # 0700 — the sshd bindmask, not a default
$ curl --unix-socket /tmp/probe.sock http://localhost/version
{"Platform":{...},"Version":"29.1.3","ApiVersion":"1.52",...}    # the REMOTE docker, through the local socket
```

Three properties confirmed, all of which the proposal depends on:

1. **No TCP listener on either end.** The socket is the only surface.
2. **The local end is created 0700** (`streamlocalbindmask 0177`), so no other local user can reach it.
3. **The socket is per-connection and dies with the SSH session** — verified: after the session ended, the path no longer existed. Dynamic and multiple, exactly as proposed.

Also relevant: agents already own per-agent runtime directories (`/run/bunker/<agent-id>/`, e.g. the live `docker.sock` there), so an agent-local socket has a natural, already-isolated home.

## 3. What is WRONG or missing in the proposal

### 3.1 The load-bearing correction: do NOT start the daemon on SSH login

A login-triggered daemon is the wrong mechanism **because `bunker exec` works by running commands over SSH**. If a login hook (sshd `ForceCommand`, a login shell wrapper) intercepts the session, every non-interactive command — which is every `bunker exec`, every scp-based `cp`/`deploy`, every scripted probe — becomes the daemon launcher or is broken outright. The mechanism would break the most-used path in the product.

**Use user-level socket activation instead**: a `systemd --user` socket unit in the agent's own session (agents already have a user manager and `XDG_RUNTIME_DIR`, per INT-SPAWN-004). The socket unit starts the service on first connect, the service runs as the agent uid, and non-interactive SSH is untouched. This also answers "multiple sockets per multiple connections" cleanly — see 3.2.

### 3.2 One daemon per SESSION reintroduces the lost-update class

"One per connection" means N concurrent sessions → N daemons → N independent in-memory states over **one tree**. That is precisely the failure the toolkit exists to prevent: `internal/lease` was built because concurrent sessions clobbered each other, and its cross-session registry is a shared file precisely because there is no shared process today. Multiple daemons that each believe they own state would silently undo that protection.

The decision must be explicit:

- **(a) One daemon per USER** (socket-activated, later sessions connect to the same socket). Shared state may then live in memory safely, and the lease registry finally gets what it has been faking: **liveness tied to a real connection** (CHT-030's "the CLI's default pid is the TRANSIENT parent" is a direct symptom of there being no daemon). **Recommended.**
- **(b) One daemon per SESSION, all shared state stays in FILES** (as today). The daemon then buys only the per-call spawn saving and streaming; it holds no state authority. Safe, smaller win.

Either way: **two concurrent sessions must still produce exactly one lease grant and one refusal naming the holder** — that test exists (`CHT-007`, and remote `CHT-052`) and must keep passing unchanged.

### 3.3 It is not a security boundary, and must never be described as one

A daemon listening inside the agent runs as the agent's user; anything running as that user — **including the untrusted repository code the agent is executing** — can connect to it and call every verb. It therefore grants the agent nothing it could not do with its own syscalls. It is a **transport and convenience layer**, not a privilege device and not an enforcement point. Two consequences:

- The socket MUST live in the agent's own 0700 runtime directory and MUST NEVER be a TCP listener. (Verified above that the SSH bindmask gives 0700 for free.)
- Docs and descriptors must not imply isolation from it. This is the same honest limitation the ADR already states: **fsops confinement is cooperative inside the tool; the hard boundary is uid + cgroup + namespace.**

### 3.4 The audit-chain regression: a private socket bypasses the daemon's trail

Today every remote operation executes as a `bunker exec`, which lands in the daemon's hash-chained audit trail (GAP-047..050) and will carry session attribution (GAP-095). Traffic over a private socket **bypasses the daemon entirely** — so remote edits would become invisible to the audit chain, undoing the security work already shipped.

Requirement, not a nice-to-have: either the socket daemon writes its own attributed audit records in the same chain (which means it needs the daemon's chain identity, or a second chain that is reconciled), or the daemon remains the audited path and the socket is additive-only for operations that never need attribution. **Decide this before building anything.** Decided — see 3.4.1.

### 3.4.1 Audit Continuity — the decision (SURF-B4)

**Option (a) is chosen: the socket surface writes attributed records for every mutating verb into the daemon's hash-chained trail.** The socket stays additive for reads and for the transport itself; only the ATTRIBUTION obligation moves, not the data path. Options (b) and (c) were rejected for the same reason from opposite directions: (b) refusing attribution-requiring verbs until the daemon can observe them puts the daemon back inside the write path — the surface then buys nothing for exactly the operations it exists to serve; (c) restricting the socket to reads halves the benefit and still leaves the trail blind to the writes that matter, merely documented as a limitation instead of fixed.

**The record contract — the SAME facts an exec record carries today.** A socket-mediated mutating verb produces one record in the daemon's chain with no new record shape and no new query surface (implemented in `internal/audit.RecordSocketVerb`, pinned by `internal/audit/socket_test.go`):

| Fact | Field | Convention |
|---|---|---|
| who | `caller` + `agent_id` | the attribution identity passed over the socket, rendered by the same `CallerFromClaims` rules as exec (`agent:<id>`); never a raw token — the socket's uid auth is named honestly in `remote_addr` |
| what verb | `method` | `<verb>@socket` (e.g. `toolsd_write@socket`) — suffixed, not replaced, so `bunker audit list --method toolsd_write` finds it either way |
| what path | `summary` | `path=<target>` as the verb received it |
| confinement root | `summary` | `root=<root>` — where inside the tree the write was allowed |
| session, outcome, duration | `session_id`, `outcome`, `duration_ms` | same redaction and normalization as every record |

The command/args summary is rendered by toolsd through the SAME credential scrubber exec uses (`RedactCommandSummary`) and RE-CHECKED by the recorder before writing (`commandSummaryAllowed`): a summary that still carries credential material is dropped, never written. A mutating verb with no attribution identity is refused at the recorder and by the surface (`RequireSocketRecord`) — refusal with a named error, never a silent gap.

**The destination — how the record reaches the daemon's chain.** Two deployments, one contract:

1. **Agent and daemon share a host** (the bunker-mvp shape): toolsd appends through the same `AuditLog` file the daemon writes. The chain survives the second writer because the log already re-seeds its chain head from the file tail on open (DF-BUNKER-29) and chains across rotation — both pinned by test (`TestRecordSocketVerbChainsAcrossRestart`, `TestRecordSocketVerbChainsAcrossRotation`). The daemon's anchoring (GAP-136) then covers socket records with no new machinery: after a socket write, `bunker audit verify` walks a chain whose head IS the socket record.
2. **Agent on a different host from the daemon** (the fleet shape): toolsd ships the attributed record to the daemon over the existing audited RPC surface (fire-and-forget, the GAP-073 shipper posture: bounded retry, named failure, never blocking the verb). The shipped record enters the chain through the daemon itself. Until that ship RPC exists, a cross-host socket deployment MUST NOT enable mutating verbs — the surface refuses them (the option-(b) refusal arm, applied per-deployment rather than per-forever).

**What does NOT count as coverage:** the CHT-038 invocation ledger is a LOCAL usage counter, not the daemon's hash-chained trail. "The ledger covers it" is not an acceptable close for this row. The acceptance test is mechanical: a verb performed over the agent-owned socket appears in the trail `bunker audit verify` accepts, with the attribution fields above; a socket write absent from the hash chain fails the audit-continuity criterion (SPEC §6.4, unchanged).

**Honest limit, stated once:** attribution-by-default is a code path, not a physical law — a rogue toolsd that never calls the recorder is indistinguishable, on THIS host, from a verb that never happened. That is what the off-host anchor (`audit.ship_to`) and the E2E battery's socket-write-appears-in-chain check are for: the battery runs the check on every surf wave, so the gap is caught by CI, not discovered in forensics.

### 3.5 The multi-protocol goal is the part to CUT

Six surfaces carrying the same verbs multiplies auth paths, error mappings and test matrices for almost no functional gain. Per protocol, honestly:

| Proposed | Verdict |
|---|---|
| **QUIC** | **No.** QUIC's entire value is surviving lossy or path-changing *networks*. Here the transport is a unix socket over an already-reliable, already-encrypted SSH channel — it adds a handshake, TLS configuration and real complexity for zero benefit. |
| **WebSocket** | **Only if we need server→client PUSH** (subscriptions, file-watch, streaming exec output). If we do, prefer **SSE over HTTP/2 on the same socket** — one code path, no extra framing. |
| **HTTP/2 vs HTTP/1.1** | Pick **one**, not both. |
| **gRPC / Connect** | **Strong candidate**: bunker already uses connectrpc for its own daemon↔CLI protocol, so reusing that IDL toolchain is the lowest-marginal-cost path, and it generates typed clients and OpenAPI. |
| **REST / JSON** | **Strong candidate**: `curl --unix-socket` is the single best debuggability property a remote surface can have, and OpenAPI is trivially generated from one IDL. |

**Recommendation: ONE primary surface, chosen by who consumes it.** If the consumer is agent/CLI tooling, a Connect RPC over the socket reusing bunker's existing IDL. If a human/`curl`/OpenAPI consumer matters, add a thin REST/JSON bridge — and derive it from the SAME IDL so there is one source of truth. "Several surfaces" is implemented as **one IDL with generated bindings**, never as several hand-written servers.

### 3.6 Trigger this by MEASUREMENT, not by taste

The ADR named exactly two triggers for an in-agent service: **(i)** a push/subscription need, and **(ii)** measured per-operation chattiness. We have the instrument already: `toolsd`'s CHT-038 invocation ledger records **every** invocation with a caller tag and outcome, so per-call cost and call frequency are measurable *today*, before any building. The daemon's gain is bounded by those numbers; if a session makes five calls, the spawn saving is noise.

So the first task is a measurement task, not a build task. That is not stalling — it is the cheap half of the decision, and it is falsifiable.

## 4. Recommended shape (the version worth building)

1. **Transport:** an **agent-local unix socket in the agent's 0700 runtime dir**, reached from the client by SSH `-L` stream-local forwarding. No TCP, ever. (Verified.)
2. **Lifecycle:** `systemd --user` **socket activation** in the agent's session. NOT a login hook. (Correction to the proposal.)
3. **Topology:** **one daemon per user** (3.2a), so shared state has one owner and leases get real liveness.
4. **Surface:** **one IDL**, Connect RPC first; REST/JSON generated from it if a non-RPC consumer appears. No QUIC. WebSocket/SSE only when a push need is demonstrated.
5. **Audit:** the socket daemon is part of the audited path (3.4) — solved before the first verb ships, not after.
6. **Honesty:** documented as a transport, never as a boundary (3.3).
7. **Phasing:** measure → socket + one verb → lease/state authority → push/subscriptions if needed. Each phase independently useful and independently abandonable.

## 5. What this does NOT change

- The **CLI contract stays** (`ADR-toolsd-integration.md`): the binary is still delivered agent-side and remains the fallback and the debug path. `curl`-free, dependency-free operation is a feature.
- **`bunker exec` stays** the audited, always-available path.
- The **mount drivers** work (MOUNT-006..009) is independent; this spec does not touch it.
- **No new network exposure**: no port allocation, no firewall change, no new credential (the uid is the auth, and the SSH key remains the credential).

## 6. Acceptance criteria (for whichever phases are approved)

1. **Mechanism:** a remote unix socket is reachable as a local socket over SSH with no TCP listener on either side, and the local end is 0700. (Partially done — see §2.)
2. **No regression to exec:** `bunker exec`, `cp`, `deploy` and the E2E battery are byte-identically unaffected by the daemon's presence.
3. **Single writer:** two concurrent sessions on one agent produce exactly **one** lease grant and one refusal naming the holder — via the socket, unchanged from CHT-007/CHT-052.
4. **Audit:** every socket-mediated mutating operation appears in the audit chain with its attribution, or the surface refuses operations that require attribution — no silent gap.
5. **Failure behaviour:** with the daemon killed mid-call the client gets a named, bounded error (no hang), and the next call re-establishes without operator action. (The same class as the mount's phantom-space criteria — no silent success.)
6. **Measured justification:** the per-call cost and call frequency that motivate the daemon are recorded as numbers, before and after.
7. **One source of truth:** any generated surface (OpenAPI, typed clients) is generated from the single IDL in CI, and a drift check fails the build if it is stale.
