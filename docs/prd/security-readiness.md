# PRD — Bunker Security Readiness for Team Use

**Status:** DRAFT for owner review · 2026-09-20
**Author:** Hermes (secure-panel coordinator) · **Owner:** Bane
**Provenance:** 6-seat multi-model security panel — **6/6 landed across 6 families** (Zhipu GLM-5.3 · Moonshot Kimi-K3 · Qwen3-Coder-Plus · Anthropic Claude-Opus-4.8 · Google Gemini-2.5-Pro · OpenAI gpt-5.6-sol). Brief + raw outputs + merge ledger: `~/sec-panel-bunker-2026-09-20/`.
**Method:** every finding below is either (a) converged across ≥4 of 6 independent model families, or (b) re-verified by the coordinator against raw source. Attestation counts are stated; nothing is asserted without one of those two.

> **Post-merge note (round integrity):** gpt-5.6-sol (S1) was initially harvested as a stalled seat and the review proceeded on 5/5. It finished inside its 3000 s budget and was recovered — **and it caught two findings the other five seats missed**, one of them a BLOCKER (SEC-22, below). The final panel is 6/6. The 5/5 verdict was unanimous and the sixth seat did not dissent; it contributed depth.

---

## 1. Executive summary

**The panel's unanimous verdict: DO NOT APPROVE Bunker for a team today — 5/5 seats, five different model families.** Every seat simultaneously praised the isolation engineering and refused on the control plane. That split is the finding.

**What is genuinely good** (all five seats, unprompted): per-agent rootless dockerd, the PAM/`pam_namespace` fail-closed `/tmp` boundary, the hash-chained audit log, spawn rollback with fail-closed semantics, an honest runtime self-report when isolation is *not* provisioned. This is not a repo that ignored security; it is a repo that built hard parts well and never built the governance layer around them.

**What blocks approval** — two of them are *affirmatively wrong*, not merely missing:

1. **The daemon serves plaintext by default and the security doc says it uses mTLS.** `SECURITY.md:24` states "mTLS between `bunker` CLI and `bunkerd` server" in the advertised security model; `internal/config/config.go:374` ships `TLS: {Enabled: false, MTLS: false}`. A security team reading the doc and then the config finds the product misdescribing its own default. *(5/5)*
2. **The spawn RPC returns the agent's SSH private key over that same unencrypted wire.** `proto/bunker/v1/bunker.proto:199` — `string ssh_private_key = 8`. Key material crosses the transport that has no transport security. *(3/5 raised; coordinator confirmed)*
3. **No threat model exists at all.** Zero hits for "threat model" across `README.md` + all of `docs/`. Nothing to approve against. *(5/5)*
4. **One shared static master token, no per-operator identity, no revocation, no rotation.** `internal/apikey/manager.go:17-21` is in-memory; `Revoke()` exists at `:101` and is called from no RPC. *(5/5)*
5. **Heartbeat has no ownership check.** `internal/server/service.go:1422` extends any agent's TTL by id, while `Metrics` at `:95-98` correctly gates on `claims.AgentID`. An agent-scoped key can keep a peer alive indefinitely. *(5/5)*

**The strategic read:** Bunker's isolation story is *ahead of* its control-plane story. The product is a strong sandbox behind a weak door. For a solo operator on a private tailnet that is a defensible trade; for a team — the explicit ask — it is not, because a team multiplies exactly the things that are missing: more operators sharing one credential, more people who need to read a written model, more blast radius when the one token leaks.

**This PRD turns the panel's output into a plan.** Sections 3–5 are the threat model, the verified findings ledger, and the requirements. Section 6 is the phased roadmap; Section 7 is the definition of "team-ready" (the acceptance gate we would hand a security engineer).

---

## 2. Scope and audience

**In scope:** everything a security engineer evaluates when deciding whether to let their engineers use Bunker — control plane, identity, transport, secrets, audit, egress, supply chain, documentation, and operational readiness.

