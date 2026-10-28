# Port-pool drift sweep (FLEET-INFRA-1)

Consolidated remedy for the shared-substrate `port_pool` condition that the
fleet filed as 27 rows across 15 projects (las-02/las-03 repeatedly observed
"port range pool exhausted" with only 1–2 agents live).

## Root cause

`internal/resource.PortAllocator` keeps its free list **in process memory**.
Every known free path was covered by `2c67b47` (free the range on a failed
spawn), but nothing ever *compared* the pool against the set of tracked
agents. Any teardown path that skipped `Free` — a future destroy fast-path, a
panic between `Unregister` and `Free` — leaked a range until the daemon
restarted, and a restart replays reservations from the durable registry
instead of healing memory. The pool could therefore only ever shrink between
boots, which is exactly the observed shape: exhaustion with 1–2 agents live.

## Remedy

| Seam | Change |
| --- | --- |
| `internal/resource/portalloc.go` | `ReconcileFrom(owned func(string) bool)` releases every reservation whose agent id has no owner; `HeldRanges()` snapshots allocations; the exhaustion error now names every holder and both remediation paths. |
| `internal/agent/reconcile.go` | `sweepPortPoolDrift()` runs the allocator sweep against the `reservationOwned` oracle (tracker record **or** spawn-in-flight marker); wired after the replay walk and again after the orphan walk on startup. |
| `internal/agent/manager.go` | `tickMaintenance()` — the TTL-reaper tick now also runs the drift sweep, so any leak self-heals within one minute instead of lasting until restart. |
| `internal/agent/manager_spawn.go` | `spawnInFlight` brackets the allocate→register window (step 1.6 → step 3, tens of seconds through rootless dockerd boot + session probe) so the sweep cannot release a range that is about to be owned. |
| `internal/config/config.go` | `Validate()` relates `agent.max_agents` to pool capacity; degenerate pool geometry refuses to boot instead of silently sharing one range. |
| `cmd/bunkerd/portpool.go` | `bunkerd port-pool check` renders the resolved geometry and the same `config.Validate` verdict a boot enforces — a config diagnostic that binds no port and does not contact the daemon. |

## Fail-closed behavior is preserved

The sweep is authoritative about **memory only** — it releases reservations,
never users or homes. Whether an agent *should* exist remains the
registry/system reconciliation's decision, and every sweep call site runs only
after that walk has settled the tracker. The existing fail-closed guarantees
are re-asserted by tests that pass on the merged tree:

- `TestReconcile_AdoptFailsClosedWithoutPortMetadata`,
  `TestReconcile_AdoptFailsClosedOnInvalidOrCollidingRange`,
  `TestReconcile_RestoreFailsClosedOnUnrestorableRange`
- `TestReconcile_RestoreReleasesReservationWhenTrackerRegistrationFails` —
  a reservation released by a failed restore, not leaked
- `TestReconcile_FailClosedAgentIsNotReprocessedAsOrphan`,
  `TestReconcile_FailClosedDestroyFailureStillFailsClosed`
- `TestReconcile_ForeignOrphanIsNeverDestroyed`,
  `TestReconcile_ForeignOwnerMarkerIsNeverAdoptedOrDestroyed` — foreign-owner
  safety across adopt and destroy modes
- `TestSweepPortPoolDrift_SkipsSpawnInFlight`,
  `TestReconcile_SweepDoesNotFightRestore` — the sweep never fights a live
  spawn or an in-progress restore
- `TestValidate_DegenerateGeometryRefusesBoot` — boot-time fail-closed

Regression coverage for the remedy itself:
`internal/resource/portalloc_fleetinfra_test.go` (5 `TestReconcileFrom_*` +
`TestExhaustionErrorNamesHolders` + `TestHeldRangesSortedSnapshot`),
`internal/agent/reconcile_portpool_test.go`,
`internal/config/config_portpool_test.go`, `cmd/bunkerd/portpool_test.go`.

## Live / deployment blocker (explicit)

The remedy is **repository-side and unshipped**. The daemons that actually
hold a pool — and therefore the ones that would need the drift sweep — still
run binaries that predate it:

| Host | Daemon version | Commit | Needs redeploy |
| --- | --- | --- | --- |
| `bunker-mvp` | 0.2.1 | `eab25686` (2026-10-23) | yes — predates `1597ec1e` |
| `dedi-2` | 0.1.4 | `071ab3b` | yes — more than a major version behind |
| `bunker4` | — | unreachable at audit time | yes |

The live leg of this row — a redeployed daemon whose drift sweep heals the
leaked ranges on the affected hosts — therefore stays **open** and is already
tracked by the deploy-freshness rows (`RELEASE-003`, `RELEASE-008`,
`PERF-005`, `PERF-006`) and the fleet-wide `FLEET-INFRA-2` stale-binary
remedy, which owns the version-identity check
(`scripts/fleet-version-check.sh`). No host mutation is performed by this
document; redeploy is an operator action.

## Verification (merged tree)

```
go build ./...                                  # exit 0
go vet ./...                                    # exit 0
go test ./... -count=1 -short -parallel 4       # full package suite: all ok
/tmp/bunkerd-fi1 --config /dev/null port-pool check
  # pool 10000-19999, per-agent 100 ports, max_agents 100,
  # capacity 100 ranges, verdict OK (max_agents fits the pool capacity)
```
