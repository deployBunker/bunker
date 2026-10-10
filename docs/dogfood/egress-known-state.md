# Egress enforcement — known state (2026-10-09)

**STATUS: allowlist/none modes are NOT usable as of this note.** Two blockers,
see board rows DF-BUNKER-86 (P0, install bug) and DF-BUNKER-87 (P1, old-daemon
silent ignore). `--egress-mode open` (the default) is unaffected and pinned by
live probe.

## Why the unit tests did not catch the P0

The egress firewall tests fake or capture the command runner, so the argv
shape is never validated against real nft. The defect is in the ARGV, not the
logic: `installArgvs` (internal/egress/manager.go:250-252) appends
`NFTHookDeclFile()` — the declaration STRING — as the `nft -f` filename. Real
nft then tries to open a file literally named `table ip bunker_egress...`.
Rule-level argvs (nftInstallArgvs, internal/egress/firewall.go:335-350) are
built correctly; only the one-shot hook declaration is broken.

Right way, when it is fixed: the declaration must travel as a FILE (temp file,
0600, removed after) or via stdin (`nft -f -`), and at least one integration
test should execute the real argvs against `nft -c -f` (check mode) or assert
argv paths exist and are readable.

## Version-gating trap (bigger than the /tmp one the README warns about)

Daemons built before fd974b51 (2026-10-07) accept `--egress-mode` and do
nothing. The CLI's `status` /tmp-policy warning pattern shows the daemon CAN
report capabilities — egress needs the same treatment (DF-BUNKER-87). Until
then: check `bunkerd version` commit date ≥ 2026-10-07 before trusting any
egress flag, and verify with `nft list table ip bunker_egress` after every
enforced spawn.

## Non-root daemon on a shared host

`bunkerd` refuses to start as non-root when `/var/lib/bunkerd/secrets/jwt_secret`
exists (a root daemon ran before) and is unreadable — and the error's suggested
fix (chmod the file) is impossible for you. Override the state root instead:

```yaml
agent:
  base_data_dir: /home/you/bunkerd-state
  registry:
    path: /home/you/bunkerd-state/agents.jsonl
```

Also expect: a JWT secret auto-generated into `<base_data_dir>/secrets/`
(harmless INFO), and an audit-log WARN pointing at /var/log/bunkerd/audit.log
you cannot write — point `--path`-style config at a writable location or
ignore (audit is disabled, not broken). Binding non-loopback plaintext is
refused by design: use `127.0.0.1` addresses or configure TLS.

## Verification recipe for any enforced spawn

```bash
uid=$(id -u bunker-<agent>)
nft list chain ip bunker_egress "bunker-egress-$uid"   # drop/accept rules + counters
# from inside the agent: non-allowlisted destination must hang/drop,
# allowlisted one must answer.
```
