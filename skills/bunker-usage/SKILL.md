---
name: bunker-usage
description: >-
  How to actually USE the Bunker platform (bunkerd + bunker CLI) — the real
  workflows, the working commands, the previously-broken-now-verified features,
  and the pitfalls that tests don't catch. Load this before doing anything with
  bunker/bunkerd, writing E2E scripts, or triaging agent lifecycle bugs.
  Verified against the live auth-enforced MVP on 2026-08-18
  (docs/dogfood/2026-08-18-integration.md); audit CLI + fleet notes added
  2026-08-29 (docs/dogfood/2026-08-29-integration.md); exec flag grammar,
  /tmp semantics, mount and install notes re-verified against live
  bunker-las-04 at CLI HEAD 66d4150 on 2026-09-16
  (docs/dogfood/2026-09-16-integration.md); REST-integrator recipes (unary +
  connect streaming) added 2026-09-18 against bunker-las-02 at HEAD 967c331
  (docs/dogfood/2026-09-18-integration.md); durability battery (TTL reap,
  kill -9 registry replay, sub-key scoping, audit chain, reconciliation)
  verified 2026-09-19 against a scratch HEAD daemon
  (docs/dogfood/2026-09-19-integration.md); isolation boundary + mount/
  scratch defects re-verified live at HEAD 93d7a53 on 2026-09-20
  (docs/dogfood/2026-09-20-integration.md, diagnostics.md §13).
version: 1.7.0
category: software-development
---

# Bunker Usage — How to Drive This System For Real

Bunker = multi-agent hosting platform. `bunkerd` (daemon, needs **root**, runs on a Linux host) + `bunker` (CLI, runs anywhere). Each agent is an isolated Linux user with its own rootless Docker daemon, cgroup limits, and port range. Control everything through the CLI or the connect-go gRPC/REST API.

## Entry points

- **Live servers (fleet, 2026-08-29):** public MVP `78.46.173.180` (REST :18080, gRPC :19090, SSH :22; v0.1.3) + Tailscale boxes `bunker-las-01..04`, `cube-las-00`, local `bunker-7840hs` (127.0.0.1:10002). **Auth is ENFORCED everywhere** (since GAP-014): empty/missing token → 401; every call needs `Authorization: Bearer <token>` (stored in `~/.bunker/config.yaml` by `bunker connect`). Switch boxes with `bunker --server <name> status|list|audit`; spawn always targets the ACTIVE server (`active_server:` in config) — check `bunker status` first. Server-reported hostnames only resolve on the server itself.
- **Binaries:** `make build` or `go build -o bunker ./cmd/bunker`, `go build -o bunkerd ./cmd/bunkerd`; `go install github.com/deployBunker/bunker/cmd/bunker@latest` serves the newest TAG (may lag HEAD — compare `bunker version` commit vs `git rev-parse HEAD`, README GAP-058 note).
- **CLI config:** `~/.bunker/config.yaml` (server aliases + active server, incl. token), keys in `~/.bunker/keys/<agent-id>`.
- **Test config for local daemon:** `test-config.yaml` (auth off, /tmp/bunkerd-test, port 9095) — daemon runs as non-root but spawn will fail at `useradd` (root required, undocumented in README).

## The working workflow (verified 2026-08-18)

```bash
bunker connect http://78.46.173.180:18080 --token <token>   # writes ~/.bunker/config.yaml
bunker status                                               # ONLINE, version, agents, real CPU/mem/disk — 0.56s
bunker list                                                 # current agents
bunker spawn --ttl 1h                                       # ~10s → connection bundle (key, ports, TTL, sshfs/tunnel cmds); use --agent-id <name> for a named agent
bunker spawn --image-spec spec.json --ttl 1h                # GAP-064: customize the agent image (apt/go/npm package adds, validated & cached server-side)
bunker info <id>                                            # status, expires, limits (CPU 2.0, mem 4 GB, disk 20 GB, 10 containers)
bunker exec <id> -- uname -a                                # isolated user: bunker-<id>
bunker exec <id> -- docker run --rm alpine echo hi          # rootless Docker works (3.4s first pull)
bunker env set <id> FOO=bar                                 # NOTE: KEY=VALUE as ONE arg
bunker env list <id>                                        # FOO=bar
bunker env get <id> FOO                                     # bar (get needs the KEY arg)
bunker exec <id> -- sh -c 'echo $FOO'                       # env visible in exec (sourced at start)
bunker cp /local/file <id>:/tmp/file                        # scp round-trip, byte-identical
bunker metrics <id>                                         # real memory/disk usage
bunker heartbeat <id>                                       # TTL extended (acknowledged + new expiry)
bunker run <id> --detach --name job -- sh -c 'sleep 3; echo done >> /tmp/j.log'   # systemd transient unit, survives disconnect
bunker destroy <id>                                         # 1.9s; server back to prior agent count
```

