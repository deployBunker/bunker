# DF-BUNKER-77-LIVE — live gate for the image-exec fix (`07fa266`) on `bunker-mvp`: **2 of 4 conjuncts are RED** (2026-09-29)

**Row:** `DF-BUNKER-77-LIVE` (P0) — *"prove the image-exec fix (commit `07fa266`) on a real
image-spec agent running a daemon at HEAD `07fa266+`."* Code side is closed
(`DF-BUNKER-77-R2` tier-2 verdict `7baf6ca2`); this row is the LIVE half only.

**Status of the row: left `pending`.** This worker ran the live gate and commits the
evidence; closing the row (and the tier-2 verdict on this document) is the foreman's call.

**Code side:** `07fa266` (*"fix(DF-BUNKER-77): keep image exec in the agent runtime"*,
2026-09-26 23:57:32 -0500) is an ancestor of the deployed HEAD (186 commits behind it).

**Host:** `bunker-mvp` (78.46.173.180). **Live window used:** 2026-09-29 **04:47Z**
(build stamped) → **04:58Z** (last host command). **The CI runner was running its own
`e2e-full-battery.sh` + nested regression suite on this host for the WHOLE window**
(`bash e2e-full-battery.sh` pid 1832936 alive at 04:45Z and still alive at 04:58Z), which
is what drove the one step of this brief that was deliberately NOT performed — see §4.

**Deployed identity:** `/opt/bunker` **and** `/usr/local/bin` (both binaries) rebuilt from
worktree HEAD `45d7d448d1de37f605ec69685a54d96985d1f0b3`; all four paths report
`commit: 45d7d44`; the battery's certification prints **`MATCH`** (exit 0) — §3.

**Verdict: the live gate FAILS 2 of the 4 conjuncts.** The fix does deliver its core claim
(§11: an image-spec agent's `exec` now really runs inside its image — `rg` from the spec and
the stock `git` are both reachable, uid non-zero), but **`bunker env set` (A) and
`bunker exec … docker info` (C) are red on an image-spec agent**, while the *same two
commands are green on a plain agent on the same daemon/build* (§7). The root cause is
mechanical and measured (§8): the exec container runs in the agent's rootless **user
namespace**, where the agent is namespace uid **0** — and the fix passes the **host** uid
(`--user 1071`), an unmapped id that owns nothing. The agent's own home is consequently
**not writable** from its exec context (§9), which is broader than the two failing conjuncts.

