// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// ── DF-BUNKER-24: persisted agent private keys ─────────────────────────────
//
// Spawn writes the agent's private key to cfg.Agent.SSHDir/<agent-id> (Step
// 4c, manager_spawn.go) and Destroy removes it as its last filesystem step.
// Every other path that concludes a MANAGED agent is gone has to converge on
// the same state, or the key outlives the agent: a stale key is not only
// disk residue (inventory.go counts it as OrphanKeys), it is a live
// credential for the NEXT agent that reuses the same id.
//
// The helpers below are the single place that derives and removes that path,
// so no cleanup path can drift into its own (and potentially unsafe)
// filepath arithmetic. Scope rules, all enforced here rather than at the
// call sites:
//
//   - the id must match validAgentID (no separators, no dots, no uppercase),
//     so the derived path can never leave the SSH key directory;
//   - an empty cfg.Agent.SSHDir is refused outright — filepath.Join("", id)
//     is the BARE id, i.e. relative to the daemon's working directory;
//   - only a regular file is removed: a directory, device or symlink that
//     happens to carry an agent id is not key material and is left alone.
//
// Both helpers are best-effort and idempotent like the rest of the destroy
// path: a missing key is a no-op, and a failure never changes the outcome of
// the caller's destroy/purge decision.

// managedAgentKeyPath returns the path of the persisted private key for
// agentID, or an error explaining why the id/directory is not a managed agent
// key location. It is the only place the key path is derived.
func (m *AgentManager) managedAgentKeyPath(agentID string) (string, error) {
	if !validAgentID.MatchString(agentID) {
		return "", fmt.Errorf("not a managed agent id: %q", agentID)
	}
	if m.cfg.Agent.SSHDir == "" {
		return "", fmt.Errorf("ssh key dir is not configured")
	}
	return filepath.Join(m.cfg.Agent.SSHDir, agentID), nil
}

// removeAgentSSHKey removes agentID's persisted private key. It is scoped to
// cfg.Agent.SSHDir/<agent-id> and idempotent: an already-removed key, or an
// id/directory this daemon cannot own, is not an error worth failing a
// destroy for — the returned error is for the caller's log line.
func (m *AgentManager) removeAgentSSHKey(agentID string, logger *slog.Logger) error {
	path, err := m.managedAgentKeyPath(agentID)
	if err != nil {
		return err
	}
	// Lstat: a symlink carrying an agent id is NOT key material, and a path
	// this daemon cannot characterize must never be followed.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // idempotent: the key is already gone
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular file %s (mode %s)", path, info.Mode())
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	logger.Debug("removed persisted ssh key", "agent_id", agentID, "path", path)
	return nil
}

// removeAgentSSHKeyBestEffort is removeAgentSSHKey plus the destroy path's
// best-effort policy: a failure is logged and never propagated, exactly like
// the other teardown steps (isolation removal, linger disable, tunnel stop).
func (m *AgentManager) removeAgentSSHKeyBestEffort(agentID string, logger *slog.Logger) {
	if err := m.removeAgentSSHKey(agentID, logger); err != nil {
		logger.Warn("failed to remove ssh key", "agent_id", agentID, "error", err)
	}
}