REST (same surface, JSON over HTTP): `POST http://<ip>:18080/bunker.v1.Bunkerd/<Rpc>` with `Authorization: Bearer <token>` and `Content-Type: application/json` — e.g. `.../ListAgents`, `.../ServerInfo`.

## REST client recipes (verified 2026-09-18, las-02 @ HEAD 967c331)

**Unary RPCs** — as documented in `docs/integration.md`: proto **snake_case in**,
protojson **camelCase out**, 64-bit fields as JSON **strings** (`"uptimeSeconds"`),
`{"code","message"}` + real HTTP status on error (`404 not_found`, `400
invalid_argument`, `401 unauthenticated`). Spawn takes **`agent_id`** (never
`name` — see pitfalls), and `{"ttl":"banana"}` is rejected **before any side effect**.

**Streaming RPCs (`ExecAgent`, `RunAgent`) — NOT `application/json`.** The
documented content type returns a bare `415` with an empty body. The shape that
works is connect streaming:

```
POST /bunker.v1.Bunkerd/ExecAgent
Content-Type: application/connect+json
Authorization: Bearer <token>
body = [flags:1][len:4 big-endian][json payload]     # ONE envelope, NO end-of-stream envelope
```

Response is `200`, `Transfer-Encoding: chunked`; de-chunk, then read envelopes:

```
flags=0x00 {"stdout":"<base64>"}   # stdout/stderr are base64 bytes, one frame per write
flags=0x00 {"stderr":"<base64>"}
flags=0x00 {"exitCode":3}          # present ONLY when non-zero
flags=0x02 {}                      # end-of-stream trailer
```

```python
import json, struct, base64, urllib.request
def frame(payload, flags=0): return struct.pack(">BI", flags, len(payload)) + payload
req = urllib.request.Request(url + "/bunker.v1.Bunkerd/ExecAgent",
      data=frame(json.dumps({"agent_id": aid, "command": "docker run --rm alpine echo hi"}).encode()),
      headers={"Content-Type": "application/connect+json", "Authorization": f"Bearer {token}"})
# parse: de-chunk the body, then [flags][len][payload] repeatedly; base64-decode stdout/stderr
```

Both spec-shaped endings are rejected: a trailing `0x02` byte → `invalid_argument
"protocol error: incomplete envelope: unexpected EOF"`, a trailing
`[0x02][0x00000000]` envelope → `internal "unmarshal end stream message:
unexpected end of JSON input"` — and both arrive as **HTTP 200 with an error
object inside the stream**, so check the frames, not just the status.
`Connect-Protocol-Version` is not required. Working reference client:
`/tmp/dogfood-bunker-rest/{bunker_rest,exec_stream}.py` (reproduced in
`docs/dogfood/2026-09-18-integration.md`).

**NEW in v0.1.3 — audit trail (GAP-047/049/050, verified live 2026-08-29):**

```bash
bunker audit list --server bunker-las-03                    # table: ts/caller/method/agent/outcome/summary
bunker audit list --server <box> --agent <id>               # per-agent RPCs incl. exec (fixed 406508b)
bunker audit list --server <box> --since 2026-08-29T00:00:00Z
bunker audit export --server <box> --since ...              # raw JSONL incl. hash+prev_hash chain
bunker audit verify --path /var/log/bunkerd/audit.log       # LOCAL ONLY (daemon host), no --server
```

