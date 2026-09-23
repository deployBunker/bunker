# Dogfood integration run — 2026-09-22/23 — the key-lifecycle surface (GAP-132/GAP-139b)

Target: `bunker` (deployBunker/bunker) at CLI HEAD `40354bd`, demo daemon `bunker-mvp`
at `16fff6d` (deployed 2026-09-22T20:13Z, ≥ `325da4c` as GAP-139b requires).

## Promise under test

GAP-132 shipped "key lifecycle": `bunker key list|rotate|revoke` against a live daemon,
with a zero-downtime overlap window on `rotate` and durable immediate revocation.
GAP-139b asked for this to be proven against the demo daemon (bunker-mvp), LIVE.

## What was actually proven LIVE (all on bunker-mvp, 78.46.173.180:18080)

| Acceptance piece | Result | Evidence |
|---|---|---|
| Daemon ≥ 325da4c | PASS | `/opt/bunker/bunkerd --version` → commit `16fff6d`, built 20:13Z |
| `key list` metadata rows | PASS | rows of `bk_…  agent=…  created=…  expires=…`, one per live agent |
| rotate: new secret printed ONCE | PASS | 64-hex output + `previous fingerprint: sha256:…`; grep of /etc/bunkerd + /var/lib/bunkerd + /var/log for the secret = empty |
| revoke: immediate + durable | PASS | live sub-key: `Agent/GetInfo` 200 → `key revoke` → 401 `invalid token` at t+0 (and the previous session proved the same sub-key stays rejected across a daemon restart) |
| audit records | PASS | `/var/log/bunkerd/audit.log`: hash-chained `{"method":"/bunker.v1.Bunkerd/RevokeKey","outcome":"ok",...,"hash":…}` + rotate record with `previous fp=`; every RPC also in `journalctl -u bunkerd` |
| rotate overlap window (REAL JWTs) | **FAIL — P0** | see below |

## THE P0: rotate never reaches the interceptors that validate requests

Method: minted real HS256 JWTs locally with each candidate signing secret and probed
`POST /bunker.v1.Agent/GetInfo` (the agent-facing surface that accepts JWTs).

Observed across THREE live rotations (23:27:18, 23:51:34, 2026-09-23T00:35:49Z), with
the daemon NOT restarted in between (same PID since 23:31:21):

- JWT signed with the daemon's **boot-time** secret (`/etc/bunkerd/jwt_secret` as loaded
  at start) → **200, accepted at every point in time**, including after all three rotations.
- JWT signed with the secret **returned by the rotate RPC** → 401 `signature is invalid`,
  immediately after the rotate and forever after.

Root cause (code): `internal/server/server.go` builds **three separate auth instances** —
`server.go:227` `s.jwtAuth = auth.NewJWTAuth(...)` (what `RotateJWTSecret` mutates via
`internal/server/keys_rpc.go:50` → `jwtAuth.RotateSecret`), but request validation runs in
`server.go:228` (`NewMasterOnlyAuthInterceptor`, Bunkerd service) and `server.go:229`
(`NewJWTAuthInterceptor`, Agent service), both built at boot with the boot secret. The
rotation switches the secret in the instance nothing reads.

Why the suite stayed green: `internal/server/gap132_test.go` tests the service methods
directly (e.g. `svc.jwtAuth.RotateSecret` + `svc.RotateJWTSecret`) and validates sub-keys
against `svc.keyMgr` — no test rotates via RPC and then sends a request through the real
interceptor stack. Same structural blindness as DF-BUNKER-38 (mount preflight had zero
coverage of the real helper).

Secondary consequences of the same instance split:

- The overlap window is untestable and effectively nonexistent on the live path: tokens
  signed with the boot secret never expire by rotation, tokens signed with the rotated
  secret never worked in the first place. A future deploy/restart silently applies the
  last-persisted secret — so "rotation" only takes effect after a restart, which is
  exactly the downtime the feature promises to avoid (the rotated secret printed by the
  CLI is also NOT auto-persisted; persistence is a manual operator step).
- The CLI's rotate output ("persist it now … and restart bunkerd to load it") implies the
  switch happens at restart. The in-memory service instance DOES switch immediately, but
  since the interceptors never see the new secret, the observable behavior matches the
  misleading reading: nothing changes until restart — and then it changes ALL AT ONCE
  (restart loads the manually-persisted file, if the operator happened to persist it;
  a restart without persistence reverts to the boot secret).

## The credential model as it really is (three tiers, not two)

