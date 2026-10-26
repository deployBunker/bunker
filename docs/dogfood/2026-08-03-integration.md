# Bunker Integration Report — 2026-08-03 (dogfood run)

**Verdict: 🟡 PROMISING-BUT-ROUGH** — the core promise holds (spawn isolated rootless-Docker environments via one CLI), but three documented feature families are broken from a remote client and one validation is silent.

This report is a real-user record: what I did, what worked, what broke, and the exact errors. It is NOT a test report.

---

## 1. The setup (what a real user does)

```bash
# Host: any Linux box with Go 1.24+. Target: a Linux server running bunkerd (root).
git clone <repo> && cd bunker
go build -o bunker ./cmd/bunker          # CLI only; ~18MB, ~10s
go build -o bunkerd ./cmd/bunkerd        # daemon (for the server side)

# Point the CLI at a bunkerd server (auth disabled in this deployment):
bunker connect http://<server-ip>:19090 --name bunker-mvp
bunker status                            # ONLINE, agents, disk
```

**Time-to-first-success: ~4 minutes** (build 10s, connect 1s, spawn 9s). That part is genuinely good — the CLI is discoverable, `--help` output is excellent, output formatting is clean.

## 2. The working core (verified against live server, 78.46.173.180)

```bash
bunker spawn --agent-id myagent --ttl 30m --cpu 1.0 --memory 1073741824
#   → Agent created in ~9s. Port range, SSH key saved to ~/.bunker/keys/, expiry set.
bunker list --status all                  # table with status/disk/created
bunker info myagent                       # limits, ports, expiry
bunker exec myagent whoami                # → bunker-myagent (isolated Linux user)
bunker exec myagent -- docker run --rm alpine echo DOCKER-OK   # → DOCKER-OK
bunker exec myagent -- docker info        # → Server 29.1.3, Storage overlayfs, rootless ✓
bunker metrics                            # server: disk used, containers, per-agent table
bunker metrics myagent                    # agent: limits
bunker heartbeat myagent                  # extends expiry
bunker destroy myagent --force            # removes user, home, ports — verified clean
```

**Resource limits are REALLY enforced** (checked inside the agent):

```
/sys/fs/cgroup/user.slice/user-<uid>.slice/memory.max   → 1073741824  (exactly the --memory 1GB)
/sys/fs/cgroup/user.slice/user-<uid>.slice/cpu.max      → 100000 100000 (100% = 1 core)
/sys/fs/cgroup/user.slice/user-<uid>.slice/pids.max     → 4096
/etc/systemd/system/user-<uid>.slice.d/50-bunker.conf   → CPUQuota/MemoryMax/TasksMax/LimitNOFILE/LimitFSIZE
```

Note: the cgroup files are NOT at `/sys/fs/cgroup/memory.max` inside an exec session — the session lives in `session-*.scope` under the user slice; read the slice's files as above.

**Trust checks that passed:** failed spawns free their port range (next spawn got `10010-10019`, not a collision); no ghost agent records after daemon restart; destroy removes the user + home; unknown-agent exec/info return clean `not_found` connect errors.

## 3. What broke (exact errors, in order of pain)

### 3.1 `bunker env` — every subcommand fails (DOGFOOD-001, P0)

```bash
$ bunker env set myagent FOO=bar
sh: 1: Syntax error: "then" unexpected
bunker: exit code 2

$ bunker env list myagent
-f: 1: [: missing ]

$ bunker env get myagent FOO
<awk usage text>      # gawk invoked with a mangled -f program

$ bunker env unset myagent FOO
-f: 1: [: missing ]
```

**Root cause** (found by reading server source after the docs gave no answer): `internal/server/service.go` `buildAgentExecCommand()` joins `Command + strings.Join(Args, " ")` **unquoted** into the remote command line, then wraps the whole thing in `sh -c '...'` for SSH. The CLI sends `Command: "sh", Args: ["-c", "<snippet>"]`, so the remote line becomes:

```
. /run/bunker/<id>/env 2>/dev/null; env ... sh -c if [ -f '...' ] && grep ...; then ...
```