Every authenticated RPC is appended (JSONL, 0600) with caller/agent/duration/
outcome; token values are NEVER logged. `verify` checks the SHA-256 chain
across rotated backups (.1–.3).

Spawn ≈ 10s. Limits are real — check them at `/sys/fs/cgroup/user.slice/user-<uid>.slice/{memory.max,cpu.max,pids.max}` (NOT `/sys/fs/cgroup/memory.max`), drop-in at `/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf`.

## Durability recipes (verified 2026-09-19, scratch daemon @ HEAD e344088)

```bash
# Prove TTL auto-destroy in ~2 minutes (the reaper ticks every 60s):
bunker spawn --ttl 2m ttl-canary
# → within ~2-3 min: "TTL expired, destroying agent" in the daemon log,
#   user gone, registry destroy event appended. No operator action needed.

# Prove crash safety the honest way — kill -9, then restart:
sudo kill -9 $(pgrep -x bunkerd | head -1)   # SIGKILL, no graceful flush
# restart the daemon → startup logs "restored:<N>" and each agent keeps
# its EXACT persisted port range; exec works immediately after replay.

# Scratch-daemon on a shared host (never touch the fleet daemon):
#   own ports + registry/audit/ssh paths in /tmp, own BUNKER_HOME,
#   reconciliation mode: adopt on the first restart if foreign bunker-*
#   users exist on the host. See docs/dogfood/diagnostics.md §8.
```

Agent-scoped sub-keys (minted on spawn when `auth.jwt_secret` is set):
the sub-key is rejected on the Bunkerd service (401) and `Agent/GetInfo`
is clamped to the sub-key's own agent — but `Agent/Metrics` currently
enforces NEITHER (DF-BUNKER-28): treat metrics data reached via a sub-key
as unscoped until that row lands.

## Previously broken — now verified (2026-08-18)

All features broken as of 2026-08-03 (tasks DOGFOOD-001..006) are fixed and verified live against the auth-enforced MVP:

| Feature (was broken ≤ 2026-08-03) | Status 2026-08-18 | Notes |
|---|---|---|
| `bunker env set/get/list/unset` | ✅ all work | `env set <id> KEY=VALUE` as ONE argument (no space between KEY and VALUE); `env get <id> KEY` — get/list fail with a clear usage error if the KEY arg is missing |
| `bunker exec <id> -- sh -c '<compound>'` | ✅ works | compound commands, `if`/`for`/`while` no longer syntax-error (DOGFOOD-001 fix holds) |
| `bunker cp` / `bunker deploy` / `bunker tunnel` | ✅ work from a client | hostname/key-path issues fixed; no manual IP/`~/.bunker/keys/<id>` substitution needed anymore |
| `bunker spawn --ttl banana` | ✅ rejected cleanly | `invalid_argument: invalid ttl "banana": must match "[0-9]+[hmd]"` — no more silent 6h agent |
| `bunker destroy <unknown-id>` | ✅ idempotent exit 0 | `Agent <id> not found.` and exit 0 (DOGFOOD-005 fix holds) |
| `bunker status` CPU/Memory/Uptime | ✅ real values | 0.56s; no more zeros/unknown |

## Key lifecycle — the real credential model (2026-09-22/23, live on bunker-mvp @ 16fff6d)

Three credential classes; never test one and claim things about another:

1. Master/static bearer token (48-char opaque; daemon `auth.token`; operator CLI config
   per server entry). `key rotate` does NOT touch it — it bypasses JWT machinery entirely.
   The ONLY credential for Bunkerd-service (admin) RPCs (`GetAgent`, `KeyList`,
   `RotateJWTSecret`, `RevokeKey`, spawn/destroy/exec).
