# Dogfood Run 20 — The Renewal Workflow (stable identity across renewals)

**Date:** 2026-09-25 (run 20 of the bunker dogfood series)
**Surface:** `bunker renew` + the destroy live-process gate + the drift
pre-flight — docs/renewal.md's operator recipe, untouched by runs 1–19.
**Promise under test:** *"An operator can renew a long-lived agent keeping the
SAME agent id / home path / system user / SSH identity, guided by
docs/renewal.md's drift report and destroy gate."*

## Method

- **Fleet daemon leg:** live agents on `bunker-las-03` (daemon 0.1.4 @ 6a6ad20,
  the deployed fleet daemon) driven by the HEAD client (a4e98ce) AND by the
  release 0.1.4 client (509fc42).
- **HEAD daemon leg:** run 19's scratch-daemon pattern — HEAD `bunkerd` (a4e98ce)
  as root on bunker-las-03, REST :28094 / gRPC :28095, private ssh_dir/registry/
  audit paths, port pool 28200-28299. Client config and keys in /tmp/df-renew-client
  (real ~/.bunker/config.yaml untouched, md5 8c1fdfd7… before and after).
- A real long-lived footprint was seeded into agents before every renewal:
  systemd `--user` units, a crontab entry, a config file embedding
  `/home/bunker-<id>`, a `.bashrc` PATH line — exactly what DF-BUNKER-34's
  original damage consisted of.

## What held (live proof)

1. **Stable identity works.** Renew (fleet daemon, clean agent) returned the
   SAME id: same uid 1001, same home, and the drift-report headline ran. 39s
   end-to-end (destroy + re-spawn).
2. **Drift report works at HEAD** — real hits, file:line-precise:
   ```
   Pre-flight drift report for df-renew-sc (searching for /home/bunker-df-renew-sc):
     2 stale-path hit(s) referencing /home/bunker-df-renew-sc across 4 scanned file(s):
       .config/systemd/user/docker.service:7: Environment=PATH=/home/bunker-df-renew-sc/bin:…
       .config/systemd/user/docker.service:8: ExecStart=/home/bunker-df-renew-sc/bin/dockerd-rootless.sh
   ```
   It correctly found the rootless-docker unit the agent image itself installs
   — the exact class of stale path DF-BUNKER-34 warned about.
3. **The destroy live-process gate fires at HEAD** and names the uid, every
   pid and its remedy — verbatim refusal captured in
   /tmp/dogfood-bunker-renew/renew-full.log.
4. **Home wipe is honest:** after renew, `data.txt` and the seeded files are
   gone (recipe says "renew does not copy your data" — held exactly).

## What broke (rows filed)

- **DF-BUNKER-65 (P1) — renew never rotates the client SSH key, so the whole
  SSH surface dies after every successful renewal.** Root cause (code):
  `internal/cli/renew.go` never calls the `GetAgentKey` RPC that
  `internal/cli/spawn.go:228` calls after spawn. The daemon persists a NEW
  keypair on re-spawn (manager_spawn.go:822 — "GetAgentKey RPC both read the
  same secret"), the client's `~/.bunker/keys/<id>` keeps the OLD one (mtime
  unchanged through the renew), and every ssh/scp/sshfs dial now fails
  `Permission denied (publickey,password)` while RPC verbs still work.
  Live: `bunker cp` exit 255, `bunker mount` preflight Permission denied,
  direct ssh denied, `bunker exec` fine. This is DF-BUNKER-59's client-side
  lesson applied to renew: the key-fetch follow-up is missing.

- **DF-BUNKER-66 (P1) — the destroy gate's remedy is unsatisfiable from the
  agent side, so renew against the deployed fleet daemon cannot renew any
  running agent.** The deployed daemon (6a6ad20, no 385-commit tail) has no
  destroy gate: renew succeeded exit-0 while the uid owned live processes
  (rootless dockerd, sd-pam, slirp4netns, a held exec session). At HEAD the
  gate fires but on dbus-daemon/pipewire/wireplumber/mpris-proxy — the
  rootless-docker login session's OWN default.target services. `bunker exec`
  cannot stop them (session close reaps them), `bunker run --detach` stop +
  pkill both leave them running (systemd respawns them). Only host-root
  `userdel`-adjacent action can satisfy the gate; the fleet daemon never
  shows this refusal at all. Net: the documented "stop those processes … then
  retry" recipe has no agent-side path.