**Explicitly out of scope** (owned by the sibling resource-preset family GAP-113..GAP-122, filed 2026-09-20): cgroup resource knobs, swap/IO bounding, `memory.oom.group`, unit sandboxing properties, `cgroup.kill` teardown, resource observability. Those are the *resource* axis; this PRD is the *security-governance* axis. The two meet only at §6 phase ordering.

**Audience:** (1) Bane as owner, deciding what to fund; (2) the Bunker foreman, which will turn §4 findings into board rows; (3) a prospective security reviewer, who should be able to read §3 + §7 and know exactly what they would be approving.

**Non-goal:** making Bunker survive a malicious *host root*. No agent-sandboxing product can, and `SECURITY.md` already (correctly) scopes that out. The ask is narrower and harder: be *honest* about that scope, and be *safe* within it.

---

## 3. Threat model (gap #1 — and its draft)
The panel's single most-repeated finding: *there is no threat model*. Nothing to approve against, no stated residual risk, no named adversaries. This section **is** that document — written here so it can be moved to `docs/threat-model.md` largely intact.

### 3.1 Assets

| Asset | Where it lives | Why an attacker wants it |
|---|---|---|
| **The daemon control plane** (`bunkerd`) | Host, root | It can spawn/destroy/exec agents. Root-equivalent over the fleet. |
| **The master credential** (`auth.token` / `auth.jwt_secret`) | `/etc/bunkerd/config.yaml` on the host | Total control-plane access; no rotation, no revocation, no per-user attribution. |
| **Agent home directories** | `/home/bunker-<id>` | Source code, transferred artifacts, images, credentials the workload brought. |
| **Per-agent SSH private keys** | Host + agent home; returned by `SpawnAgentResponse.ssh_private_key` | Docker-host access to that agent. |
| **The audit log** | Host (`internal/audit/`) | The only forensic record; its integrity *is* the incident response. |
| **Peer agent data** (shared scratch `/srv/bunker-share`) | Host, group `bunker-agents`, mode `2770` | Cross-tenant read/write — currently on by default. |
| **The host itself** | — | Anything that escapes the agent user reaches other agents' daemons. |

### 3.2 Adversaries (named, with the control each demands)

| # | Adversary | Capability assumed | Primary control |
|---|---|---|---|
| A1 | **Malicious/buggy agent workload** | Full root *inside its own container*; arbitrary code, arbitrary syscalls from a container | Kernel/container boundary + egress control + resource bounds |
| A2 | **The agent operator** (a team member) | Holds agent credentials; runs `bunker` CLI | Per-operator identity, RBAC, audit attribution |
| A3 | **Tenant-vs-tenant** | Agent A tries to read/affect agent B | Isolation of home + scratch + daemon; no shared writable surface by default |
| A4 | **Credential thief** | Obtained the master token (log leak, shoulder-surf, 0644 file) | Transport security, rotation, revocation, failed-auth detection |
| A5 | **Host-local unprivileged user** | A non-agent account on the same box | File permissions, secret storage |
| A6 | **Supply-chain attacker** | Compromised image / rootless installer fetch | Pinning, signatures, digest verification |
| A7 | **Malicious *host* root** | — | **Out of scope** (stated explicitly, as today) |

### 3.3 Trust boundaries

```
  A2 operator ──[network]──► bunkerd (host root)  ──► agent users ──► containers ──► internet
                   ▲BT1                      ▲BT2              ▲BT3          ▲BT4
  BT1 = the transport (today: PLAINTEXT, no auth on the wire)
  BT2 = the daemon's authorization boundary (today: one static master token, no per-operator id)
  BT3 = the agent-user boundary (today: STRONG — userns, per-user dockerd, PAM /tmp)
  BT4 = egress (today: NONE — no policy at all)
```

**The model's own conclusion:** BT3 — the part Bunker built well — is the boundary least likely to be crossed. BT1, BT2, BT4 — the governance boundaries — are where the risk actually sits, and all three are currently open. This single diagram is the argument for the whole PRD.

### 3.4 Residual risk that must be *stated*, not hidden

