// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/registry"
	"github.com/deployBunker/bunker/internal/resource"
)

// SystemAgent is a managed agent observed on the host (a bunker-* system
// user). It is the system side of reconciliation.
type SystemAgent struct {
	AgentID  string
	Username string
	Home     string
}

// ReconcileReport summarises one startup reconciliation pass.
type ReconcileReport struct {
	// Mode is the effective reconciliation mode ("adopt" or "destroy").
	Mode string
	// ReplayedLive/Known are the registry set sizes after replay.
	ReplayedLive  int
	ReplayedKnown int
	// SystemAgents is the number of managed users found on the host.
	SystemAgents int
	// Restored counts replayed records whose system user still exists and
	// were reinstated in the tracker (with their exact port reservation).
	// A record that could not be restored exactly is counted under
	// Destroyed instead — reconciliation fails closed for it.
	Restored int
	// RestoredForeignPool (INT-CI-042) counts replayed records restored
	// despite a persisted port range lying entirely OUTSIDE this daemon's
	// pool: pool-geometry drift (e.g. a CI battery configuring a different
	// pool than the one the persisted reservation came from). Such a range
	// cannot collide with any port this daemon allocates, so the agent is
	// restored with its exact persisted range while the out-of-pool
	// reservation is deliberately NOT re-tracked in the allocator (see
	// restoreAgent). Counted separately from Restored — never
	// double-counted.
	RestoredForeignPool int `json:"restored_foreign_pool,omitempty"`
	// Purged counts registry records with no system user (stale).
	Purged int
	// Adopted counts orphans adopted into the tracker + registry.
	Adopted int
	// Destroyed counts orphans removed from the host.
	Destroyed int
	// Foreign counts orphans left untouched because they belong to another
	// daemon instance: either their persisted ports lie outside this
	// daemon's pool, or (DF-BUNKER-18) their `.bunker/owner` marker names a
	// different daemon instance than this one.
	Foreign int
}

// Reconcile replays the durable registry against system state and waits for
// the whole pass, including the orphan walk. It is the synchronous contract
// used by tests and the CLI.
func (m *AgentManager) Reconcile(ctx context.Context) ReconcileReport {
	_, finalCh := m.reconcile(ctx, false)
	return <-finalCh
}

// ReconcileStartup is the daemon-startup entry point (INT-CI-043): the
// readiness-critical phases (replay checks, restore, purge) run
// synchronously, but the orphan walk — adopt or destroy, each potentially
// slow (home archiving tar-to-disk, userdel -rf on multi-GB homes) — is
// dispatched to a background goroutine so the caller can bind its listeners
// and serve traffic without waiting on orphan cleanup. The measured failure
// this fixes: a daemon starting against a stale registry (live=0, known=709)
// spent the ENTIRE 30s readiness window archiving one orphan's home before
// its first listener existed (CI regression run 36180131056).
//
// DF-BUNKER-33 note: the orphan walk keeps archive-before-delete. Skipping
// or bounding the archive at startup was considered and rejected — the
// archive is the data-loss protection that lets destroy delete a home at
// all; post-ready async execution removes the readiness block without
// weakening it.
//
// The TTL-reaper invariant is preserved: m.reconcileDone still closes only
// after the orphan walk completes (in either entry point), so the reaper can
// never destroy an agent before the registry has been fully reconciled
// against system state.
//
// Returns the interim report (replay counts, Restored, Purged — the orphan
// counters are still zero in the interim snapshot) and a channel that
// delivers the FINAL report once the orphan walk completes. The interim
// snapshot and the final report are independent values: the background
// goroutine owns its copy.
func (m *AgentManager) ReconcileStartup(ctx context.Context) (ReconcileReport, <-chan ReconcileReport) {
	return m.reconcile(ctx, true)
}