- **DF-BUNKER-67 (P1) — spawn on the standard preset hard-fails on
  bunker-las-03: "containment landing did not converge within 5 reads:
  memory.swap.max = \"max\", want \"0\"".** The slice drop-in IS written
  correctly (`/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf` with
  `MemorySwapMax=0`) but systemd 257.13 lands it in ~125–349ms (measured:
  direct probe landed MemorySwapMax=0 after 349ms incl. daemon-reload), and
  `internal/agent/isolation.go:790` allows only 5 reads × 25ms = 125ms before
  a hard spawn failure with full user rollback. Run 19 (same box, same binary,
  this morning) spawned fine — this is a timing-sensitive gate under
  headroom. Blast radius on this box: every standard/hardened spawn AND every
  `renew` of a standard-preset agent (renew hard-codes the default preset,
  has no --preset flag) fails. The `open` preset spawns fine (drop-in says
  `MemorySwapMax=infinity`; no landing gate).

- **DF-BUNKER-68 (P2) — anonymous re-spawn after a failed renew leaves the
  agent destroyed with no id-bound recovery hint.** renew = destroy + respawn;
  when the re-spawn leg fails (see 67), the agent is GONE (destroy already
  ran) and the error names no pre-renewal archive path. The docs' archive
  step (docs/renewal.md §The full recipe, step 0) is what stands between an
  operator and total data loss here, and the CLI does not print it on failure.

- **DF-BUNKER-69 (P2) — renew shipped only at HEAD while release 0.1.4
  (509fc42) rejects the subcommand with bare `unknown command "renew"`** —
  no "requires a newer CLI" hint, and no server-version probe; the operator
  following docs/renewal.md hits a dead end on the shipped release.

- **DF-BUNKER-70 (P2) — the drift-report RPC is missing on the deployed
  daemon, and the CLI degrades it to one warn line** ("drift pre-flight
  unavailable: unimplemented: 404 Not Found") mid-renewal — the safety report
  silently disappears instead of failing the renew or printing a loud
  "report unavailable, check files by hand" recipe step.

## Version-skew matrix (the run's summary table)

| renew leg | fleet daemon 6a6ad20 | HEAD daemon a4e98ce |
|---|---|---|
| stable identity | ✅ same id/uid/home | ✅ (drift report 2/3 hits, file:line) |
| destroy gate | ❌ absent — destroys live agents | ✅ fires, but remedy unsatisfiable (66) |
| drift report | ⚠️ 404 → one warn line (70) | ✅ |
| client key rotation | ❌ SSH surface dead (65) | ❌ same (65) |
| standard-preset respawn | n/a (no gate) | ❌ landing gate (67) |

## Integration notes (for the next agent)

- Scratch-daemon pattern (run 19's) works but needs: `server.grpc_addr` /
  `server.rest_addr` (not `listen:`), `agent.base_data_dir` +
  `agent.ssh_dir` + `agent.port_range_*` overrides for non-root paths,
  `auth.enabled/mode/token` (not `auth.master_token`), and
  `BUNKER_ALLOW_TLS_INSECURE=1` per-invocation for the plaintext client
  (`tls_insecure: true` alone is refused by the ack gate — good).
- Never scratch-spawn a HEAD daemon on the CONTROL host here: one local
  docker container already runs as uid 1001 (DF-BUNKER-63's precondition).
- `bunker run --detach` survived the session (PPID 1, verified via root ssh)
  but its systemd unit shows `failed` for pure-sleep workloads.

## Timing

- renew (clean agent, fleet daemon): 39s end-to-end
- renew with re-spawn failure (HEAD): 49s to loud failure
- drift-report-only scan: sub-second on a 4-file home
