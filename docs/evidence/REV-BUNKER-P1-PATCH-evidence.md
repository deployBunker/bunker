# REV-BUNKER-P1-PATCH — the boot-time orphan sweep refuses an unproven mass destroy

**Row.** Boot-time reconciliation must not bulk-destroy unregistered `bunker-*`
users. The danger was reproduced, not theorised: a scratch NON-ROOT `bunkerd`
with its own config and an EMPTY registry booted on a host running real agents and
ran the full destroy walk on another deployment's users; only the daemon's lack of
privilege saved them. Run as root — the documented deployment — the same walk
archives each home and deletes the user.

Every claim below is a line in one of the transcripts listed in §11, and the
transcripts were all produced by one script (`REV-BUNKER-P1-PATCH-arms.sh`) against
the tree frozen at commit `c3f246f`.

---

## 1. THE DECISION, and the number that changed it

**`mode: destroy` stays the default — behind a new sweep-level guard whose default
threshold is 3.**

Why destroy stays: it is the documented semantic (an unmanaged `bunker-*` user is a
leftover), and the alternative has its own residual — an adopted orphan carries no
TTL (`adoptAgent`), so nothing ever expires it and a host can accumulate agents
nobody asked to keep. The incident in this project's memory is the *missing registry
record* path (`empty registry ⇒ orphan ⇒ destroyed at boot`), and that path is what
the guard now bounds. Adopt mode is untouched by the guard for the plain reason that
adoption deletes nothing.

Why the threshold is 3 and not 5: the first pass derived 5 from the CI side alone —
`e2e-full-battery.sh` section 8 spawns a five-agent burst (`e2e-agent-2..5` beside
`e2e-main`, asserted `5 agents spawned`). Then the **deployment** side was measured,
and it is the same number:

| host | `bunker-*` users | registry | shape |
|---|---|---|---|
| `bunker-mvp` (production), measured while this row was open | **5** (`bunker-718bf3d4`, `-15be6938`, `-a9ebf454`, `-7dfbd5ea`, `-95f3325a`) | `/var/lib/bunkerd/agents.jsonl`, 1 729 856 bytes, mode 0600 | a real deployment the guard must cover |
| legacy H4F host | 7 tenants (project record) | — | a larger deployment |
| this dev host | 2 (`bunker-probeok-18967`, `-probeok-39467`) | none of ours | leftovers |

With the limit at 5, a wiped registry on `bunker-mvp` sweeps **every one of its
agents**: five unknown users, five allowed. A threshold that does not cover the
deployment we can actually point at is not a guard. At 3 the two sides separate:

* **≤ 3 unknown users on an unproven registry still sweep** — the "handful of
  leftovers" arm (`…Arms_SmallUnprovenSweepStillHappens`: 3 in, 3 destroyed).
* **≥ 4 REFUSES**, with the refusal counted and the remedy in the log.

The cost is named rather than hidden: a host carrying four or more genuine leftovers
on an unproven registry is refused and must be re-run with
`unproven_orphan_limit` raised (or `orphan_sweep_guard_disabled: true`). One
greppable config line, named in the refusal itself — against a silent sweep of a
live fleet. The residual is inherent to a threshold and is in §10: a deployment
*smaller* than the limit (a single-tenant host) is not protected by it, which is what
`unproven_orphan_limit: 0` is for.

**This guard converts a documented human convention into an enforced one.**
`docs/dogfood/diagnostics.md` already prescribes the scratch-daemon pattern for
running this daemon on a shared host, and its second half is a warning plus a manual
defence:

> private config in /tmp (own ports, own registry/audit paths, own SSH dir),
> `BUNKER_HOME` pointed at a private CLI home, and reconciliation `adopt` for the
> first restart if foreign `bunker-*` users exist on the host (**defense against
> destroying another lane's agents by mistake**).

Until now that defence was a convention an operator had to remember, on exactly the
hosts where forgetting it is unrecoverable. It is now a refusal in the sweep itself,
in the default mode, with no configuration required to get it.

`REV-BUNKER-P1-PATCH-red.txt` (filed tree) and `-control.txt` (guard neutered) are
the two directions of this number; `-green.txt` pins the boundary with the counters.

## 2. The anchors, verified in source before anything was changed

| anchor | what it actually is | verified |
|---|---|---|
| `internal/agent/reconcile.go:222-283` | the orphan walk: `for _, sa := range list`, foreign check, adopt branch, `destroyOrphan` | yes — nothing above the loop asked whether the pass had any standing |
| `internal/agent/reconcile.go:492-508` | `orphanIsForeign` returns `false` when `.bunker/owner` **and** `.bunker/ports` are missing/unreadable | yes — it fails OPEN toward destroy; this is REV-BUNKER-002's mechanism |
| `internal/config/config.go:871-873` | `Reconciliation: {Mode: ReconcileModeDestroy}` | yes (`DefaultConfig`) |
| `internal/registry/registry.go:240-266` | `os.MkdirAll` + `os.OpenFile(path, O_CREATE\|O_WRONLY\|O_APPEND)` — a MISSING file is fabricated EMPTY | yes — "refuses to start when it cannot open the registry" covers permission errors, not absence |
| `internal/agent/manager_spawn.go:1059-1075` | `writeOwnerMarker` is best-effort (and absent when the daemon has no identity) | yes — users created outside spawn carry no marker at all |
| `internal/agent/reconcile.go:128-135` | a failed system probe aborts the reconcile instead of reading as "no agents" | yes — untouched by this row (§3) |

## 3. What the guard is

New provenance, captured once, before the file can exist:

* `registry.Store.CreatedThisBoot()` — `Open` stats the ACTIVE file before its
  `O_CREATE` and records whether it had to fabricate the durable state. This is the
  difference between *"the durable state says zero agents"* and *"there is no durable
  state"*, and it is announced at boot
  (`agent registry replayed … created_this_boot=true`).

New sweep-level decision (`AgentManager.sweepRefusal`), evaluated **before the first
per-orphan decision**, inside the single closure both entry points
(`Reconcile`, `ReconcileStartup`) reach the walk through:

```
REFUSE  ⇔  guard armed (default)
        ∧ mode == destroy
        ∧ provenance unproven:  replayed live == 0  OR  CreatedThisBoot()
        ∧ len(orphans) > unproven_orphan_limit (default 3)
```

On refusal: **no destroy, no adopt, no registry write** — the walk returns before the
loop, `ReconcileReport.Refused` carries the withheld population, and ONE `ERROR` line
names the survivors, the reason and the remedy. The registry log's byte-identity
across a refused pass is asserted (`…_RefusalTouchesNothing`).

Deliberately unchanged: adopt mode (byte-for-byte; `…Arms_AdoptModeStillAdopts` adopts
8 adoptable orphans), the foreign-owner marker path DF-BUNKER-18, fail-closed destroy
with a verified archive DF-BUNKER-33, and the failed-probe-aborts rule
(`reconcile.go:128-135` never reaches the walk, so the guard cannot be reached either).

### The guard is not defeated by REV-BUNKER-002's fail-open classification

The guard counts **what the walk was about to act on** — the classification's own
output — so a fail-open `orphanIsForeign` makes the count LARGER and the guard trip
SOONER. That defect can only push in the guard's direction; it cannot under-report a
population the guard needs to see.

## 4. The refusal counter, and where it is reported

The repo's standing law (BFS-031/BFS-032 both shipped the defect): a bound that is
not counted is not one. So:

| surface | field | transcript |
|---|---|---|
| `ReconcileReport.Refused` (`json:"refused,omitempty"`) | the withheld population, never double-counted with `Destroyed` | asserted in the GREEN cells |
| refusal log line | `action=refuse guard=orphan_sweep_guard refused_orphans=N unproven_orphan_limit=L reason=… remedy=…` | both live arms, `…-live-green-daemon.log:12` |
| async completion line | `…"destroyed":0,"refused":2` | `…-live-green-daemon.log:13` |
| boot provenance line | `created_this_boot=true` | `…-live-green-daemon.log:8` |
| boot guard-state line | `agent reconciliation sweep guard armed … unproven_orphan_limit=L` | `…-live-green-daemon.log:9` |

## 5. The reproduction (the filed behaviour)

### 5a. Live, on this host, with this row's own binary built from the FILED blobs

`REV-BUNKER-P1-PATCH-live-red.txt` + `-live-red-daemon.log`: a scratch **non-root**
daemon (config entirely under `/tmp`, `127.0.0.1:42000/42001`), registry path
**absent at boot**, default limit:

```
{"msg":"destroying agent","agent_id":"probeok-18967"}
{"msg":"destroying agent","agent_id":"probeok-39467"}
{"msg":"agent registry reconciliation complete (async orphan walk)","mode":"destroy","adopted":0,"destroyed":2,"foreign":0}
destroying_agent_lines=2   userdel_permission_denied_lines=2
/etc/passwd sha256 before=f5645b6c…d5a7  after=f5645b6c…d5a7  → UNCHANGED
```

The walk claimed both of this host's real `bunker-*` users and entered the destroy
path for each. The only reason they survived is `userdel: Permission denied` — the
row's claim, reproduced by this row's own build in its own transcript.

### 5b. Independent, pre-existing capture on the same host

Before this row's build ran, another scratch daemon had already produced the same
shape twice that day (its own config, empty/fresh registry in `/tmp`, the same two
real users): `replayed_live:0` → `registry reconcile: destroyed orphan agent` for
`probeok-18967` and `probeok-39467`, and `userdel failed in force mode … surviving
state: user record still present … home directory … still exists`. Cited as
corroboration, not as this row's evidence.

### 5c. Deterministic RED — the filed code, the FINAL test text

`REV-BUNKER-P1-PATCH-red.txt` (`unfixed`: base blobs of the four product files
swapped in, the three GREEN-only test files set aside, every swap sha256-asserted to
differ from the fixed tree):

```
--- FAIL: TestReconcileSweepGuardArms_UnprovenMassDestroyIsRefused
    reconcile_sweep_guard_arms_test.go:95: the unproven sweep destroyed 4 unknown bunker-* users
        ([host00 host01 host02 host03]): a registry that does not exist on a host this daemon has
        never seen must destroy NOTHING
--- FAIL: TestReconcileSweepGuardArms_EmptyRegistryFileIsRefused
    a present-but-EMPTY registry destroyed 4 unknown bunker-* users ([host00 host01 host02 host03])
--- PASS: …SmallUnprovenSweepStillHappens    (3 leftovers swept — the control)
--- PASS: …ProvenRegistrySweepsLargeSets     (live=1 + 8 leftovers swept — the control)
--- PASS: …AdoptModeStillAdopts              (8 orphans adopted — the control)
```

The two defect cells fail on the filed tree **with the final test text**, and all
three non-vacuity controls pass — which is what attributes the failures to this row's
defect rather than to a harness that refuses everything.

## 6. The GREEN

* `REV-BUNKER-P1-PATCH-green.txt` — every cell in the three packages passes on the
  frozen tree.
* `REV-BUNKER-P1-PATCH-live-green.txt` + `-live-green-daemon.log` — the same scratch
  non-root daemon shape as 5a, built from the fixed tree:
  `destroying_agent_lines=0`, `refusal_lines=1`, `refused_orphans=2`,
  `/etc/passwd` byte-identical, and the completion line reports
  `"destroyed":0,"refused":2`.

Named explicitly: this host carries only TWO unknown users, which is *below* the
shipped default of 3, so the live-green arm sets `unproven_orphan_limit: 1` — printed
in the transcript with the reason — to exercise the refusal at the host's real scale.
The shipped boundary (three swept / four refused) is proven deterministically by the
cells, not by this two-user host.

## 7. The negative control

`REV-BUNKER-P1-PATCH-control.txt` — the guard NEUTERED at its call site by
`REV-BUNKER-P1-PATCH-negative-control.patch` (the filed behaviour and nothing else):

```
pre-mutation: internal/agent/reconcile.go sha256=b914c8e4…5335
mutation applied: → sha256=59784f04…be5a
--- FAIL: …UnprovenMassDestroyIsRefused      (destroyed 4, want 0)
--- FAIL: …EmptyRegistryFileIsRefused        (destroyed 4, want 0)
--- PASS: …SmallUnprovenSweepStillHappens    (control unaffected)
--- PASS: …ProvenRegistrySweepsLargeSets     (control unaffected)
--- PASS: …AdoptModeStillAdopts              (control unaffected)
restore: internal/agent/reconcile.go sha256=b914c8e4…5335 (verified against the pre-mutation hash)
```

The cell goes red exactly when the guard is removed, and only those cells; the
restore is byte-identical.

## 8. The regression tests

`internal/agent/reconcile_sweep_guard_arms_test.go` (base-compatible — the RED text),
`internal/agent/reconcile_sweep_guard_test.go` (GREEN-only),
`internal/config/reconciliation_sweep_guard_test.go`,
`internal/registry/registry_provenance_test.go`. The cells that **fail without the
guard** are the two arms defect cells above; both are asserted through the destroy
SEAM (0 calls) *and* the report, so "destroyed nothing" cannot be a reporting
artefact. The config surface is proven through the real loader — an omitted key still
yields an armed guard, and the documented YAML keys are shown to bind (a renamed
mapstructure tag would otherwise pass every struct-level cell).

## 9. Gates

* `gitreins` Tier-1 (full test mode) PASS on every commit of this row: `112b91a`,
  `89378e9`, `353d102`, `e1d29f3`, `c3f246f`.
* `REV-BUNKER-P1-PATCH-suites.txt` — `gofmt -l`, `go build ./...`, `go vet ./...`,
  `go test ./...` on the frozen tree.
* Three pre-existing, load-dependent flakes blocked guard runs during this row and
  are reported rather than papered over (all in code this row does not touch; the
  attribution control is in each entry):
  * `TestSpawnRollbackSurvivesStuckCleanupStep` (internal/agent) — a race between the
    rollback budget and a real `userdel` child being killed; **1/10 on the fixed tree
    and 2/10 on the FILED tree at loadavg 96**, same notice shape. Not this row.
  * `TestStreamingEnvelope_LeavesOtherPathsAlone/unary_rest_json` (internal/server) —
    `uptimeSeconds` advanced between two sequential requests (16 vs 17). 40/40 pass
    on re-run.
  * `TestSameSurfaceOverHTTP1AndHTTP2/op_capabilities` (internal/server/webdav) —
    the response carries `read_at`/`duration_ms`, which differ between the two
    requests by construction.

## 10. What remains open, and what this row did NOT do

**REV-BUNKER-002 remains OPEN.** `orphanIsForeign` still returns `false` — i.e. "ours,
fair game" — when the `.bunker/owner` and `.bunker/ports` metadata is missing or
unreadable, and the reproduced incident depended on exactly that: the two real users
carried no marker, so a foreign daemon classified them as its own. This row does not
fix it (§3 explains why the guard stands regardless: the guard counts the
classification's output, so fail-open pushes toward refusal, never away from it).
Anything that still decides per-orphan on that classification — including the ADOPT
branch, which re-registers an orphan rather than deleting it — is unchanged.

Other residuals, stated rather than absorbed:

* A registry that EXISTS and replays a non-empty live set is outside the guard, even
  if it was restored from an older backup and the host carries far more agents. A
  relation trigger ("more orphans than live records") was considered and **rejected**:
  it would refuse exactly the legitimate case the row requires to keep working — a
  host whose registry knows two agents while twenty stale test users await a sweep.
* A deployment smaller than the threshold (the single-tenant bunker) is not protected
  by the default; `unproven_orphan_limit: 0` is the strictest setting and exists for
  those hosts.
* `AGENTS.md` asks for the live-server E2E battery on `bunker-mvp` for changes to
  spawn/destroy behaviour. That battery's procedure is *build, **push**, pull on
  `bunker-mvp`, restart `bunkerd`* — and this row forbids pushing and does not
  unilaterally restart a live production daemon. **Named gap.** In its place: a
  read-only probe of that host (the five agents and the 1.7 MB registry in §1) and the
  live scratch-daemon arms in §5a/§6, which exercise the same reconcile path with a
  real non-root daemon binary.
* The guard's threshold is a POLICY number; its correctness argument is the
  measurement in §1, not a law.

## 11. Evidence files

| file | produced by |
|---|---|
| `REV-BUNKER-P1-PATCH-arms.sh` | the one script; modes `green`, `unfixed`, `noguard`, `suites`, `live-red`, `live-green`, `live` |
| `REV-BUNKER-P1-PATCH-negative-control.patch` | the neuter, applied and reversed by `noguard` |
| `REV-BUNKER-P1-PATCH-green.txt` | `…arms.sh green` |
| `REV-BUNKER-P1-PATCH-red.txt` | `…arms.sh unfixed` (base `4879343`) |
| `REV-BUNKER-P1-PATCH-control.txt` | `…arms.sh noguard` |
| `REV-BUNKER-P1-PATCH-suites.txt` | `…arms.sh suites` |
| `REV-BUNKER-P1-PATCH-live-red.txt`, `-live-red-daemon.log` | `…arms.sh live-red` + the daemon's own stdout |
| `REV-BUNKER-P1-PATCH-live-green.txt`, `-live-green-daemon.log` | `…arms.sh live-green` + the daemon's own stdout |

Frozen tree (commit `c3f246f`), product-file sha256 as they appear in every
transcript: `internal/agent/reconcile.go b914c8e4…5335`,
`internal/agent/manager.go ee06df5b…3ac9`, `internal/config/config.go 24376f04…c3c3`,
`internal/registry/registry.go 216be342…a6fa`.