func (m *AgentManager) reconcile(ctx context.Context, asyncOrphans bool) (ReconcileReport, <-chan ReconcileReport) {
	finalCh := make(chan ReconcileReport, 1)
	rep := ReconcileReport{Mode: m.cfg.Agent.Reconciliation.ModeOrDestroy()}
	if err := m.cfg.Agent.Reconciliation.Validate(); err != nil {
		// Validation happens at config load; if a caller built a config by
		// hand, fall back to the documented default rather than guessing.
		m.logger.Warn("invalid reconciliation mode, using default", "mode", rep.Mode, "error", err)
		rep.Mode = config.ReconcileModeDestroy
	}
	// earlyFinish ends a reconciliation that never reaches the orphan walk
	// (registry unavailable, failed system probe): unblock the reaper and
	// deliver the report exactly once, like a completed pass.
	earlyFinish := func() {
		m.reconcileOnce.Do(func() { close(m.reconcileDone) })
		finalCh <- rep
		close(finalCh)
	}

	if m.registry == nil {
		m.logger.Info("registry reconcile skipped — durable registry unavailable",
			"reason", registryUnavailableReason(m))
		earlyFinish()
		return rep, finalCh
	}

	rep.ReplayedLive = m.registry.LiveCount()
	rep.ReplayedKnown = m.registry.KnownCount()

	systemAgents, err := m.listSystemAgents()
	if err != nil {
		// A failed probe must not be read as "no agents exist": that would
		// purge every live record and destroy every orphan in one sweep.
		m.logger.Error("registry reconcile: cannot enumerate system agents; skipping reconciliation", "error", err)
		earlyFinish()
		return rep, finalCh
	}
	rep.SystemAgents = len(systemAgents)
	present := make(map[string]SystemAgent, len(systemAgents))
	for _, sa := range systemAgents {
		present[sa.AgentID] = sa
	}

	// (1) + (2): walk replayed records against system state.
	//
	// handled remembers every agent the walk dealt with. The orphan walk
	// below must not re-process one: an agent that just failed closed has
	// had its live record dropped, so it would otherwise look like an
	// unknown leftover and be destroyed (or adopted) a second time.
	handled := make(map[string]bool, len(m.registry.Live()))
	for _, rec := range m.registry.Live() {
		sa, onHost := present[rec.AgentID]
		handled[rec.AgentID] = true
		if !onHost {
			if err := m.registry.AppendDestroy(rec.AgentID); err != nil {
				m.logger.Error("registry reconcile: purge failed", "agent_id", rec.AgentID, "error", err)
				continue
			}
			rep.Purged++
			// DF-BUNKER-24: the system user is gone but a purge only dropped
			// the durable record — the agent's persisted private key stayed
			// on disk forever. Convergence here is the same scoped removal the
			// destroy path uses (best-effort, idempotent). Foreign orphans are
			// skipped earlier and are not reached by this walk: it only visits
			// records THIS daemon spawned.
			m.removeAgentSSHKeyBestEffort(rec.AgentID, m.logger)
			m.logger.Info("registry reconcile: purged stale registry record",
				"action", "purge", "agent_id", rec.AgentID, "reason", "no system user")
			continue
		}
		if err := m.restoreAgent(rec); err != nil {
			// INT-CI-042: a persisted range ENTIRELY DISJOINT from this
			// daemon's pool is pool-geometry drift, not corruption — the
			// same DF-BUNKER-13 principle the orphan walk applies via
			// orphanIsForeign. A disjoint range cannot collide with any
			// port this daemon allocates, so force-destroying the agent
			// buys nothing and loses a healthy one (the CI regression
			// battery lost exactly such an agent to its own pool change).
			// Only a PROVABLY disjoint range is tolerated here: both ends
			// readable, end < poolStart or start > poolEnd. ValidateRange
			// is deliberately NOT used for this test — it also rejects
			// in-pool-but-misaligned ranges, which must keep their
			// fail-closed treatment below.
			if start, end := rec.PortStart, rec.PortEnd; m.portAlloc != nil &&
				start != 0 && end != 0 {
				poolStart, poolEnd := m.portAlloc.Bounds()
				if end < poolStart || start > poolEnd {
					m.tracker.Register(registryToRecord(rec))
					rep.RestoredForeignPool++
					m.logger.Warn("registry reconcile: foreign-pool port reservation tolerated",
						"action", "restore-foreign-pool",
						"agent_id", rec.AgentID,
						"persisted_range", fmt.Sprintf("%d-%d", start, end),
						"pool", fmt.Sprintf("%d-%d", poolStart, poolEnd),
						"reason", "disjoint from the configured pool (pool-geometry drift): no collision possible, out-of-pool reservation not re-tracked in the allocator")
					continue
				}
			}
			// Fail closed: the agent's exact port reservation could not be
			// re-established, so serving it risks a later spawn
			// double-allocating its ports. Drop the half-managed state,
			// force-destroy the system agent and report it as destroyed.
			if m.failClosedRestore(ctx, rec, err) {
				rep.Destroyed++
			}
			continue
		}
		rep.Restored++
		m.logger.Info("registry reconcile: restored agent from registry",
			"action", "restore", "agent_id", rec.AgentID,
			"port_start", rec.PortStart, "port_end", rec.PortEnd,
			"system_user", sa.Username)
	}

	// (3): orphans — system users the registry does not know as live.
	//
	// INT-CI-043: this is the SLOW phase (adopt/destroy per orphan; destroy
	// archives the home to disk before userdel and a multi-GB home can hold
	// the walk for tens of seconds), so in startup mode it runs in a
	// background goroutine and the caller proceeds to listener bind
	// immediately. The reaper invariant is untouched: reconcileDone — the
	// channel startTTLReaper waits on — closes only AFTER this walk
	// completes, in both modes.
	orphans := make([]SystemAgent, 0, len(systemAgents))
	for _, sa := range systemAgents {
		if m.registry.Get(sa.AgentID) != nil || handled[sa.AgentID] {
			continue // known live agent (handled above) or already handled
		}
		orphans = append(orphans, sa)
	}
	runOrphanWalk := func(list []SystemAgent) ReconcileReport {
		final := rep
		for _, sa := range list {
			// Foreign check BEFORE the mode branch: an orphan whose persisted
			// ports lie outside this daemon's pool cannot collide with any
			// port this daemon allocates, so it belongs to another daemon
			// instance (or an older pool geometry) and must never be
			// destroyed or adopted here — not even in destroy mode.
			// Unreadable or malformed metadata is not foreign (the daemon
			// cannot prove it is safe to leave), so it keeps the fail-closed
			// treatment below.
			//
			// DF-BUNKER-18: an orphan carrying ANOTHER daemon instance's
			// ownership marker is foreign regardless of its persisted range,
			// which also covers overlapping pools and unreadable port
			// metadata — the two destructive shapes the port test could not.
			if foreign, start, end := m.orphanIsForeign(sa); foreign {
				m.logForeignOrphanSkip(sa, start, end)
				final.Foreign++
				continue
			}
			if final.Mode == config.ReconcileModeAdopt {
				if err := m.adoptAgent(ctx, sa); err != nil {
					m.logger.Warn("registry reconcile: adopt failed, destroying orphan instead",
						"action", "adopt", "agent_id", sa.AgentID, "error", err)
					// Adoption is exact-port or nothing: drop any residue so a
					// failed adopt can never leave a tracker record or a port
					// reservation behind for the orphan.
					m.dropHalfManagedState(sa.AgentID)
					if derr := m.destroyOrphan(ctx, sa.AgentID); derr != nil {
						m.logger.Error("registry reconcile: destroy after failed adopt failed",
							"agent_id", sa.AgentID, "error", derr)
						continue
					}
					final.Destroyed++
					m.logger.Info("registry reconcile: destroyed orphan agent",
						"action", "destroy", "agent_id", sa.AgentID, "system_user", sa.Username)
					continue
				}
				final.Adopted++
				start, end, _ := m.portAllocRange(sa.AgentID)
				m.logger.Info("registry reconcile: adopted orphan agent",
					"action", "adopt", "agent_id", sa.AgentID, "system_user", sa.Username,
					"port_start", start, "port_end", end)
				continue
			}
			if err := m.destroyOrphan(ctx, sa.AgentID); err != nil {
				m.logger.Error("registry reconcile: destroy orphan failed",
					"action", "destroy", "agent_id", sa.AgentID, "error", err)
				continue
			}
			final.Destroyed++
			m.logger.Info("registry reconcile: destroyed orphan agent",
				"action", "destroy", "agent_id", sa.AgentID, "system_user", sa.Username)
		}
		return final
	}
	if asyncOrphans {
		go func() {
			final := runOrphanWalk(orphans)
			m.reconcileOnce.Do(func() { close(m.reconcileDone) })
			m.logger.Info("agent registry reconciliation complete (async orphan walk)",
				"mode", final.Mode,
				"adopted", final.Adopted,
				"destroyed", final.Destroyed,
				"foreign", final.Foreign,
			)
			finalCh <- final
			close(finalCh)
		}()
		return rep, finalCh
	}
	final := runOrphanWalk(orphans)
	m.reconcileOnce.Do(func() { close(m.reconcileDone) })
	finalCh <- final
	close(finalCh)
	return final, finalCh
}

