# SEC-BUNKER-002 — Tenant network isolation: the shared-mode truth, the systemd boundary, the socket permissions

Row: SEC-BUNKER-002 — "localhost is SHARED between agents on one host, and
ports above 1024 are not uid-scoped — any agent can bind another agent's
allocated port (squat or impersonate)".

Spec (design authority): [specs/network-isolation.md](../../specs/network-isolation.md)
(§0 constraint evidence, §1.1/§1.2, §2 T2/T3, §5, §6).

STATUS: complete (docs honesty + focused tests; no new isolation mechanism —
none was invented, and none was needed for this row)
TIER: T1
EVIDENCE: commit (head of this branch, `wt/SEC-BUNKER-002`) + the tests below
WITNESS: `go test ./internal/netmode/ ./internal/docscheck/ -run 'Tenant|SECBunker002|Socket' -count=1 -v`
VERDICT: none:foreman-will-run
COMMIT: none:worker-pending
MEMORY: none:worker-does-not-write-memory

## 1. The premise, verified against live HEAD

The row's premise is TRUE for the shipped default. `DefaultMode` is `shared`
(`internal/netmode/netmode.go`, spec §5.1: default unchanged until
NET-BUNKER-008 measures otherwise), and in `shared` mode the agent runs in the
**host** network namespace:

- every agent's `127.0.0.1` is the host's loopback — co-tenant reach (spec
  threat T2);
- ports above 1024 are not uid-scoped by Linux; the per-agent 100-port block
  (default 10000–19999, `internal/resource/portalloc.go`) is BOOKKEEPING ONLY
  (`PortAllocator.Allocate` hands out ranges; nothing stops another uid from
  `bind()`ing a port inside the block) — bind-squat/impersonation (spec
  threat T3).

The spec already states both facts (§0 "Constraints", SEC-BUNKER-002 entry;
§2 threat table). This row's job was to make the **shipped documentation**
equally explicit and internally consistent — not to build a new mechanism.

## 2. The concrete tenant-network isolation option already exists: `systemd`

The `systemd` mode (NET-BUNKER-002, landed in 089f5f8) is the existing opt-in
boundary: the agent's dockerd transient unit is created with
`--property=PrivateNetwork=yes`
(`internal/netmode.PropertyPrivateNetworkYes`), giving the unit its own
network namespace containing only loopback — a private `127.0.0.1` and a
private port space (closes T2 and T3 for processes in the unit).

Its stated cost/boundary (spec §1.2/§3, `netmode.BoundaryFor(ModeSystemd)`):
the agent loses ALL outbound networking (no NAT, no DNS, image pulls fail) —
that is the accepted cost, not a bug; the boundary covers only processes
inside the unit. The exec/socket contract is unchanged (unix sockets are
filesystem objects and cross network namespaces). It is opt-in per agent and
never silently substituted: an unknown mode refuses the spawn (§5.2
refuse-loudly law, `netmode.RefusalError`).

## 3. Unix-socket isolation is file-permission based — and it is test-asserted

Socket privacy comes from the socket file and its parent directory (spec §6.1),
and both properties are ASSERTED BY TESTS, not asserted in prose (spec §6.3
assertion law):

- directory 0700, owner = agent uid: `EnsureAgentSocketDir` /
  `AssertAgentSocketDir` (stat-it-back), pinned by
  `TestEnsureAgentSocketDir_SetsModeExplicitly` and
  `TestAssertAgentSocketDir_Bites` in `internal/agent/sockdir_test.go`;
- socket file not group/world accessible: `EnsureSocketFileNotGroupWorld` /
  `AssertSocketFileNotGroupWorld` (`SocketFileMode` = 0600), pinned by
  `TestAssertSocketFileNotGroupWorld_Bites`;
- peer-uid enforcement (SO_PEERCRED, spec §6.2, NET-BUNKER-007):
  `CheckUnixSocketPeer` with live-connection table
  `TestCheckUnixSocketPeer_LiveConnectTable`; client-side ownership read-back
  `VerifySocketOwnership` pinned by `TestVerifySocketOwnership_RefusesForeignOwner`;
  abstract-socket ban (spec §7) pinned by
  `TestNoAbstractUnixSocketBindsInThisTree`.

Source: `internal/agent/sockdir.go`, `internal/agent/sockdir_linux.go`
(NET-BUNKER-007 follow-up commit 953fa5f4).

## 4. Documentation corrected by this row

1. `specs/architecture.md` — the "Per-Agent Port Isolation" section described
   the dedicated port range as if the allocation itself kept agents apart
   ("Overlapping ranges are prevented"). Overlap prevention is true of the
   ALLOCATOR's bookkeeping, not of the network: any agent can bind any port in
   a peer's range. The section now states the shared loopback / global port
   space truth, names `systemd` (`PrivateNetwork=yes`) as the tenant-network
   boundary with its outbound cost, and cites this row.
2. `specs/api.md` — the `port_range_start`/`port_range_end` field description
   ("Per-agent port isolation") now says the range is bookkeeping, not
   isolation, and points at the architecture section.
3. `specs/container-mode.md` — §6 risk 4 claimed the rootless builtin
   port-driver "binds published ports to localhost by default". That is
   misleading: `localhost` there is the HOST's loopback, which every agent on
   the host shares; and the `-p 127.0.0.1:<ext>:...` publishing shown in §3
   lands ports in the agent's ALLOCATED block, which is bookkeeping — nothing
   stops a co-tenant from binding the same port first. The default
   (rootlesskit, host-shared loopback) is not tenant-private on any network
   axis. The risk now states both facts and names the `systemd` isolation
   mode (NET-BUNKER-002) as the per-agent option that closes cross-tenant
   reach/impersonation for the unit's processes.

No shipped container/network documentation is left claiming — explicitly or
by structure — that `localhost`, the port block, or the default rootless
networking provide tenant isolation.

## 5. Regression pin (docs honesty, per the DF-BUNKER-54 / SEC-BUNKER-001 idiom)

`internal/docscheck/tenant_port_isolation_docs_test.go` pins:

- the three corrected doc sections carry the honest statements (line-scoped:
  the qualifier must sit in the port-isolation section, the field description,
  and container-mode risk 4 — with the row id `SEC-BUNKER-002` present);
- the token `SEC-BUNKER-002` resolves inside this artifact;
- RED proof: the pre-fix wordings are flagged by the pure rule function, so
  the rule is not vacuous.

## 6. Scope note

Only the honesty surface of THIS row was touched. NET-BUNKER-003..012
implementation, container lifecycle, and the SEC-BUNKER-001 listener-bind fix
remain their own rows.
