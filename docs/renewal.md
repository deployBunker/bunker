# Renewal Recipe — stable agent identity across renewals (DF-BUNKER-34)

**Status:** v1 (2026-09-24). The recipe this document describes is enforced
by `bunker renew` (requires a build from HEAD): the command REFUSES to run
without a stable agent id, so the anonymous-spawn renewal that caused the
aa189273 → eduos-agent → 2cdce4d0 home-path chain can no longer happen
silently.

## The rule

A renewal = destroy + re-spawn on the same host. The agent id IS the
identity: `/home/bunker-<id>` (the home path), `bunker-<id>` (the system
user), the SSH identity and every stored path (fleet.toml, scheduler.db,
systemd units) derive from it. Therefore:

> **Every renewal MUST carry the same `--agent-id` it renews.**

An anonymous spawn mints a new random id and a new home path every time.
Everything long-lived under the old home — systemd `--user` units
(duckbrain-local.service :3000, sync timers, forward services, docker
units), cron entries, config files — keeps pointing at the old home while
its user no longer exists. On the host where this was measured, five unit
files had to be rewritten by hand and a scheduler daemon kept ticking
against a deleted workdir for 20+ hours.

## The supported command

```bash
bunker renew --agent-id <agent-id> [--ttl 7d] [--server staging]
```

What it does, in order:

1. **Pre-flight drift report** — reads the agent's CURRENT stored home (from
   its sshfs mount) and scans that home's systemd `--user` units, cron
   entries, shell/env files and home-level config files (`*.toml`, `*.json`,
   `*.yaml`, `*.env`, `*.conf`, `*.sh`, `.bashrc`, `.profile`, …) for
   references to the old home path. Every hit is printed with its file, line
   number and content. The scan is REPORT-ONLY: it never rewrites (an
   automatic rewrite of a user's service units is a data mutation the daemon
   has no authority over).
2. **Destroy** — the daemon's destroy gate refuses loudly (`live_processes`)
   if the agent's uid still owns live processes; see the next section.
3. **Re-spawn the SAME id** — the spawn request carries the agent id, so the
   home path, system user and SSH identity are unchanged across the renewal.
   The command verifies the daemon answered with the same id and fails
   loudly if a daemon ever mints a different one.

Without `--agent-id`, renew fails before any RPC with:

```
renewal refused: no --agent-id given — renewals MUST carry the stable agent id
(`bunker renew --agent-id <id>`): spawning anonymously mints a new home path
every renewal and every long-lived service, cron entry and stored path under
the old home goes stale (DF-BUNKER-34; the recipe is docs/renewal.md)
```

## Destroy refuses while the uid still holds processes

`bunker destroy` (and therefore renew) verifies that the agent's uid owns NO
live process before `userdel -rf` runs, reading `/proc/<pid>/status`
directly. A live process — the scheduler daemon, node server or forward
script the agent's operator installed — produces a hard refusal that names
the uid and every process:

```
destroy refused: user bunker-eduos-agent (uid 1002) still owns live processes
that userdel -rf would orphan. 2 live process(es) under uid 1002: pid 1220620:
/home/bunker-eduos-agent/bin/schedulerd ...; pid 477038: python
offbyone_forward.py. Stop those processes on the host (they are NOT killed by
bunker destroy — a previous destroy that orphaned them is exactly the failure
this gate exists to prevent), then retry the destroy
```

Nothing is deleted when this fires. `--force` does NOT bypass the gate: the
historical alternative is the partial state userdel -rf leaves when it fails
on a busy home — user record gone, ~35 uid processes alive for 20+ hours,
one of them holding port 3000 and shadowing the next agent's daemon — while
the destroy reported not_found and the fleet looked healthy.

If userdel itself fails on anything other than an already-gone user, the
destroy now reports it as a hard error (`userdel_failed`) carrying the
surviving-process evidence, instead of the historical silent not_found.

## Orphan detection (the state that was invisible)

An agent whose user record is gone while processes still run under its uid
is now surfaced on `bunker info` and `bunker list`:

```
⚠  eduos-agent: ORPHANED UID: user record bunker-eduos-agent is GONE from the
   host but 3 live process(es) under uid 1002: pid 1220620: schedulerd;
   pid 1189: node duckbrain.js ...
```

The field is non-empty ONLY for the orphan class; healthy, unknown and
not-applicable states all render nothing, so absence never fabricates a
"healthy" verdict. The uid is resolved from the agent home's on-disk
ownership when the user record is gone — the artifact userdel-without-clean-
home leaves.

## The full recipe (operator)

```bash
# 0. Archive what matters BEFORE the renewal (renew does not copy your data).
ssh <host> 'tar czf /root/eduos-pre-renewal-$(date +%s).tgz -C /home/bunker-eduos-agent .'

# 1. Stop long-lived services cleanly (renew reports them; it does not kill them).
ssh <host> 'systemctl --user stop duckbrain-local.service cube-duckbrain-sync.timer off-by-one-forward.service'

# 2. Renew with the STABLE identity.
bunker renew --agent-id eduos-agent --ttl 7d

# 3. Restore the archived data into the (same-path) home and re-enable the
#    services; the pre-flight drift report told you exactly which files
#    carried the old path — the post-renewal files now point at a path that
#    is VALID again (the id did not change).
ssh <host> 'tar xzf /root/eduos-pre-renewal-*.tgz -C /home/bunker-eduos-agent'
```

If you renew with an anonymous `spawn` (not this command), the drift report
is what protects you: run `bunker renew --agent-id <old-id>`'s pre-flight
scan — or check the agent's files by hand — before swapping any path.