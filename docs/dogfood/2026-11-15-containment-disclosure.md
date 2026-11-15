# Dogfood run 2026-11-15 — containment disclosure, read as a tenant would

Tick `bunker-dogfood-2026-11-15-15-54-49`. Binary built from `67fbe1eb` (the tip under test).
Scratch daemon on `bunker-mvp` at `127.0.0.1:19221` REST / `19222` gRPC, own data, ssh,
registry, audit and archive dirs, `max_agents: 2`, `reconciliation.mode: adopt`,
`unproven_orphan_limit: 0` — the walk could not destroy anything on the host.

Written by the third delivery of this tick from the committed evidence under
`.coding-hermes/evidence/bunker-dogfood-2026-11-15-15-54-49/` (probe script, per-arm
stdout/stderr, `bunkerd.log`, both install ledgers). Every marker count below was re-counted
from those files, not copied from earlier prose.

## Verdict

**PASS with findings.** The disclosure channel does what `internal/server/disclosure.go` says
it does, and it says clearly that it is a signal and not a wall. One agent, 20 arms, no false
positives with the channel off, exact agreement with the allowlist with it on. The parts a
tenant would reasonably assume are covered — `env`, an uploaded script, a piped or
semicolon-joined command, `cat /etc/passwd` — are not covered, by design, and that gap is the
thing to know before this is sold as containment.

## What a real user does

