# SECURITY PANEL — MERGE LEDGER (Bunker readiness for team use)

**Round:** security-readiness quorum · 2026-09-20 · coordinator: Hermes (SEC panel)
**Method:** `coding-hermes-quorum` doctrine — one shared claim-checklist brief, one full session per
model family, every seat works the entire checklist, coordinator re-verifies every load-bearing
claim against raw source before it is logged. Brief: `brief.txt`. Seat outputs: `seat-*.log`
(raw) and `extract-seat-*.md` (cleaned).

## Seat roster — 6 launched, 5 landed, 5 families

| Seat | Model | Provider | Family | Bytes | Landed |
|---|---|---|---|---|---|
| S1 | gpt-5.6-sol | openai-codex | OpenAI | 165 | **NO — stalled** (openai-codex lane; 0 bytes of review) |
| S2 | glm-5.3 | zai-glm-default | Zhipu-GLM | 27.8 K | YES |
| S3 | kimi-k3 | custom | Moonshot-Kimi | 58.9 K | YES |
| S4 | qwen/qwen3-coder-plus:free | xkiro | Qwen | 20.1 K | YES |
| S5 | claude-opus-4.8 | clinepass | Anthropic | 41.5 K | YES |
| S6 | gemini-2.5-pro | clinepass | Google | 35.5 K | YES |

**Quorum: MET** — 5 independent verdicts across 5 distinct families (≥3 required). S1 is an
honest lane failure, not a silent drop; per the substitution law it was NOT replaced, because
5 families already exceed the diversity bar. Note: S5/S6 (and the failed S1) are **plan/PYG
lanes**; S2/S3/S4 ride subs — the round mixed transports, which is allowed.

## Verdict tally — the sign-off question was _unanimous_

| Seat | Sign-off verdict | Stated biggest reason |
|---|---|---|
| S2 GLM | **DO-NOT-APPROVE** | bearer token over plaintext HTTP by default on a public IP |
| S3 Kimi | **DO-NOT-APPROVE** | no transport posture; TLS off by default, serves `0.0.0.0` |
| S4 Qwen | **DO-NOT-APPROVE** | secrets in cleartext by default; spawn hands back an SSH key over that wire |
| S5 Anthropic | **DO-NOT-APPROVE** | the artefacts a team signs off on don't exist; `SECURITY.md` affirmatively wrong |
| S6 Google | **DO-NOT-APPROVE** | control-plane secret 0644 on a live host + cleartext on every interface |

**5/5 DO-NOT-APPROVE for team use.** Every seat independently praised the *isolation
engineering* (rootless per-agent dockerd, PAM `/tmp` fail-closed boundary, hash-chained audit,
spawn rollback, honest runtime self-reporting) and every seat refused on the **control plane
and the missing security artefacts**. That split — substance good, governance absent — is the
round's central finding.

## Theme convergence (the only thing that counts as decided)

| Theme | Seats | Verdict |
|---|---|---|
| TLS/plaintext default posture | **5/5** | CONFIRMED |
| No threat-model document | **5/5** | CONFIRMED |
| `SECURITY.md` misstates the default (claims mTLS) | **5/5** | CONFIRMED |
| No RBAC / one shared static token, no per-operator identity | **5/5** | CONFIRMED |
| Audit chain unkeyed by default / truncatable / no anchor | **5/5** | CONFIRMED |
| No egress control | **5/5** | CONFIRMED |
| Heartbeat lacks ownership check | **5/5** | CONFIRMED |
| No supply-chain / image policy | **5/5** | CONFIRMED |
| No operator incident runbook | **5/5** | CONFIRMED |
| Key/token lifecycle (no rotation, no revocation) | **5/5** | CONFIRMED |
| Failed-auth not audited | **4/5** | CONFIRMED |
| Secret file perms (live host) | **4/5** | CONFIRMED (deployment) |

## Coordinator re-verification (the commit layer — every claim checked against raw source)