// restoreAgent reinstates a replayed registry record in the tracker together
// with its EXACT persisted port reservation (so a later spawn cannot double
// allocate the same ports). The reservation is taken BEFORE the tracker
// record is registered and released again if registration fails, so a failed
// restore can never leave a tracker record without the reservation that makes
// it safe.
//
// A port allocator is only non-nil when the pool geometry is valid; in that
// case a record without a persisted range cannot be restored exactly and
// returns an error — Reconcile then fails closed for that agent
// (failClosedRestore) instead of serving it without isolation metadata.
// Registering a record that is already tracked is not an error — the
// registry is the durable copy, the tracker the live one.
//
// INT-CI-042: an error whose cause is a persisted range ENTIRELY DISJOINT
// from the current pool (pool-geometry drift) is handled by the CALLER
// (Reconcile's replay walk): the tracker record is restored and the exact
// out-of-pool reservation is deliberately NOT re-tracked in the allocator —
// ValidateRange/Reserve would reject it for being out-of-pool. That is safe
// by construction: spawn allocates EXCLUSIVELY through the allocator, which
// only ever hands out in-pool ranges, so an out-of-pool reservation that is
// not tracked in the allocator can never be handed out again. Everything
// else (no persisted range, in-pool misaligned, in-pool colliding) keeps the
// fail-closed treatment.
func (m *AgentManager) restoreAgent(rec *registry.Record) error {
	trackerRec := registryToRecord(rec)
	if m.portAlloc != nil {
		if rec.PortStart == 0 || rec.PortEnd == 0 {
			return fmt.Errorf("registry record carries no persisted port range: " +
				"cannot restore its exact reservation")
		}
		if err := m.portAlloc.Restore(rec.AgentID, rec.PortStart, rec.PortEnd); err != nil {
			return fmt.Errorf("restore port reservation %d-%d: %w", rec.PortStart, rec.PortEnd, err)
		}
	}
	if m.tracker.Get(trackerRec.AgentID) == nil {
		if err := m.tracker.Register(trackerRec); err != nil {
			// The reservation was made for an agent that is not going to
			// be tracked: give it back rather than leaking pool capacity.
			m.releasePortReservation(rec.AgentID)
			return fmt.Errorf("restore tracker record: %w", err)
		}
	}
	return nil
}