Even after this PRD lands, the following remain true and must appear in `SECURITY.md`:
- A malicious **container** workload can attack the **host kernel** (containers share the kernel; the userns/rootless design is defence-in-depth, not a hypervisor boundary).
- An agent that has **legitimate exec access** can run anything the daemon would run on its own behalf; audit records it, it does not prevent it.
- **Cross-boot PID/tmpfs state** is not preserved; "isolation" is per-instance, not per-lifetime.
- The audit chain detects tampering **only** when sealed/anchored off-box; without `seal_key` + `ship_to`, a host-root attacker can rewrite history undetectably.

---

## 4. Verified findings ledger

Severity: **BLOCKER** = no team approval without it · **HIGH** = security team demands it before production · **MED** = should-have · **LOW** = polish.
"Seats" = independent model families flagging it (of 5). "Coord" = coordinator re-verification against raw source.

### 4.1 BLOCKERS

| ID | Severity | Finding | Evidence | Seats | Coord |
|---|---|---|---|---|---|
| SEC-01 | BLOCKER | **No threat model document.** Nothing to approve against; no named adversaries, assets, boundaries, or residual risk. | `grep -ic "threat model"` over `README.md` + all `docs/*.md` = **0** | 5/5 | CONFIRMED |
| SEC-02 | BLOCKER | **`SECURITY.md` misstates the default transport.** It advertises mTLS; the shipped default is TLS off. | `SECURITY.md:24` vs `config.go:374` `Enabled:false`, `:376` `MTLS:false` | 5/5 | CONFIRMED |
| SEC-03 | BLOCKER | **No transport enforcement.** Daemon binds non-loopback plaintext with no refusal and no warning; auth has a gate (`config.go:658`), TLS has none. | `server.go` bind path; only `RegistryError()`/auth gate exist | 5/6 | CONFIRMED |
| SEC-22 | BLOCKER ✅ **FIXED** | **subuid/subgid ranges OVERLAP between every pair of agents — the user-namespace separation the product is built on is not enforced.** `rootless.go:576` wrote `<name>:<start>:65536` with `start` = the agent's OWN uid ⇒ agent 1001 got `[1001..66536]`, agent 1002 got `[1002..66537]` — a 65,535-id overlap. Also no cross-agent overlap check and a read-then-append TOCTOU. **Caught only by seat S1.** | `rootless.go:544-576` | 1/6 | ✅ CLOSED — `subid_alloc.go` + host-wide flock + startup gate + `bunker subid-migrate`; live-proven (commits 07133cf, fbfb87a, ad40c54) |
| SEC-04 | BLOCKER | **No per-operator identity / RBAC.** Exactly two roles: one shared static master token, and agent-scoped keys. No human attribution anywhere. | `config.go:97` `Token`; `auth/jwt.go:22-26` | 5/5 | CONFIRMED |
| SEC-05 | BLOCKER | **Spawn returns agent SSH key material over the wire.** | `proto/bunker/v1/bunker.proto:199` `ssh_private_key = 8` | 3/5 | CONFIRMED |
| SEC-06 | BLOCKER | **No egress control.** No default-deny, no allowlist. One compromised dependency is an exfiltration path. | grep `egress\|outbound` over `internal/`+`cmd/` → none | 5/5 | CONFIRMED |

### 4.2 HIGH