The remote shell parses `if`/`[` as arguments to the inner `sh -c`, then hits the orphaned `then` → syntax error. **Any `bunker exec <id> -- sh -c '<compound snippet>'` fails the same way**; `&&`-chains and plain commands are fine. Same root cause, two user-facing symptoms.

**Fix direction:** quote the snippet when joining server-side (`sh -c '<snippet>'`), or stop the CLI from self-wrapping and pass args through.

### 3.2 `bunker cp` / `bunker deploy` / `bunker tunnel` — unusable from a remote client (DOGFOOD-002, P1)

```bash
$ bunker cp ./file myagent:/home/bunker-myagent/file
ssh: Could not resolve hostname bunker-mvp: Name or service not known
scp: Connection closed
bunker: scp: exit status 255

$ bunker tunnel myagent
Opening tunnel to myagent on local port 2376. Press Ctrl-C to stop.
Warning: Identity file /etc/bunkerd/ssh/myagent not accessible: Permission denied.
ssh: Could not resolve hostname bunker-mvp: Name or service not known
```

Two compounding bugs: (a) the SSH target host is the server's **self-reported hostname** (`ServerInfo.hostname` = `bunker-mvp`), which only resolves on the server itself — a client must reach the server by IP/URL and there is **no `--ssh-host` override flag**; (b) the tunnel command uses the **server-side** key path `/etc/bunkerd/ssh/<id>` while the client key is at `~/.bunker/keys/<id>` (which spawn correctly saves). The spawn output bundle prints SSHFS/tunnel commands with both problems. These commands work only when run ON the server host — which is why the E2E battery (runs on-server, hostname resolves) never caught it.

### 3.3 Invalid TTL silently accepted (DOGFOOD-003, P1)

```bash
$ bunker spawn --agent-id x --ttl banana     # no error!
Agent created: x ... Expires: 2026-08-03T23:23:29Z   # +6h default TTL
```

The API spec promises `CodeInvalidArgument` for bad TTL format; instead the server falls back to the default. A typo'd `--ttl 6hh` gives you a 6-hour agent you didn't want.

### 3.4 Minor frictions

- **Root requirement undocumented** (DOGFOOD-004): running `bunkerd` as a normal user works for status/list, then the first spawn dies with `useradd: Permission denied. cannot lock /etc/passwd`. README quick start never mentions root.
- **Destroy-unknown error UX** (DOGFOOD-005): `bunker destroy nope` → `userdel: user 'bunker-nope' does not exist` (raw exit 6) instead of clean `not_found`.
- **Exit-code inconsistency** (DOGFOOD-005): CLI prints `bunker: exit code 2` while the process exits 1.
- **Status gaps** (DOGFOOD-006): `bunker status` shows `CPU: 0.0% / Memory: 0 B / Uptime: unknown` even though `bunker metrics` has the data. `bunker version` reports `commit: unknown` (no ldflags injection) — you cannot tell whether a deployed bunkerd matches repo HEAD.

## 4. Local non-root daemon behavior

`bunkerd` starts fine as a non-root user (gRPC up, ServerInfo/list work). Spawn fails at `useradd` — expected, but the failure is clean: the failed spawn leaks nothing (port freed, no ghost records after restart).

## 5. What I'd fix first (1 hour of maintainer time)

1. `buildAgentExecCommand` arg join quoting → unblocks env + compound exec (DOGFOOD-001).
2. SSH host/key-path resolution for cp/deploy/tunnel (DOGFOOD-002).
3. TTL validation (DOGFOOD-003) — 10-minute fix, prevents silent 6h agents.

## 6. Working example to copy (placeholder values)

```bash
# server side (root):
bunkerd --config /etc/bunkerd/config.yaml
# client side:
bunker connect http://<SERVER_IP>:19090 --name prod
bunker spawn --agent-id ci-runner --ttl 2h --cpu 2.0 --memory 4294967296
bunker exec ci-runner -- docker run --rm alpine echo hello
bunker exec ci-runner whoami
bunker metrics ci-runner
bunker destroy ci-runner --force
```

