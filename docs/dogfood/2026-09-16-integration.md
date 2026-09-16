# Dogfood Integration Report — 2026-09-16

**Verdict:** ✅ SHIPPABLE (third consecutive run: SHIPPABLE 08-29/09-01 → PROMISING-BUT-ROUGH 09-07 → SHIPPABLE 09-16)
**CLI under test:** built at HEAD `66d4150` (v0.1.4, `make build`)
**Live daemon:** bunker-las-04 (`http://100.95.199.98:10001`, daemon v0.1.3, uptime 1h48m)
**Promise tested:** spin up isolated, resource-limited rootless-Docker agents on a remote host and drive the whole lifecycle from one CLI.

This run was performed against the **live fleet daemon**, not a local test daemon, and every
command below was actually executed. Commands that worked are safe to copy. Findings are
tracked as DF-BUNKER-8..12 (see `.coding-hermes/board/tasks.jsonl`).

## The working workflow (verified live, 2026-09-16)

```bash
# 0. Build at HEAD (the binary matters: repo moves fast)
make build && ./bunker version    # commit must match git rev-parse HEAD

# 1. Point at a live server (las-04; auth enforced)
./bunker --server bunker-las-04 status     # ONLINE, v0.1.3, agents 1/8
./bunker use bunker-las-04                 # set active server — DO THIS BEFORE exec, see Pitfall 1

# 2. Spawn a NAMED agent (~20-45s; --agent-id, not a positional name)
./bunker spawn --agent-id df0916a --ttl 2h
# → connection bundle: SSH key path, port range, Expires (daemon-local TZ), sshfs/tunnel cmds

# 3. Drive it
./bunker info df0916a
./bunker exec df0916a -- sh -c 'whoami; echo $((6*7))'     # bunker-df0916a, 42
./bunker env set df0916a KEY=value && ./bunker env get df0916a KEY
./bunker cp /path/file df0916a:file                        # NOTE <agent-id>:<path> destination form
./bunker exec df0916a -- docker run --rm alpine:latest echo hello   # rootless docker, first pull ~10s
./bunker run df0916a --detach /bin/sh -c 'echo proof > $HOME/detach-proof.txt'
./bunker metrics df0916a                   # agent-scoped: Memory Used 376.7 MB / Limit 8 GB

# 4. Image customization (GAP-064) — package-add spec, validated & cached server-side
cat > spec.json <<'EOF'
{"packages": [{"manager": "apt", "packages": ["jq", "curl"]}]}
EOF
./bunker spawn --agent-id df0916b --ttl 1h --image-spec spec.json   # 44s incl. build
./bunker exec df0916b -- jq --version                      # jq-1.7 present

# 5. Docker socket tunnel (run in background; then use docker against :2376)
./bunker tunnel df0916a & docker -H localhost:2376 ps

# 6. Audit trail (remote query, no root needed with --server)
./bunker audit list --server bunker-las-04 --agent df0916a
# → every ExecAgent now attributed with agent_id (closed 08-29's DOGFOOD-012)

# 7. Tear down — keys are auto-removed unless --keep-key
./bunker destroy df0916a && ./bunker destroy df0916b
```

## REST contract (curl-probed, matches docs exactly)

| Probe | Result |
|---|---|
| `POST /bunker.v1.Bunkerd/ServerInfo` no auth | `401` |
| `GET` same path, no auth | `405` (POST-only surface) |
| `POST` with wrong bearer | `401` |
| `POST` with valid bearer | `200` |
| `GET /healthz` (no auth) | `200` |

Request fields are proto snake_case (`agent_id`); every RPC is `POST /bunker.v1.Bunkerd/<Rpc>`.

## Errors hit, and what each one means

| Error | Cause | Right way |
|---|---|---|
| `agent "--server" not found` | exec peels flags only AFTER the agent-id; global `--server` position becomes the agent-id (DF-BUNKER-8) | `bunker use <server>` once, then bare `bunker exec ...` |
| `agent "--timeout" not found` | same trap: `--timeout 900` must come after the agent-id, before `--` | `bunker exec <id> --timeout 900 -- cmd` |
| `cp: accepts 2 arg(s), received 3` | destination must be `<agent-id>:/path`, not a bare local path | `bunker cp ./file <id>:file` |
| `mount: read: Connection reset by peer` (2/2) | agent-host sshd limiting parallel sessions; raw ssh + sftp both fine (DF-BUNKER-11) | retry later / close other tunnels; no CLI workaround yet |
| `exec df0916a -- cat /tmp/j.log` → missing | `run --detach` writes to its own unit's private /tmp, NOT the exec-session /tmp (and on v0.1.3 daemons the exec /tmp is the host /tmp — DF-BUNKER-9) | write to `$HOME`, as in the workflow above |
| first `go build` in a fresh agent: `deadline_exceeded` at default exec timeout | cold module downloads ≈ 3-4 min | `bunker exec <id> --timeout 900 -- ...` (after the id!) |

## Verified-fixed since the 2026-08-29 / 09-01 / 09-07 runs

- **DOGFOOD-011 (P1):** `bunker metrics` now reports agent-scoped memory (376.7 MB vs host 31.1 GB); host-level fallback is explicit in code.
- **DOGFOOD-012 (P1):** audit ExecAgent records carry `agent_id`; `--agent` filtering works; fresh spawn→exec→destroy all attributed (verified with a marked cycle).
- **DOGFOOD-008:** named spawn via `--agent-id` (20s warm, 33-44s with image-spec).
- **Mixed-case hostname (DF-BUNKER-1):** fixed for NEW registrations — `connect --name KaraCaseTest` then `--server KaraCaseTest` round-trips exactly (viper replaced by case-preserving yaml/v3 I/O, 749386c). Pre-fix config files that store a lowercase key with a mixed-case `name:` field still fail lookup by that name (legacy-data edge; `bunker use` error message now helpfully lists available servers).
- **Destroy noise:** `systemctl disable` WARN suppression (DF-BUNKER-5, 66d4150) verified live — destroy is clean.
- **GAP-027:** `go install github.com/deployBunker/bunker/cmd/bunker@v0.1.3` succeeds (published tags now complete).

## Install leg (fresh-install rehearsal)

`las-bunker-03` (the standard ephemeral host) was down (ssh timeout; its daemon also unreachable
— DF-BUNKER-10), so the leg ran **on the project's own spawn path**: `bunker spawn --image-spec`
agent (git+curl) → `git clone --depth 1 https://github.com/deployBunker/bunker.git` (HEAD 0bd45e4,
public clone fine) → toolchain fetch → `make build` fails `make: not found` (DF-BUNKER-12, docs
gap) → `go build -o bunker ./cmd/bunker && go build -o bunkerd ./cmd/bunkerd` → **exit 0 in 6s**
(warm) after ~3-4 min cold downloads → `./bunker version` prints HEAD. Smoke = PASSED. Agent
destroyed afterwards; las-04 back to its pre-run state (only the pre-existing `crier-lab` agent).

## Two-minute reality check for "is this thing useful?"

Yes: one CLI, no client state beyond `~/.bunker/`, a disposable Linux box with rootless Docker
in ~20-45s, per-agent env/cp/detach/tunnel/audit, TTL + heartbeat for lifecycle hygiene, and an
append-only hash-chained audit trail queryable per agent. The 09-16 gaps are grammar
inconsistency (exec), an isolation promise ahead of the deployed daemon, and mount reliability —
all real, none fatal to the core promise.