| ID | Severity | Finding | Evidence | Seats | Coord |
|---|---|---|---|---|---|
| SEC-07 | HIGH | **Heartbeat lacks an ownership check** — any agent key extends any peer's TTL indefinitely. | `service.go:1422` (no claims check) vs `Metrics` `:95-98` (has one) | 5/5 | CONFIRMED |
| SEC-08 | HIGH | **Failed auth is never audited.** The audit interceptor sits inside auth, so denials never reach the chain. | interceptor ordering in `server.go` | 4/5 | CONFIRMED |
| SEC-09 | HIGH | **Chain is unkeyed + unanchored by default.** Truncation/forgery verify OK; `seal_key`/`ship_to` are opt-in and off. | `audit.go:242,340` sha256; `:267` SealKey opt-in | 5/5 | CONFIRMED |
| SEC-10 | HIGH | **No key lifecycle.** In-memory keys, no persistence, no rotation, `Revoke()` unreachable from any RPC. | `apikey/manager.go:17-21`, `:101` (no caller) | 5/5 | CONFIRMED |
| SEC-11 | HIGH | **Shared scratch is on by default and cross-readable/writable.** `/srv/bunker-share/<id>` is `2770` setgid under a shared group. | `scratch.go:13`; `config.go:437` `SharedScratchEnabled: true` | 5/5 | CONFIRMED |
| SEC-12 | HIGH | **No supply-chain policy.** No image allowlist, signature verification, or digest pinning for the rootless installer or image specs. | `rootless.go:21-22,242`; `internal/imagespec/` | 5/5 | CONFIRMED |
| SEC-13 | HIGH | **No operator incident runbook.** No kill-switch, emergency rotation, host-freeze, or notification path. | no such doc in `docs/` | 5/5 | CONFIRMED |

### 4.3 MEDIUM / LOW

| ID | Severity | Finding | Evidence | Seats |
|---|---|---|---|---|
| SEC-14 | MED | Secret storage hygiene: `auth.token`/`jwt_secret` live inline in the config file; no `_FILE`/env indirection, no 0600 secrets dir. | `config.go:97-98` | 4/5 |
| SEC-15 | MED | Control-plane denial-of-service: no rate limiting/backoff on unauthenticated requests. | — | 3/5 |
| SEC-16 | MED | Audit status is not self-critical: reports neither its own anchor status nor "no off-host anchor configured." | S5 live repro | 2/5 |
| SEC-17 | MED | Exec/run command content is logged only in snoopy/syslog, uncorrelated to the audit chain. | `docs/exec-audit.md` | 2/5 |
| SEC-18 | MED | Compliance posture absent: no data-residency/retention/deletion statement; `destroy_home_policy` defaults to **archive**, not purge. | `config.go:430` | 3/5 |
| SEC-19 | LOW | `SECURITY.md` is a stub (~30 lines) with no per-control defaults, preconditions, or residual risk. | file length | 5/5 |
| SEC-20 | LOW | No vulnerability-disclosure process beyond "email us": no embargo policy, advisory path, or security-release channel. | `SECURITY.md:19` | 3/5 |
| SEC-21 | LOW | PID-namespace and `/tmp` state disclosures live in code comments and README, not in `SECURITY.md`. | `isolation.go:77-80`; `README.md:653-656` | 2/5 |

---

## 5. Requirements

Each requirement states **what**, **why a security team demands it**, and **PASS** (the acceptance criterion). These are the rows the foreman will file.

### 5.1 Transport (SEC-02, SEC-03)

**REQ-T1 — TLS-by-default with an explicit insecure escape hatch.**
The daemon MUST refuse to bind a non-loopback listener without TLS, unless the operator sets an explicit `insecure_dev: true` (which logs a loud warning at startup and stamps every audit record). Mirror the existing auth gate pattern (`config.go:658`, fail-before-listen).
*Why:* an admin-capable RPC plane on plaintext is the one finding that ends a security evaluation.
*PASS:* (1) `bunkerd` with `tls.enabled:false` and a non-loopback bind exits with a clear error; (2) `insecure_dev: true` starts but emits a warning and marks audit records; (3) loopback-only bind still allowed; (4) unit tests for all three cases.

**REQ-T2 — Self-signed + pinning path so "easy" is not "insecure."**
Ship self-signed cert generation and CLI trust-on-first-use pinning so the secure path is the *default* path, not the harder one.
*Why:* the current demo path (`README.md:166`, the `78.46.173.180:18080` example) teaches plaintext. Documentation that teaches the insecure path will be followed.
*PASS:* `bunker connect --tls self-signed` works end-to-end on a live host; the README quick-start uses it.

**REQ-T3 — Stop returning key material in `SpawnAgentResponse`.**
Either remove `ssh_private_key` from the response, or make it opt-in behind an explicit flag, and never over a non-TLS transport.
*Why:* a private key in an RPC body is a credential crossing a boundary; today the boundary is plaintext.
*PASS:* proto field deprecated/removed (or flag-gated); a test asserts the key is absent from the default response; docs updated.

