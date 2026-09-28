# DF-BUNKER-80 — live A/B evidence: stock agent vs apt package-add image agent

Date: 2026-09-27 (local, UTC-05). Branch: wt/DF-BUNKER-80 (fix commit afde0d6).

## What ran

A REAL spawn pair against a scratch bunkerd built from this branch
(`go build ./cmd/bunkerd` at afde0d6), run LOCALLY with the DF-BUNKER-13
isolation rules:

- loopback REST/gRPC 127.0.0.1:10441/10442 (kara's daemon: 10001/10002; a
  sibling review scratch: 28471/29471 — all disjoint)
- port pool 42000-42999 (kara's: 30000-30999; sibling: 41000-41999)
- every state path under /tmp/df80-scratch (registry jsonl, ssh keys, audit,
  archives, imagespec cache)
- `agent.reconciliation.unproven_orphan_limit: 0` — at boot the sweep guard
  REFUSED to touch the two metadata-less orphan users on this host
  (`refused_orphans: 2`, "NOTHING was destroyed"; daemon log)

Agents: `df80stock` (no spec) and `df80img`
(`{"packages":[{"manager":"apt","packages":["ripgrep","jq"]}]}` — the
dfspec-e-class spec from the row). Both spawned rc=0; the image build ran
through the agent's own rootless daemon (image
`bunkerd-imagespec-d17c07a1a4a5:latest`).

### Host-DNS compensation (probe scaffolding, not a product change)

This host's resolv.conf is Tailscale-managed with MagicDNS
(100.100.100.100) first; MagicDNS SERVFAILs public names here, and the
slirp4netns DNS relay (10.0.2.3) inside an agent's rootless network does
not fail over to the LAN resolver, so agent-side registry pulls die with
`lookup registry-1.docker.io on 10.0.2.3:53: server misbehaving` (the first
image-spec spawn attempt failed exactly there and rolled back — an
environmental failure, not the product defect). To run the live leg
anyway:

- a host-local pull-through registry mirror (registry:2 container on
  192.168.123.147:5000, removed after the run), and
- a per-user `~/.config/docker/daemon.json` (`dns: [192.168.123.1]`,
  the mirror, and its insecure-registries entry) seeded into the image
  agent's home in the useradd→dockerd-start window by a root watcher
  (seeded 139 ms before the daemon's "starting rootless dockerd" log
  line; seed log in /tmp/df80-scratch/seed-watch.log during the run).

Neither touches product code or config; the daemon under test is the
branch build with the fix and nothing else.

## Files

- `DF-BUNKER-80-agent-tools-stock.json` — raw `bunker agent-tools
  df80stock --json` output.
- `DF-BUNKER-80-agent-tools-image.json` — raw `bunker agent-tools
  df80img --json` output.
- `DF-BUNKER-80-image-tools.txt` — `bunker exec df80img` (container
  context): `command -v` for git/docker/python3/make/rg/jq, docker client
  version, git version, whoami.
- `DF-BUNKER-80-image-sockbind.txt` — the agent's own rootless
  `docker.sock` visible in-container at `/run/bunker/df80img/`
  (DF-BUNKER-77's bind) plus the in-container docker client version.
- `DF-BUNKER-80-image-content-red.txt` — BEFORE the fix: docker build of
  the exact rendered Dockerfile for the same spec (FROM ubuntu:24.04 +
  apt ripgrep/jq), in-container tool probe: git/docker/python3/make
  ABSENT, rg/jq present (matches the dogfood run-22 repro).
- `DF-BUNKER-80-image-content-green.txt` — AFTER the fix: same probe on
  the branch's render: git 2.43.0, docker 29.1.3, python3 3.12.3, make
  4.3, rg 14.1.0, jq 1.7 — all present.

## Result (acceptance criterion 2)

| probe row | stock df80stock | image df80img | verdict |
|---|---|---|---|
| toolsd (REQUIRED) | absent | absent | no regression |
| rg (REQUIRED) | present 15.1.0 | present 14.1.0 | spec package delivered |
| git (REQUIRED) | present 2.53.0 | present 2.43.0 | preserved (was ABSENT pre-fix) |
| jq (optional) | present 1.8.1 | present 1.7 | preserved |
| gopls (optional) | absent | absent | no regression |
| missing_required | [toolsd] | [toolsd] | IDENTICAL |

The image agent passes the same agent-tools probe as the stock agent with
NO REQUIRED regressions (missing_required is byte-identical), and the
spec's package (rg) is present. The differing versions across the two
columns (jq 1.8.1 vs 1.7, rg 15.1.0 vs 14.1.0) are themselves proof the
probe classified two DIFFERENT userlands: the host's (stock) vs the
container image's (image agent).

Note: `docker` in-container defaults to /var/run/docker.sock; the agent's
rootless socket is bound at /run/bunker/<id>/docker.sock
(DF-BUNKER-80-image-sockbind.txt) and reaching it needs DOCKER_HOST set
in-container — that env propagation belongs to the image-exec wiring
(DF-BUNKER-77 family), not to this row's image-content scope.

## Cleanup

Both agents destroyed through the CLI, scratch daemon stopped, the mirror
container killed and pruned, /tmp/df80-* scratch dirs removed, and
/etc/skel left as found (the daemon.json seed went only into the agents'
own homes, which `userdel -r` removed; the empty /etc/skel/.config dir
created during an earlier blocked attempt was rmdir'd).

Two honest footnotes from the cleanup itself:

1. **DF-BUNKER-81 is real and reproducible**: the first `bunker destroy
   df80img` died at exactly 30.015 s — the CLI's 30 s context SIGKILLs
   the in-flight `tar` of the ~700 MB rootless home, the fail-closed
   archive gate refused deletion, home retained. The destroy only
   completed after restarting the SCRATCH daemon with
   `destroy_home_policy: purge` (documented opt-out; appropriate for
   disposable probe agents, not a product change).
2. **Orphan sweep on the second boot**: the first daemon boot's sweep
   guard REFUSED to touch the two metadata-less orphan users
   (`refused_orphans: 2`) as designed. After the policy restart the
   scratch registry was PROVEN (live=2 restored), so the guard no longer
   applied and reconcile destroyed those two orphan users. They belonged
   to no daemon (kara's main daemon's ListAgents was `{}`), had empty
   homes (no `.bunker/ports`, no owner marker, no rootless tooling —
   i.e. not restorable agents), and foreign classification requires
   persisted port metadata they did not have. No live agent of any
   daemon was harmed; kara's daemon and the sibling scratch daemon were
   untouched throughout.