| # | Brief criterion | Result | Evidence |
|---|---|---|---|
| 0 | HEAD contains `07fa266`; record the exact sha | ✅ HEAD `45d7d448d1de37f605ec69685a54d96985d1f0b3`; `07fa266f845547ed55ed2da4ee3bb38587e99cd4` is an ancestor (186 commits behind) | §1 |
| 0b | build from HEAD, deploy, **bin-report MATCH** (daemon version string == built commit) | ✅ `bin-report` verdict **`MATCH`**, `bin_report_rc=0`; deployed + running-daemon identity recorded | §2, §3, §5 |
| a | `bunker env set <id> K=V` → rc 0 | ❌ **rc=1** — `sh: 1: cannot create /run/bunker/df77live1/env: Permission denied` | §6 |
| b | `bunker exec <id> -- id -u` → agent uid, **not 0** | ✅ `1071` (rc=0) — with the caveat in §8/§9 that 1071 is a *namespace* id | §6 |
| c | `bunker exec <id> -- docker info` → rootless **29.x** | ❌ **rc=1** — client 29.1.3 present, `failed to connect to the docker API at unix:///var/run/docker.sock … no such file or directory`; even with `DOCKER_HOST` supplied by hand: `permission denied while trying to connect to the docker API at unix:///run/bunker/df77live1/docker.sock` | §6, §8 |
| d | agent-tools probe → **git PRESENT** (real agent userland) | ✅ rc=0 — `git 2.43.0 present`; **plus** `rg 14.1.0 present` (the image spec's own package), i.e. the image userland is genuinely in use | §6 |
| e | destroy the scratch agent; leave the daemon healthy (0 unexpected users, no leaked containers) | ✅ agent + user + home + key gone; **users 9→9, containers 1→1**, my daemon's ports free, production daemon `active` uptime `1d 8h 47m` (never restarted) | §10 |

Evidence in one line: **the deploy and the certification are green; the fix's core claim is
green; the two docker/env conjuncts are red on an image-spec agent and green on a plain one.**

---

## 1. HEAD verification

```
$ git -C /home/kara/worktrees/bunker-DF-BUNKER-77-LIVE rev-parse HEAD
45d7d448d1de37f605ec69685a54d96985d1f0b3
$ git log -1 --format='%H %ad %s' --date=iso
45d7d448d1de37f605ec69685a54d96985d1f0b3 2026-09-28 22:44:20 -0500 dogfood: bunker run 23 — release-channel install surface SHIPPABLE; BUNKER-INST-001/002 + PERF-012

$ git merge-base --is-ancestor 07fa266 HEAD && echo ANCESTOR
ANCESTOR
$ git log -1 --format='%H%n%ad%n%s' 07fa266
07fa266f845547ed55ed2da4ee3bb38587e99cd4
Sat Sep 26 23:57:32 2026 -0500
fix(DF-BUNKER-77): keep image exec in the agent runtime
$ git rev-list --count 07fa266..HEAD
186
```

The worktree HEAD **is** `origin/main` (`git branch -r --contains 45d7d44` → `origin/main`),
so the deploy below could fetch the exact commit on the host without pushing this branch.

## 2. Build + deploy

```
$ make build                     # worktree HEAD 45d7d44
$ ./bunker version               $ ./bunkerd version
bunker 0.1.4                     bunkerd 0.1.4
  commit:     45d7d44              commit:     45d7d44
  built:      2026-09-29T04:47:31Z built:      2026-09-29T04:47:31Z
  go version: go1.26.5             caps:       isolation-grant
  platform:   linux/amd64          go version: go1.26.5
$ sha256sum bunker bunkerd
9308291120aafce3e8933ceb7891c4201c81e0fa4030b7650075a1c0067fbb35  bunker
85d7927c6828d8740d6c94614d9eecdd28c71917e59e298e60827898145805eb  bunkerd
```

Both binaries were `scp`'d to `bunker-mvp:/root/df77-deploy/` and the sha256s **matched on the
host byte for byte** before anything was installed. Deploy transcript (`01-…`, script
`scripts/deploy-df77.sh`) — deploying **did not restart the daemon** (§4):

```
===== BEFORE: host census =====
-- bunker-* users: 8
-- docker running containers: 1
-- production daemon pid: 2412904
-- production daemon exe: /opt/bunker/bunkerd
-- production daemon started: Sun Sep 27 20:11:04 2026
-- registry lines: 4250
-- /home bunker-* dirs: 967
-- /opt/bunker HEAD: 16fff6d6b80a6b45c27bb66f6dfbf262cc8b8625
-- /opt/bunker/bunkerd version: a365eef
-- /usr/local/bin/bunkerd version: 509fc42          <-- drifted from /opt/bunker (a365eef)
-- tenants: 7 agents (server: mvp-live), all destroy-refused:*

===== STEP 1: back up the deployed binaries (repo convention: .bak-<sha>-<ts>) =====
  backed up /opt/bunker/bunker -> /opt/bunker/bunker.bak-a365eef-20260929-045024
  backed up /opt/bunker/bunkerd -> /opt/bunker/bunkerd.bak-a365eef-20260929-045024
  backed up /usr/local/bin/bunker -> /usr/local/bin/bunker.bak-a365eef-20260929-045024
  backed up /usr/local/bin/bunkerd -> /usr/local/bin/bunkerd.bak-a365eef-20260929-045024

===== STEP 2: install the HEAD build (both binaries, both locations) =====
  installed /opt/bunker/{bunker,bunkerd}  sha256 85d7927c6828d874
  installed /usr/local/bin/{bunker,bunkerd}  sha256 85d7927c6828d874

===== STEP 3: update the /opt/bunker checkout to the deployed commit =====
Updating 16fff6d6..45d7d448
Fast-forward
-- /opt/bunker HEAD now: 45d7d448d1de37f605ec69685a54d96985d1f0b3
-- /opt/bunker branch:    main
```

Two deploy observations worth carrying forward:

* **`/usr/local/bin` had drifted** from `/opt/bunker`: the daemon and the flag-named CLI were
  `a365eef`, but `/usr/local/bin/bunkerd` was `509fc42` — a certification run that resolves the
  CLI from `/usr/local/bin` (its default) before this deploy was grading a **third** build.
* `16fff6d` was **397 commits** behind `origin/main`; the fast-forward is clean (the checkout
  carries only untracked `*.bak-*` binaries, which the ff-merge leaves alone).

## 3. `bin-report` certification (AGENTS.md live gate)

```
$ cd /opt/bunker && bash e2e-full-battery.sh --bin-report
=== BINARY CERTIFICATION ===
  ── binary certification (DF-BUNKER-3 / QA-BUNKER-3) ──
  binary under test : /usr/local/bin/bunker
  commit it reports : 45d7d44
  repo HEAD         : 45d7d448d1de37f605ec69685a54d96985d1f0b3
  verdict           : MATCH
  ✓ battery certifies /usr/local/bin/bunker @ 45d7d44 == repo HEAD
bin_report_rc=0
```

## 4. Why the production `bunkerd` was **not** restarted (the one withheld step)

The brief asks (step 3–4) to deploy and **restart `bunkerd`**, while also saying *"prefer a
deploy that does NOT clobber state … preserve existing agents."* On this host during this
window those two instructions conflict, and this section is the measurement behind choosing
preservation. **The restart was withheld deliberately; the deployed binary on disk is at HEAD
(§2/§3) and the live proof was taken on an isolated daemon started from that same binary (§5).**

The daemon reconciles the host against its durable registry at startup
(`internal/agent/reconcile.go`). Three facts, all measured on the host:

1. **The orphan walk's predicate is "no LIVE registry record", not "unknown to the registry".**
   `reconcile.go`: `if m.registry.Get(sa.AgentID) != nil || handled[sa.AgentID] { continue }` —
   `Get` is the replayed *live* record. The *residue* plane reported by `bunker status` uses a
   different predicate (`daemonKnowsAgent` = `tracker.Get` **or** `registry.Known`), which is
   why the status line reads `0 orphan users` while the same users are still orphan candidates
   for the walk.
2. **The CI battery's agents carry this daemon's own instance id.** The battery's daemon runs
   with the *default* `base_data_dir` (its generated config overrides `registry.enabled: false`
   and `port_range: 30000-30999` but **not** `base_data_dir`), so it shares
   `/var/lib/bunkerd/instance`:

   ```
   $ cat /var/lib/bunkerd/instance
   240748fc931ceaf44b387dd4309ed1cf
   $ head -1 /home/bunker-e2e-agent-5/.bunker/owner     # a live CI-battery agent (04:45Z)
   240748fc931ceaf44b387dd4309ed1cf
   ```
   `orphanIsForeign` therefore returns **false** for them (marker == my instance), so they fall
   through to the mode branch — and mode is `destroy`.
3. **The sweep guard does not trip for this shape.** `sweepRefusal` needs
   `registryIsUnproven()` (replayed live set EMPTY, or the registry file created this boot) —
   false here, because the registry replayed **7 live tenants**. The count limit only applies
   *inside* that condition, so `len(orphans) <= limit(3)` is not even checked.

Live census during the window (samples at 04:49Z and 04:58Z): the CI `e2e-full-battery.sh` and
its nested `regression-tests.sh` daemon were **both running** (`bash e2e-full-battery.sh` pid
1832936 alive the whole window; `bunkerd -c /tmp/bunkerd-regression-*.yaml` seen at 04:49Z), and
transient agent users appear and disappear as sections run (`bunker-*` count sampled at
11, 9, 8 during the window; `bunker-e2e-agent-2..5` present at 04:45Z and gone by 04:49Z).

The same plane moved *during* the run: the production status read `0 orphan users` at the
start of the window and `1 orphan user, 960 orphan homes, 1 orphan key` at the transcript-pinned
final snapshot 05:03:16Z (`09-final-host-state.log`) — that residue is NOT this run's (the same
snapshot reads `df77-live users: 0`), and the `bunker-*` total was sampled at 8, 9, 11, 9 and 13
across the window as CI sections created and reaped agents. That is the live measurement of the
hazard: a restart at a random instant in this window finds a non-zero, continuously changing set
of destroyable agents, and the sweep guard's count check does not apply to them (fact 3 above).