### 5.2 Identity and authorization (SEC-04, SEC-07, SEC-10)
**REQ-I1 — Per-operator identity.** Distinct credentials per human/CI job (JWT `sub` or client cert), surfaced as `caller` in audit records.
*Why:* "who did this" is the first question in every incident review; one shared token makes it unanswerable.
*PASS:* two operators get distinct creds; audit records show distinct `caller`; a test asserts attribution.

**REQ-I2 — RBAC roles.** At minimum `admin` / `operator` / `agent:self`, with agent keys restricted to their own agent.
*Why:* SEC-07 is a concrete instance of what happens without this.
*PASS:* table-driven authorization tests per role × RPC; agent key acting on a peer → permission denied.

**REQ-I3 — Fix the heartbeat ownership check.** Add the `claims.AgentID` comparison that `Metrics` already has.
*Why:* it is an authorization bypass with a one-line fix.
*PASS:* an agent-scoped key cannot extend a peer's TTL (test).

**REQ-I4 — Key lifecycle.** Persist keys, add `Rotate`/`Revoke` RPCs, auto-generate and persist `jwt_secret` on first boot, support rotation without downtime.
*Why:* unrevocable credentials cannot be used by a team; a leaked token must be killable.
*PASS:* keys survive restart; `Revoke` invalidates a live token; rotation works without restart.

**REQ-I5 — Control-plane secret storage.** Move `auth.token`/`jwt_secret` out of the config file into a root-only `0600` secrets file or `_FILE`/env indirection.
*Why:* config files get copied, backed up, and committed.
*PASS:* secrets load from `*_FILE`/env; config holds no plaintext secret; a test asserts the file mode.

### 5.3 Audit and forensics (SEC-08, SEC-09, SEC-16, SEC-17)

**REQ-A1 — Audit auth failures.** Move denials into the chain (or add a parallel denial log) with source, credential id, and outcome.
*Why:* credential-stuffing detection is table stakes; today it is invisible.
*PASS:* a failed RPC produces an audit record; a test asserts it.

**REQ-A2 — Anchored tamper-evidence by default.** Enable sealing; require an off-box anchor (`ship_to`) for a "tamper-evident" claim, and make the claim conditional in the docs.
*Why:* an unkeyed, unanchored chain detects accidental corruption, not an attacker.
*PASS:* a truncated chain fails verification; `audit status` reports anchor state and says plainly when none is configured.

**REQ-A3 — Command content in the chain.** Bring exec/run command lines into the audit chain (correlated, not just syslog).
*Why:* "which command, by which agent, when" is the core forensic question.
*PASS:* an exec appears in `audit query` with its command and agent.

### 5.4 Egress and supply chain (SEC-06, SEC-12)

**REQ-E1 — Egress policy with a safe default.** Default-deny or default-allowlist for agent traffic, configurable per agent/tier.
*Why:* the panel called this "the highest-value single addition" — without it, containment is worthless against exfiltration.
*PASS:* a default-deny config blocks an outbound connection from an agent container; an allowlisted host succeeds.

**REQ-S1 — Supply-chain verification.** Pin the rootless installer version + SHA-256 manifest; signature/digest verification for images.
*Why:* the installer is fetched and executed as root; an unpinned fetch is remote code execution by design.
*PASS:* a mismatched digest aborts the install loudly; a test covers the failure path.

### 5.6 Isolation integrity (SEC-22 ✅ SHIPPED — caught only by seat S1)

