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
	// Purged counts registry records with no system user (stale).
	Purged int
	// Adopted counts orphans adopted into the tracker + registry.
	Adopted int
	// Destroyed counts orphans removed from the host.
	Destroyed int
}

// Reconcile replays the durable registry against system state before the
// daemon serves traffic. It performs four things, each logged on ONE startup
// line per agent:
//
//  1. restores replayed live records whose system user still exists into the
//     tracker together with their exact persisted port reservation. A record
//     whose reservation cannot be re-established exactly is NOT served:
//     reconciliation fails closed and force-destroys that agent (see
//     failClosedRestore);
//  2. purges registry records whose system user is gone (stale);
//  3. handles orphans — bunker-* users the registry does not know — by
//     destroying them (default) or adopting them, per reconciliation.mode.
//     Adoption requires readable, valid, free port metadata: an orphan that
//     cannot be adopted with its exact reservation is destroyed instead;
//  4. unblocks the TTL reaper (which waits for this to finish).
//
// It never fails hard: every action is logged and the daemon keeps running.
// Without a registry (disabled, or unavailable) it only unblocks the reaper.
func (m *AgentManager) Reconcile(ctx context.Context) ReconcileReport {
	defer m.reconcileOnce.Do(func() { close(m.reconcileDone) })

	rep := ReconcileReport{Mode: m.cfg.Agent.Reconciliation.ModeOrDestroy()}
	if err := m.cfg.Agent.Reconciliation.Validate(); err != nil {
		// Validation happens at config load; if a caller built a config by
		// hand, fall back to the documented default rather than guessing.
		m.logger.Warn("invalid reconciliation mode, using default", "mode", rep.Mode, "error", err)
		rep.Mode = config.ReconcileModeDestroy
	}

	if m.registry == nil {
		m.logger.Info("registry reconcile skipped — durable registry unavailable",
			"reason", registryUnavailableReason(m))
		return rep
	}

	rep.ReplayedLive = m.registry.LiveCount()
	rep.ReplayedKnown = m.registry.KnownCount()

	systemAgents, err := m.listSystemAgents()
	if err != nil {
		// A failed probe must not be read as "no agents exist": that would
		// purge every live record and destroy every orphan in one sweep.
		m.logger.Error("registry reconcile: cannot enumerate system agents; skipping reconciliation", "error", err)
		return rep
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
			m.logger.Info("registry reconcile: purged stale registry record",
				"action", "purge", "agent_id", rec.AgentID, "reason", "no system user")
			continue
		}
		if err := m.restoreAgent(rec); err != nil {
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
	for _, sa := range systemAgents {
		if m.registry.Get(sa.AgentID) != nil || handled[sa.AgentID] {
			continue // known live agent (handled above) or already handled
		}
		if rep.Mode == config.ReconcileModeAdopt {
			if err := m.adoptAgent(sa); err != nil {
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
				rep.Destroyed++
				m.logger.Info("registry reconcile: destroyed orphan agent",
					"action", "destroy", "agent_id", sa.AgentID, "system_user", sa.Username)
				continue
			}
			rep.Adopted++
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
		rep.Destroyed++
		m.logger.Info("registry reconcile: destroyed orphan agent",
			"action", "destroy", "agent_id", sa.AgentID, "system_user", sa.Username)
	}
	return rep
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
func (m *AgentManager) adoptAgent(sa SystemAgent) error {
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
	rec := &resource.AgentRecord{
		AgentID:           sa.AgentID,
		Status:            "running",
		Limits:            m.defaultLimits(),
		CreatedAt:         homeCreatedAt(sa.Home),
		SshPrivateKeyPath: m.sshKeyPath(sa.AgentID),
		PortRangeStart:    start,
		PortRangeEnd:      end,
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