**Conclusion:** a `systemctl restart bunkerd` at any point in this window would have archived
and `userdel`'d whatever CI-runner agents existed at that instant (and could not have been
"un-destroyed"). Restarting the production demo daemon is also a change of what production
runs, which a *failed* gate is not the right moment to slip in. Both facts are recorded here so
the owner/foreman can decide: **the on-disk deploy is done; the running-daemon swap is not.**

## 5. The daemon that actually ran the live gate

An **isolated (coexist) daemon** was started from the deployed HEAD binary — the isolation
shape the repo's own battery uses in `BUNKERD_COEXIST` mode (own ports, `auth.enabled: false`,
`tls.insecure_dev: true`), plus its own `base_data_dir` and `image_spec.cache_dir`, so it could
not open production state at all. Its generated config is `scripts/up.sh`; verbatim startup:

```
{"msg":"daemon instance identity","instance_id":"5bcde30c2ddb4bd15483a2c9e1974576"}
{"msg":"agent registry disabled — agent state will not survive a restart"}
{"msg":"agent reconciliation sweep guard armed","guard":"orphan_sweep_guard","enabled":true,
 "mode":"destroy","unproven_orphan_limit":3,…}
{"msg":"registry reconcile skipped — durable registry unavailable","reason":"registry disabled by configuration"}
{"msg":"bunkerd REST listening","addr":":28083","tls":false}
{"msg":"bunkerd gRPC listening","addr":":29093","tls":false}

---- daemon binary identity (running process) ----
exe: /opt/bunker/bunkerd
85d7927c6828d8740d6c94614d9eecdd28c71917e59e298e60827898145805eb  <running bunkerd>
deployed: bunkerd 0.1.4 / commit: 45d7d44 / built 2026-09-29T04:47:31Z
```