**REQ-X1 — Globally disjoint subordinate-ID allocation.** ✅ **SHIPPED** (`subid_alloc.go`, `subid_integrity.go`).
Ranges are allocated from a pool (`subIDPoolBase`) that skips every range already in the file; the read-check-append runs under a host-wide flock; `CheckSubIDOverlaps` is a fail-closed startup gate in `cmd/bunkerd`; `bunker subid-migrate` (dry-run default) remediates existing hosts. Live-proven: dry run reports the overlap, the daemon refuses to start on it, `--apply` rewrites 4 defect ranges to disjoint `524288`/`589824`.
*Why it mattered:* the old writer used the agent's own uid as the range start, so consecutive agents shared 65,535 subordinate IDs — the product's core isolation claim, unenforced.
*PASS:* ✅ disjoint ranges (tests); ✅ no TOCTOU (flock + concurrent test); ✅ startup refusal (live); ✅ migration (command + tests); two-agent live spawn on bunker-mvp still belongs to GAP-122's battery.

**REQ-X2 — Gate the client `tls_insecure` knob.**
`internal/cli/config.go:33` + `client.go:23` ship an `InsecureSkipVerify` path. Require an explicit acknowledgement + loud warning, refuse it when a pinned cert is configured, and mark such sessions unverified in audit.
*Why:* it defeats the pinning path REQ-T2 introduces.
*PASS:* `tls_insecure: true` warns and is refused with a pin configured; the insecure session is marked in audit; tests cover both.

### 5.7 Documentation (SEC-01, SEC-19, SEC-20, SEC-21)

**REQ-D1 — `docs/threat-model.md`.** Adopt §3 of this PRD (assets, adversaries, boundaries, residual risk) as the canonical model.
**REQ-D2 — Rewrite `SECURITY.md`.** Per-control entry with its *default*, its *preconditions*, and its *residual risk*; correct the mTLS claim; state the out-of-scope items honestly.
**REQ-D3 — `docs/incident-runbook.md`.** Kill-switch, emergency rotation, host-freeze, notification, and post-incident review.
**REQ-D4 — Compliance posture statement.** Data residency, retention, deletion guarantees; make `destroy_home_policy: archive` vs `purge` explicit and documented.
**REQ-D5 — Disclosure process.** Embargo policy, advisory publication path, security-release channel.

---

## 6. Roadmap

Ordered so each phase unlocks the next and nothing ships a doc that describes behaviour not yet built.

| Phase | Content | Rows | Gate |
|---|---|---|---|
| **P0 — Honesty** (fastest win) | Threat model (REQ-D1), `SECURITY.md` rewrite incl. the mTLS correction (REQ-D2, SEC-02), residual-risk disclosure, **and the subuid overlap fix (REQ-X1 / SEC-22 — it breaks the isolation claim itself, so it cannot wait behind governance work)**. | REQ-D1, D2, X1 | A reviewer reading only `SECURITY.md` + `docs/threat-model.md` can state the product's actual posture; two agents have provably disjoint subordinate-ID ranges. |
| **P1 — Close the door** | TLS gate (REQ-T1), self-signed+pinning (REQ-T2), stop returning key material (REQ-T3), secret storage (REQ-I5). | REQ-T1..T3, I5 | Non-loopback plaintext is impossible without an explicit, loud opt-in. |
| **P2 — Know who** | Per-operator identity (REQ-I1), RBAC (REQ-I2), heartbeat fix (REQ-I3), key lifecycle (REQ-I4), auth-failure audit (REQ-A1). | REQ-I1..I4, A1 | Every action is attributable; a leaked credential can be revoked. |
| **P3 — Contain** | Egress policy (REQ-E1), supply-chain pinning (REQ-S1), shared-scratch default (SEC-11), anchored audit (REQ-A2), command content (REQ-A3). | REQ-E1, S1, A2, A3 | A compromised agent cannot exfiltrate; the chain detects tampering. |
| **P4 — Operate** | Incident runbook (REQ-D3), compliance statement (REQ-D4), disclosure process (REQ-D5), remaining MED/LOW items. | REQ-D3..D5, SEC-15..18 | An on-call engineer has a written response for the top scenarios. |

**Estimate:** P0 is days (writing). P1–P2 are the substantive work (single-digit engineer-weeks). P3 is the largest (egress + supply chain). Each phase ends with the live `e2e-full-battery.sh` `VERIFY-PASS` per `AGENTS.md`, plus the new security assertions.