2. Agent sub-keys (43-char opaque; in the spawn bundle `API Key:` line ONLY — the agent's
   own `~/.bunker/` has NO config file, and no local SSH key is fetched: the bundle even
   warns `could not fetch SSH key: unauthenticated`). Valid ONLY on the Agent service
   (`/bunker.v1.Agent/GetInfo|Metrics|Heartbeat`). On Bunkerd endpoints a LIVE sub-key
   gets `401 {"message":"agent-scoped tokens are not allowed for this endpoint"}` BY
   DESIGN — scope-denial = key is LIVE; `invalid token` = revoked/unknown. Prove
   revoke-immediacy against `Agent/GetInfo`, not `Bunkerd/GetAgent`.
3. JWTs signed by the HS256 secret — what `key rotate` governs. NO CLI command currently
   issues one; to observe rotation, mint one yourself (secret = raw hex bytes of the
   output; HMAC over `<b64url(header)>.<b64url(payload)>`, standard HS256).

KNOWN P0 (DF-BUNKER-45): `key rotate` mutates `s.jwtAuth` but request validation runs in
interceptor instances built at boot — so rotation NEVER takes effect on the live path
until the daemon restarts with a manually-persisted secret file. Until that is fixed,
treat rotate as "deferred to restart", not zero-downtime, and do not build automation on
the overlap window. Rotate output (`auth.jwt_secret_file` + restart wording) describes
the JWT SIGNING secret — never paste it into the CLI config `token:` field (that is the
static master token; you will lock yourself out — recovery: older config backup or
`auth: token:` in /etc/bunkerd/config.yaml on the daemon host).

Revoke is the strong part of the surface: immediate, survives restart (both verified
live), hash-chained audit records in /var/log/bunkerd/audit.log (rotate and revoke
each append one correlated record; fingerprints/key-ids only, never secrets).

## Pitfalls