// failClosedRestore handles a replayed live agent whose exact port
// reservation could not be re-established. Serving it would let the next
// spawn double-allocate its ports, and leaving it half-managed (a tracker or
// registry record without the reservation) is worse than destroying it, so
// reconciliation fails closed: any partial state the failed restore could
// have left is dropped, the unsafe live registry record is removed, and the
// system agent is force-destroyed through the destroy seam. Exactly one
// action log line is emitted for the agent. It reports whether the agent was
// actually destroyed.
func (m *AgentManager) failClosedRestore(ctx context.Context, rec *registry.Record, cause error) bool {
	agentID := rec.AgentID

	// No tracker record without the reservation, no reservation without the
	// tracker record.
	m.dropHalfManagedState(agentID)

	// The live record describes an agent the daemon can no longer manage
	// exactly; drop it so a restart cannot resurrect it. A destroy event is
	// idempotent (the destroy below appends its own), so a failure here only
	// degrades to "reconciliation retries on the next start".
	persistErr := error(nil)
	if err := m.registry.AppendDestroy(agentID); err != nil {
		persistErr = err
	}

	derr := m.destroyOrphan(ctx, agentID)

	fields := []any{
		"action", "destroy",
		"agent_id", agentID,
		"reason", "exact port reservation not restorable",
		"error", cause,
	}
	if persistErr != nil {
		fields = append(fields, "registry_error", persistErr)
	}
	if derr != nil {
		fields = append(fields, "destroy_error", derr)
		m.logger.Error("registry reconcile: unsafe agent destroy failed", fields...)
		return false
	}
	m.logger.Warn("registry reconcile: unsafe agent force-destroyed", fields...)
	return true
}

// releasePortReservation gives back agentID's reservation when it holds one.
// It is the rollback half of Restore/Restore-adopt: Free is a no-op for an ID
// with no reservation, so it is safe to call on any failure path.
func (m *AgentManager) releasePortReservation(agentID string) {
	if m.portAlloc != nil {
		m.portAlloc.Free(agentID)
	}
}

// dropHalfManagedState removes any live tracker record and port reservation
// held for agentID. It is the fail-closed cleanup: an agent may never be
// served with one half of its state (a tracker record without the exact port
// reservation, or a reservation without a tracker record), so both halves are
// dropped together on any adoption/restoration failure.
func (m *AgentManager) dropHalfManagedState(agentID string) {
	if m.tracker.Get(agentID) != nil {
		m.tracker.Unregister(agentID)
	}
	if m.portAlloc != nil && m.portAlloc.Has(agentID) {
		m.portAlloc.Free(agentID)
	}
}