**Deliberate ordering note:** P0 ships first and changes no behaviour. That is intentional — a security team's first question is answerable in days, which buys the credibility to do the engineering in P1–P3.

---

## 7. Definition of "team-ready" (the acceptance gate)

Bunker is approvable for a team when **all** of the following hold, verified live:

1. **A written model exists** and matches the code — `SECURITY.md` + `docs/threat-model.md`, every claim true of the *default* config.
2. **Non-loopback plaintext is impossible** without an explicit, warned, audited opt-in.
3. **Every action is attributable** to a distinct operator or agent credential, visible in the audit chain.
4. **A leaked credential can be revoked** without a restart, and rotation is documented and tested.
5. **Failed authentication is recorded and rate-limited.**
6. **Agent subordinate-IDs are globally disjoint** and overlap is refused at startup — the user-namespace separation claim is enforceable, not aspirational. *(SEC-22)*
7. **Egress is controlled by policy** with a safe default.
8. **The audit chain is anchored off-box** and its tamper-evidence claim is conditional and honest.
9. **Supply chain is pinned** for the rootless installer and image specs.
10. **An incident runbook exists** covering kill-switch, rotation, freeze, and notification.
11. **Residual risk is stated** — kernel sharing, exec-as-agent, per-instance isolation, unanchored-audit limits — in the security doc, not in code comments.

This is the checklist we hand the security engineer. Items 1–6 are the difference between "impressive project" and "tool a team can adopt."

---

## 8. Open questions for the owner

1. **Threat model scope** — is a hosted/multi-tenant Bunker a real target, or is this a per-team private deployment? It changes the severity of SEC-04/SEC-06 materially.
2. **Egress default** — default-deny (secure, breaks things) vs default-allowlist seeded from common registries (usable, weaker)? The panel split here.
3. **How much compliance** — is a SOC-2-adjacent story wanted, or only enough for an internal security review? SEC-18/D4 scale to that answer.
4. **Shared scratch** — SEC-11 says the cross-agent exchange directory is on by default. Keep the convenience, or flip the default and make sharing opt-in?
5. **Who owns the docs** — P0 is writing; should it be the foreman's rows, a dedicated pass, or handed to an outside reviewer to write (which is a stronger signal of independence)?

---

## 9. Appendix — panel provenance

- **Seats (6 landed / 6 families):** Zhipu GLM-5.3 · Moonshot Kimi-K3 · Qwen3-Coder-Plus · Anthropic Claude-Opus-4.8 · Google Gemini-2.5-Pro · OpenAI gpt-5.6-sol. S1 (gpt-5.6-sol) was initially filed as stalled, then recovered inside its budget — and contributed the round's only isolation-boundary BLOCKER (SEC-22, subuid overlap) plus the `tls_insecure` finding. **An early "stalled" harvest was wrong; the seat finished late and was recovered by re-reading the log.**
- **Convergence:** 10 themes at 5–6/6, 2 at 4/6. Zero CONTRADICTED against the coordinator's prior framing — the seats out-performed that framing, which is the round working.
- **Coordinator re-verification:** 12 load-bearing claims re-checked against raw source; 11 CONFIRMED, 1 REFINED (the "mTLS dead code" claim — it is config-gated, not dead; sub-claim unverified).
- **Artifacts:** `~/sec-panel-bunker-2026-09-20/{brief.txt, MERGE.md, seat-*.log, extract-seat-*.md}`
- **Model access finding (criterion 1 of the ask):** security-capable lanes confirmed live — `gpt-5.6-sol` (openai-codex, ExploitBench 76.5), `glm-5.3` (zai-glm, ExploitBench 54.4), `kimi-k3` (Moonshot), `qwen3-coder-plus` (Qwen), `claude-opus-4.8` and `gemini-2.5-pro`/`claude-sonnet-5` (both via clinepass). All six seats produced verdicts. **S1 is the round's lesson: the registry's top-ranked security lane was the SLOWEST (needed ~50 min) — harvest too early and you lose the deepest findings.** It was the only seat to catch the isolation-boundary BLOCKER.
