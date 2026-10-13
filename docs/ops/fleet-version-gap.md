# DF-BUNKER-10 — Daemon fleet version gap

**Filed:** 2026-09-16 (dogfood run 11, `docs/dogfood/diagnostics.md` §11) · **Status:** deployment
process added; fleet state re-measured 2026-10-11.

## 1. The gap as originally filed

| Host | Deployed daemon | Repo HEAD | Unshipped at the deployed tag |
|---|---|---|---|
| bunker-las-04 | v0.1.3 | 66d4150+ | durable registry, audit hardening, per-agent `/tmp` isolation (GAP-070/073/075) |
| bunker-las-03 | (daemon unreachable — deadline_exceeded; ssh also timing out) | 66d4150+ | same line |
| bunker-mvp | v0.1.4 | 66d4150+ | same line |
| bunker-las-02 | v0.1.4 | 66d4150+ | same line |

Consequences measured in the dogfood run: `exec` saw the HOST's `/tmp` (README's
"Private /tmp per agent" promised a feature no tagged daemon implemented —
DF-BUNKER-9), audit hardening absent, and behavior differences that read like bugs
were version drift. The repo CLI was already at HEAD while the fleet served a
major-older daemon.

## 2. las-03 unreachability — root cause (documented, closed)

Re-investigated in **QA-BUNKER-34** (`docs/dogfood/2026-09-29-las03-recovery.md`):
the outage was a HOST event, not a daemon defect. Full probe matrix (ssh / ICMP /
REST) showed the host had rebooted ~2026-09-27 evening ("one event ~Sep 26 took
both las hosts" per the tailscale log); after the reboot ssh answered, the daemon
(`bunkerd` pid 2889, `/opt/bunker/bunkerd`) came back active and listening on
:10001. The outage **self-resolved host-side**; no fleet action was needed and the
row was closed verified-and-closed (zero diff).

Residual from that closure: the running build's version was never re-verified
against HEAD — which is exactly what the deploy script below now checks.

## 3. Fleet state re-measured 2026-10-11 (read-only; this fix ships no live change)

Per `tailscale status` from the control host:

| Host | Tailscale state | Interpretation |
|---|---|---|
| bunker-las-03 (100.69.3.13) | offline, last seen 6d ago | down again since ~2026-10-05 — repeat of the same host-reliability pattern; needs operator attention, not a code fix |
| bunker-las-04 (100.95.199.98) | offline, last seen 9d ago | down since ~2026-10-02 |
| bunker-las-02 (100.116.99.35) | online (idle) | reachable — upgrade candidate |
| bunker-las-01 (100.78.165.52) | offline, last seen 6d ago | down |

The las hosts are repeatedly dropping off the tailnet for days at a time. The
outage CLASS is environmental (host power/network/provider), outside what any repo
change can fix — but the version gap is a deploy-process gap, and that is
fixable in the repo.

## 4. Deployment process (the code half of this fix)

New: **`scripts/fleet-upgrade.sh`** — a one-command, verifiable fleet upgrade.

Per host: preflight (ssh + current daemon version read) → build from THIS
checkout at HEAD with the repo's ldflags version stamping (refuses on a dirty
tree or build failure — a fleet upgrade must ship a committed, tested build) →
scp + atomic swap (`.new` over the live binary, old kept as `.old` until
verified) → service restart → verify the RUNNING version reports the built
commit, else VERIFY-FAILED. Unreachable hosts are skipped with a loud per-host
line and a non-zero exit so schedulers notice.

```sh
# plan only
scripts/fleet-upgrade.sh --dry-run
# upgrade the default fleet (bunker3, bunker4 — override with --hosts)
scripts/fleet-upgrade.sh --hosts bunker2
```

Defaults follow the live fleet's layout (`/opt/bunker/bunkerd`, systemd unit
`bunkerd`); every path is overridable via `BUNKER_REMOTE_BIN`,
`BUNKER_REMOTE_UNIT`, `BUNKER_SSH_USER`, `BUNKER_SSH_KEY`.

This complements the existing pieces rather than replacing them:
`install.sh` (fresh-host installs from releases), `scripts/start-bunkerd-remote.sh`
(single-host restart), and the release workflow (`make release-binaries` →
GitHub release assets). The gap was that *no* step upgraded already-deployed
daemons to repo HEAD — install.sh ships release tags, not HEAD.

## 5. Acceptance

1. Version gap documented — §1 above (original) and §3 (re-measured live).
2. Deployment script — `scripts/fleet-upgrade.sh` (dry-run, verify-after,
   refuse-on-dirty-tree).
3. las-03 root cause — §2: host reboot event, self-resolved; current state
   re-offline, environmental.
4. Commit — `fix(ops): address daemon fleet version gap (DF-BUNKER-10)`.
5. `go build ./...` / `go vet ./...` — shell-only change; both run clean (no Go
   source touched).

Related: [fleet-inventory.md](fleet-inventory.md) (GAP-083 — the committed
declaration of which hosts exist, on which ports, at which version floor, and
the `bunker status --all` audit that reconciles the client registry against it).