- **Never run the E2E battery from a client** (`e2e-full-battery.sh` needs root + on-server paths; it creates/deletes `bunker-e2e-*` users).
- **Server-reported hostname ≠ reachable host.** Any raw SSH/SCP/SSHFS command printed by spawn uses `bunker-mvp` and server paths — substitute the real IP and client key. (The `bunker cp` / `deploy` / `tunnel` CLI subcommands are fixed and work from a client directly.)
- **Don't trust "CI green" for SSH features** — the battery runs on the server where hostname+keys resolve. Remote-client behavior is the real test.
- **Auth is REQUIRED** — the MVP has enforced auth since GAP-014; an empty/missing token is rejected (unauthenticated → connect error; no Content-Type → 415; wrong service path → 404). Never commit real tokens — the test token `test-regression-token` (in e2e scripts) is fine for the MVP.
- **REST path is `bunker.v1.Bunkerd`, not `bunkerd.v1.Bunkerd`** — `POST http://<ip>:18080/bunker.v1.Bunkerd/<Rpc>` with `Content-Type: application/json`; a guessed service name 404s, errors are connect codes (`CodeNotFound`, `CodeInvalidArgument`, `CodeUnauthenticated`, `CodeResourceExhausted`). **Exception: streaming RPCs** (`ExecAgent`/`RunAgent`) need `application/connect+json` + envelope framing — see the REST recipes section; `application/json` there is a bare `415` with no envelope.
- **`SpawnAgent` takes `agent_id`; a `name` field does not exist (2026-09-18, DF-BUNKER-23)** — `docs/integration.md` §5 still advertises "name" (and env vars). Posting `{"name":"build-1"}` returns `200` with a RANDOM `agentId` and no warning; `GetAgent {"agent_id":"build-1"}` then 404s. Use `{"agent_id":"build-1"}` for a deterministic handle, and treat an unknown-field `200` as a silent drop.
- **Orphan private keys accumulate on daemon hosts (2026-09-18, DF-BUNKER-24)** — `destroy` cleans its own key, but hosts carry keys for users that no longer exist: las-03 139/139, las-02 4/7, demo host 1/1 (check with `id bunker-<key>`). Same residue family as GAP-080 (orphan homes + linger). If an experiment uses a *known* agent id, re-spawn reuses the same key name — don't assume a fresh key means a fresh host.
- **A spawn can hang ~5 minutes and then fail with `user manager did not start` (2026-09-18, DF-BUNKER-21)** — measured on bunker-las-02 when the agent's UID still carried the previous occupant's systemd state (`user-<uid>.slice` active, linger entry, stale `/run/user/<uid>`; 145 linger entries on that host). The client sees only a socket timeout at the documented 300s request timeout (the 500 lands at 5m0.001s), and the rollback ALSO fails (`rollback userdel failed: context deadline exceeded`), leaving the user, home and a fresh linger entry behind — invisible to `GetAgent` (404) and `bunker list`. Diagnose with `journalctl -u bunkerd` on the daemon host; a spawn on a clean UID took 21.8s on the same box.
- **Scratch agents:** always `bunker destroy <id>` after a run (1.9s, idempotent); TTL auto-destroys but don't rely on it. `bunker list` to confirm zero.
- **spawn positional name arg works (fixed DOGFOOD-008)** — `bunker spawn --ttl 30m my-agent` names the agent `my-agent`; `--agent-id` also accepted; ids match `^[a-z0-9-]{1,64}$`.
- **`bunker metrics <id>` memory is PER-AGENT (fixed: DOGFOOD-011 + GAP-060)** — read from the agent's own cgroup: `/sys/fs/cgroup/user.slice/user-<uid>.slice` (cgroupv2), whose `memory.max` IS the agent's `--memory` limit (the systemd-run dockerd unit and every exec session scope both live in that slice). When the per-agent read is unavailable (stopped/destroyed agent, deleted user), the values fall back to the HOST-level read — and since GAP-060 (61dafd2) that fallback is explicit: the `AgentMetricsResponse` carries `host_level_fallback` (field 10, proto/bunker/v1/bunker.proto) and the CLI prints `NOTE: host-level fallback (agent cgroup unavailable — metrics are HOST values, not agent values)`. If you see the NOTE, treat the numbers as host values. Disk numbers are per-agent; real limits via `bunker info` or on-host `user.slice/user-<uid>.slice/*`.
- **Audit `--agent <id>` exec filter works (fixed: DOGFOOD-012, commit 406508b)** — ExecAgent is a server-streaming RPC (the interceptor never sees its request message), but the handler now stamps the real target into a per-request sink (`audit.StampStreamAgentID`) and `remote_addr` comes from `conn.Peer()`, so exec records carry the right `agent_id` and address. Older logs from before the fix still show `agent_id:""` rows — filter those by `--since` timestamps + `bunker audit export`.
- **`bunker audit verify` is local-only** — runs on the daemon host (`--path /var/log/bunkerd/audit.log`); `--server` → `unknown flag` (DOGFOOD-013; group help over-promises).
- **exec is the ONE command with different flag grammar (2026-09-16, DF-BUNKER-8)** — `DisableFlagParsing: true` + a hand-rolled peeler means flags are only recognized AFTER the agent-id: `bunker --server X exec <id> ...` fails `agent "--server" not found` and `bunker exec <id> --timeout 900 -- ...` (flag after `--`) fails too. Right form: `bunker exec <id> --server X --timeout 900 -- cmd`, or simplest: `bunker use <server>` once, then bare exec. Everything else (spawn/list/status/info/audit/cp/env/metrics/heartbeat/destroy) accepts the global `--server`.
- **/tmp semantics depend on the DAEMON version, not the CLI (2026-09-16, DF-BUNKER-9)** — on v0.1.3 daemons (all current tags), exec sessions see the HOST /tmp and `run --detach` units get their own PrivateTmp, so the three tmp views differ; per-agent private /tmp (GAP-075) is only in daemons built from ≥ 9703082, which is in NO release tag yet. Never look for detached-job output in /tmp — write/read `$HOME`.
- **`bunker mount` can fail with raw `read: Connection reset by peer` against a healthy agent (2026-09-16, DF-BUNKER-11)** — raw ssh + sftp both fine; consistent with agent-host sshd parallel-session limiting. No CLI retry yet; wait and retry manually.
- **`bunker cp` destination is `<agent-id>:<path>`** — a bare local path errors `accepts 2 arg(s), received 3`-style usage noise; the remote form is mandatory.
- **Server version check before diagnosing** — `bunker status` prints the daemon's Version; the fleet can lag repo HEAD by a major version (las-04 = v0.1.3 on 2026-09-16), so behavior differences may be version drift, not bugs (DF-BUNKER-10).
- **`bunker audit verify` false-positives after ANY daemon restart (2026-09-19, DF-BUNKER-29)** — the hash-chain head lives only in process memory; a restarted daemon's first record carries an empty `prev_hash`, so verify reports "tamper detected" on a log nobody touched. Until fixed, `audit verify` is only meaningful on a daemon that hasn't restarted since the log was created.
- **Adopt mode destroys orphans without readable port metadata (2026-09-19, DF-BUNKER-30)** — `reconciliation.mode: adopt` adopts ONLY if `/home/bunker-<id>/.bunker/ports` is readable and in-pool; a hand-made `useradd bunker-x` orphan is WARNed and destroyed even in adopt mode. This is fail-closed by design, but the config.example.yaml comment doesn't mention the precondition.
- **Cleanup probes: use exact names** — `pgrep -f 'bunkerd --config …'` matches your own checking shell (two false "still running" readings in one session on 09-19); use `pgrep -x bunkerd` + `getent passwd bunker-<id>` instead.
- **🔴 `bunker mount` is BROKEN at HEAD (2026-09-20, DF-BUNKER-38)** — EVERY documented form fails: `bunker mount <id>` → `mount preflight: no remote path to check`; `bunker mount <id> <mnt> --path <direxists>` → `remote path "…" does not exist or is not a directory` even when it provably does. Root cause: `internal/cli/mount_preflight.go` `runWithTimeout` does `cmd.Start()` and then hands the same `*exec.Cmd` to `cmd.CombinedOutput()` (which calls Start again → `exec: already started`, empty capture → the confident wrong message). The same helper breaks `bunker umount <mountpoint>` (`unmount … failed (normal: exec: already started; lazy: exec: already started…)`, rc=1). **To actually mount today:** `BUNKER_SKIP_MOUNT_PREFLIGHT=1 bunker mount <id> <mnt> --path /home/bunker-<id>` — that works end-to-end (verified: read + write + agent sees the write). Do NOT conclude "my agent is broken" from a mount refusal: check with `ssh -i ~/.bunker/keys/<id> bunker-<id>@<ip> 'test -d $HOME && echo DIR_OK'` first.
- **`bunker mount <id>` with the documented optional mountpoint can never work (2026-09-20, DF-BUNKER-39)** — the preflight is called with an empty remote path and refuses before ssh runs. Always pass an explicit `--path` until this is fixed; a `--path` that is the agent home is `/home/bunker-<id>`.
- **The shared scratch exchange point needs `bunker host-provision --apply` FIRST (2026-09-20, DF-BUNKER-40)** — the spawn path creates each agent's capped tmpfs directory under whatever `/srv/bunker-share` already exists, so on a host where the root was never provisioned for real (`2750 root:bunker-agents`) every README exchange example fails `Permission denied` while the daemon logs `"shared scratch ready"`. Check before you trust it: `ssh <host>-root 'stat -c "%n %a %U:%G" /srv/bunker-share'`. Measured: mvp `2750` ✅, las-01 `750 root:root` ✗, las-02/las-04 missing ✗.
- **`--server` placement for `run`/`env` (2026-09-20, DF-BUNKER-41/42)** — `bunker run --server X <id> -- cmd` and `bunker --server X run <id> -- cmd` both fail `no target bound`; only `bunker run <id> --server X -- cmd` works. `bunker env set/get` accept `--server` ONLY before the subcommand (`bunker env --server X set <id> K=V`); anywhere else it is eaten as the positional and you get `requires exactly one KEY=VALUE argument`. (exec was unified in DF-BUNKER-31 but the peeler was not extended to its siblings.)
- **Some failures exit 0 when stdout is piped (2026-09-20, DF-BUNKER-43)** — `bunker audit list --server X | head` and `bunker status --json | head` print `bunker: <error>` yet report exit 0, against the README's exit-code table. In scripts, take `${PIPESTATUS[0]}` AND grep stderr for `^bunker:` rather than trusting the status; the audit `--server` path also fell back to the local root-owned log in this run.

