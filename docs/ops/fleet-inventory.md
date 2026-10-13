# Fleet inventory — who is in the fleet, on which port, at which floor

**Status:** shipped (GAP-083) · **Declaration:** [`internal/fleet/inventory.yaml`](../../internal/fleet/inventory.yaml)
· **Audit:** `bunker status --all`

## 1. What this is, and why it is committed

The CLI's **server registry** (`bunker connect`) is per-operator state: it lists
whatever that operator has registered on that machine. It is not a statement of
which hosts *exist*. Before this change nothing was, so a host that served
agents but was never registered — or was registered on a port its daemon does
not listen on — was invisible to every status surface.

That is not hypothetical. **bunker-las-01 ran an ACTIVE `bunkerd` for 32+ days
while `bunker status --all` — the documented cross-host overview — omitted it
entirely.** The row exists because the omission survived three stand-in-PM
cycles; the host was only noticed by SSH-ing to it by hand.

`internal/fleet/inventory.yaml` is now the fleet's **contract**: which hosts
exist, which ports each is expected to answer on, and the minimum daemon version
each must run. It is embedded in the CLI (`go:embed`), so a `go install`ed
binary carries it and no deployment step can forget it.

**The declaration is a contract, not a snapshot.** A declared port is the port
the host is *supposed* to answer on. A daemon answering somewhere else is drift
to report — and to fix at the source, or to amend here with a documented
reason — never a reason to relax the check.

## 2. The audit: `bunker status --all`

`bunker status --all` prints a section for the **union** of the local registry
and the declaration, so:

* every registered server keeps its section (nothing regressed — your other
  servers are still reported), and
* every **declared** host gets a section even when the registry does not know
  it, with a loud `Fleet: FAILED` line and an explanation of the impact.

Every declared host also carries a `Fleet:` verdict line — `OK` with the
coverage evidence (endpoint, port, version against the floor) or `FAILED` naming
what is wrong — and the overview ends with a coverage table. The command exits
**non-zero** if any declared host is failing, so a scheduler or CI notices.

| Verdict | Meaning |
|---|---|
| `OK` | registered, reachable, on a declared port, daemon at or above the floor |
| `NOT_REGISTERED` | declared in the inventory, absent from the client registry — **no client can select it by name**. This is the las-01 hole |
| `PORT_MISMATCH` | registered on a port the host is not declared to serve (evaluated *before* reachability: the registration is wrong whether or not the host is up) |
| `UNREACHABLE` | registered on a declared port, but its daemon did not answer |
| `BELOW_FLOOR` | reachable, but the daemon is older than the floor (`0.1.4`, the same `internal/hostsetup.MinDaemonVersion` value `bunker host-provision` enforces — pinned equal by a test) |
| `VERSION_UNPROVEN` | reachable, but the daemon reported no version, so the floor cannot be proved either way |
| `RETIRED` | declared decommissioned: reported as information, never a failure |

A declaration that cannot be parsed (no name, no port, no role, a duplicate, an
out-of-range port) fails at load time: a host that checks nothing would shrink
the audit silently, which is the defect class this row closes.

## 3. The declared fleet

| Host | Address | Role | Declared ports |
|---|---|---|---|
| bunker-las-01 | 100.78.165.52 | agent-host | 10001, 10002 |
| bunker-las-02 | 100.116.99.35 | agent-host | 10001, 10002 |
| bunker-las-03 | 100.69.3.13 | agent-host | 10001, 10002 |
| bunker-las-04 | 100.95.199.98 | agent-host | 10001, 10002 |
| bunker-mvp | 100.115.67.116 (public 78.46.173.180) | demo | 18080, 19090 |
| cube-las-00 | 100.84.195.43 | agent-host | 18080, 19090 |

### Port layouts — three exist in this fleet, and only one is the convention

| Layout | REST | gRPC | Used by |
|---|---|---|---|
| daemon defaults | `:8080` | `:9090` | `config.go` `DefaultConfig`; a bare `bunkerd` on a fresh host |
| **fleet convention** | **`:10001`** | **`:10002`** | the `bunker-las-0x` agent hosts |
| demo layout | `:18080` | `:19090` | `bunker-mvp` and `cube-las-00` |