// logForeignOrphanSkip emits the loud skip warning for an orphan this daemon
// leaves completely alone. It always names the agent, its persisted port
// range (0-0 when unreadable) and this daemon's pool — nil-safe, because an
// ownership-marker skip is possible with NO allocator configured — plus the
// owning instance id and the pool geometry that daemon recorded in the
// marker, when the marker is readable.
func (m *AgentManager) logForeignOrphanSkip(sa SystemAgent, start, end uint32) {
	var poolStart, poolEnd uint32
	if m.portAlloc != nil {
		poolStart, poolEnd = m.portAlloc.Bounds()
	}
	attrs := []any{
		"action", "skip",
		"agent_id", sa.AgentID,
		"system_user", sa.Username,
		"persisted_range", fmt.Sprintf("%d-%d", start, end),
		"pool", fmt.Sprintf("%d-%d", poolStart, poolEnd),
	}
	if owner, ownerPoolStart, ownerPoolEnd, ok := readPersistedOwner(sa.Home); ok {
		attrs = append(attrs, "owner", owner)
		if ownerPoolStart != 0 || ownerPoolEnd != 0 {
			attrs = append(attrs, "owner_pool", fmt.Sprintf("%d-%d", ownerPoolStart, ownerPoolEnd))
		}
	}
	attrs = append(attrs, "reason",
		"agent is owned by another daemon instance (or an older pool geometry): "+
			"destroy it from the daemon that owns it")
	m.logger.Warn("registry reconcile: skipping foreign orphan agent", attrs...)
}

// orphanIsForeign reports whether an orphan observed on the host belongs to
// ANOTHER daemon instance and must therefore be left completely untouched
// (never adopted, never destroyed).
//
// Precedence (DF-BUNKER-18):
//
//  1. OWNERSHIP MARKER NAMES ANOTHER INSTANCE — when `<home>/.bunker/owner`
//     carries a non-empty instance id that is not this daemon's own (and this
//     daemon has an identity of its own to compare against), the orphan is
//     FOREIGN. This branch is checked FIRST and independently of the persisted
//     port range, so it also holds when the two daemons' pools OVERLAP and
//     when the port metadata is missing or unreadable — the two destructive
//     shapes the port test could not classify;
//  2. MARKER NAMES THIS DAEMON — the agent is OURS, so it is NOT foreign and
//     the port test below is deliberately not applied to it: the marker, not
//     the ports, decides ownership. The agent takes the ordinary adopt/destroy
//     decision, fail-closed rules included, exactly as an orphan with no
//     marker does when its metadata is unreservable. A disjoint range on an
//     agent WE stamped is a leftover of our own from an older pool geometry;
//     adoption refuses it (the range is outside the pool) and the fail-closed
//     path destroys it from the daemon that owns it, which is the same
//     treatment reconcile documents for any orphan that cannot be adopted with
//     its exact reservation. Two daemons SHARING one data dir would share one
//     identity file and could then claim each other's agents — that
//     configuration is the isolation requirement the daemon already
//     documents as unsupported, and it is not what this marker introduces;
//  3. NO MARKER (or this daemon has no identity of its own) — the legacy
//     behaviour, byte for byte: with no allocator this daemon cannot prove any
//     range foreign, so nothing is ever skipped for it by the port test, and
//     otherwise a persisted port range ENTIRELY disjoint from this daemon's
//     pool is foreign. Unreadable or malformed metadata is NOT foreign — the
//     daemon cannot prove it is safe to leave, so the existing fail-closed
//     path keeps handling it. That disjoint test deliberately does not use
//     ValidateRange, which also rejects in-pool-but-unaligned ranges: those
//     keep their current fail-closed treatment.
//
// The pool line recorded inside the ownership marker is NOT part of this
// decision: it is operator information. A legitimate pool-geometry change on
// THIS daemon would otherwise reclassify its own agents as foreign and leak
// them forever.
func (m *AgentManager) orphanIsForeign(sa SystemAgent) (foreign bool, start, end uint32) {
	if id, _, _, ok := readPersistedOwner(sa.Home); ok && m.instanceID != "" {
		// The marker decides ownership. The reported range stays the agent's
		// persisted ports (informational; 0-0 when unreadable).
		start, end, _ = readPersistedPortRange(sa.Home)
		return id != m.instanceID, start, end
	}
	if m.portAlloc == nil {
		return false, 0, 0
	}
	start, end, ok := readPersistedPortRange(sa.Home)
	if !ok {
		return false, 0, 0
	}
	poolStart, poolEnd := m.portAlloc.Bounds()
	return end < poolStart || start > poolEnd, start, end
}

