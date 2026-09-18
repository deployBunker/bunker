// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/resource"
)

// ── Status vocabulary ─────────────────────────────────────────
//
// resource.AgentRecord.Status already documents "running", "stopped" and
// "failed" (internal/resource/tracker.go). Lifecycle control (GAP-071) uses
// that vocabulary as-is — no second status language is invented here.

const (
	// StatusRunning is a live agent (session units armed).
	StatusRunning = "running"
	// StatusStopped is an agent whose session units and processes were
	// stopped but whose user, home and port range were KEPT.
	StatusStopped = "stopped"
)

// Response status strings for the StopAgent / StartAgent / RestartAgent RPCs.
const (
	StatusStoppedValue       = "stopped"
	StatusAlreadyStopped     = "already_stopped"
	StatusStartedValue       = "started"
	StatusAlreadyRunning     = "already_running"
	StatusRestarted          = "restarted"
	StatusNotFound           = "not_found"
	StatusInvalidValue       = "error"
	stoppedToken             = "agent_stopped"
	defaultLifecycleTTLValue = 6 * time.Hour
)

// ErrAgentStopped is the sentinel distinguishing "this agent exists but is
// stopped" from "this agent does not exist". Exec/Run/Heartbeat paths wrap it
// with the agent id (see StoppedError); the server maps it to
// CodeFailedPrecondition — never CodeNotFound, so a client can react by
// starting or restarting the agent instead of giving up on it.
var ErrAgentStopped = errors.New(stoppedToken)

// StoppedError builds the wrapped sentinel error for a stopped agent. The
// message carries the stable token "agent_stopped" (its own text plus the
// wrapped ErrAgentStopped, so errors.Is works) and names the recovery command.
func StoppedError(agentID string) error {
	return fmt.Errorf("%w: agent %q is stopped; run 'bunker start %s'", ErrAgentStopped, agentID, agentID)
}

// StoppedStatusError returns StoppedError(agentID) when rec is a stopped
// agent record, and nil for every other record — including a nil record, which
// callers keep handling as their own not-found path. It is the single
// predicate every stopped-agent guard uses, so the three RPCs cannot drift.
func StoppedStatusError(rec *resource.AgentRecord, agentID string) error {
	if rec != nil && rec.Status == StatusStopped {
		return StoppedError(agentID)
	}
	return nil
}

// IsAgentStopped reports whether an error is (or wraps) the stopped sentinel.
func IsAgentStopped(err error) bool { return errors.Is(err, ErrAgentStopped) }

// ── Host-command seams ────────────────────────────────────────
//
// Every host command the lifecycle path runs goes through one of these
// package-level seams (the disableUserUnit pattern in manager_destroy.go), so
// the stop/start/restart paths are exercised WITHOUT root and WITHOUT touching
// a live host. Production code never swaps them.

// stopUserUnit runs `systemctl --user stop <unit>` and returns its combined
// output. It is the "session unit" stop: the per-agent dockerd unit is a
// systemd unit started for the agent user, and this is the user-manager form
// of stopping it.
var stopUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).CombinedOutput()
}

// startUserUnit runs `systemctl --user start <unit>` and returns its combined
// output — the re-arm half of start/restart.
var startUserUnit = func(ctx context.Context, unit string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", "--user", "start", unit).CombinedOutput()
}

// listUserUnits runs `systemctl --user list-units --all --no-legend <pattern>`
// and returns the unit names it printed. It discovers the agent's own
// transient `bunker-run-<id>-*` units so stop tears down the detached runs
// belonging to THIS agent only.
var listUserUnits = func(ctx context.Context, pattern string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", "--user", "list-units", "--all", "--no-legend", pattern).CombinedOutput()
}

// agentDockerCLI runs a docker CLI command against the AGENT's own rootless
// socket (never the host daemon, never another agent's socket). GAP-071 uses
// it for container-mode stop/start: the container is stopped and can be
// started again — it is never removed (that is Destroy's job).
var agentDockerCLI = func(ctx context.Context, agentID string, args ...string) ([]byte, error) {
	sock := "/run/bunker/" + agentID + "/docker.sock"
	full := append([]string{"--host", "unix://" + sock}, args...)
	return exec.CommandContext(ctx, "docker", full...).CombinedOutput()
}

// terminateAgentProcesses stops the agent's dockerd/rootlesskit processes and
// waits (bounded) for them to exit. Default is the destroy path's primitives:
// stopDockerdDirect (SIGTERM → SIGKILL) plus waitAgentProcessesExit. Seam so
// tests never signal a live host process.
var terminateAgentProcesses = func(ctx context.Context, username, unitName string, logger *slog.Logger) {
	if err := stopDockerdDirect(ctx, username, unitName, logger); err != nil {
		logger.Debug("no dockerd process to stop", "user", username, "error", err)
	}
	waitAgentProcessesExit(ctx, username, logger)
}

// ── Lifecycle control ─────────────────────────────────────────