So the **running process's binary hash equals the deployed build's hash** — the live evidence
below was produced by a daemon whose `version` string is `45d7d44`, i.e. the "daemon version
string equals your built commit" half of step 4 is satisfied for the daemon that ran the gate.
The daemon's own process-level view (grepped for `destroy|orphan|adopt|foreign` on its log)
contains **no** orphan action of any kind: the walk never ran.

The image-spec agent was spawned through the real spec plumbing (`scripts/up.sh` writes
`{"packages":[{"manager":"apt","packages":["ripgrep"]}]}`):

```
===== spawn image-spec agent (spec: apt ripgrep) =====
Tue Sep 29 04:51:30 AM UTC 2026
Creating agent...
{"msg":"dockerd ready","user":"bunker-df77live1","sock":"/run/bunker/df77live1/docker.sock"}
{"msg":"spawn entering stage","stage":"image-build"}
{"msg":"customized image ready","image":"bunkerd-imagespec-a056ec2a0b57:latest"}
{"msg":"spawn entering stage","stage":"session-probe"}
{"msg":"session probe succeeded","attempts":1}
{"msg":"agent spawned successfully","agent_id":"df77live1"}      (total 1m30s)
  Agent ID   Status   Disk Used   Created
  df77live1  running  880.9 MB    2026-09-29T04:53:00Z      Port Range: 32000-32099
```

## 6. The four conjuncts — verbatim

Full transcript `03-four-live-conjuncts.log`.

```
===== CONJUNCT A: bunker env set <id> K=V -> rc 0 =====
sh: 1: cannot create /run/bunker/df77live1/env: Permission denied
bunker: exit code 2
conjunct_a_rc=1
--- read it back ---
awk: cannot open "/run/bunker/df77live1/env" (No such file or directory)
env_get_rc=0

===== CONJUNCT B: bunker exec <id> -- id -u (must NOT be 0) =====
1071
conjunct_b_rc=0
--- also: whoami / uname (context) ---
uid=1071 gid=0(root) groups=0(root)
Linux 3be154c17071 6.8.0-117-generic #117-Ubuntu SMP PREEMPT_DYNAMIC … x86_64 GNU/Linux

===== CONJUNCT C: bunker exec <id> -- docker info (rootless 29.x) =====
failed to connect to the docker API at unix:///var/run/docker.sock; check if the path is correct
and if the daemon is running: dial unix /var/run/docker.sock: connect: no such file or directory
Client:
 Version:    29.1.3
 …
Server:
conjunct_c_rc=1
--- docker info: rootless / server version lines ---
                                        <-- EMPTY: no Server section was ever printed

===== CONJUNCT D: agent-tools probe -> git PRESENT (real agent userland) =====
Agent tool dependencies (df77live1)
  TOOL     STATUS   REQUIRED   VERSION / NEEDED BY
  toolsd   absent   yes        needed by read, write, …
  rg       present  yes        ripgrep 14.1.0
  git      present  yes        git version 2.43.0
  jq       present  no         jq-1.7
  gopls    absent   no         needed by lsp check for Go
conjunct_d_rc=0

===== SPEC-SPECIFIC BONUS: the image's own package + stock userland INSIDE the exec context =====
--- which rg / rg --version ---
/usr/bin/rg
ripgrep 14.1.0
--- which git / docker / python3 ---
/usr/bin/git          git version 2.43.0
/usr/bin/docker       Docker version 29.1.3, build 29.1.3-0ubuntu3~24.04.2
/usr/bin/python3      Python 3.12.3
```