A tenant runs a program inside an agent and asks "am I sandboxed, and what am I sharing with
the other tenants?" The cheapest honest probe is a handful of identity commands. Driven through
`bunker exec` against one agent (`df1115-marker`): 19 arms plus one `--raw` pair and one
`--detach` pair. The retained per-arm outputs are the **disclosure-on** battery (true phase
started 16:21:47Z per `bunkerd.log:122`); the disclosure-off battery reused the same filenames
in the same directory and was overwritten, so the "zero markers with the channel off" result
rests on the earlier fire's report and was **not** re-verifiable from these artifacts. What the
artifacts do prove is that both configurations ran (`bunkerd.log:3` "containment disclosure
disabled (default)", `bunkerd.log:122` "containment disclosure ENABLED") and what each arm
produced with the channel on:

| arm | command | marker | bytes |
|---|---|---|---|
| `uname_shell` | `uname -a` | **yes** | 339 |
| `uname_abs` | `/usr/bin/uname -r` | **yes** | 230 |
| `raw_uname` | `exec --raw … /usr/bin/uname -a` | **yes** | 339 |
| `cat_osrelease` | `cat /etc/os-release` | **yes** | 612 |
| `cat_n_osrelease` | `cat -n /etc/os-release` | **yes** | 703 |
| `cat_nsv_osrelease` | `cat -nsv /etc/os-release` | **yes** | 703 |
| `lsbrelease_a` | `lsb_release -a` | **yes** | 298 |
| `lsbrelease_missing` | `lsb_release` (bare) | **yes** | 212 |
| `shell_env` | `env \| grep -F BUNKER_SANDBOX` | no | 17 |
| `raw_env` | `exec --raw … /usr/bin/env` | no | 564 |
| `script_upload` | `exec --script probe-script.sh` | no | 23 |
| `uname_pipe` | `uname -a \| head -1` | no | 127 |
| `uname_semicolon` | `uname -a; whoami` | no | 148 |
| `cat_passwd` | `cat /etc/passwd` | no | 5109 |
| `df_operand` | `df /etc` | no | 113 |
| `free_badflag` | `free -x` | no | 1153 (stderr) |
| `echo_plain` | `echo hello` | no | 6 |
| `detach_run` | `run --detach … sh -c 'echo … > /tmp/…'` | no | 174 |
| `detach_read` | `cat /tmp/df1115-detach.txt` | no | 0 |

The marker is two lines on stdout, network first:

```
[bunker: network isolation mode shared — host network namespace (no network isolation: shared loopback, global port space) — NOT tenant isolation]
[bunker: managed sandbox environment — containment active]
```

## Why the allowlist looks like that (the daemon's own words)

`disclosure.go:243` builds argv with `probeTokens`, which calls `splitSimple`
(`disclosure.go:258-263`): **any** shell metacharacter or newline anywhere in the command string
returns `nil`, and `nil` means "not a probe". That single rule is why `uname -a | head -1` and
`uname -a; whoami` are unmarked — they are not judged unsafe, they are not judged at all.
`cat` is special-cased (`:176-190`): the last token must be exactly `/etc/os-release` and every
token between must pass `isHarmlessCatFlag` (`:139`), which is why `cat /etc/passwd` and `cat -n
/etc/passwd` are silent while `cat -nsv /etc/os-release` speaks. An absolute path must be exactly
`trusted-dir/base` (`probeBasename`, `:222-240`), so a copy anywhere else is not a probe either —
that arm was never exercised, see below.

Read the above as the contract it is: a cooperative program learns where it runs; a program
that wants to hide appends `| cat` or moves the binary.

## What a tenant should not assume

- `cat /etc/passwd` is unmarked and returns **42 `bunker-*` accounts** on this host with home
  directories and shells. That is the same "NOT tenant isolation" the network banner admits,
  in a form that is trivially easy to read silently.
- `BUNKER_SANDBOX=1` is visible through `env`, through `--raw env`, **and through an uploaded
  `--script`** (`SCRIPT_ENV=1`), while none of those three arms carries the banner. The env var
  is the reliable disclosure channel; the banner is the conditional one.
- `df /etc` answers 113 bytes of host filesystem numbers with no marker — a tenant reading
  `df` learns about other tenants' disks without being told they are shared.

None of this is a new defect: `disclosure.go` documents the allowlist, and the run's rows cover
the observable defects. It is the sentence to keep when someone asks whether a bunker agent is
a sandbox.

## Install leg — the fresh machine does install

Two fires. Fire 1 (default 45 s spawn deadline) never got past `spawn` in 3/3 attempts on
`bunker-las-03` while the host was healthy (bunkerd active, load 2.4, 44 G free) — filed as
`DF-BUNKER-121`, "fresh-machine installability UNPROVEN". Fire 2 re-ran the identical battery
with only the deadline raised (`BUNKER_QA_SPAWN_TIMEOUT=180`) and agent `8eb14067` was created
on the **first** attempt in 27 s, completing the battery.

Ledger (`install/fire2-ledger.jsonl`, sha256 `e581e48f07a6376c017badb595ae1bbba3bdc4bda54cad2c409cd3bbbf87071d`,
re-hashed at harvest time): **27 evidence rows over 17 distinct cells — 14 OK, 4 FAIL, 5 N/A,
4 INFO.** `fresh-install`, `upgrade`, `launch`, `chaos-disconnect` OK. `tests` and `ci-pass`
FAIL on the already-owned `QA-BUNKER-099` classes. `collect` FAILED twice with "cannot reach
agent" before succeeding — the ssh-port resolution bug, `QA-BUNKER-110`. So: a fresh box
installs, runs, and survives a disconnect; the red cells are owned elsewhere.

## Numbers

| what | value | source |
|---|---|---|
| `spawn` | 23.70 s idle, 44.6 s busier, 27 s install leg | run 32 / fire 2 ledger |
| `exec` round-trip warm | 0.43–0.48 s over 8 runs | run 32 |
| `bunker list --status all` | 0.454–0.484 s, median 0.478, mean 0.472, n=5 | `perf/perf-arms.jsonl` |
| `bunker info <agent>` | 0.431–1.299 s, median 0.487, mean 0.639, n=5 (one 1.30 s outlier) | `perf/perf-arms.jsonl` |
| CLI floor (`--version`) | 0.0048–0.0057 s, mean 0.0053, n=5 | `perf/perf-arms.jsonl` |
| `bunker help` (local, no daemon) | 0.0058–0.0072 s, mean 0.0064, n=5 | `perf/perf-arms.jsonl` |

All 20 samples carry `load1` 8.35 → 8.08 on a 16-core host. The read path is flat against load
and sits at ~0.47 s: a daemon round-trip, not CPU, and below the threshold where a user notices.
The one 1.30 s `info` sample is a tail worth knowing about (2.7× the median) but a single sample
is not a finding. Spawn is the only figure a user feels, it is already `PERF-022`, and this
delivery did not file a second perf row for a 0.47 s read.

## Rows this run carries

- `DF-BUNKER-119` (recurrence of `DF-BUNKER-51`) — `--detach` advertises `systemctl status
  <unit>` as the handle and the unit is gone seconds later; no `bunker logs`/`ps` to recover a
  detached run's output or exit code. Confirmed again here: `detach_run.out` prints a Run ID
  and a unit name, `detach_read` returns 0 bytes, so a detached write to `/tmp` is invisible to
  `exec` while the same write under `$HOME` is visible.
- `DF-BUNKER-120` (recurrence of `QA-BUNKER-58`) — a client-deadline cancel is logged as
  "cancelled by the server request timeout (request_timeout, default 300s)" at ~50 s, pointing
  the operator at the wrong knob.
- `DF-BUNKER-121` — the fire-1 install leg was FAILED; closed by fire 2's green leg against the
  same host and battery.
- `PERF-022` — spawn cost with the phase table.
- `QA-BUNKER-110`, `QA-BUNKER-111`, `QA-BUNKER-112`, `QA-BUNKER-099-R2` — harness defects and
  the second-host reproduction of the registry-gate test red.

## What was not done

- No second product walk by the third delivery: the tick was already recorded twice, so this
  pass verified the records against artifacts, harvested the evidence into the repo, and
  closed the documentation leg. Product verdict is unchanged.
- Scratch residue `/root/df1115` (140 M) on `bunker-mvp` was not removed — the cleanup command
  was refused at the execution gate and not retried. `bunker-mvp:/` is 93 % full,
  `bunker-las-03:/` 83 %.
- Agent `3c9c4402` on `bunker-las-03` (running since 16:09:20Z, 248 MB, no URL) belongs to
  another writer and was left alone.

## Evidence index

```
.coding-hermes/evidence/bunker-dogfood-2026-11-15-15-54-49/
  manifest.json                              sha256 + byte size + source path per file
  fire1-containment/remote.sh                the probe driver (setup/start/spawn/arms/teardown)
  fire1-containment/bunkerd.log              daemon log for both phases
  fire1-containment/arms/*.out|*.err         20 arms, per-arm stdout and stderr
  install/fire1-ledger.jsonl                 fire-1 leg (1 row: the 3/3 spawn failure)
  install/fire1-launch.log                   fire-1 launch log
  install/fire2-ledger.jsonl                 27 rows, 17 cells
  install/fire2-launch.log                   fire-2 launch log
  install/fire2-cell-logs/*                  per-cell logs harvested before the destroy trap
  install/fire2-chaos-disconnect.log         disconnect cell log
  install/fire2-spawn-bundle-redacted.txt    spawn bundle with the agent API key redacted
  perf/perf-arms.py, perf/perf-arms.jsonl    the read-path measurement above
```