## Cross-agent isolation — what is actually verified (2026-09-20, las-03 @ HEAD 93d7a53)

Read this before writing anything that depends on the isolation promises.

- **Detached units DO get their own `/tmp` (G3, confirmed live for the first time).** On the same agent:
  `exec -- sh -c 'echo m > /tmp/x; ls /tmp | wc -l'` sees the session `/tmp` (host-shared when the host is not
  provisioned — the host's own files and `/tmp | wc -l` ≈ 1219); `run --detach -- sh -c 'ls /tmp | wc -l'` sees
  **1** and cannot see either the session marker or the host marker. Never look for detached output in `/tmp` —
  write to `$HOME` (or `/home/bunker-<id>`).
- **Agent-to-agent `/tmp` isolation is real** — B cannot read or overwrite A's `0600` file (Permission denied),
  and A's value survives B's attempt untouched.
- **The per-agent scratch cap is a real kernel bound** — a 300 MiB write into the 256 MiB agent directory stops
  at exactly 268435456 bytes (`df` 100%). The cap works; reaching the directory is what does not (§ above).
- **Confirmed fixed at HEAD (do not re-litigate):** `metrics`/`info`/`heartbeat` on a never-spawned id return
  `not_found` (DF-BUNKER-28 fabricated records gone); `audit list --server` carries `Caller` + `Agent` per
  record and `--agent` filters correctly; `stop`/`start` keep user/home/env/`/tmp`/files and `exec` against a
  stopped agent fails with the `agent_stopped` token; spawn 29-48s on a warm host.

## Right way to validate changes

1. `go build ./... && go vet ./... && go test ./... -short`
2. For exec/env changes: from a CLIENT machine, `bunker exec <id> -- "if true; then echo ok; fi"` and all 4 env subcommands.
3. For cp/deploy/tunnel: from a client machine against the live MVP (IP-based).
4. For spawn/destroy/cgroups: on a root host with the battery, PLUS a client-side lifecycle check.
5. Update `.coding-hermes/tasks.md` + `.gitreins/tasks.yaml`; run `gitreins guard` before commit.

## Where to look when something breaks

- Exec/env weirdness → `internal/server/service.go` (`buildAgentExecCommand` L~372, `buildExecSSHCommand` L~437), `internal/cli/env.go`
- Audit trail gaps → `internal/audit/interceptor.go` (`WrapStreamingHandler`; the msg-invisible streaming gap is closed for ExecAgent via `StampStreamAgentID`, DOGFOOD-012/406508b), `internal/audit/audit.go` (chain/rotation/shipping/seals), `internal/cli/audit.go`
- Metrics wrong numbers → `internal/resource/cgroup.go` (`ReadAgentCgroupMetrics` reads the per-agent `user-<uid>.slice`; host fallback flagged via `host_level_fallback`, DOGFOOD-011/GAP-060), `internal/server/service.go` `AgentMetrics`, `internal/cli/metrics.go` (the NOTE line)
- SCP/tunnel host/key issues → `internal/cli/cp.go`, `internal/cli/deploy.go`, `internal/cli/tunnel.go`
- Key hygiene after destroy → `internal/cli/destroy.go` (client key cleanup landed — CLI prints `Removed local SSH key`, DOGFOOD-014 verified fixed 2026-09-18) and the daemon-side sweep for orphaned `/etc/bunkerd/ssh/<id>` keys (DF-BUNKER-23, still open)
- Spawn/destroy/TTL/cgroups → `internal/agent/`
- API contract → `specs/api.md`; architecture → `specs/architecture.md`
- Full dogfood evidence + diagnostics → `docs/dogfood/2026-09-18-integration.md` (current: REST-integrator run), `docs/dogfood/2026-09-16-integration.md`, `docs/dogfood/2026-08-29-integration.md`, `docs/dogfood/2026-08-18-integration.md` (prior runs), `docs/dogfood/diagnostics.md` (§12 = REST/protocol state)