1. **Master/static bearer token** (48-char opaque, `auth.token` in daemon config; lives in
   the operator's `~/.bunker/config.yaml` per server entry). NEVER rotated by `key rotate`
   — it bypasses the JWT secret entirely (static-token path, `internal/auth/jwt.go:283`).
   This is the only credential that can call Bunkerd-service (admin) RPCs.
2. **Agent sub-keys** (43-char opaque `ApiKey` in the spawn bundle; server stores only a
   SHA-256 hash in `/var/lib/bunkerd/keys/apikeys.jsonl`). Accepted ONLY on the
   Agent-service RPCs (`/bunker.v1.Agent/GetInfo|Metrics|Heartbeat`); every Bunkerd-service
   RPC answers `{"code":"unauthenticated","message":"agent-scoped tokens are not allowed
   for this endpoint"}` — that is BY DESIGN (`masterKeyOnly`), so probing a sub-key
   against `GetAgent` returns 401 even when the key is perfectly live. The LIVE/REVOKED
   discriminator is the message: scope-denial = live, `invalid token` = revoked/unknown.
3. **JWTs** signed with the rotating HS256 secret (what `key rotate` is supposed to
   rotate). In the current build no CLI command asks the daemon for a JWT — they matter
   for the auth design (and for the P0 above), but the practical agent credential today
   is the sub-key.

## Working techniques for the next person

- Probe a sub-key against `POST /bunker.v1.Agent/GetInfo` with `{"agentId":"<own agent>"}`:
  200 = live, 401 `invalid token` = revoked/expired. Do NOT conclude from a Bunkerd-endpoint
  401 — there the scope denial fires first (by design).
- The daemon has TWO mux surfaces on one port: `/bunker.v1.Bunkerd/*` (master-only) and
  `/bunker.v1.Agent/*` (agent-scoped). Any auth matrix must test both.
- The agent's sub-key is only in the spawn bundle output (`API Key:` line). The agent's
  own `~/.bunker/` inside the container has NO config/token (just `owner` + `ports`), and
  no local SSH key is written (see findings below), so capture the bundle output if you
  need the credential later.
- JWT mint for auth debugging (python reference):
  `b64url(json({"alg":"HS256","typ":"JWT"})) + "." + b64url(json({"agent_id":…,"exp":…}))`
  signed with `hmac.new(secret, "<h>.<p>", sha256)` — secret as raw bytes, no
  transformation. Verify a mint against a second implementation before trusting a 401.
- Exit-code trap: `EXIT=$?` after a pipe reports the pipe; check the CLI's own output line,
  not just the status.

## Defects found this run (board rows DF-BUNKER-45..47)

1. **DF-BUNKER-45 (P0)** — rotate's secret switch never reaches the validating
   interceptors; overlap window cannot exist on the live path. Evidence: minted-JWT probes
   across three live rotations; `server.go:227-229`; test gap in `gap132_test.go`.
2. **DF-BUNKER-46 (P1)** — `key rotate` output + `--help` text mislead the operator about
   what the secret is (JWT signing secret, NOT a bearer token), where it goes
   (`auth.jwt_secret_file`, NOT the CLI config token), and when it takes effect (service
   instance immediately; validating path only after manual persist + restart — or never,
   see DF-BUNKER-45). This specific confusion locked a dogfood run out of its own CLI
   config mid-run (recovered from a config backup).
3. **DF-BUNKER-47 (P2)** — spawn bundle defects: the post-spawn SSH-key fetch runs
   unauthenticated (`warn: could not fetch SSH key: unauthenticated: missing Authorization
   header` — so no local key file is ever written for the agent), and the printed
   `Docker Tunnel:` command carries an empty `-i` argument plus a server-side identity path
   (`/etc/bunkerd/ssh/<id>`) that no client host can use.

## Install leg (fresh-machine, ephemeral bunker)

`bunker-qa.sh` battery on a fresh ephemeral agent `2b991bc9` on bunker-las-03 (ttl 4h,
destroyed after): toolchain bootstrap OK (go 1.26.5, zig cc, make 4.4.1, compose+buildx
plugins), fresh clone + build OK, harness OK, chaos cells: disconnect OK, resource 3G cap
PASS, errorpath OK, corruption-restart INFO (ambiguous harness detail — recorded in the
evidence file `/tmp/bunker-qa-evidence-20260922T232343Z-*.jsonl`). Not SKIPPED — the leg
ran in full and the agent was destroyed afterwards.