// adoptAgent re-registers an orphan: it rebuilds the tracker record from
// persisted metadata (the agent's own .bunker/ports file, written at spawn
// time) and pins the exact port sub-range with Restore. Adopted agents carry
// no durable TTL (ExpiresAt zero) so the TTL reaper never destroys an agent
// the operator never gave an expiry for.
//
// Adoption is exact-port or nothing. Missing or malformed metadata, an
// invalid persisted range, or a range another agent already holds all FAIL
// adoption before it can leave a tracker or registry record; Reconcile then
// force-destroys the orphan. An agent whose ports the next spawn could
// double-allocate must not be served, and a leftover user is cheaper to
// recreate than a port collision.
//
// DF-BUNKER-53: adoption also RE-APPLIES the agent's isolation to the host —
// the exact gap live evidence exposed (an adopted agent reported limits while
// `systemctl show user-<uid>.slice` read CPUQuotaPerSecUSec=infinity,
// MemoryMax=infinity, no drop-in, no constrained unit). Two contracts:
//
//   - the applied AND reported limits come from the agent's OWN durable
//     record (the KindSpawn event persistSpawn wrote, read back from the
//     registry file including rotated backups), never from the current
//     m.defaultLimits(): an operator who raised the config between the spawn
//     and the adopt must not silently re-limit an agent to the new defaults.
//     When no readable record exists (older build, rotated-away history) the
//     documented fallback is m.defaultLimits(), logged loudly — never
//     silently;
//   - the host stages a fresh spawn performs run here too, through the same
//     functions: the user-slice drop-in + daemon-reload + containment landing
//     check (applyUserSliceLimitsAndVerify), and the bunker-docker-<agentID>
//     systemd-run unit with the resolved limit properties. Both are function
//     seams (nil = skip, the documented degradation for hand-built managers)
//     so tests inject recorders instead of touching systemd.
//
// Failure discipline is unchanged: a failing host stage fails adoption before
// the tracker/registry record exists, and the caller's rollback drops any
// reservation — an adopted agent is never served with one half of its state.
func (m *AgentManager) adoptAgent(ctx context.Context, sa SystemAgent) error {
	start, end, ok := readPersistedPortRange(sa.Home)
	if !ok {
		return fmt.Errorf("no readable port metadata at %s", persistedPortsPath(sa.Home))
	}
	if m.portAlloc != nil {
		if err := m.portAlloc.ValidateRange(start, end); err != nil {
			return fmt.Errorf("persisted port range %d-%d is not a legal pool sub-range: %w", start, end, err)
		}
		if err := m.portAlloc.Restore(sa.AgentID, start, end); err != nil {
			return fmt.Errorf("persisted port range %d-%d cannot be reserved: %w", start, end, err)
		}
	}

	// DF-BUNKER-53: source the agent's OWN persisted record (limits + knob
	// properties it was spawned with). The live fold cannot carry it — an
	// orphan is by definition an agent the registry does not know as live —
	// so the record is read back from the durable store ON DISK. Without a
	// readable record the documented fallback is the current config defaults,
	// stated loudly in the log so the adoption's limit source is always
	// attributable.
	persisted, hasRecord := m.readPersistedAgentRecord(sa.AgentID)
	var (
		cpuQuota   float64
		memMax     uint64
		diskMax    uint64
		maxProcs   uint64
		maxFiles   uint64
		reported   *v1.ResourceLimits
		unitKnobs  []SystemdKnob
		sliceKnobs []SystemdKnob
	)
	if hasRecord && persisted != nil && persisted.Limits != nil {
		limits := persisted.Limits
		reported = &v1.ResourceLimits{
			CpuQuota:            limits.CpuQuota,
			MemoryMaxBytes:      limits.MemoryMaxBytes,
			DiskMaxBytes:        limits.DiskMaxBytes,
			MaxDockerContainers: limits.MaxDockerContainers,
		}
		cpuQuota = limits.CpuQuota
		memMax = limits.MemoryMaxBytes
		diskMax = limits.DiskMaxBytes
		// The process/fd limits never rode the durable record; they resolve
		// from this daemon's config exactly like a fresh spawn's.
		maxProcs = m.cfg.Agent.DefaultMaxProcesses
		maxFiles = m.cfg.Agent.DefaultMaxOpenFiles
		unitKnobs, sliceKnobs = knobsFromLimits(persisted, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
		m.logger.Info("adopting agent with its persisted limits",
			"agent_id", sa.AgentID,
			"cpu_quota", cpuQuota, "memory_max_bytes", memMax, "disk_max_bytes", diskMax,
			"safety_preset", persisted.SafetyPreset)
	} else {
		// No readable durable record: fall back to the current defaults.
		// The log line is the "say so" part of the fallback contract.
		def := m.defaultLimits()
		reported = &v1.ResourceLimits{
			CpuQuota:            def.CpuQuota,
			MemoryMaxBytes:      def.MemoryMaxBytes,
			DiskMaxBytes:        def.DiskMaxBytes,
			MaxDockerContainers: def.MaxDockerContainers,
		}
		cpuQuota = def.CpuQuota
		memMax = def.MemoryMaxBytes
		diskMax = def.DiskMaxBytes
		maxProcs = m.cfg.Agent.DefaultMaxProcesses
		maxFiles = m.cfg.Agent.DefaultMaxOpenFiles
		unitKnobs, sliceKnobs = knobsFromLimits(nil, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
		m.logger.Warn("adopting agent without a readable persisted record; applying CURRENT config defaults",
			"agent_id", sa.AgentID,
			"cpu_quota", cpuQuota, "memory_max_bytes", memMax, "disk_max_bytes", diskMax)
	}

	// Apply the same host stages a fresh spawn applies, in the same order:
	// the docker unit first (a fresh spawn's Step 5), then the user-slice
	// drop-in + landing check (Step 5c). Both stages are seams so tests never
	// run systemd; a nil seam skips its stage (the documented degradation for
	// hand-built managers), a FAILING seam fails adoption loudly below.
	unitName := "bunker-docker-" + sa.AgentID

	// The knob stages are preset-parameterised (containment rides the tier
	// table), and the tier tables fail LOUD on an unknown name — so the
	// preset is resolved ONCE here through the single precedence resolver,
	// exactly like a fresh spawn. A record that carries a preset uses ITS
	// value (what the agent was spawned under); a record without one, or no
	// record at all, resolves to this daemon's effective preset. The record
	// keeps its own (possibly empty) preset for REPORTING: a pre-GAP-116
	// agent keeps reporting no preset rather than a fabricated one.
	knobsPreset := config.SafetyPresetStandard
	if hasRecord && persisted != nil && persisted.SafetyPreset != "" {
		knobsPreset = persisted.SafetyPreset
	} else {
		resolved, perr := m.cfg.ResolveSafetyPreset("")
		if perr != nil {
			m.releasePortReservation(sa.AgentID)
			return fmt.Errorf("resolve safety preset for adopted agent %s: %w", sa.AgentID, perr)
		}
		knobsPreset = resolved
	}

	// The agent's uid/gid feed the docker unit (systemd-run --uid/--gid on a
	// fresh spawn). The user must exist — an adoptable orphan always has one
	// (it is a bunker-* system user) — and a failed lookup is an adoption
	// failure, never a warning: without a uid there is no unit to constrain.
	u, userErr := lookupAgentUser(sa.Username)
	if userErr != nil {
		m.releasePortReservation(sa.AgentID)
		return fmt.Errorf("lookup adopted agent user %s: %w", sa.Username, userErr)
	}
	if m.runAdoptedDockerUnit != nil {
		if err := m.runAdoptedDockerUnit(ctx, unitName, u.Uid, u.Gid, unitKnobs); err != nil {
			m.releasePortReservation(sa.AgentID)
			return fmt.Errorf("apply adopted docker unit %s: %w", unitName, err)
		}
	}
	var dropinContent string
	var createdUserSlice bool
	if m.applyAdoptedSliceLimits != nil {
		containment := resolveContainmentKnobs(knobsPreset, memMax, m.cfg.Agent)
		content, sliceErr := m.applyAdoptedSliceLimits(ctx, u, cpuQuota, memMax, diskMax, maxProcs, maxFiles, sliceKnobs, containment)
		if sliceErr != nil {
			// DF-BUNKER-53 failure contract: a failing host stage fails
			// adoption LOUDLY. Reconcile's rollback below drops the port
			// reservation; because the tracker record has not been created
			// yet, nothing half-managed can survive — the agent is then
			// destroyed instead of being served unconstrained.
			m.releasePortReservation(sa.AgentID)
			return fmt.Errorf("apply adopted user slice limits for %s: %w", sa.AgentID, sliceErr)
		}
		dropinContent = content
		createdUserSlice = true
	}

	rec := &resource.AgentRecord{
		AgentID:           sa.AgentID,
		Status:            "running",
		Limits:            reported,
		CreatedAt:         homeCreatedAt(sa.Home),
		SshPrivateKeyPath: m.sshKeyPath(sa.AgentID),
		PortRangeStart:    start,
		PortRangeEnd:      end,
		// GAP-116 reporting fields: the adopted agent reports the same shape
		// a fresh spawn records — the preset it was spawned with (when known)
		// and the knob set the host stages just applied.
		SafetyPreset:     presetForRecord(persisted, hasRecord),
		UnitProperties:   systemdKnobsToProto(unitKnobs),
		SliceProperties:  systemdKnobsToProto(sliceKnobs),
		SliceDropIn:      dropinContent,
		SliceDropInState: sliceDropInState(createdUserSlice),
	}
	if m.tracker.Get(sa.AgentID) == nil {
		if err := m.tracker.Register(rec); err != nil {
			m.releasePortReservation(sa.AgentID)
			return fmt.Errorf("register adopted agent: %w", err)
		}
	}
	if err := m.persistSpawn(rec); err != nil {
		// The adopt path must not leave a tracker record (or a
		// reservation) the durable store does not know about.
		m.tracker.Unregister(sa.AgentID)
		m.releasePortReservation(sa.AgentID)
		return fmt.Errorf("persist adopted agent: %w", err)
	}
	return nil
}

// knobsFromLimits resolves the unit + slice knob sets for an adopted agent
// (DF-BUNKER-53). When the agent's durable record carries the GAP-116
// property lists, THOSE are used verbatim — they are exactly what the agent
// was spawned under, and re-deriving could drift (a sibling row owns changing
// the semantics; adoption must apply what spawn applied). A record without
// properties (or the no-record fallback) resolves through KnobsForPreset,
// which fails LOUD on an unknown preset name.
func knobsFromLimits(persisted *registry.Record, cpuQuota float64, memMax, diskMax, maxProcs, maxFiles uint64) (unit, slice []SystemdKnob) {
	if persisted != nil {
		if unit = protoToSystemdKnobs(persisted.UnitProperties); len(unit) > 0 {
			slice = protoToSystemdKnobs(persisted.SliceProperties)
			return unit, slice
		}
	}
	preset := config.SafetyPresetStandard
	if persisted != nil && persisted.SafetyPreset != "" {
		preset = persisted.SafetyPreset
	}
	return KnobsForPreset(preset, cpuQuota, memMax, diskMax, maxProcs, maxFiles)
}

// protoToSystemdKnobs converts a durable record's plain-JSON property list
// back into knob form (nil-safe; the registry's proto-free storage form).
func protoToSystemdKnobs(props []registry.SystemdProperty) []SystemdKnob {
	if len(props) == 0 {
		return nil
	}
	out := make([]SystemdKnob, 0, len(props))
	for _, p := range props {
		out = append(out, SystemdKnob{Name: p.Name, Value: p.Value})
	}
	return out
}

// presetForRecord returns the preset an adopted agent should report: the one
// its durable record carries, or the empty string when none is known (a
// pre-GAP-116 agent keeps reporting no preset rather than a fabricated one).
func presetForRecord(persisted *registry.Record, hasRecord bool) string {
	if hasRecord && persisted != nil {
		return persisted.SafetyPreset
	}
	return ""
}

// destroyOrphan removes an orphan through the manager's destroy path.
func (m *AgentManager) destroyOrphan(ctx context.Context, agentID string) error {
	fn := m.destroyAgent
	if fn == nil {
		fn = m.Destroy
	}
	_, err := fn(ctx, agentID, true)
	return err
}

// portAllocRange returns the range currently held for agentID (0,0 if none).
func (m *AgentManager) portAllocRange(agentID string) (uint32, uint32, bool) {
	if m.portAlloc == nil {
		return 0, 0, false
	}
	return m.portAlloc.AllocatedRange(agentID)
}

// defaultLimits mirrors the server-side default resource limits so an
// adopted agent looks like a freshly spawned one.
func (m *AgentManager) defaultLimits() *v1.ResourceLimits {
	return &v1.ResourceLimits{
		CpuQuota:            m.cfg.Agent.DefaultCPUQuota,
		MemoryMaxBytes:      m.cfg.Agent.DefaultMemoryBytes,
		DiskMaxBytes:        m.cfg.Agent.DefaultDiskBytes,
		MaxDockerContainers: m.cfg.Agent.DefaultMaxDockerContainers,
	}
}

func (m *AgentManager) sshKeyPath(agentID string) string {
	return m.cfg.Agent.SSHDir + "/" + agentID
}

// homeCreatedAt uses the agent home directory's modification time as a
// best-effort creation timestamp (the registry, not the filesystem, is the
// authoritative source when a record exists).
func homeCreatedAt(home string) time.Time {
	if home == "" {
		return time.Time{}
	}
	info, err := os.Stat(home)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

// registryUnavailableReason describes why reconciliation was skipped.
func registryUnavailableReason(m *AgentManager) string {
	if m.registryErr != nil {
		return m.registryErr.Error()
	}
	return "registry disabled by configuration"
}
