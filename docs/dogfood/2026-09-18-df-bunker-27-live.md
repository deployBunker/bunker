# DF-BUNKER-27-LIVE — deployed both-pipe ExecAgent stream, live verification (2026-09-18)

**Row:** DF-BUNKER-27-LIVE (P1) — *"deploy the pushed HEAD to `bunker-mvp` during a
CI-idle window, prove the fixed `ExecAgent` both-pipe stream on the live host 10/10
with no `superfluous response.WriteHeader`, run `e2e-full-battery.sh` to
`VERIFY-PASS`, and prove no orphan docker-sock SSH tunnel remains."*

**Code side:** DF-BUNKER-27 was closed at `bcdfad7` (*"fix(server): serialize
ExecAgent stream writes so both-pipe commands do not corrupt the response"*), with
the mutation-proven regression battery `internal/server/service_exec_stream_test.go`.
This file is the **live** half: the same defect class, measured on the deployed
daemon instead of on a local `httptest` handler.

**Host:** `bunker-mvp` (78.46.173.180). **Date:** 2026-09-18, 20:03–20:15 UTC.
Every command and every line of output below is copied from the run's captured
transcript; nothing here is reconstructed from memory. No token value appears
anywhere in this file.

**Verdict: VERIFY-PASS, all four live acceptance criteria met.**

| # | Criterion | Result |
|---|---|---|
| 1 | Daemon built from current `origin/main`, `healthz` 200 | ✅ checkout/binaries `59853c4` (= `origin/main`), `HEALTHZ 200`, unauthenticated `ServerInfo` 401 |
| 2 | Both-pipe probe 10/10 decodable, valid framing, no `superfluous response.WriteHeader` | ✅ 10/10 (`A`), plus heavy 10/10 (`B`), non-zero-exit 5/5 (`C`), single-stream 5/5 + 5/5 (`D`,`E`); journal `superfluous` count **0** |
| 3 | `e2e-full-battery.sh` exits 0 with literal `VERIFY-PASS` | ✅ exit **0**, `Pass: 119 / Fail: 0 / Notes: 4`, `STATUS: ALL CORE TESTS PASS`, `VERIFY-PASS`, certified binary `MATCH` @ `59853c4` |
| 4 | Zero orphan docker-sock tunnels post-run, residue recorded | ✅ `docker_sock_forward_count=0`, `orphan_forward_count=0` (and the battery's own reap check passed twice) |

---

## 0. Negative control — what makes the 10/10 mean something

A green probe run only proves something if the same probe is **red** on a daemon
that predates the fix. `bunker-las-02` (`http://100.116.99.35:10001`) still runs the
pre-fix build (`0.1.4`, build `cef10fc`), and the *identical* probe file, the
*identical* command and the *identical* expectations were run against it:

```
########## RED CONTROL A'. pre-fix daemon: canonical both-pipe, 10 runs ##########
run  1/10: FAIL        status line is not HTTP/1.1 200
            status_line='\x00\x00\x00\x00\x15{"stderr":"RVJSCg=="}\x00\x00\x00\x00\x15{"stdout":"T1VUCg=="}HTTP/1.1 200 OK' raw_len=310
run  2/10: FAIL        status line is not HTTP/1.1 200
            status_line='\x00\x00\x00\x00\x15{"stderr":"RVJSCg=="}{"stdout":"T1VUCg=="}HTTP/1.1 200 OK' raw_len=321
...
run  6/10: FAIL        chunked body undecodable: invalid literal for int() with base 16: b'tion: close'
run  9/10: FAIL        status line is not HTTP/1.1 200
            status_line='\x00\x00\x00\x00\x15{}std{"stderr":"RVJSCg=="}{"stdout":"T1VUCg=="}HTTP/1.1 200 OK' raw_len=315
RESULT: 0/10 decoded clean
PROBE-FAIL: 0/10
A_PRIME_EXIT=1

########## RED CONTROL D'. pre-fix daemon: stdout-only (single stream), 5 runs ##########
run  1/5: DECODED OK  frames=3 status_lines=1 stdout=4B(sha256:ef5683848bd3) stderr=0B(sha256:e3b0c44298fc) exit=0 exit_frame=False
...
RESULT: 5/5 decoded clean
PROBE-PASS: 5/5
D_PRIME_EXIT=0
CONTROL_START_UTC=2026-09-18T20:06:33Z
CONTROL_END_UTC=2026-09-18T20:06:41Z
```

`0/10` both-pipe, `5/5` stdout-only, **same client, same command shapes, same
second** — the probe discriminates the defect from healthy streaming, and the
corruption it captures on the old build is byte-for-byte the signature
`docs/dogfood/2026-09-18-integration-rest-streaming.md` §4.2 recorded (a second
`HTTP/1.1 200 OK` status line inside the body, interleaved/duplicated envelope
frames, header text wedged into the chunk-size position).

---

## 1. CI-idle gate (checked before the host was touched)

```
$ gh run list --repo deployBunker/bunker --limit 5 --json status,conclusion,databaseId,headBranch,createdAt
[{"conclusion":"success","createdAt":"2026-09-18T19:43:58Z","databaseId":35387569965,"headBranch":"main","status":"completed"},
 {"conclusion":"success","createdAt":"2026-09-18T18:35:55Z","databaseId":35381115046,"headBranch":"main","status":"completed"},
 {"conclusion":"success","createdAt":"2026-09-18T18:28:38Z","databaseId":35380398326,"headBranch":"main","status":"completed"},
 {"conclusion":"success","createdAt":"2026-09-18T18:27:07Z","databaseId":35380252032,"headBranch":"main","status":"completed"},
 {"conclusion":"success","createdAt":"2026-09-18T18:07:24Z","databaseId":35378318677,"headBranch":"main","status":"completed"}]
```

Run `35387569965` — the one the foreman flagged as `in_progress` — had finished
(`completed / success`); no run was `in_progress`/`queued` at 20:03Z, and the
deploy, the probe matrix and the battery all ran inside that window. Re-checked
immediately before the battery (20:06Z): still idle.

---

## 2. Before — host state at 2026-09-18T20:03:04Z

```
hostname: bunker-mvp
--- /opt/bunker checkout ---
HEAD: 27e06f0ab8dd813ad787c3f57c48af6b8e91edd1
origin/main: 27e06f0ab8dd813ad787c3f57c48af6b8e91edd1
--- deployed binaries ---
opt/bunkerd:      commit: 27e06f0   built: 2026-09-18T17:56:04Z
opt/bunker:       commit: f3bc048   built: 2026-09-18T17:58:48Z
usr-local/bunkerd: commit: a570197  built: 2026-09-18T08:08:23Z
usr-local/bunker:  commit: a570197  built: 2026-09-18T08:08:23Z
--- systemd ---
active
MainPID=756164
NRestarts=0
ExecMainStartTimestamp=Fri 2026-09-18 17:56:09 UTC
/proc/<MainPID>/exe -> /opt/bunker/bunkerd
--- healthz ---
healthz=200
--- docker containers ---
docker_ps_count=0
--- docker-sock forwards (-L <port>:...docker.sock) ---
forward_count=0
--- ALL sshd sessions ---
sshd_count=0        # NOTE: this counter is `grep -c '^/usr/sbin/sshd'` and does not match the
                    # `sshd: /usr/sbin/sshd -D [listener] ...` argv form, so it reads 0 on a host
                    # whose listener is healthy; the load-bearing number here is forward_count=0
--- bunker-* system users ---
bunker_users=0
--- /home/bunker-* dirs ---
bunker_homes=5
--- linger entries ---
linger_entries=7
  bunker-0f4d1805  bunker-2c05d847  bunker-2d8bb740  bunker-6dd64410
  bunker-c307cebb  bunker-imgspec-none  root
--- ssh keys ---
etc_bunkerd_ssh_keys=1   (tick453-beta)
--- dockerd processes (agent rootless) ---
agent_dockerd_count=0
```

So: three different builds were live on the host before this tick (checkout
`27e06f0`, `/opt/bunker` CLI `f3bc048`, `/usr/local/bin` `a570197`), **none of them
post-dating the `bcdfad7` fix** — i.e. nothing on the host had ever run the fixed
`ExecAgent`. 0 agents, 0 containers, 0 forwards.

---

## 3. Deploy — `/opt/bunker` reset to `origin/main`, rebuilt, reinstalled, restarted

```
$ ssh bunker-mvp
$ export PATH=/usr/local/go/bin:$PATH
$ cd /opt/bunker
$ git fetch origin -q && git reset --hard origin/main -q
CHECKOUT 59853c4216b0e9016cee085a5ebe8b31e1bd2065 2026-09-18T14:43:50-05:00 chore(foreman): tick 475 - GAP-079 CLOSED code side ...
$ which go; go version
/usr/local/go/bin/go
go version go1.26.5 linux/amd64
$ make build
go build -ldflags "... Version=0.1.4 ... Commit=59853c4 ... BuildDate=2026-09-18T20:03:12Z" -o ./bunkerd ./cmd/bunkerd
go build -ldflags "... Commit=59853c4 ... BuildDate=2026-09-18T20:03:12Z" -o ./bunker ./cmd/bunker
$ make install
install -m 0755 ./bunkerd /usr/local/bin/bunkerd
install -m 0755 ./bunker  /usr/local/bin/bunker
$ ./bunkerd --version    → bunkerd 0.1.4  commit: 59853c4  built: 2026-09-18T20:03:15Z  go1.26.5
$ ./bunker  version      → bunker  0.1.4  commit: 59853c4  built: 2026-09-18T20:03:15Z  go1.26.5
$ /usr/local/bin/bunkerd --version → 59853c4   (same)
$ /usr/local/bin/bunker  version   → 59853c4   (same)
$ systemctl restart bunkerd && sleep 4
active
MainPID=989712
NRestarts=0
ExecMainStartTimestamp=Fri 2026-09-18 20:03:18 UTC
HEALTHZ 200
SERVERINFO_UNAUTH 401        # auth still enforced without a token
```

`/opt/bunker` (daemon + CLI), `/usr/local/bin` (both) and the running process now
all report **`59853c4`** = `origin/main` = repo HEAD. `/usr/local/bin/bunkerd` was
installed too even though `ExecStart` points at `/opt/bunker/bunkerd`, because
`e2e-full-battery.sh` defaults `BUNKER_BIN=/usr/local/bin/bunker` and certifies
*that* binary against repo HEAD.

---

## 4. The both-pipe probe — 10/10 on the deployed daemon

### 4.1 The probe

A stdlib-only raw-socket client (no HTTP library, so the wire bytes themselves are
the evidence). It opens a **fresh connection per iteration**, sends one
`application/connect+json` envelope, reads to EOF, and then requires **all seven**
of:

1. the response begins with exactly one `HTTP/1.1 200` status line;
2. `HTTP/1.1` occurs **exactly once** in the whole response (a second status line
   inside the body is the corruption signature);
3. the chunked body de-chunks strictly and every envelope
   (`[flags:1][len:4 BE][payload]`) is length-consistent with no trailing bytes;
4. every payload is valid JSON and no `{"error":...}` frame appears;
5. exactly one end-of-stream trailer (`flags=0x02`) exists **and it is the last
   frame**;
6. decoded stdout and decoded stderr each equal the expected bytes (both pipes,
   complete, in order);
7. the exit-code frame matches (protojson omits a zero `exitCode`, so absence
   means 0; a non-zero code must appear as `{"exitCode":N}`).

The command under test is the one the row names — `sh -c 'echo OUT; echo ERR 1>&2;
exit 0'` — i.e. a process writing to **both** pipes. The probe is scratch tooling;
its source is byte-identical on this host and on `bunker-mvp`
(`sha256 2571bf12f9bb35514304ec11ddd68ea66aceb20068857896860bb0d496e48496`) and
the matrix it produced is what is quoted below. The in-repo equivalent of the same
checks (same seven properties, driven through the real connect handler) is
`internal/server/service_exec_stream_test.go`.

### 4.2 The 10/10 (canonical both-pipe)

```
$ ssh bunker-mvp   # BUNKER_TOKEN read from /etc/bunkerd/config.yaml, value never printed
$ python3 /tmp/dfb27/probe.py http://127.0.0.1:18080 dfb27-probe \
    --cmd 'echo OUT; echo ERR 1>&2; exit 0' \
    --expect-stdout $'OUT\n' --expect-stderr $'ERR\n' --expect-exit 0 --runs 10
== DF-BUNKER-27-LIVE — deployed 59853c4 both-pipe probe ==
daemon       : 127.0.0.1:18080
agent        : dfb27-probe
command      : sh -c "echo OUT; echo ERR 1>&2; exit 0"
expected     : stdout=b'OUT\n' stderr=b'ERR\n' exit=0
expected sha : stdout=ef5683848bd3(4B) stderr=2f04b3a77625(4B)
token        : <not printed>

run  1/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.539s)
run  2/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.554s)
run  3/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.552s)
run  4/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.548s)
run  5/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.547s)
run  6/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.533s)
run  7/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.515s)
run  8/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.494s)
run  9/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.519s)
run 10/10: DECODED OK  frames=4 status_lines=1 stdout=b'OUT\n' stderr=b'ERR\n' exit=0 exit_frame=False (0.514s)

RESULT: 10/10 decoded clean
PROBE-PASS: 10/10
PROBE_EXIT=0
PROBE_START_UTC=2026-09-18T20:04:44Z
PROBE_END_UTC=2026-09-18T20:04:49Z
```

`frames=4` = one stdout frame + one stderr frame + the exit frame (`{}`, exit 0) +
the trailer. Both pipes arrived intact in every run.

### 4.3 The rest of the matrix (widest race window + exit-code + single-stream controls)

```
MATRIX_START_UTC=2026-09-18T20:06:05Z
########## A. canonical both-pipe (echo OUT; echo ERR 1>&2; exit 0), 10 runs ##########
run  1/10: DECODED OK  frames=4 status_lines=1 stdout=4B(sha256:ef5683848bd3) stderr=4B(sha256:2f04b3a77625) exit=0 exit_frame=False (0.576s)
... (runs 2–10 identical: 10/10 clean)
RESULT: 10/10 decoded clean      A_EXIT=0

########## B. heavy interleaved both-pipe (100 OUT + 100 ERR lines), 10 runs ##########
expected sha : stdout=add0c51ee69c(692B) stderr=88090ffc43ef(692B)
run  1/10: DECODED OK  frames=15 status_lines=1 stdout=692B(sha256:add0c51ee69c) stderr=692B(sha256:88090ffc43ef) exit=0
run  2/10: DECODED OK  frames=4  status_lines=1 stdout=692B(sha256:add0c51ee69c) stderr=692B(sha256:88090ffc43ef) exit=0
run  3/10: DECODED OK  frames=6  ...  run  4/10: frames=6  ...  run  5/10: frames=10 ...
run  6/10: frames=10 ...  run  7/10: frames=8  ...  run  8/10: frames=5  ...
run  9/10: frames=8  ...  run 10/10: frames=17 status_lines=1 stdout=692B(sha256:add0c51ee69c) stderr=692B(sha256:88090ffc43ef) exit=0
RESULT: 10/10 decoded clean      B_EXIT=0

########## C. both-pipe with non-zero exit (exit-code frame must carry 3), 5 runs ##########
run  1/5: DECODED OK  frames=4 status_lines=1 stdout=4B(sha256:ef5683848bd3) stderr=4B(sha256:2f04b3a77625) exit=3 exit_frame=True (0.533s)
... (runs 2–5 identical)
RESULT: 5/5 decoded clean        C_EXIT=0

########## D. stdout-only (single-stream control), 5 runs ##########    RESULT: 5/5   D_EXIT=0
########## E. stderr-only (single-stream control), 5 runs ##########    RESULT: 5/5   E_EXIT=0
MATRIX_END_UTC=2026-09-18T20:06:24Z
```

Variant **B** is the load-bearing one for a race: each run makes the child write
100 interleaved lines to each pipe (~200 writes/run, 2000 writes across the matrix),
and the frame counts vary run to run (4 → 17) — i.e. the two sinks really were
chunked differently every time — while the **decoded byte streams stayed identical**
(`add0c51ee69c` / `88090ffc43ef`, 692 B each, 0/10 mismatches). A reintroduced
concurrent `Send` has 2000 chances to interleave here and does not.

Note on variant B's first attempt: it was run once with the expectation passed via
shell `$( )`, which **strips the trailing newline**; the probe then reported
"stdout mismatch" on all 10 runs while the decoded stdout was in fact byte-perfect.
That was a harness bug (fixed by passing the expectations from files with
`--expect-stdout-file`), not a daemon defect — the corrected run is the one quoted
above. It is recorded because a reader diffing transcripts would otherwise see ten
"FAIL" lines in the earlier artifact.

### 4.4 Journal — no `superfluous response.WriteHeader`, no stream-write noise

```
########## JOURNAL: superfluous WriteHeader / stream-write errors in the window ##########
$ journalctl -u bunkerd --since '2026-09-18T20:06:05Z' --no-pager | grep -c -i superfluous
0
$ journalctl -u bunkerd --since '2026-09-18T20:06:05Z' --no-pager \
    | grep -iE 'superfluous|panic|broken pipe|interleav|concurrent' | wc -l
0
$ journalctl -u bunkerd --since '2026-09-18T20:06:05Z' --no-pager | wc -l
35
--- journal, window, non-empty lines (verbatim) ---
Sep 18 20:06:06 bunker-mvp bunkerd[989712]: ... [bunker-mvp/MRlSp7eQFX-000039] "POST http://127.0.0.1:18080/bunker.v1.Bunkerd/ExecAgent HTTP/1.1" from 127.0.0.1:49964 - 200 66B in 573.995769ms
Sep 18 20:06:06 bunker-mvp bunkerd[989712]: ... [bunker-mvp/MRlSp7eQFX-000040] "POST ... ExecAgent ..." from 127.0.0.1:47600 - 200 66B in 511.092158ms
... (35 lines total; every one is a normal 200 access-log line for ExecAgent, one distinct
     request id and source port per iteration — the 40/66/78B bodies are the response
     envelope sizes for the tiny commands, 1898–2152B for the 200-line interleaved runs)
```

The counter is taken over the **same window** the matrix ran in, and the daemon
that produced those lines is the one restarted at 20:03:18Z (PID 989712) — so the
"0" belongs to the deployed `59853c4` build, not to an earlier process. The
pre-fix daemon produced `superfluous response.WriteHeader call from
...middleware.(*basicWriter).Write` under the same test (recorded in
`docs/dogfood/2026-09-18-integration-rest-streaming.md` §4.3):

```
Sep 18 09:41:34 bunker-las-02 bunkerd[269333]: http: superfluous response.WriteHeader call from github.com/go-chi/chi/v5/middleware.(*basicWriter).Write (wrap_writer.go:99)
Sep 18 09:41:35 bunker-las-02 bunkerd[269333]: http: superfluous response.WriteHeader call from github.com/go-chi/chi/v5/middleware.(*basicWriter).WriteHeader (wrap_writer.go:91)
```

---

## 5. `e2e-full-battery.sh` on `bunker-mvp` — VERIFY-PASS

### 5.1 How it was invoked

```
$ bash e2e-full-battery.sh --show-plan          # preview, no side effects, before the real run
  battery CLI binary    : /usr/local/bin/bunker
  battery daemon binary : /usr/local/bin/bunkerd
  certification verdict : MATCH (binary reports 59853c4; repo HEAD 59853c4216b0e9016cee085a5ebe8b31e1bd2065)
  mode                  : standalone — talks to the daemon already on the ports above
  daemon URL (connect)  : http://localhost:18080  [default]
  REST port             : :18080   gRPC port : :19090
  token source          : env (fingerprint sha256:53f24d84; the token itself is never printed)
  battery CLI state dir : /tmp/bunker-battery-cli-WDLzOs  (removed on exit)
  operator CLI config   : /root/.bunker/config.yaml  (never read or written)
PLAN: OK (nothing was changed)
```

Real run (root on the host, `cwd=/opt/bunker`, standalone mode, token read from
`/etc/bunkerd/config.yaml` at runtime, `BUNKER_STRICT_BIN=1` so a binary/HEAD
mismatch would have been fatal **before** any host mutation):

```
=== DF-BUNKER-27-LIVE battery run ===
BATTERY_START_UTC=2026-09-18T20:07:12Z
cwd=/opt/bunker
repo HEAD=59853c4216b0e9016cee085a5ebe8b31e1bd2065
BUNKER_BIN=/usr/local/bin/bunker (default)
BUNKERD_BIN=/usr/local/bin/bunkerd (default)
BUNKER_STRICT_BIN=1
BUNKERD_COEXIST=<unset — standalone>
token=48 chars (value never printed)
```

### 5.2 The verdict

```
=== BINARY CERTIFICATION ===
  ── binary certification (DF-BUNKER-3 / QA-BUNKER-3) ──
  binary under test : /usr/local/bin/bunker
  commit it reports : 59853c4
  repo HEAD         : 59853c4216b0e9016cee085a5ebe8b31e1bd2065
  verdict           : MATCH
  ✓ battery certifies /usr/local/bin/bunker @ 59853c4 == repo HEAD
...
  ✓ tunnel reap check: 0 leftover docker-sock forward(s)      # after section 6a
...
  ✓ tunnel reap check: 0 leftover docker-sock forward(s)      # run-wide, before the verdict
...
==========================================
 RESULTS SUMMARY
==========================================
  ✓ Pass:  119
  ✗ Fail:  0
  ⚠ Notes: 4

  CERTIFIED BINARY: /usr/local/bin/bunker (commit reported: 59853c4; verdict: MATCH)

  STATUS: ALL CORE TESTS PASS
  VERIFY-PASS
  (4 issues documented — see notes above)

BATTERY_EXIT_CODE=0
BATTERY_END_UTC=2026-09-18T20:11:25Z
VERIFY_PASS_LINES=1
VERIFY_FAIL_LINES=0
STATUS_LINE=  STATUS: ALL CORE TESTS PASS
```

Complete transcript: 218 lines, captured at `/tmp/dfb27/battery-transcript.log` on
`bunker-mvp` (the four notes are listed below; nothing else was withheld).

### 5.3 The 4 notes (verbatim, none is a failure)

```
line  70:  ⚠ server metrics — no agent summary
line  91:  ⚠ operator CLI config /root/.bunker/config.yaml untouched (sha256:7e4f8752...bcec)
line 105:  ⚠ NOT killing bunkerd processes: systemd manages the running bunkerd (MainPID 989712) — this suite only manages what it creates
line 106:  ⚠ NOT sweeping /run/bunker/* and /etc/bunkerd/ssh/t*: systemd manages the running bunkerd (MainPID 989712) — this suite only manages what it creates
line 118:  ⚠ changed-spec image count probe: '0'
line 184:  ⚠ per-agent cap is 268435456 bytes — exhaustive ENOSPC fill skipped (cap too large for a battery run; kernel-reported size asserted above)
```

The 4 notes counted in the parent tally are lines 70 (`server metrics — no agent
summary`, the agent-summary cell on an empty registry), 91 (the harness asserting
the operator's CLI config is byte-identical before/after), 118 (the changed-spec
image-count probe reporting `'0'`) and 184 (an intentional skip of an exhaustive
disk fill — the kernel-reported cap is asserted instead). Lines 105/106 are printed
by the **nested** suite (section 12) and are its own restraint statements, not part
of this script's tally: the systemd-managed production daemon is left alone and its
agent state is not swept. Section 12 reported `PASS: 33 / FAIL: 0`, and the battery
asserted it exited 0. Section 6a proved the docker tunnel worked (`docker version
through SSH tunnel`) **and** that its own teardown left no forward behind.

---

## 6. Post-run residue / orphan inspection (2026-09-18T20:15:04Z)

```
--- (1) docker-sock SSH forward processes: '-L <port>:...docker.sock' ---
docker_sock_forward_count=0
orphan_forward_count=0

--- (2) all ssh/sshd sessions ---
1019563 2667675 root Ss 00:01 sshd: root@notty
2667675       1 root Ss 6-13:44:31 sshd: /usr/sbin/sshd -D [listener] 0 of 10-100 startups
(no `ssh ... -L` client process at all; a bare `ps | grep ' ssh '` count of 1 was the
 invoking pipeline matching itself — re-run with the pipeline excluded matches nothing)

--- (3) bunkerd service state ---
active ; MainPID=989712 ; NRestarts=0 ; ExecMainStartTimestamp=Fri 2026-09-18 20:03:18 UTC ; healthz=200

--- (4) agents registered ---
agents=0 (totalCount field: None — protojson omits a zero default) ids=[]

--- (5) docker containers / agent dockerd ---
docker_ps_count=0
agent_dockerd_count=0

--- (6) bunker-* system users / homes / keys / linger ---
bunker_users=0
bunker_homes=5   (/home/bunker-gap075-41528, -4213, -44014, -73954, -79054)
linger_entries=7 (bunker-0f4d1805, bunker-2c05d847, bunker-2d8bb740, bunker-6dd64410, bunker-c307cebb, bunker-imgspec-none, root)
etc_bunkerd_ssh_keys=1 (tick453-beta)

--- (7) battery scratch leftovers ---
battery_cli_state_dirs=0            # the battery removed its own state dir
battery_tmp_configs=339             # ALL pre-existing (newest Sep 18 19:45 = the 19:43 CI run); 0 created in the 20:05–20:12 window
bunker_run_dirs=6                   # all Sep 17 (1dd412d0, 2abff2b5, 34ec2821, 42a1b1e1, ff86d69a, tick453-beta)
                                    # only /run/bunker itself carries a 20:11 mtime: the battery's own e2e-main dir was
                                    # created and removed there; no agent run dir was left behind

--- (8) the daemon's OWN residue surface (bunker status) ---
  Residue:  0 orphan users, 5 orphan homes, 0 orphan keys, 5 stale linger entries (0 registered agents)
            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them
  Agents:   0/50
```

**The orphan-tunnel criterion is met: 0 forwards, 0 of them orphaned (ppid 1 or
under an orphaned parent), and no ssh client process for a socket forward exists on
the host.** The scratch probe agent (`dfb27-probe`) was destroyed *before* the
battery, and its home/run dir/key are gone (`bunker_*` users = 0, no
`/home/bunker-dfb27-probe`, no `/etc/bunkerd/ssh/dfb27-probe`), so nothing the
probe phase created survives either.

### 6.1 Two residue observations, recorded honestly

1. **The 5 orphan homes and 6 of the 7 linger entries are pre-existing host state,
   not products of this run.** Their mtimes are 18:14–19:51 (the earlier CI/battery
   runs), the `gap075-*` names identify the GAP-075 test agents, and this run's own
   before-snapshot already recorded `bunker_homes=5 / linger_entries=7`. The
   battery's standalone cleanup sweeps *users*, and these entries have no user
   behind them, so they persist by design; the daemon's own surface now reports
   them (`Residue:` line), which was GAP-080's deliverable.
2. **The daemon counts 5 stale linger entries where the host holds 6 `bunker-*`
   files — and the cause is the durable registry, not the prefix.** Measured live
   against the daemon's own state file:

   ```
   linger_ids: ['0f4d1805', '2c05d847', '2d8bb740', '6dd64410', 'c307cebb', 'imgspec-none']
   registry_ids_count: 248
   known_to_registry: ['imgspec-none']
   count_known: 1   count_stale: 5
   ```

   `internal/agent/inventory.go` counts a linger entry as stale only when
   `!daemonKnowsAgent(id)`, and `/var/lib/bunkerd/agents.jsonl` still holds an entry
   for `imgspec-none`, so it is treated as a known agent with no live process. This
   is a *different* mechanism from the already-filed INT-HOST-004 (which is about
   names that drift from the `bunker-` prefix — all six names here are prefix-clean).
   It is recorded here as an observation with its measurement; filing a row for it is
   the foreman's call, not this worker's (no board files were touched).

---

## 7. Local gates (independent of the live run)

```
$ git diff --check
(clean)

$ gofmt -l internal/ cmd/
(no output — clean)

$ go build ./...
OK

$ go vet ./...
OK

$ go test ./internal/server/ -count=1 -run 'TestExecStream' -v
--- PASS: TestExecStreamBothPipesNotCorrupted (0.18s)
--- PASS: TestExecStreamInterleavedBothPipesNotCorrupted (0.26s)
--- PASS: TestExecStreamBothPipesNonZeroExit (0.03s)
--- PASS: TestExecStreamLargeOutputIsComplete (0.61s)
--- PASS: TestExecStreamSinglePipeStillStreams (0.30s)
    --- PASS: TestExecStreamSinglePipeStillStreams/stdout-only (0.13s)
    --- PASS: TestExecStreamSinglePipeStillStreams/stderr-only (0.11s)
    --- PASS: TestExecStreamSinglePipeStillStreams/no-output (0.06s)
PASS
ok  github.com/deployBunker/bunker/internal/server  1.391s
```

The in-repo suite drives the **real connect handler** with the exec builder seam
replaced by a local `sh -c` (so both pipes are written by a real child and parsed by
a real connect client) and repeats the both-pipe cases `dfb27Iterations = 20` times
— the same contract the live probe checks on the wire.

---

## 8. What this does NOT claim

- The `10/10` is a property of **`59853c4` as deployed on `bunker-mvp`**, not of
  every host. `bunker-las-02` still runs the pre-fix build and still fails the same
  probe `0/10` (§0) — that host must be redeployed on its own tick.
- The probe proves the *stream framing and content* are intact and that the journal
  stayed free of `superfluous response.WriteHeader` **during the measured window**.
  It is not a proof of the absence of any other concurrency window in `ExecAgent`,
  and it does not exercise `RunAgent`, `cp`/`deploy`, or the CLI's own `bunker exec`
  consumer end-to-end (the CLI drives the same RPC and should be affected
  identically; that consequence is expected, not measured here).
- Variant **B** (`frames=4..17`) shows the frame *count* is not deterministic — by
  design, one envelope per pipe write. The probe asserts the **decoded stream**
  (`sha256` of stdout and stderr), not the frame shape, which is the property that
  matters to a client.
- No claim is made about hosts or lanes this row did not touch; no board or
  `.gitreins` file was modified by this worker.

---

## 9. Deployed state at tick end

- `/opt/bunker` checkout: `59853c4` (= `origin/main`); `bunkerd` + `bunker` built
  from it at `2026-09-18T20:03:15Z` and installed to both `/opt/bunker` and
  `/usr/local/bin`.
- `bunkerd` **active**, PID 989712, `NRestarts=0`, started `2026-09-18 20:03:18 UTC`;
  `healthz` 200; unauthenticated `ServerInfo` 401.
- Host agents 0/50, docker containers 0, docker-sock forwards 0, orphan forwards 0.
- CI-idle window used: 20:03–20:11Z, with no `in_progress`/`queued` run on
  `deployBunker/bunker` before the deploy or before the battery.
