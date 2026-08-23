# Bunker Exec-Level Audit (snoopy)

Every shell command a bunker agent runs on a host is now recorded. This
document explains how the audit works, how to map a logged uid to an agent,
and how to query the audit trail with `journalctl`.

Exec-level audit is provisioned by
[`scripts/install-exec-audit.sh`](../scripts/install-exec-audit.sh) (GAP-048):
it installs and configures snoopy on Debian/Ubuntu bunker hosts. The host
`karaHermes` uses the same mechanism (snoopy via `LD_PRELOAD`), so the query
patterns below are uniform across the fleet.

## 1. What snoopy is and what bunker gets from it

[snoopy](https://github.com/a2o/snoopy) is a syslog-based **execve() logger**.
It ships as a shared library that is injected into *every* process on the host
via `/etc/ld.so.preload` (an `LD_PRELOAD` mechanism applied system-wide).
Whenever any process calls `execve()` — i.e. runs *any* command — snoopy
intercepts the call and emits one syslog line describing it before the new
program starts.

The bunker message format (`/etc/snoopy.ini`) records four fields per exec:

| Field | Datasource | Meaning |
|-------|-----------|---------|
| uid   | `%{uid}`   | Real user id that executed the command |
| euid  | `%{euid}`  | Effective user id (e.g. after setuid) |
| cwd   | `%{cwd}`   | Current working directory at exec time |
| cmd   | `%{cmdline}` | Full command line, with arguments |

> Note: `%{cmdline}` is the snoopy 2.x datasource name for the full command
> line — the 1.x name `%{cmd}` was renamed upstream. The provision script
> always writes the 2.x form.

On a systemd host (all bunker hosts), syslog lines land in **journald**, so
every agent command becomes queryable with `journalctl -t snoopy`, tagged,
timestamped, and retained per the journal's own rotation policy. Nothing
records this at the bunkerd level today — snoopy is the host-level audit
layer that fills the gap: even commands that bypass bunkerd's own logging
(plain shells, `bunker exec`, background jobs) are captured.

Because snoopy intercepts `execve()` at the kernel/userspace boundary, it
captures the command *before* it runs — including commands that later fail
or exit non-zero. This makes it a complete execution record, not a success
log.

## 2. Correlating a uid to an agent id

Bunker agents are plain Linux users named `bunker-<agent-id>` (see
`internal/agent/run.go` and the root-suite conventions in
`scripts/root-suite.sh`). Each agent's home is `/home/bunker-<agent-id>`.

To map a uid found in the audit log to an agent:

```bash
# 1. resolve the uid to a username
getent passwd 1001
# -> bunker-abc123:x:1001:1001::/home/bunker-abc123:/bin/bash

# 2. strip the bunker- prefix to get the agent id
#    bunker-abc123  ->  agent id "abc123"
```

The mapping rules:

- A username that starts with `bunker-` is an agent: strip the prefix and the
  remainder is the agent id (`bunker-abc123` → agent `abc123`).
- The agent's home directory is `/home/bunker-<agent-id>` — so `cwd`
  values starting with `/home/bunker-` identify the agent context directly.
- The euid field distinguishes setuid execution: a command run by root
  (`euid=0`) *on behalf of* an agent shows the agent's uid with `euid=0`.
- Non-agent users (root, system users) do not carry the `bunker-` prefix and
  can be excluded from agent queries with a simple grep.

## 3. journalctl query patterns

Snoopy logs under the syslog ident `snoopy`, so the tag filter is `-t snoopy`.
All examples below work on any systemd bunker host.

**All commands in the last 10 minutes:**

```bash
journalctl -t snoopy --since "10 min ago"
```

**Commands run by a specific agent (by uid):**

```bash
AGENT_UID=$(getent passwd bunker-abc123 | cut -d: -f3)
journalctl -t snoopy --since today | grep " uid=$AGENT_UID "
```

**Commands run from an agent's home directory (cwd):**

```bash
journalctl -t snoopy | grep " cwd=/home/bunker-abc123 "
```

**All agent activity (any bunker- user, excludes root/system noise):**

```bash
journalctl -t snoopy --since "1 hour ago" | grep -E " cwd=/home/bunker- "
```

**Windowed query with the marker pattern:**

```bash
journalctl -t snoopy --since "10 min ago" | grep AUDIT-EXEC-MARKER
```

**Stream live:**

```bash
journalctl -t snoopy -f
```

Expected output shape for a marker exec (format
`uid=<uid> euid=<euid> cwd=<cwd> cmd=<cmdline>`):

```
Aug 23 14:05:11 bunker-mvp snoopy[12345]: uid=1001 euid=1001 cwd=/home/bunker-abc123 cmd=echo AUDIT-EXEC-MARKER
```

If the `message_format` in `/etc/snoopy.ini` is customized on a host, the
field labels may differ — grep for the marker text and the agent's home path
regardless of label style.

## 4. Verification walkthrough

This is the GAP-048 pass procedure. Run on the target host (`bunker-mvp`):

```bash
# 1. provision (first run — installs and configures)
./scripts/install-exec-audit.sh

# 2. re-run to prove idempotency (must print the no-op message, exit 0)
./scripts/install-exec-audit.sh
# -> snoopy already installed and configured — nothing to do (idempotent no-op).

# 3. spawn an agent and run a marker command through it
bunker spawn my-agent
bunker exec my-agent echo AUDIT-EXEC-MARKER

# 4. find the marker in the audit trail with the agent's uid and cwd
AGENT_UID=$(getent passwd bunker-my-agent | cut -d: -f3)
journalctl -t snoopy --since "10 min ago" | grep AUDIT-EXEC-MARKER
# expect: ... uid=$AGENT_UID euid=$AGENT_UID cwd=/home/bunker-my-agent cmd=echo AUDIT-EXEC-MARKER
```

Pass criteria: the journal line contains the exact cmdline
(`echo AUDIT-EXEC-MARKER`), the agent's uid in `uid=`/`euid=`, and
`cwd=/home/bunker-my-agent`.

## 5. Ops notes

**Idempotent re-run.** The provision script is safe to run any time: it
short-circuits when the package is installed, the `.so` line is present in
`/etc/ld.so.preload`, and `/etc/snoopy.ini` carries the managed marker.
Re-runs make zero config changes and exit 0.

**Uninstall.** To disable exec audit:

```bash
# remove the snoopy line from the preload file
sed -i '\#libsnoopy\.so#d' /etc/ld.so.preload
# then remove the package
apt-get purge -y snoopy
```

`/etc/snoopy.ini` (and any `.bak-<timestamp>` backups made by the installer)
can be removed manually if no longer wanted.

**Privacy and volume.** Snoopy logs *every* command on the host — agent
commands and root/system activity alike — which is a significant journal
volume increase on busy hosts. journald rate limits apply (default
`RateLimitIntervalSec=30s`, `RateLimitBurst=10000`); very noisy hosts may
drop audit lines, so raise the limits in
`/etc/systemd/journald.conf.d/` if markers go missing under load. Treat the
audit trail as sensitive: it contains full command lines from all users.
Retention is governed by the journal's `SystemMaxUse` setting — size-capped
journals rotate audit data out; archive or forward it if long-term retention
is required.