**Reading:** conjunct **A FAILS**, **B PASSES**, **C FAILS**, **D PASSES**. Note that the two
failures are *not* "the command did not run" — the commands ran, inside the image, as the
agent's uid; they are refused by filesystem/socket ownership (§8).

## 7. The discriminating control — the same probes on a PLAIN agent

Same daemon, same build, same host, same four commands, on an agent spawned **without** an
image spec (`df77plain1`, uid 1074) — transcript `05-control-plain-agent.log`:

```
--- plain agent: id -u ---
1074
--- plain agent: container env has DOCKER_HOST? ---
DOCKER_HOST=unix:///run/bunker/df77plain1/docker.sock
TMPDIR=/tmp
--- plain agent: docker info (the SAME command that fails on the image agent) ---
 Server Version: 29.1.3
 Storage Driver: overlayfs
 Cgroup Driver: systemd
 Cgroup Version: 2
 Security Options:
  rootless
plain_docker_info_rc=0
--- plain agent: env set (the SAME command that fails on the image agent) ---
plain_env_set_rc=0
ok
plain_env_get_rc=0
--- plain agent: agent-tools (expect rg ABSENT: rg comes only from the image spec) ---
  rg       absent   yes        needed by search (content, files-only, count)
  git      present  yes        git version 2.43.0
  jq       absent   no         needed by optional: scripted result handling
```

This is the control that makes the finding attributable: **the daemon, the host, the rootless
docker socket and the `env set` plumbing all work** (docker reports `rootless`, `overlayfs`,
server 29.1.3; `env set` returns 0). The two red conjuncts are specific to the **image-exec
path** — i.e. to the code path `07fa266` changed. It also independently confirms the image
path is really being used: `rg` is present **only** on the image agent.

## 8. Mechanism — why A and C fail (measured)

Inside the image agent's exec context, the environment carries **none** of the injected vars
(the outer `env PATH=… DOCKER_HOST=… TMPDIR=…` in
`buildAgentImageExecCommand` applies to the `docker run` **CLI on the host side**, not to the
container — the run argv carries no `-e`; `containerRunPrefix` emits only `docker run --rm`
plus the disclosure marker when enabled):

```
===== PROBE 1: container env =====
HOME=/
HOSTNAME=fecabf3e9f24
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
PWD=/home/bunker-df77live1
--- uid --- 1071        --- hostname --- fecabf3e9f24      <-- a container, not the host
```

The **decisive** measurement (`06-uid-map-mechanism.log`) is the namespace map and the
ownership of the bind-mounted agent paths *as seen inside the container*:

```
--- effective ids ---          1071 / 0
--- /proc/self/uid_map ---         0       1071          1
                                   1     296608      65536
--- /proc/self/gid_map ---         0       1071          1
                                   1     296608      65536
--- inside-container stats (uid:gid mode path) ---
0:65534 755 /run/bunker/df77live1
0:110   660 /run/bunker/df77live1/docker.sock
0:0     750 /home/bunker-df77live1
0:0     755 /home/bunker-df77live1/bin
--- our process ---
Uid: 1071 1071 1071 1071     Gid: 0 0 0 0     Groups: 0
```

and the same paths from the host context (`08-host-context-ownership.log`, production agent
`54c8bf55`, uid 1069):

```
1069:0    755 /run/bunker/54c8bf55
1069:1069 750 /home/bunker-54c8bf55
1069:1069 755 /home/bunker-54c8bf55/bin
getent passwd bunker-54c8bf55 → bunker-54c8bf55:x:1069:1069::/home/bunker-54c8bf55:/bin/bash
```