`specs/api.md` states the demo layout explicitly ("demo-instance ports, not
defaults"). Both listeners of a daemon serve the **same handler** and the same
protocol set (`internal/server/server.go`, the gRPC and REST listeners are both
built from `rootHandler`), so a client may use either port of a *running*
listener; the check is therefore about the **registration matching a port the
host actually serves**, not about which of the two a client prefers.

Not declared, deliberately — the boundary is a decision, written down here so a
reader can see it was made rather than missed:

* `dedi-2` / `dedi-3` — DC probe hosts for the filesystem/latency work
  (`docs/prd/PRD-bunker-fs.md`). `dedi-2` does run a live `bunkerd` (v0.1.4,
  measured 2026-10-13) for that probe work, but it is not a member of the
  bunker agent fleet, so it is not declared. Registering it locally is fine:
  the audit only reconciles DECLARED hosts, and a registered server always keeps
  its own section.
* `hermes-fleet-01..04` — the coding-hermes fleet's remote bunker **agents**
  (hostnames `coding-hermes-remote-bunker-0N`), a different, per-agent
  population administered by that fleet, not bunker daemon hosts.
* `judgebox`, `karahermes-mde-7840hs`, `dfhead` — operator-local boxes (the
  control host's own daemon and a local test instance).

A host belongs here when the fleet expects every operator to be able to select
it by name; add it with a role and its declared ports, or it stays out of the
audit.

## 4. The bunker-las-01 port discrepancy (`:18080` vs `:10001`)

This is acceptance criterion 3 of GAP-083, answered: **the port is documented,
not silently "fixed", because fixing it is a live-host action and the repo's job
is to make the deviation visible and explain it.**

Measured facts:

| When | What was measured |
|---|---|
| 2026-09-18 (stand-in PM, read-only over ssh) | `hostname` = bunker-las-01; `systemctl is-active bunkerd` = active; `/opt/bunker/bunkerd --version` = `bunkerd 0.1.2 / commit ee6a2dc`; `ss -ltn` shows a **single listener `*:18080`** — no `:10001`, no `:19090`; `curl http://100.78.165.52:10001/healthz` = connection refused (000) while `:18080/healthz` = 200 |
| 2026-09-19 (re-probe) | still live on `:18080`; the daemon's own boot log says `bunkerd REST listening addr :18080` |
| 2026-09-19/20 (ops redeploy, crier tick 359) | las-01 upgraded to `bunkerd 0.1.4 / 198f6b4`; the host was then registered in the client config as `bunker-las-01` |
| 2026-10-13 (this change) | the registry entry is `http://100.78.165.52:19090` — a port this host has never listened on; `tailscale` reports the host offline (last seen 8d) |

The audit's first live run against the real registry (same day, raw transcript in
[`docs/evidence/GAP-083-live-inventory-audit.txt`](../evidence/GAP-083-live-inventory-audit.txt))
reported `Fleet inventory coverage (6 declared, 4 failing)` — las-01
`PORT_MISMATCH` (plus unreachable), las-02/03/04 `UNREACHABLE`, bunker-mvp and
cube-las-00 `OK` with their versions — and exited **1**. Before this change the
same run printed no las-01 coverage verdict at all.

So there are **three** different ports associated with one host:

* `:18080` — what its daemon actually serves (the demo layout, on an agent host
  that the convention says should serve `:10001`/`:10002`);
* `:10001` — the fleet convention it is declared against here, and what the
  original row asked it to answer on;
* `:19090` — what its **registry entry** says, which is neither: a port the
  daemon has never opened, so the entry cannot connect even while the host is
  up. This is the concrete registration defect the audit now reports as
  `PORT_MISMATCH` regardless of the host's liveness.

Remedies (any one closes the finding, and the audit will say so):

1. **Normalize the host** — deploy its daemon with the fleet's ports
   (`:10001`/`:10002`) and re-register (`bunker connect bunker-las-01 …`). Then
   the declaration needs no change.
2. **Normalize the registry** — register it on `:18080`, the port it actually
   serves, **and** amend the declaration's `ports` for this host with a
   `port_note` saying why an agent host runs the demo layout. A denial port
   without the note fails `TestCommittedInventoryEveryPortDeviationIsDocumented`.
3. **Retire it** — `retired: true` plus the reason, and remove it from DNS, so
   no client can select it. A retired declaration is reported as information and
   stops being a hole without deleting the record.

### Which registry file is authoritative

The declaration is reconciled against the **resolved** client config, not a
guessed path: `bunker paths` shows the current resolution for this host
(per GAP-181: flag > env > `${XDG_CONFIG_HOME:-~/.config}/bunker` > the legacy
`~/.bunker` **read alias** > system defaults).

This matters for the row's own premise: "absent from the client registry" was
measured by grepping `~/.bunker/config.yaml`, which on the control host is a
**stale legacy alias** — the authoritative file was
`~/.config/bunker/config.yaml`, where las-01 *was* present (on the wrong port).
Read the resolved path, not the historical one; the audit does.

## 5. Changing the declaration

* **Adding a host** — add an entry (name, address, role, ports, optional
  `min_version`, `note`) **and** register it, or the audit reports a
  `NOT_REGISTERED` hole naming it. That is the point: the two must agree.
* **Retiring a host** — `retired: true` + a note; it stops being a hole and the
  record of it stays.
* **Raising the floor** — bump `default_min_daemon_version` (or a host's
  `min_version`). The default is pinned equal to
  `internal/hostsetup.MinDaemonVersion` by a test, so the inventory audit and
  `bunker host-provision`'s skew probe cannot disagree.
* **A layout deviation** — declare it and carry a `port_note`; a deviation
  without a note fails the test above.

## 6. Verification

| Claim | Proof |
|---|---|
| every known bunker host is declared (AC 2 and 4) | `TestCommittedInventoryDeclaresEveryKnownFleetHost` pins the six names literally |
| the las-01 port discrepancy is documented (AC 3) | `TestCommittedInventoryLas01PortDiscrepancyIsDocumented` requires `18080`, `19090` and `10001` in the host's `port_note` and forbids declaring `19090` |
| a declared host is never silently omitted | `TestReconcileReportsEveryDeclaredHost`; `TestStatusCommand_AllServers_FleetDeclaredHostIsAlwaysRendered` (section + loud line + non-zero exit) |
| a hole really exits non-zero, and a covered fleet does not | the same cell plus `TestStatusCommand_AllServers_FleetCoveredFleetExitsZero` |
| the port mismatch is reported even when the host is down | `TestStatusCommand_AllServers_FleetPortMismatch`, `TestReconcile/a wrong port … also down` |
| the floor is the repo's floor | `TestFleetFloorIsTheRepoFloor` |
| the production default really is the embedded inventory | `TestLoadFleetInventoryDefaultLoadsTheCommittedInventory` |

Related: [fleet-version-gap.md](fleet-version-gap.md) (DF-BUNKER-10 — the
deployed-daemon version gap and `scripts/fleet-upgrade.sh`).
