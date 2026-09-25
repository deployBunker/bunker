# Dogfood Integration Report — 2026-09-25 (18th run): the RAW-REST / credential-surface angle

**Project:** bunker · **Workdir:** /home/kara/bunker · **Live daemon:** bunker-mvp
(78.46.173.180:18080 REST / :19090 gRPC, auth enforced, daemon 0.1.4 at f525639,
uptime 19h44m, /tmp isolation `private`)

## The angle

Runs 1–17 covered CLI lifecycle, isolation/mount, key lifecycle, remote-dev workflow,
agent-tools/lifecycle, durability. **Nobody ever drove the daemon WITHOUT the CLI** —
the raw REST surface a language-binding integrator would use: connect-go unary JSON,
the single server-streaming ExecAgent with envelope framing, and the GAP-128
credential-fetch path (`GetAgentKey`) that the CLI itself depends on.

## Promise under test

"An integrator can drive the full agent lifecycle (spawn → get → heartbeat → metrics →
exec → destroy) over plain REST, using only docs/integration.md and the proto — no CLI."

## What held up (verified live)

- **Unary REST is exactly as documented.** POST `/bunker.v1.Bunkerd/<Method>`,
  `Content-Type: application/json`, snake_case request fields in, protojson camelCase
  out, int64-as-string confirmed (`uptimeSeconds: "71420"` is a str, `maxAgents: 50`
  a number). Auth: no token → 401 `{"code":"unauthenticated","message":"missing
  Authorization header"}`; GET → 405; unknown RPC path → plain-text 404 (not JSON —
  as the docs say). ServerInfo/ServerMetrics/ListAgents/GetAgent/AgentMetrics/
  HeartbeatAgent/QueryAudit all answered correctly on the first try.
- **The full lifecycle over raw REST worked end-to-end**: SpawnAgent (18.1s incl.
  rootless-docker install on the shared demo) → GetAgent → HeartbeatAgent
  (`expiresAt` extended) → AgentMetrics → DestroyAgent → idempotent re-destroy
  (`200 {"status":"destroyed"}`) → never-known id (`404 not_found`). The two
  idempotency cases are distinct exactly as documented.
- **ExecAgent streaming with hand-rolled envelopes worked** after one correction:
  the request needs `[0x00][len:4 BE][protojson]` framing (application/connect+json),
  responses arrive HTTP-chunked with the same 5-byte envelope framing, stdout/stderr
  base64, zero exit code omitted, non-zero `exitCode` present, 0x02 trailer ends
  the stream. A minimal 60-line stdlib-only client (§ below) drives a real command.
- **RunAgent is unary and detach-only** — `{"runId","status":"running","exitCode":-1,
  "unitName":"bunker-run-<id>-<uuid>"}`. The detached process genuinely survives the
  SSH session (PPID 1, writes to $HOME visible to later exec sessions).
- **Error taxonomy held**: bad agent_id → `invalid_argument` naming the failing
  stage (`spawn … failed at stage validate`); unknown fields on SpawnAgent are
  discarded (documented), but an INVALID value for a known field fails loud.
- **Audit chain**: every REST call produced a hash-chained record
  (`hash`/`prevHash`) visible through QueryAudit, incl. `[INSECURE-PLAINTEXT]`
  disclosure tags for the http:// path — trustworthy evidence trail.

## What broke (findings → board)

1. **DF-BUNKER-59 (P1) — `bunker spawn` cannot fetch the agent SSH key on an
   auth-enforced daemon.** Every spawn prints `(warn: could not fetch SSH key:
   unauthenticated: missing Authorization header)` and saves NO local key. Code
   root cause: `internal/cli/spawn.go` sets `Authorization` only on the SpawnAgent
   request; the GAP-128 follow-up `GetAgentKey` call (spawn.go:223) sends a fresh
   request with no header. Proven with a 3-call standalone Go repro against the
   live daemon (WITH header → 200 + 411-byte key; WITHOUT → 401). Raw REST with
   the same token works, so it is purely client-side. Blast radius: every agent
   is ssh/mount/cp-dead from the CLI that spawned it; bunker-qa.sh's ssh
   reachability probe then destroys healthy agents 3× (observed live).
   Workaround this run: fetch the key via raw REST and write ~/.bunker/keys/<id>.
2. **DF-BUNKER-60 (P2) — fresh-machine `make test-short` is red**: two
   host-provision tests write `/etc/pam.d/sshd.bunker-tmp` for real and fail
   `permission denied` as non-root. A clean-machine suite pass is structurally
   impossible without root — inverts the fresh-machine guarantee.
3. **DF-BUNKER-61 (P2) — `bunker destroy` deadline_exceeded** on an agent that
   ran a big build, while raw REST destroyed the same agent in seconds.
4. **PERF-002 (P2)** — bunker-qa.sh preflight assumes 4-space YAML indent
   (patched in place; recorded on the board).

## The copy-pasteable client (validated)

A ~70-line stdlib-only Python client (ExecAgent with envelope framing, de-chunking,
base64 stdout decode) is in this report's companion `2026-09-25-rest-probes/`
directory together with the unary lifecycle script — both were run live against
bunker-mvp and produced the transcripts quoted above. That is the "integration
depth" deliverable: an integrator can start from these instead of reverse-
engineering connect framing like this run did.

## Install leg (bunker-qa.sh, fresh machine)

PASS on the ephemeral agent (bc0e2890, bare Debian, non-root, toolchains preinstalled:
go 1.22 + make):
- real public clone (repo is public): 2s, HEAD f525639
- `scripts/install.sh` (release binaries): 3s, SHA256-verified, smoke `--version` → 0.1.4
- `make build` from source: RC=0, ~60s (pinned ldflags version matches HEAD)
- `make test-short`: FAIL (see DF-BUNKER-60) — 23 packages ok, internal/cli red
- battery cells: not run to completion (the CLI key-fetch bug broke the harness's
  ssh probe); not SKIPPED — the install path itself was fully proven, and the
  harness breakage is filed as DF-BUNKER-59/PERF-002.
- ALL agents destroyed afterwards (destroy verified by re-list + host-side
  user/home absence). No repo visibility/permission changes; no credentials minted.

## Judgement

The REST surface is genuinely integrator-grade: the docs match the wire (they were
clearly captured from a real daemon), errors are typed and honest, the audit chain
covers every call. The FAIL is in the CLI's own use of that surface — the credential
path — which is exactly where a "tests green" report would never look.