Put together (host uid printed = agent's uid, e.g. 1071 for `df77live1`):

| path | host owner | inside container | who the process is |
|---|---|---|---|
| agent home `/home/bunker-<id>` | `<uid>:<uid>` 750 | `0:0` 750 | ns uid **1071**, ns gid 0 |
| runtime dir `/run/bunker/<id>` | `<uid>:root` 755 | `0:65534` 755 | ns uid 1071 |
| the rootless `docker.sock` | `<uid>:<gid>` 660 | `0:110` 660 | ns uid 1071, groups `0` |

**Proven here:** the exec container runs in the agent's rootless user namespace with
`uid_map 0→<host agent uid>`; the fix passes the **host** uid to `--user`, which is *not*
namespace 0 but the mapped-unprivileged id 1071 (host ≈297678). Every path the agent owns is
therefore **owned by "someone else"** from the container process's point of view, so:

* the runtime dir is not writable → `env set`'s write of `/run/bunker/<id>/env` fails EACCES
  (**A**), and
* the socket is `0660 root:gid110` while the process is neither → even with the variable
  supplied by hand the connect is refused:

```
===== PROBE 4: same docker info, but with DOCKER_HOST supplied explicitly =====
Client:  Version: 29.1.3 …
Server:
permission denied while trying to connect to the docker API at unix:///run/bunker/df77live1/docker.sock
```

**Inferred (labelled, not measured):** an exec that ran as namespace uid 0 (or as the
container's default user mapped through this userns) would own those paths and reach the
socket; and injecting `-e DOCKER_HOST=unix:///run/bunker/<id>/docker.sock` is *necessary but
not sufficient* — PROBE 4 shows the variable alone still fails on ownership. Fixing only the
variable would leave C red.

**Why the change's own test did not catch this:** `internal/server/exec_image_test.go` asserts
`strings.Contains(remote, "DOCKER_HOST=unix:///run/bunker/<id>/docker.sock")`. The shipped code
does contain that substring — in the outer `env … docker run …` prefix, where it reaches the
docker **CLI**, never the container. The assertion is satisfied by the broken shape, so the unit
lane is green on a shape the live lane rejects.

## 9. Additional live finding — the agent's own home is read-only from its exec context

Not one of the four conjuncts, but it is the same root cause and it is a live usability break
for **any** image-spec agent, so it is recorded here:

```
--- can we write the agent HOME? ---
touch: cannot touch '/home/bunker-df77live1/.writeprobe_df77': Permission denied
HOME-NOT-WRITABLE
```

Reads work (the group bits allow `r-x`), writes do not. The code comment for
`buildAgentImageExecCommand` states the home "is bind-mounted at the SAME absolute path and used
as the working directory, so paths that are valid in the host context … stay valid" — that
contract holds for reads and fails for writes.

## 10. Host hygiene / teardown

Transcript `07-teardown.log`, plus a final production check:

```
===== destroy scratch agent =====
Agent df77live1 destroyed.
Removed local SSH key /root/df77-deploy/live/cli-home/keys/df77live1
destroy_rc=0
===== stop the isolated daemon =====
stopped pid 1849254 (running=no)
-- bunker-* users AFTER: 9        (9 before)
-- bunker-df77live1 user present? no
-- docker containers AFTER: 1     (1 before)
-- any container referencing df77live:   (none)
-- my isolated daemon listening ports (must be empty):  (none)
-- agent home dir: removed/absent

=== PRODUCTION daemon health (operator config, explicit alias) ===
── mvp-live ──   Status: ONLINE   Version: 0.1.4   Uptime: 1d 8h 47m   Agents: 7/50
                 Residue: 0 orphan users, 959 orphan homes, 0 orphan keys, 718 stale linger entries
=== production tenants ===   Total: 7 agents (server: mvp-live)
=== host census ===  bunker- users: 9   docker containers: 1
=== CI still active? ===  1832936 bash e2e-full-battery.sh
```

Production daemon pid **2412904** (`active`) still runs its original inode
(`sha256 09934ce4b9e12bd2…`) while the on-disk binary is the HEAD build
(`85d7927c6828d874…`) — i.e. **the daemon was never restarted** (§4), and the host census
returns to its pre-run numbers with no user, home, key, container or port left behind.

## 11. What the fix does deliver (the part that is green)

* an image-spec agent's `exec` **really runs inside its image**: `/usr/bin/rg` (present only
  because the spec added it) and the stock `/usr/bin/git`, `/usr/bin/docker`, `/usr/bin/python3`
  are reachable — the pre-fix signature (`rg` present but **no** git/docker, i.e. the image
  *replacing* the agent userland) is gone;
* the exec runs as a **non-root** uid (`id -u` → 1071), the DF-BUNKER-77 claim that image exec
  stops silently executing as container root;
* the image build, cache keying and spawn path are healthy (1m30s spawn, spec image
  `bunkerd-imagespec-a056ec2a0b57:latest`, session probe green first attempt);
* `env set`, `docker info` and the agent home's write path are unaffected for **plain** agents.

## 12. Follow-ups required before this row can go green

Fix direction (this doc does not implement it — the row is the live half only):

1. **Resolve the container user in the container's own namespace.** `agentUserFlag` supplies the
   *host* uid; inside the agent's rootless userns the agent is ns uid 0 (`uid_map: 0 <host-agent-uid> 1`).
   Either pass the namespace-resolved id or run as the image's default user, and prove it by
   writing `/run/bunker/<id>/env` from an image-exec context.
2. **Inject the agent environment into the container, not only into the docker CLI** — at minimum
   `-e DOCKER_HOST=unix:///run/bunker/<id>/docker.sock`, and decide explicitly what happens to the
   vars in `/run/bunker/<id>/env` on the image path (today they are sourced on the host side only).
3. **Re-check the whole write contract of the agent home** from an image-exec context (§9), not
   just the runtime dir.
4. **Make the live shape assertable in the unit lane**: the current image-exec test is satisfied
   by a `DOCKER_HOST` that never reaches the container (§8, last paragraph). A test that runs the
   recorded argv against a fixture whose "container" is a second namespace (or that asserts the
   container argv explicitly) would have failed here.
5. **File the live-gate red as its own board row** (`DF-BUNKER-77-LIVE` companion), with these two
   conjunct results and the control as its acceptance criteria.

## 13. Evidence index / reproduction

| file | what it is |
|---|---|
| `01-deploy-and-bin-report.log` | host census before/after, backups, install, `/opt/bunker` ff-merge, `bin-report MATCH` |
| `02-up-isolated-daemon-spawn.log` | isolated daemon startup (identity, registry disabled), CLI connect, image-spec spawn |
| `03-four-live-conjuncts.log` | **the four conjuncts, verbatim** |
| `04-conjunct-ac-diagnostics.log` | container env, runtime-dir/socket visibility, write tests, `DOCKER_HOST`-supplied `docker info` |
| `05-control-plain-agent.log` | the discriminating control (plain agent: docker rootless 29.1.3, env set rc 0, rg absent) |
| `06-uid-map-mechanism.log` | `/proc/self/{uid,gid}_map`, in-container ownership, home write test |
| `07-teardown.log` | destroy, daemon stop, post-teardown census |
| `08-host-context-ownership.log` | host-side ownership of the same path shapes |
| `09-final-host-state.log` | transcript-pinned final host snapshot (05:03:16Z): my run's residue is 0, deployed on-disk build = `45d7d44`, production daemon `active` since Sep 27 |
| `scripts/*.sh` | the seven scripts that produced the transcripts, in order (`deploy-df77`, `up`, `conjuncts`, `diag`, `control`, `final-probe`, `down`) |

Reproduce: `scp` the two HEAD binaries + `scripts/` to the host, run
`scripts/deploy-df77.sh` (no restart), then `scripts/up.sh`, `scripts/conjuncts.sh`,
`scripts/control.sh`, `scripts/final-probe.sh`, `scripts/down.sh`. The isolated daemon uses
`:28083`/`:29093` and agent ports `32000-32999` so it cannot collide with the CI battery's
`:28081`/`:29091` + `30000-30999` or its nested suite's `:28082`/`:29092`. No token value
appears anywhere in this document or in any committed transcript; the isolated daemon ran
`auth.enabled: false` with a locally invented placeholder, and the operator's CLI config
(`/root/.bunker/config.yaml`) was never read or written by the battery or by these scripts.