**Status update (2026-08-06, GAP-009):** this "Do NOT use" warning is **resolved** — DOGFOOD-001/002 landed in ticks #192-193 (commits 9896c99..e6879ab, fd282b1) and were live-verified at the time; a fresh remote-client re-verification ran 2026-08-06 (tick #231): `bunker env set/list/get/unset`, `bunker cp`, `bunker deploy`, `bunker tunnel` (docker through forwarded socket) all PASS from kara against bunker-mvp. A follow-up client fix (IdentitiesOnly=yes for ssh/scp/sshfs/tunnel) closed the last remote-client gap found during that re-verification. `bunker env *`, `bunker cp`, `bunker deploy`, `bunker tunnel` ARE safe to use from a remote client.

**Status update (2026-10-26, DF-BUNKER-61): the destroy timeout no longer
misreports a completed destroy.** The §2 teardown line above
(`bunker destroy myagent --force`) was the only thing this run exercised, so it
never met the failure mode a busy agent produces: on 2026-09-25
`bunker destroy bc0e2890 --server bunker-mvp` failed three times with
`destroy agent: deadline_exceeded: context deadline exceeded` while the same
`DestroyAgent` over raw REST answered `200 {"status":"destroyed"}` in seconds
(the agent had just run `make test-short` — a ~960MB home). The CLI was turning
its **own** client deadline into a statement about the daemon, and the operator
could not tell an aborted teardown from a finished one.

What destroy does now:

- **The deadline is size-derived** (DF-BUNKER-81): the daemon archives the
  whole home before `userdel`, so both the daemon's archive budget and the CLI
  deadline scale with the home's measured size, and the command prints
  `home <size>; archiving before delete, deadline <d>` before sending the
  request. A home whose size cannot be resolved falls back to the 12-minute
  floor rather than a fixed literal.
- **A deadline expiry is re-checked, not reported as failure**: destroy
  re-lists the daemon's agents. Agent gone → `Agent <id> destroyed — the daemon
  finished the teardown after the client's <d> deadline`, exit 0. Agent still
  listed → `Agent <id> NOT confirmed destroyed` plus the `bunker info <id>`
  re-check and the `--timeout` hint, exit non-zero. Listing impossible (old
  daemon, permission refusal) → the same NOT-confirmed report naming the
  reason. It never resends the destroy while one may be in flight: a second
  archive + `userdel` on the same home is the opposite of idempotent.
- **`--timeout <duration>`** overrides the derived deadline and skips the size
  probe — for a home whose derivation is too short, or when the probe itself
  failed. Raising the deadline is always safe; lowering it below the daemon's
  archive budget aborts the archive and the daemon refuses the destroy with the
  home retained.

Live verification against `bunker-mvp` (2026-10-26, this tick, using the built
CLI): spawned `df61-live` (1 CPU / 1GB, 15m TTL) and wrote 200MiB of
incompressible payload into its home, then

```
$ bunker destroy df61-live --server bunker-mvp --timeout 3s      # exit 1
Destroying agent df61-live (home size unknown; archiving before delete, deadline 3s)…
Agent df61-live NOT confirmed destroyed: the client deadline (3s) expired and the daemon still lists the agent after 20s of re-checking.
  The teardown may still be running — re-check with `bunker info df61-live` before retrying (destroy is idempotent).
  For a home that needs longer than the derived deadline, raise it: bunker destroy df61-live --timeout 45m
bunker: destroy agent: deadline_exceeded: context deadline exceeded

$ bunker info df61-live --server bunker-mvp                       # truthful: still running
  Status:           running

$ bunker destroy df61-live --server bunker-mvp                    # exit 0, default deadline
Destroying agent df61-live (home size unknown; archiving before delete, deadline 12m0s)…
Agent df61-live destroyed.
```

So the 3s arm reported NOT CONFIRMED and the agent really was still there
(no false "destroyed"), and the default-deadline destroy of the same 200MiB
home completed with **no** `deadline_exceeded` — the defect's exact symptom is
gone. Note the `home size unknown` prefix: on this deployment the size probe did
not resolve a figure for the freshly written home, so the 12m floor applied.
That is exactly the case `--timeout` exists for, and it means a genuinely huge
home on such a server can still need an explicit raise. Unit coverage:
`internal/cli/destroy_deadline_relist_test.go`
(gone → success; still-listed → NOT confirmed; list error → fail-closed after
one attempt; `--timeout` reaching the RPC context; no resent DestroyAgent).

