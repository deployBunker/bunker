# Bunker dogfood — REST integration report (2026-09-18)

**Target:** `bunker-pm` (PM lane row) → the real project it serves, **bunker**
(`/home/kara/bunker`, public origin `https://github.com/deployBunker/bunker.git`).
The pm lane's workdir (`~/.hermes/stand-in/pm-lane/bunker`) is an intentionally
empty stand-in: `ops/pm-standin/pm-standin-tick.sh` resolves the *base* project's
workdir from `scheduler.db` and files proposals onto the base board (proof: the
lane filed GAP-078..GAP-083 onto `.coding-hermes/board/tasks.jsonl` at
2026-09-18T07:15Z). Manual-pick fallback to the real repo, same as the
2026-08-29 run for `bunker-sync`.

**Verdict: 🟡 PROMISING-BUT-ROUGH** (fourth run in the series:
SHIPPABLE → PROMISING-BUT-ROUGH → SHIPPABLE → SHIPPABLE → *this*).

The CLI lifecycle is still solid. This run took the **fresh-integrator path that
no previous run took**: build a real client from `docs/integration.md` alone —
no CLI, no protobuf, no generated stubs — and drive a full agent lifecycle over
the documented REST surface. The unary surface holds up; the **streaming
surface does not**: it is undocumented, and the "obvious" call documented in §2
returns a bare `415` with an empty body that contradicts the doc's own error
promise. The run also produced its most serious finding on a **fleet daemon
host**: a spawn that landed on a UID still carrying a previous agent's systemd
state hung the client for 5 minutes, failed, and its rollback failed too —
leaving a user + home + linger entry behind with no API surface that can see or
clean them (§4.5).

---

## 1. Promise (null hypothesis)

> An integrator can drive the whole agent lifecycle against a `bunkerd` daemon
> over plain HTTP using `docs/integration.md` as the only reference: JSON POSTs
> to `/bunker.v1.Bunkerd/<Method>` with a bearer token, proto field names in,
> protojson (camelCase, string-encoded 64-bit) out, `{"code","message"}` error
> envelopes on failure, and a server-streaming `ExecAgent` for command I/O.

## 2. Method (what was actually run, live)

| Item | Value |
|---|---|
| CLI/daemon built from | HEAD `967c331` (v0.1.4), `make build` = **6.0s**, go1.26.5 |
| Primary target daemon | `bunker-las-02` (:10001, **v0.1.4** — matches HEAD) |
| Secondary probes | `bunker-mvp` (v0.1.4, tmpIsolation=private), `bunker-las-03` (v0.1.3), `bunker-las-04` (v0.1.3), control-host `karaHermes-mde-7840hs` (v0.1.3) |
| Client written | `/tmp/dogfood-bunker-rest/bunker_rest.py` + `exec_stream.py` — stdlib only (urllib + raw sockets), no gRPC/protobuf |
| Agents used | `70f21686` (auto-id), `df0918-named` (explicit `agent_id`), `df0918-install` (las-03, install leg), `df0918-ttl` (2-minute TTL hygiene probe) |
| Cleanup | all scratch agents destroyed via the REST client; las-02 back to its baseline 2 agents, las-03 back to 0, no leaked users/homes; the one leaked-key class found is filed as a finding (§6 DF-BUNKER-23) |

**Time-to-first-success:** `ServerInfo` answered on the first call, ~0.5s after
the client existed. First agent created purely over REST: **~20s** (cold image;
the follow-up named spawn returned in well under a second with the image
cached). First successful `ExecAgent` over REST: **~10 minutes of protocol work**
(see §4) — that gap is the finding.

## 3. What worked exactly as documented

- `ServerInfo` / `ServerMetrics` / `ListAgents` / `GetAgent` / `AgentMetrics` /
  `HeartbeatAgent` / `DestroyAgent` / `SpawnAgent` / `QueryAudit` — all unary
  RPCs, all correct over REST with `Content-Type: application/json` +
  `Authorization: Bearer <token>`.
