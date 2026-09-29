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
	// Unproven (REV-BUNKER-002) counts orphans whose ownership/port metadata
	// is MISSING or UNREADABLE. The daemon cannot prove it owns the agent and
	// cannot prove it is foreign, so the agent is skipped (never destroyed).
	// Destroying on unreadable metadata is fail-open data loss: an unreadable
	// marker is indistinguishable from another daemon's root-only agent. It is
	// never double-counted with Foreign or Destroyed.
	Unproven int `json:"unproven,omitempty"`
	// Refused (REV-BUNKER-P1-PATCH) counts orphans the SWEEP-LEVEL guard
	// deliberately left alone: the durable registry could not vouch for this
	// host (empty replayed live set, or a registry file this boot created)
	// while the walk found more unknown bunker-* users than
	// agent.reconciliation.unproven_orphan_limit. A refused sweep destroys
	// NOTHING — the count is the population that survived, not a subset of
	// Destroyed, and the two are never both incremented by one pass. It is
	// reported here, in the refusal log line, and in the completion log line
	// because a bound nobody can read is not a bound.
	Refused int `json:"refused,omitempty"`
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
	// REV-BUNKER-P1-PATCH: the sweep guard's state is announced at every
	// boot, so an operator can see whether the boot-time bulk-destroy guard
	// is armed and where its threshold sits without reading the source —
	// and so the ONE configuration that turns it off is loud. A guard whose
	// state is not reported is not a guard (the same law that produced the
	// counters on this pass).
	m.logGuardState(rep.Mode)
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
		// REV-BUNKER-P1-PATCH: the sweep-level guard runs BEFORE the first
		// per-orphan decision, so a refused pass touches nothing at all —
		// no destroy, no adopt, no registry write — and counts the
		// population it withheld. Both entry points (Reconcile,
		// ReconcileStartup) reach the walk through this one closure, which
		// is what keeps the runtime path and the startup path from
		// disagreeing about the guard.
		if refuse, withheld, reason := m.sweepRefusal(list); refuse {
			final.Refused = withheld
			m.logSweepRefused(withheld, reason)
			return final
		}
		for _, sa := range list {
			// Classification BEFORE the mode branch. Three dispositions
			// (REV-BUNKER-002):
			//
			//  - FOREIGN: an orphan whose persisted ports lie outside this
			//    daemon's pool, or whose ownership marker names ANOTHER daemon
			//    instance (DF-BUNKER-18), is skipped — never destroyed or
			//    adopted here, not even in destroy mode.
			//  - UNPROVEN: an orphan whose ownership/port metadata is MISSING or
			//    UNREADABLE is skipped and counted, never destroyed. Unreadable
			//    metadata is indistinguishable from another daemon's root-only
			//    agent; destroying it is fail-open data loss.
			//  - OURS: everything else takes the ordinary adopt/destroy decision,
			//    fail-closed rules included (malformed-but-readable metadata and
			//    in-pool-but-unaligned ranges keep their destroyed treatment).
			switch disposition, start, end, cause := m.classifyOrphan(sa); disposition {
			case orphanForeign:
				m.logForeignOrphanSkip(sa, start, end)
				final.Foreign++
				continue
			case orphanUnproven:
				m.logUnprovenOrphanSkip(sa, cause)
				final.Unproven++
				continue
			case orphanOurs:
				// fall through to the adopt/destroy decision below
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
				"unproven", final.Unproven,
				"refused", final.Refused,
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

// sweepRefusal is the SWEEP-LEVEL fail-closed guard (REV-BUNKER-P1-PATCH).
//
// The per-case guards around the orphan walk each decide about ONE orphan:
// foreign-orphan classification (DF-BUNKER-18), exact-port-or-nothing
// adoption, archive-before-delete (DF-BUNKER-33), the live-process gate. All
// of them are correct, and none of them can see the pass as a whole — which is
// the shape that reached another deployment's agents: a daemon booted with a
// registry file that did not exist (Open fabricates an empty one, so the
// "refuses to start when it cannot open the registry" rule never fired),
// replayed a live set of ZERO, and therefore recognised every bunker-* user on
// the host as an orphan of its own. Run as root — the documented deployment —
// that walk archives each home and deletes the user.
//
// The guard asks about PROVENANCE, not per-user guilt: when the durable
// registry cannot vouch for this host at all AND the pass would remove more
// than the configured limit of unknown users, the correct action is to remove
// NOTHING, count the refusal, and say what to do about it.
//
// It is deliberately blind to the per-orphan classification, which is what
// kept it standing while REV-BUNKER-002 (orphan classification fails OPEN when
// the ownership/port metadata is missing or unreadable) was open — and what
// keeps it standing independently now that REV-BUNKER-002 has closed that gap
// (missing/unreadable metadata is classified UNPROVEN and skipped, never
// destroyed): the guard counts what the walk was ABOUT to act on, i.e. the
// classification's own output. A fail-open classification therefore makes the
// count LARGER and the guard trip SOONER — that defect cannot defeat the guard
// by under-reporting, which is the only direction in which it could have
// helped it.
//
// Adoption is untouched: adopting re-registers an orphan and deletes nothing,
// so the mass-destroy shape does not exist in that mode.
func (m *AgentManager) sweepRefusal(orps []SystemAgent) (refuse bool, withheld int, reason string) {
	rc := &m.cfg.Agent.Reconciliation
	if len(orps) == 0 || !rc.SweepGuardEnabled() || !m.registryIsUnproven() {
		return false, 0, ""
	}
	// Only the destroy path can delete; anything else is unguarded by
	// design, which is what keeps adopt mode byte-for-byte unchanged.
	if rc.ModeOrDestroy() != config.ReconcileModeDestroy {
		return false, 0, ""
	}
	limit := rc.UnprovenOrphanLimit
	if len(orps) <= limit {
		return false, 0, ""
	}
	live, created := 0, false
	if m.registry != nil {
		live, created = m.registry.LiveCount(), m.registry.CreatedThisBoot()
	}
	return true, len(orps), fmt.Sprintf(
		"the durable registry cannot vouch for this host (replayed live=%d, registry file created by this boot=%t) "+
			"while the orphan walk found %d unknown bunker-* users, above the limit of %d",
		live, created, len(orps), limit)
}

// registryIsUnproven reports whether the durable registry has NO standing to
// describe this host: either it replayed an EMPTY live set, or the file itself
// was created by this boot (see registry.Store.CreatedThisBoot). Both mean the
// same thing to a destructive walk — "no agents" here is not evidence about
// the host, it is the absence of evidence — and both are the reproduced
// incident's precondition.
func (m *AgentManager) registryIsUnproven() bool {
	if m.registry == nil {
		// No registry at all: reconcile() never reaches the orphan walk in
		// that case, and the guard must not claim provenance it has no
		// source for.
		return false
	}
	return m.registry.LiveCount() == 0 || m.registry.CreatedThisBoot()
}

// logGuardState announces the sweep guard once per reconciliation. It is a
// WARNING when the guard is off, because that single line is the difference
// between "a mass sweep can only happen on proven state" and "nothing stands
// between this boot and another deployment's agents".
func (m *AgentManager) logGuardState(mode string) {
	rc := &m.cfg.Agent.Reconciliation
	if rc.SweepGuardEnabled() {
		m.logger.Info("agent reconciliation sweep guard armed",
			"guard", "orphan_sweep_guard",
			"enabled", true,
			"mode", mode,
			"unproven_orphan_limit", rc.UnprovenOrphanLimit,
			"refuses", "an unproven sweep (empty replayed live set, or a registry file created by this boot) "+
				"that would destroy more than the limit")
		return
	}
	if mode != config.ReconcileModeDestroy {
		// Adopt mode deletes nothing, so the guard is irrelevant here and
		// a warning would be noise.
		return
	}
	m.logger.Warn("agent reconciliation sweep guard DISABLED — an unproven destroy sweep is NOT bounded",
		"guard", "orphan_sweep_guard",
		"enabled", false,
		"mode", mode,
		"unproven_orphan_limit", rc.UnprovenOrphanLimit)
}

// logSweepRefused emits the ONE loud, actionable line a refused sweep gets. It
// names the population that survived, why the registry could not vouch for it,
// and both ways out — the operator's next action is in the line itself,
// because a refusal that does not say what to do is indistinguishable from a
// hang.
func (m *AgentManager) logSweepRefused(withheld int, reason string) {
	path := m.cfg.Agent.Registry.Path
	if m.registry != nil {
		path = m.registry.Path()
	}
	m.logger.Error("registry reconcile: REFUSING to destroy unproven orphans — NOTHING was destroyed",
		"action", "refuse",
		"guard", "orphan_sweep_guard",
		"refused_orphans", withheld,
		"unproven_orphan_limit", m.cfg.Agent.Reconciliation.UnprovenOrphanLimit,
		"reason", reason,
		"mode", config.ReconcileModeDestroy,
		"registry_path", path,
		"remedy", fmt.Sprintf(
			"NOTHING was destroyed. First check whether the durable registry was lost: if it was, restore %s "+
				"(or its .1/.2/.3 backups) so bunkerd can recognise its own agents — do NOT raise the limit, that path "+
				"deletes live agents. If instead these are leftover test/battery users and the registry is intact, "+
				"restart bunkerd with agent.reconciliation.unproven_orphan_limit raised above %d "+
				"(env BUNKERD_AGENT_RECONCILIATION_UNPROVEN_ORPHAN_LIMIT), or accept the whole population being "+
				"removed by setting agent.reconciliation.orphan_sweep_guard_disabled: true.",
			path, withheld))
}

// orphanDisposition classifies what this daemon should do with an orphan
// observed on the host (REV-BUNKER-002). Three outcomes, each a different
// action in the orphan walk: skip-and-count-as-foreign, skip-and-count-as-
// unproven, or take the ordinary adopt/destroy decision.
type orphanDisposition int

const (
	orphanOurs     orphanDisposition = iota // this daemon's agent (adopt/destroy)
	orphanForeign                           // another daemon's agent (skip)
	orphanUnproven                          // metadata missing/unreadable (skip)
)

// classifyOrphan decides an orphan's disposition from its persisted metadata.
// It extends orphanIsForeign's two-way foreign/ours decision with a THIRD
// outcome — UNPROVEN — so the walk never destroys what it cannot classify.
//
// Precedence:
//
//  1. READABLE OWNERSHIP MARKER — when `<home>/.bunker/owner` carries a
//     non-empty instance id and this daemon has an identity of its own, the
//     marker decides: another instance's id is FOREIGN (checked FIRST and
//     independently of the port range, so it also holds for overlapping pools
//     and missing/unreadable port metadata — the shapes the port test cannot
//     classify); this daemon's id is OURS and the port test is deliberately
//     not applied (a disjoint range on an agent we stamped is our own leftover
//     from an older geometry, destroyed by the daemon that owns it);
//
//  2. UNREADABLE OWNERSHIP MARKER — a marker that exists but cannot be read is
//     UNPROVEN: it is indistinguishable from another daemon's root-only agent,
//     so destroying it is fail-open data loss and the agent is skipped;
//
//  3. NO USABLE MARKER (missing, empty, or this daemon has no identity of its
//     own) — the legacy port test, now distinguishing three outcomes: a
//     persisted range ENTIRELY disjoint from this daemon's pool is FOREIGN; a
//     readable in-pool range is OURS; a MALFORMED range is OURS (fail-closed,
//     destroyed exactly as before — a readable garbage file is this daemon's
//     own corrupted write); a MISSING or UNREADABLE range is UNPROVEN.
//
// The pool line recorded inside the ownership marker is NOT part of this
// decision: it is operator information. A legitimate pool-geometry change on
// THIS daemon would otherwise reclassify its own agents as foreign and leak
// them forever.
func (m *AgentManager) classifyOrphan(sa SystemAgent) (disposition orphanDisposition, start, end uint32, cause string) {
	ownerID, _, _, ownerState := readPersistedOwnerState(sa.Home)
	if ownerState == metadataValid && m.instanceID != "" {
		// The marker decides ownership. The reported range stays the agent's
		// persisted ports (informational; 0-0 when unreadable).
		start, end, _ = readPersistedPortRange(sa.Home)
		if ownerID != m.instanceID {
			return orphanForeign, start, end, "ownership marker names another daemon instance"
		}
		return orphanOurs, start, end, ""
	}
	if ownerState == metadataUnreadable {
		return orphanUnproven, 0, 0, "ownership marker unreadable"
	}

	// No usable marker (missing or empty, or this daemon has no identity of
	// its own): the legacy port test decides, now distinguishing malformed
	// from missing/unreadable.
	start, end, portState := readPersistedPortRangeState(sa.Home)
	switch portState {
	case metadataValid:
		if m.portAlloc == nil {
			return orphanOurs, start, end, ""
		}
		poolStart, poolEnd := m.portAlloc.Bounds()
		if end < poolStart || start > poolEnd {
			return orphanForeign, start, end, "persisted range is disjoint from this daemon's pool"
		}
		return orphanOurs, start, end, ""
	case metadataMalformed:
		return orphanOurs, 0, 0, ""
	case metadataMissing:
		return orphanUnproven, 0, 0, "port metadata missing"
	default:
		return orphanUnproven, 0, 0, "port metadata unreadable"
	}
}

// orphanIsForeign is the legacy two-state wrapper over classifyOrphan: it
// reports only whether the orphan is FOREIGN (unproven and ours both read as
// "not foreign"). Retained for the classifier tests that pin the pre-REV-002
// boundary.
func (m *AgentManager) orphanIsForeign(sa SystemAgent) (foreign bool, start, end uint32) {
	disposition, start, end, _ := m.classifyOrphan(sa)
	return disposition == orphanForeign, start, end
}

// logUnprovenOrphanSkip emits the loud skip warning for an orphan this daemon
// cannot classify: it always names the agent and the cause (which metadata was
// missing or unreadable), so an operator can see WHY nothing was destroyed.
func (m *AgentManager) logUnprovenOrphanSkip(sa SystemAgent, cause string) {
	m.logger.Warn("registry reconcile: skipping unproven orphan agent",
		"action", "skip",
		"agent_id", sa.AgentID,
		"system_user", sa.Username,
		"cause", cause,
		"reason", "ownership/port metadata missing or unreadable: cannot prove this agent is ours "+
			"and cannot prove it is foreign — never destroy what cannot be classified")
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

// destroyOrphan removes an orphan through the manager's destroy path. The
// seam (m.destroyAgent) keeps its 3-argument shape — orphan cleanup runs under
// the configured destroy policy, with no operator to take a per-request
// archive opt-out from (DF-BUNKER-81).
func (m *AgentManager) destroyOrphan(ctx context.Context, agentID string) error {
	if fn := m.destroyAgent; fn != nil {
		_, err := fn(ctx, agentID, true)
		return err
	}
	_, err := m.Destroy(ctx, agentID, true)
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