// StopAgent stops an agent's session units and processes while KEEPING the
// agent: the Linux user, home directory, agent container and allocated port
// range all survive, and the tracker record is kept with status "stopped".
//
// Outcomes:
//   - unknown / never-registered agent id → status "not_found" (+ error);
//   - already stopped                    → status "already_stopped" (nil
//     error: stopping a stopped agent is a no-op, i.e. idempotent);
//   - running                            → the units/processes are stopped,
//     status "stopped".
//
// Nothing here runs userdel, frees the port range, removes the home
// directory, the SSH key, or the agent's container — those are Destroy's
// steps. Restarting a stopped agent is therefore a real recovery path.
func (m *AgentManager) StopAgent(ctx context.Context, agentID string) (*v1.StopAgentResponse, error) {
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return &v1.StopAgentResponse{AgentId: agentID, Status: StatusInvalidValue},
			fmt.Errorf("invalid agent_id %q", agentID)
	}
	rec := m.tracker.Get(agentID)
	if rec == nil {
		// An id the durable store never saw is simply unknown. (A known but
		// destroyed agent is also gone from the tracker; destroy owns that
		// path, so "not_found" stays the honest answer here.)
		return &v1.StopAgentResponse{AgentId: agentID, Status: StatusNotFound},
			fmt.Errorf("agent %q not found", agentID)
	}
	if rec.Status == StatusStopped {
		m.logger.Info("agent already stopped; stop is a no-op", "agent_id", agentID)
		return &v1.StopAgentResponse{AgentId: agentID, Status: StatusAlreadyStopped}, nil
	}

	m.logger.Info("stopping agent", "agent_id", agentID)
	m.stopAgentRuntime(ctx, agentID, m.logger)

	// The tracker record and every resource it owns stay in place; only the
	// status changes. Persist the transition so a daemon restart replays the
	// agent as stopped (GAP-070 registry).
	m.tracker.UpdateStatus(agentID, StatusStopped)
	rec.Status = StatusStopped
	if err := m.persistHeartbeat(agentID, rec.ExpiresAt, StatusStopped); err != nil {
		m.logger.Warn("registry stop append failed", "agent_id", agentID, "error", err)
	}
	m.logger.Info("agent stopped", "agent_id", agentID,
		"port_range_start", rec.PortRangeStart, "port_range_end", rec.PortRangeEnd)
	return &v1.StopAgentResponse{AgentId: agentID, Status: StatusStoppedValue}, nil
}

