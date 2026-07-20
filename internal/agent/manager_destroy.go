// Package agent manages agent lifecycle: create users, generate SSH keys, start dockerd.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func (m *AgentManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	// Step 0: validate agent_id
	if agentID == "" || !validAgentID.MatchString(agentID) {
		return &v1.DestroyAgentResponse{AgentId: agentID, Status: "error"},
			fmt.Errorf("invalid agent_id %q", agentID)
	}

	m.logger.Info("destroying agent", "agent_id", agentID)

	// Step 0.5: Remove user slice cgroup drop-in so stale limits don't
	// accumulate after the agent is destroyed.
	removeUserSliceLimits(ctx, agentID, m.logger)

	// Step 1: Stop the dockerd systemd user unit
	unitName := "bunker-docker-" + agentID
	username := "bunker-" + agentID

	// Look up the UID before userdel so we can clean up the actual rootless socket
	// created under /run/user/<uid>.
	var uid string
	if u, err := user.Lookup(username); err == nil {
		uid = u.Uid
	} else {
		m.logger.Warn("cannot lookup user before destroy", "username", username, "error", err)
	}

	// The dockerd unit was started via systemd-run --user, so it runs under
	// the agent's user session. systemctl --user from the root foreman session
	// targets the wrong user manager. We must either:
	//   (a) use systemctl --user --machine=<user>@.host, or
	//   (b) find the dockerd PID and kill it directly.
	// Option (b) is simpler and avoids DBus/machined dependencies.
	if err := stopDockerdDirect(ctx, username, unitName, m.logger); err != nil {
		m.logger.Warn("direct dockerd stop failed", "unit", unitName, "error", err)
		// Fallback: try systemctl --user (may work if user linger is enabled)
		cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", unitName)
		if out, err := cmd.CombinedOutput(); err != nil {
			m.logger.Warn("systemctl stop failed (may not exist)", "unit", unitName, "error", err, "output", string(out))
		}
	}

	// Step 2: Disable the unit (prevent auto-restart)
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "disable", unitName)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("systemctl disable failed", "unit", unitName, "error", err, "output", string(out))
	}

	// Step 3: Remove the Linux user
	cmd = exec.CommandContext(ctx, "userdel", "-rf", username)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Check if user doesn't exist (already destroyed)
		if !force {
			m.tracker.Unregister(agentID)
			return &v1.DestroyAgentResponse{AgentId: agentID, Status: "not_found"},
				fmt.Errorf("userdel %s failed: %w (output: %s)", username, err, string(out))
		}
		// Force mode: log and continue even if userdel fails
		m.logger.Warn("userdel failed in force mode", "username", username, "error", err, "output", string(out))
	}

	// Step 4: Clean up /run/bunker/<id>/ directory
	runDir := fmt.Sprintf("/run/bunker/%s", agentID)
	if err := os.RemoveAll(runDir); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove run dir", "dir", runDir, "error", err)
	}

	// Step 4a: Clean up the actual rootless socket under /run/user/<uid>. A stale
	// socket here would prevent the next agent that reuses this UID from binding.
	if uid != "" {
		actualSock := fmt.Sprintf("/run/user/%s/docker.sock", uid)
		if err := os.Remove(actualSock); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove actual docker socket", "path", actualSock, "error", err)
		}
	}

	// Step 4.5: Clean up persisted SSH key
	sshKeyPath := filepath.Join(m.cfg.Agent.SSHDir, agentID)
	if err := os.Remove(sshKeyPath); err != nil && !os.IsNotExist(err) {
		m.logger.Warn("failed to remove ssh key", "path", sshKeyPath, "error", err)
	}

	if m.tunnelMgr != nil {
		if err := m.tunnelMgr.Stop(agentID); err != nil {
			m.logger.Warn("tunnel stop failed", "agent_id", agentID, "error", err)
		}
	}

	if m.tailscaleMgr != nil {
		if err := m.tailscaleMgr.Stop(agentID); err != nil {
			m.logger.Warn("tailscale stop failed", "agent_id", agentID, "error", err)
		}
	}

	m.tracker.Unregister(agentID)

	if m.portAlloc != nil {
		m.portAlloc.Free(agentID)
		m.logger.Info("freed port range", "agent_id", agentID)
	}

	m.logger.Info("agent destroyed", "agent_id", agentID)
	return &v1.DestroyAgentResponse{AgentId: agentID, Status: "destroyed"}, nil
}