- protojson conventions from §2 held exactly: `agent_id` in, `agentId` out;
  64-bit values as JSON strings (`uptimeSeconds: "427061"`, `memoryUsedBytes`),
  32-bit/float as numbers (`agentCount: 2`, `cpuUsagePercent: 87.55`).
- Error envelope + status mapping: `GetAgent` miss → `404 {"code":"not_found",
  "message":"agent \"df0918-rest\" not found"}`; `SpawnAgent {"ttl":"banana"}`
  → `400 {"code":"invalid_argument","message":"invalid ttl \"banana\": ..."}`
  **with no side effects** (agent list unchanged — the "validate before any side
  effect" claim holds over REST, not just in the CLI).
- Destroy semantics: `404 not_found` for a never-known id (per §5).
- Audit attribution: `QueryAudit` returns records for every authenticated REST
  call, including `ExecAgent`, with `agentId` + `remoteAddr` populated.
- Agent metrics are **agent-scoped**: `AgentMetrics 70f21686` →
  `memoryUsedBytes: 401108992` against `memoryLimitBytes: 8589934592` — not host
  numbers (DOGFOOD-011 stays fixed over REST).

## 4. What did NOT work as documented (the core of this run)

### 4.1 `ExecAgent` over REST: undocumented, and the documented call returns a bare 415

§5 lists `ExecAgent` as **server-streaming** and §2 says REST clients "POST JSON
to the connect-go path convention … `Content-Type: application/json`". Doing
exactly that:

```bash
$ curl -si -X POST http://100.116.99.35:10001/bunker.v1.Bunkerd/ExecAgent \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d '{"agent_id":"70f21686","command":"id"}'
HTTP/1.1 415 Unsupported Media Type
Content-Length: 0
```

`415`, empty body — **no `{"code","message"}` envelope**, which §2 explicitly
promises for non-2xx responses ("Errors carry a JSON `{"code","message"}` envelope
and a real HTTP status"). An integrator following the guide has no path forward
here: not from §2, not from §5, not from §9.

### 4.2 The recipe that does work (connect streaming, reverse-engineered)

```
POST /bunker.v1.Bunkerd/ExecAgent
Content-Type: application/connect+json      # NOT application/json
Authorization: Bearer <token>
body: [flags:1][len:4 BE][json payload]     # one envelope per message
```

Response: HTTP 200, `Transfer-Encoding: chunked`, body = a sequence of envelopes
with the same 5-byte prefix. Observed frames for `id -un; hostname; echo $HOME`:

```
flags=0x00 {"stdout":"YnVua2VyLTcwZjIxNjg2Cg=="}   # base64 of "bunker-70f21686\n"
flags=0x00 {"stdout":"YnVua2VyLWxhcy0wMgo="}       # "bunker-las-02\n"
flags=0x00 {"stdout":"L2hvbWUvYnVua2VyLTcwZjIxNjg2Cg=="}  # "$HOME"
flags=0x00 {}                                       # exit code 0 (omitted: default)
flags=0x02 {}                                       # end-of-stream trailer
```

Four things the guide never mentions, each of which cost time:

1. **Content type must be `application/connect+json`**; `application/json`
   gives the unhelpful 415 above. The guide never mentions connect streaming at all.
2. **stdout/stderr are base64** (protojson `bytes`), one envelope per write —
   the guide documents the int64-as-string rule but not the bytes-as-base64 rule,
   which is the one that actually bites a stdout consumer.
3. **The response is chunked**; a client must de-chunk (or use an HTTP library
   that does) before parsing envelopes.
4. **`exitCode` appears only when non-zero** (`{"exitCode":3}` for
   `sh -c 'exit 3'`), and the last frame is a `flags=0x02` trailer — neither is
   documented.

The end-of-stream envelope is also a trap. Both spec-shaped endings are rejected:

| Request tail | Result |
|---|---|
| none (rely on Content-Length) | ✅ 3 frames, command output intact |
| `[0x02]` single byte | `HTTP 200` + `{"error":{"code":"invalid_argument","message":"protocol error: incomplete envelope: unexpected EOF"}}` |
| `[0x02][0x00000000]` 5-byte envelope | `HTTP 200` + `{"error":{"code":"internal","message":"unmarshal end stream message: unexpected end of JSON input"}}` |

Note the shape of that failure: a *protocol* error delivered as an HTTP 200 with
an `error` object in the stream — a client checking only the status code sees
success and an empty result.

`Connect-Protocol-Version: 1` turned out **not** to be required (works without it).

### 4.3 Real workload through the client (proof it is a usable surface once known)

```python
body = frame(json.dumps({"agent_id":AID,"command":"docker run --rm alpine echo REAL-USE-REST-PASS"}).encode())
# frames: 8 stderr frames (image pull) + {"stdout":"REAL-USE-REST-PASS\n"} + {} + 0x02 trailer
```

Executed inside the isolated agent (`bunker-70f21686`, hostname `bunker-las-02`,
`$HOME=/home/bunker-70f21686`), rootless Docker pulled `alpine:latest` and ran
it in 9.0s. A real cross-agent workflow (spawn → exec a container workload →
metrics → heartbeat TTL extension → destroy) completed entirely over REST.

### 4.4 `SpawnAgent`: the documented field does not exist and is silently dropped

§5 describes `SpawnAgent` as creating an agent "(name, TTL, resource limits,
network mode, env vars)". The proto's request has **`agent_id`** (optional), TTL,
limits, network, `ssh_public_key`, `labels`, `image_spec` — **no `name`, no env
vars**. Following the doc:

```
POST SpawnAgent {"name":"df0918-rest","ttl":"1h"}  -> 200, agentId "70f21686"   # name ignored, no error, no warning
POST GetAgent   {"agent_id":"df0918-rest"}         -> 404 not_found
POST SpawnAgent {"agent_id":"df0918-named","ttl":"30m"} -> 200, agentId "df0918-named"   # works
```

A CI client that trusts the doc loses its handle on the agent it just created —
the id is random and there is no error to tell you the field was ignored. This
is the REST twin of the old `DOGFOOD-008` CLI bug (positional name silently
dropped), still present at the protocol level because §5 was never corrected.

### 4.5 A spawn that landed on a recycled UID hung for 5 minutes, failed, and left residue (the run's most serious finding)

While probing TTL-expiry hygiene, a spawn on `bunker-las-02` never answered the
client. Verbatim sequence from the daemon host (`journalctl -u bunkerd`):

```
03:11:50  INFO  creating user          username=bunker-df0918-ttl
03:11:50  INFO  persisted SSH private key   path=/etc/bunkerd/ssh/df0918-ttl
03:11:50  INFO  starting rootless dockerd   unit=bunker-docker-df0918-ttl
03:11:50  INFO  resetting user manager runtime   uid=1012 runtime_dir=/run/user/1012
03:11:50  INFO  lazily unmounted stale runtime mount   mount=/run/user/1012
   ...5 minutes of silence...
03:16:50  WARN  rolling back: removing user            username=bunker-df0918-ttl
03:16:50  ERROR rollback userdel failed   error="context deadline exceeded"
03:16:50  ERROR spawn agent failed   error="install rootless docker for bunker-df0918-ttl:
          user manager did not start for bunker-df0918-ttl: context deadline exceeded"
03:16:50  POST .../SpawnAgent - 500 154B in 5m0.001132966s
```

Client side: `urllib` raised a socket timeout at **300s** — the response only
arrived at 5m0.001s. A client on the documented 300s request timeout never sees
the 500 body at all; a client with a longer timeout sees only
`internal`/500 with no progress signal in between.

State on the host before that spawn (measured): UID 1012 belonged to a
**deleted** agent (`bunker-59bd09c1`) whose `user-1012.slice` was **still active
since 2026-09-14** (3 days) and whose home was gone;
`/var/lib/systemd/linger/` held **145** `bunker-*` entries; `/run/user/` held
stale numeric dirs (1002–1008) from deleted users; 5 orphan homes and 4 orphan
SSH keys were present.

Aftermath of the failure — residue the API cannot see or clean:

| Artifact | State |
|---|---|
| `/etc/passwd` uid 1012 `bunker-df0918-ttl` | **left behind** (rollback's `userdel` timed out) |
| `/home/bunker-df0918-ttl` | **left behind** (36 KB, partially installed) |
| `/etc/bunkerd/ssh/df0918-ttl` | removed by the rollback (key cleanup works) |
| `/var/lib/systemd/linger/bunker-df0918-ttl` | **left behind** (created by the failed spawn) |
| `GetAgent df0918-ttl` | `404 not_found` — the daemon has no record of the leftovers |

**Control experiment:** after deleting that residue user + home at the host
operator level, a new spawn that landed on the **same** UID 1012 (fresh state)
succeeded normally in **21.8s** (`dockerd ready` → `agent registered` →
`agent spawned successfully`), and was destroyed cleanly. So the failure
correlates with stale per-UID systemd state, not with UID reuse alone — and the
successful path proves the host itself is healthy once that state is gone.

Hypothesis to test in a worker (not claimed as proven): `useradd` reuses a freed
UID while the old occupant's `user-<uid>.slice` / linger entry / `/run/user/<uid>`
still exist; the new agent's `systemd --user` manager then cannot start within
the spawn deadline, and because rollback reuses the already-expired spawn
context, its `userdel` fails too. Consequence: **hosts drift into a state where
spawns fail for 5 minutes and litter**, and nothing in `bunker status` /
`bunker list` shows it. This is the failure-mode version of the GAP-080 residue
story (which measured orphan homes + linger on the demo host without connecting
them to spawn failures).

## 5. Install leg — fresh host, from scratch (ephemeral agent on bunker-las-03)

Per the skill's ephemeral-install rule (fresh user, documented path only, no
access changes):

| Step | Result |
|---|---|
| Spawn ephemeral agent (project's own CLI, `--server bunker-las-03 --ttl 2h`) | 22s, agent `df0918-install`, bare Debian 13 (trixie) |
| `git clone https://github.com/deployBunker/bunker.git` (the ORIGIN URL in the repo, existing access, nothing widened) | ✅ 2s, HEAD `967c331` — identical to the control host |
| documented `make build` | ❌ `/bin/sh: 1: go: not found` / `make: *** [Makefile:24: build] Error 127` — **`make` exists on this host, Go does not** |
| install Go 1.26.5 (README prerequisite; README gives no how) | ✅ `tar -C ~ -xzf go1.26.5.linux-amd64.tar.gz`, warning: `both GOPATH and GOROOT are the same directory (.../go)` |
| documented `make build` again | ✅ **49s**, `./bunker --version` → `0.1.4 / commit 967c331` (smoke PASS, `./bunkerd --help` exit 0) |
| destroy + host check | ✅ agent destroyed, `Removed local SSH key`, no leaked user/home; `ls /etc/bunkerd/ssh/df0918-install` gone |

**Install verdict:** the documented path works *once Go exists*, but nothing in
the repo tells a user how to get Go and no prebuilt binary is shipped
(GAP-036), so the very first documented command fails on a stock Debian host.
Filed as DF-BUNKER-24.

## 6. Findings filed (board rows on `.coding-hermes/board/tasks.jsonl`)

| ID | Pri | Finding |
|---|---|---|
| DF-BUNKER-21 | P1 | A spawn that lands on a UID with stale systemd state hangs 5 minutes, fails with `user manager did not start` (client sees only a socket timeout at 300s), and its rollback fails too — leaving an orphan user + home + linger entry that no API surface reports; hosts carry 145 linger entries / 5 orphan homes / 4 orphan keys (las-02) |
| DF-BUNKER-22 | P1 | `ExecAgent`/`RunAgent` streaming over REST has no recipe in `docs/integration.md`; the §2-documented call returns a bare `415` with no `{code,message}` envelope |
| DF-BUNKER-23 | P2 | `docs/integration.md` §5 documents a `name` field (and env vars) that `SpawnAgentRequest` does not have — unknown JSON fields are silently dropped, so a doc-following client gets a random id and a 404 on its own handle |
| DF-BUNKER-24 | P2 | Orphan private keys: `/etc/bunkerd/ssh/<agent>` survives agent death — 139/139 orphaned on las-03, 4/7 on las-02, 1/1 on the demo host (measured by matching each key to a `bunker-<id>` user) |
| DF-BUNKER-25 | P2 | First-run adoption gate: no prebuilt binaries and the Go prerequisite has no install path; on a stock Debian host `make build` dies with `go: not found` (Error 127), then the naive tarball install adds a confusing GOPATH==GOROOT warning |
| DF-BUNKER-26 | P3 | Doc/cosmetic drift bundle: audit record schema in §4 omits `hash`/`prevHash` (the log is hash-chained and `QueryAudit` returns them); `ServerMetrics` now returns an undocumented `agents[]` array; the invalid-TTL error text double-escapes its regex (`must match "\\d+[hmd]"`); destroy of a *just-destroyed* agent returns `200 destroyed` while a never-known id returns `404` |

### Verified fixed this run (previous dogfood findings, re-checked live)

- **DOGFOOD-011** (metrics reported host memory) — `AgentMetrics` is agent-scoped
  (401 MB vs 8 GB limit over REST).
- **DOGFOOD-012** (audit exec records missing `agent_id`/`remote_addr`) — every
  successful `ExecAgent` record now carries both, REST streaming included
  (`{"method":"/bunker.v1.Bunkerd/ExecAgent","agentId":"70f21686","remoteAddr":"100.97.236.14:39642","outcome":"ok"}`).
  Failed decodes still record `agentId: null` (expected — the request never decoded).
- **DOGFOOD-014** (destroy leaves the client SSH key) — CLI prints
  `Removed local SSH key /home/kara/.bunker/keys/df0918-install`.
- **DOGFOOD-008** (named spawn) — named agents work over REST via `agent_id`
  (`df0918-named` round-tripped), which is why the §5 wording is now the only
  part still wrong.

## 7. Judgement

| Question | Answer (evidence) |
|---|---|
| Does it work? | **Yes** for the CLI and for unary REST; the lifecycle (spawn → exec → metrics → heartbeat → destroy) completed over REST with a real Docker workload, and the install path builds from a fresh clone in 49s once Go exists. |
| Is it useful? | **Yes** — an isolated Linux user + rootless Docker + cgroup limits behind one HTTP call is a genuinely useful primitive; the REST surface makes it consumable from any language without generated stubs. |
| Is it usable? | **Only half** — the unary path is now well documented (post-DF-BUNKER-19 rewrite), the streaming path is not documented at all, and one documented request field (`name`) silently does nothing. A competent integrator hits a dead end within 10 minutes of wanting command output. |
| Is it trustworthy? | **Yes** — TTL validation happens before side effects, errors carry codes, the audit log is hash-chained with correct per-call attribution, and destroy/cleanup left no leaked user or home on two hosts. |

**Top 3 to fix first** (an hour of maintainer time): (1) the spawn
hang/rollback-failure/residue chain — DF-BUNKER-21 (make rollback use a fresh
context, clean the per-UID state before reuse, and surface residue counts);
(2) add a copy-pasteable streaming recipe (and/or return a proper envelope on the
415) — DF-BUNKER-22; (3) correct §5's `SpawnAgent` field list to `agent_id` —
DF-BUNKER-23.

**Artifacts:** this report, `diagnostics.md` §12, `skills/bunker-usage/SKILL.md`
v1.4.0 (streaming recipe + pitfalls), the working client
(`/tmp/dogfood-bunker-rest/`, reproduced inline in §4.2).