// stopAgentRuntime performs the host-side stop for one agent: the agent's own
// container (container-mode agents; the container is STOPPED, never removed),
// the agent's session units, and the agent's processes. Every step is
// best-effort and logged — a stop must not fail because one leg was already
// down (that is exactly the wedged-agent case stop exists for).
func (m *AgentManager) stopAgentRuntime(ctx context.Context, agentID string, logger *slog.Logger) {
	username := "bunker-" + agentID
	unitName := "bunker-docker-" + agentID

	if rec := m.tracker.Get(agentID); rec != nil && rec.Image != "" {
		if out, err := agentDockerCLI(ctx, agentID, "stop", "-t", "5", agentContainerName(agentID)); err != nil && !isNoContainerErr(string(out)) {
			logger.Warn("agent container stop failed (continuing stop)", "agent_id", agentID,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	for _, unit := range m.agentSessionUnits(ctx, agentID, unitName, logger) {
		if out, err := stopUserUnit(ctx, unit); err != nil {
			logger.Debug("user unit stop failed (may not be loaded for this caller)",
				"agent_id", agentID, "unit", unit, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}

	terminateAgentProcesses(ctx, username, unitName, logger)
}

// agentSessionUnits returns the systemd user units belonging to one agent: the
// deterministic dockerd unit plus any transient `bunker-run-<id>-*` units the
// agent started through RunAgent. Unit discovery is best-effort — a caller
// without a user session bus (the normal root daemon case) gets just the
// dockerd unit, because that is the unit stop can always name.
func (m *AgentManager) agentSessionUnits(ctx context.Context, agentID, dockerdUnit string, logger *slog.Logger) []string {
	units := []string{dockerdUnit}
	pattern := fmt.Sprintf("bunker-run-%s-*", agentID)
	out, err := listUserUnits(ctx, pattern)
	if err != nil {
		logger.Debug("run-unit discovery unavailable; stopping only the dockerd unit",
			"agent_id", agentID, "pattern", pattern, "error", err, "output", strings.TrimSpace(string(out)))
		return units
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ".service")
		// Only units this agent owns, and never a duplicate of the dockerd
		// unit: the pattern is matched again here so a loosely-matching
		// systemd version cannot make stop touch another agent's unit.
		if !strings.HasPrefix(name, "bunker-run-"+agentID+"-") {
			continue
		}
		units = append(units, name)
	}
	return units
}

// StartAgent re-arms a stopped agent: the user and home still exist, so the
// agent's session is restored (unit re-started; for a container-mode agent the
// kept container is started again through the agent's own socket) and the
// tracker status returns to "running". A running agent reports
// "already_running" and does nothing.
func (m *AgentManager) StartAgent(ctx context.Context, agentID string) (*v1.StartAgentResponse, error) {
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return &v1.StartAgentResponse{AgentId: agentID, Status: StatusInvalidValue},
			fmt.Errorf("invalid agent_id %q", agentID)
	}
	rec := m.tracker.Get(agentID)
	if rec == nil {
		return &v1.StartAgentResponse{AgentId: agentID, Status: StatusNotFound},
			fmt.Errorf("agent %q not found", agentID)
	}
	if rec.Status != StatusStopped {
		m.logger.Info("agent already running; start is a no-op", "agent_id", agentID, "status", rec.Status)
		return &v1.StartAgentResponse{AgentId: agentID, Status: StatusAlreadyRunning}, nil
	}

	m.logger.Info("starting agent", "agent_id", agentID)
	m.startAgentRuntime(ctx, agentID, rec, m.logger)

	m.tracker.UpdateStatus(agentID, StatusRunning)
	rec.Status = StatusRunning
	if err := m.persistHeartbeat(agentID, rec.ExpiresAt, StatusRunning); err != nil {
		m.logger.Warn("registry start append failed", "agent_id", agentID, "error", err)
	}
	m.logger.Info("agent started", "agent_id", agentID)
	return &v1.StartAgentResponse{AgentId: agentID, Status: StatusStartedValue}, nil
}

// startAgentRuntime performs the host-side re-arm for one agent. Best-effort
// and logged, mirroring stopAgentRuntime: a unit that is already active is not
// an error, and the agent's own container (container-mode) is started through
// the agent's own socket only.
func (m *AgentManager) startAgentRuntime(ctx context.Context, agentID string, rec *resource.AgentRecord, logger *slog.Logger) {
	if rec != nil && rec.Image != "" {
		if out, err := agentDockerCLI(ctx, agentID, "start", agentContainerName(agentID)); err != nil && !isNoContainerErr(string(out)) {
			logger.Warn("agent container start failed (continuing start)", "agent_id", agentID,
				"error", err, "output", strings.TrimSpace(string(out)))
		}
	}
	unitName := "bunker-docker-" + agentID
	if out, err := startUserUnit(ctx, unitName); err != nil {
		logger.Debug("user unit start failed (may not be loaded for this caller)",
			"agent_id", agentID, "unit", unitName, "error", err, "output", strings.TrimSpace(string(out)))
	}
}

// RestartAgent stops and starts an agent in one call and RESETS the heartbeat
// expiry to now + the daemon's default TTL, returning the refreshed expiry.
func (m *AgentManager) RestartAgent(ctx context.Context, agentID string) (*v1.RestartAgentResponse, error) {
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return &v1.RestartAgentResponse{AgentId: agentID, Status: StatusInvalidValue},
			fmt.Errorf("invalid agent_id %q", agentID)
	}
	rec := m.tracker.Get(agentID)
	if rec == nil {
		return &v1.RestartAgentResponse{AgentId: agentID, Status: StatusNotFound},
			fmt.Errorf("agent %q not found", agentID)
	}

	m.logger.Info("restarting agent", "agent_id", agentID)
	// Stop + start unconditionally (even when the status already reads
	// "stopped"): the point of restart is to recover a WEDGED session, whose
	// processes may be half-dead while the tracker still says nothing useful.
	m.stopAgentRuntime(ctx, agentID, m.logger)
	m.startAgentRuntime(ctx, agentID, rec, m.logger)

	// A restart re-arms the session, so the TTL clock restarts with it: the
	// expiry is RESET to now+TTL (not merely extended like a heartbeat).
	rec.ExpiresAt = time.Now().Add(m.lifecycleTTL())
	m.tracker.UpdateStatus(agentID, StatusRunning)
	rec.Status = StatusRunning
	if err := m.persistHeartbeat(agentID, rec.ExpiresAt, StatusRunning); err != nil {
		m.logger.Warn("registry restart append failed", "agent_id", agentID, "error", err)
	}
	m.logger.Info("agent restarted", "agent_id", agentID, "expires_at", rec.ExpiresAt)
	return &v1.RestartAgentResponse{
		AgentId:   agentID,
		Status:    StatusRestarted,
		ExpiresAt: rec.ExpiresAt.Format(time.RFC3339),
	}, nil
}

// lifecycleTTL is the TTL a restart resets an agent's heartbeat expiry to: the
// daemon's configured default, or 6h when unset — the same default
// HeartbeatAgent applies when it falls back.
func (m *AgentManager) lifecycleTTL() time.Duration {
	if m.cfg != nil && m.cfg.Agent.DefaultTTL > 0 {
		return m.cfg.Agent.DefaultTTL
	}
	return defaultLifecycleTTLValue
}
