package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// agentContainerName is the deterministic name of the agent's own container.
// The spec grammar (internal/imagespec) cannot influence container naming,
// flags, or mounts — teardown is structural so it can never be customized
// into an escape hatch.
func agentContainerName(agentID string) string {
	return "bunker-" + agentID
}

// cleanupAgentContainers stops and removes the agent's own container through
// ONLY that agent's rootless socket, before the dockerd stop in Destroy. It
// never touches the host docker daemon, other agents' sockets, or anything
// beyond the one deterministic container name.
//
// Both stop and rm are best-effort: an agent whose daemon already died (crash,
// TTL reap, failed spawn) has nothing to clean up, and Destroy must still
// proceed to user removal. Errors are logged and returned only for callers
// that want them (tests).
func cleanupAgentContainers(ctx context.Context, agentID string, force bool, logger *slog.Logger) error {
	if !validAgentID.MatchString(agentID) {
		return fmt.Errorf("invalid agent id %q", agentID)
	}
	sock := "/run/bunker/" + agentID + "/docker.sock"
	name := agentContainerName(agentID)

	stopSub := []string{"stop", "-t", "5", name}
	rmSub := []string{"rm", name}
	if force {
		stopSub = nil // rm -f implies stop
		rmSub = []string{"rm", "-f", name}
	}

	if stopSub != nil {
		if out, err := exec.CommandContext(ctx, "docker",
			append([]string{"--host", "unix://" + sock}, stopSub...)...,
		).CombinedOutput(); err != nil && !isNoContainerErr(string(out)) {
			logger.Warn("agent container stop failed (continuing)", "agent_id", agentID, "error", err, "output", strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.CommandContext(ctx, "docker",
		append([]string{"--host", "unix://" + sock}, rmSub...)...,
	).CombinedOutput(); err != nil && !isNoContainerErr(string(out)) {
		logger.Warn("agent container rm failed (continuing)", "agent_id", agentID, "error", err, "output", strings.TrimSpace(string(out)))
		return fmt.Errorf("remove agent container %s: %w (output: %s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isNoContainerErr recognizes docker's "no such container" responses so
// idempotent destroy does not warn on already-gone containers.
func isNoContainerErr(out string) bool {
	o := strings.ToLower(out)
	return strings.Contains(o, "no such container") || strings.Contains(o, "cannot connect") || strings.Contains(o, "no such object")
}