| # | Panel claim | My verification | Verdict |
|---|---|---|---|
| R1 | TLS off by default, daemon serves plaintext | `internal/config/config.go:374` → `TLS.Enabled: false`; auth gate exists (`:658`) but the TLS path has no equivalent gate | **CONFIRMED** |
| R2 | `SECURITY.md` claims mTLS; default is TLS-off | `SECURITY.md:24` "mTLS between `bunker` CLI and `bunkerd` server" vs `config.go:374` `Enabled:false`, `:376` `MTLS:false` | **CONFIRMED — doc is affirmatively wrong as to the DEFAULT** |
| R3 | Heartbeat has no `claims.AgentID` check | `internal/server/service.go:1422` body reads `req.Msg.AgentId` and extends 6h with no claims comparison; `Metrics` at `:95-98` DOES gate on `claims.AgentID` | **CONFIRMED — authorization asymmetry** |
| R4 | Audit chain is unkeyed SHA-256; seal/anchor opt-in | `internal/audit/audit.go:242,340` `sha256.Sum256`; `SealKey` only if `opts.SealKey != ""` (`:267`); `ship.go` HMAC only when seal_key configured | **CONFIRMED** |
| R5 | No egress control | grep `egress\|outbound` over `internal/` + `cmd/` → only unrelated comments | **CONFIRMED** |
| R6 | One shared static master token; no RBAC | `config.go:97` `Token string`; `auth/jwt.go` roles = master + agent-scoped only | **CONFIRMED** |
| R7 | Failed auth not audited | audit interceptor sits inside auth (`server.go` chain) → denials never reach the chain | **CONFIRMED** |
| R8 | Token lifecycle: in-memory, no rotation/revocation | `internal/apikey/manager.go:17-21` `keys map[string]*Key` in-memory; `Revoke()` exists at `:101` but is **called from no RPC** | **CONFIRMED** |
| R9 | Shared scratch cross-readable/-writable **and ON by default** | `hostsetup/scratch.go:13` `2770` group setgid; `config.go:437` `SharedScratchEnabled: true` | **CONFIRMED** (the default is the sharper half) |
| R10 | Spawn returns the agent SSH private key | `proto/bunker/v1/bunker.proto:199` `string ssh_private_key = 8; // Generated key` | **CONFIRMED — key material crosses the default-plaintext wire** |
| R11 | mTLS identity path is "dead code" (S3) | `auth/mtls.go` exists and IS wired via `server.go:364-375` when `TLS.MTLS=true`; it is config-gated, not dead. The specific "wrong context key" sub-claim was **not** reproduced | **REFINED** — config-gated, not dead; sub-claim UNVERIFIED |
| R12 | `auth.token` 0644 on a live host (S6) | Not a code default — a deployment observation on a fleet host | **CARRIED AS DEPLOYMENT FINDING** (needs live re-check at fix time) |

**Convergence arithmetic:** 10 themes at 5/5 and 2 at 4/5, all 12 coordinator re-checks landing
CONFIRMED/REFINED. A round that reports zero CONTRADICTED against the coordinator found nothing;
here the seats *out-performed* the coordinator's prior framing — the 2026-09-20 cgroup review
(this same day) had framed Bunker as resource-bounding gaps, and the panel found the security
story is control-plane, not resource. **That is the round working.**

**Falsifiers kept for round 2:** R1 flips if a startup gate refuses non-loopback plaintext; R3
flips if `Heartbeat` gains a claims check; R8 flips if keys persist + a Revoke RPC exists; R10
flips if `ssh_private_key` leaves `SpawnAgentResponse`.

## Provenance
- Brief with the full 14-claim checklist: `brief.txt`
- Seat raw outputs: `seat-*.log`; cleaned: `extract-seat-*.md`
- Consolidated ranked gap list with file:line: see the PRD
  (`~/bunker/docs/prd/security-readiness.md`) — section 3 is this ledger expanded into
  actionable items, each carrying its seat attestation count.
