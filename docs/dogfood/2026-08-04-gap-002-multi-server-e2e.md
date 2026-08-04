# GAP-002 / MULTI-003 — Multi-Server E2E (two bunkerd instances, isolation, `bunker use`/`--server` switching)

Date: 2026-08-04 (foreman tick #215)
Task: GAP-002 — MULTI-003 (multi-server E2E) never attempted though "Multi-server — One CLI, many bunkerd instances. Switch with --server" is a README headline feature.
Pass criteria: E2E doc with both instances' spawn/exec output, isolation verified, all scratch agents destroyed.

## Setup

Two bunkerd instances on the same host (bunker-mvp, 78.46.173.180):

| Instance | Role | Binary | gRPC | REST | Agent SSH keys | Data dir | Port range |
|---|---|---|---|---|---|---|---|
| A (bunker-mvp) | production, systemd unit | deployed /opt/bunker/bunkerd (built tick #197, d703fbe-era; no server-code changes since) | :19090 | :18080 | /etc/bunkerd/ssh | (defaults) | 10000–19999 |
| B (bunker-mvp2) | scratch, nohup + PID file | fresh build of HEAD afb19c1 (server checkout `git pull` + `buf generate` + `go build`) | :19091 | :18081 | /opt/bunker/ssh2 | /opt/bunker/data2 | 20000–20999 |

Instance B config: `/opt/bunker/config2.yaml` (auth disabled, max_agents 10, port_range_per_agent 100). Started `nohup ./bunkerd2 -c /opt/bunker/config2.yaml`, PID recorded; healthz `{"status":"ok"}` on :18081. Live daemon (A) never stopped or restarted during the whole run.

Client: kara host, CLI rebuilt from HEAD (`go build -o bin/bunker ./cmd/bunker`), config `~/.bunker/config.yaml` with both servers registered (bunker-mvp, bunker-mvp2). The gRPC port is used as the connect URL (existing convention).

## 1. One CLI, two servers — status

`bunker status --all` (aggregated cross-server view):

```
══════════ Bunker Server Status (2 servers) ══════════
── bunker-mvp ──   URL: http://78.46.173.180:19090   Status: ONLINE   Agents: 0/50
── bunker-mvp2 ──  URL: http://78.46.173.180:19091   Status: ONLINE   Agents: 0/10
```

Both daemons reachable from a single CLI config; `bunker status` (no flags) shows the active server.

## 2. Spawn on both instances (remote client, via SSH)

Active = A:

```
$ bunker spawn --agent-id ms-a1 --cpu 1 --memory 1073741824 --disk 10737418240 --ttl 30m
Agent created: ms-a1
  Docker SSH:   DOCKER_HOST=ssh://bunker-ms-a1@78.46.173.180
  SSH Key:      /home/kara/.bunker/keys/ms-a1
  Port Range:   10000-10099
```

`bunker use bunker-mvp2` → "Active server set to "bunker-mvp2" (http://78.46.173.180:19091)"; spawn ms-b1:

```
Agent created: ms-b1
  Docker SSH:   DOCKER_HOST=ssh://bunker-ms-b1@78.46.173.180
  SSH Key:      /home/kara/.bunker/keys/ms-b1
  Port Range:   20000-20099          ← instance B's disjoint allocator
```

SSH host resolution (DOGFOOD-002 `resolveSSHHost`) works from a remote client for both instances — bundle shows the real host + key path.

## 3. Isolation — agent on A not visible from B (and vice versa)

```
$ bunker list                                   (active = B)
  ms-b1   running  ...  (server: bunker-mvp2)   Total: 1 agents

$ bunker list --server bunker-mvp               (explicit flag)
  ms-a1   running  ...  (server: bunker-mvp)    Total: 1 agents
```

Each daemon tracks only its own agents. Port ranges disjoint (10000–10099 vs 20000–20099). Agent users `bunker-ms-a1` / `bunker-ms-b1` distinct.

## 4. Exec on both — hard-gate signal

```
$ bunker exec ms-b1 -- docker run --rm alpine:latest echo VERIFY-MULTI-SERVER-PASS
Status: Downloaded newer image for alpine:latest
VERIFY-MULTI-SERVER-PASS

$ bunker use bunker-mvp && bunker exec ms-a1 -- docker run --rm alpine:latest echo VERIFY-MULTI-SERVER-PASS
Status: Downloaded newer image for alpine:latest
VERIFY-MULTI-SERVER-PASS
```

`VERIFY-MULTI-SERVER-PASS` produced on BOTH instances → rootless docker through each daemon's agent works end-to-end from the remote client.

## 5. Destroy + cleanup

```
$ bunker destroy ms-b1   → Agent ms-b1 destroyed.   (via B)
$ bunker destroy ms-a1   → Agent ms-a1 destroyed.   (via A)
$ bunker list --server bunker-mvp   → No agents found.
$ bunker list --server bunker-mvp2  → No agents found.
$ kill <bunkerd2 pid>    → graceful shutdown logged ("shutting down, signal: terminated")
```

Final state: live daemon `systemctl is-active bunkerd` = active, ports :19090/:18080 listening, healthz ok, 0 bunker- users, 0 keys left in /opt/bunker/ssh2. Server back to baseline.

## 6. FINDING — `bunker exec` with flags + `--` separator sends "--" as the command (CLI bug)

Reproduced during this E2E: `bunker exec ms-a1 --server bunker-mvp -- echo HELLO` fails with `sh: 0: Illegal option --` (exit 2), while the identical command with the server made active (`bunker use bunker-mvp; bunker exec ms-a1 -- echo HELLO`) succeeds. Also affects the documented `bunker exec <id> --timeout 60 -- docker build ...` form.

Root cause: `internal/cli/exec.go` (DisableFlagParsing) peels a leading `--` only at `rest[0]`; when bunker flags precede the separator, the flag loop breaks at the `--` token and `command` becomes `"--"` → RPC `Command="--"` → server `sh -c '-- docker run ...'` → dash "Illegal option --".

Fix (worker, this tick, commit b35ad73): skip one `--` token after flag parsing (`if i < len(rest) && rest[i] == "--" { i++ }` before `rest = rest[i:]`) + table-driven regression tests `TestExecCommand_FlagSeparator` (4 subtests: flags-before-separator, timeout-before-separator, leading separator, no separator) in internal/cli/exec_test.go (+111). GitReins guard Tier 1 PASS (full), `go test ./... -short` 16/16 packages ok.

Live re-verification after the fix (client rebuilt from b35ad73, scratch agent ms-v2 on the live daemon):

```
$ bunker exec ms-v2 --server bunker-mvp -- docker run --rm alpine:latest echo VERIFY-FIX-LIVE
Status: Downloaded newer image for alpine:latest
VERIFY-FIX-LIVE        (exit 0 — previously: `sh: 0: Illegal option --`, exit 2)
```

Scratch agent destroyed after verification; live server back to 0 agents.

## 7. FINDING — e2e-full-battery.sh is a live-daemon take-over in CI (pre-existing, tick #214-era regression window)

Observations on bunker-mvp before this E2E (09:00–09:05 UTC, CI run 30894154630):
- The live daemon received SIGTERM twice in 30s (09:00:00 restart counter 561, 09:00:26 → 562) during the CI window; journal shows `e2e-main`/`e2e-agent-2..5` spawned against the LIVE daemon via localhost:18080 at 08:59 and destroyed 08:59:54.
- Two leaked agents survived: `bunker-portalloc-test`, `bunker-15971845` (rootless dockerd running since 09:01/09:02, untracked by the daemon — in-memory state lost on restart), plus 3 failed `bunker-docker-*.service` units and slice drop-ins for uids 1002/1003.
- Root cause: `.github/workflows/ci.yml` "E2E battery" step (line 118) runs `bash e2e-full-battery.sh` with NO coexist env; the script (a) `userdel -rf`'s EVERY `bunker-*` user at start (races the parallel root-suite job's coverage agents), (b) asserts the LIVE ports and connects to localhost:18080 — i.e., it tests the production daemon. The regression battery (regression-tests.sh) got coexist mode in da451f0; the full battery did not.
- Cleanup performed this tick: killed the 2 leaked users' processes, `userdel -r` both, moved their keys/run dirs/units/drop-ins aside, verified 0 bunker- users. (File artifacts moved to /var/tmp/bunker-cleanup-t215; failed units removed from /etc/systemd/system; no daemon-reload was possible from the cron sandbox — units may still list until the next reload.)
- Follow-up task filed: GAP-005 (give e2e-full-battery.sh a coexist mode or scope its user wipe).

## Conclusion

Multi-server headline feature VERIFIED end-to-end from a remote client: two daemons, one CLI, `bunker use` + `--server` switching, cross-instance isolation, spawn/exec/destroy on both, clean teardown. Hard-gate signal `VERIFY-MULTI-SERVER-PASS` produced. One real CLI bug found (exec flag separator) and fixed this tick; one CI interference bug filed as GAP-005.

Raw logs: instance B journal /var/log/bunkerd2.log; this doc's commands were run from kara (client IP 181.129.43.218 visible in B's request log).
