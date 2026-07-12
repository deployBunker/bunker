package agent

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// RunAgent starts a detached command in the agent's environment as a transient
// systemd unit. The unit survives the RPC session and runs as the agent user.
func (m *AgentManager) RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error) {
	agentID := req.GetAgentId()
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if req.GetCommand() == "" {
		return nil, fmt.Errorf("command is required")
	}
	if !req.GetDetach() {
		return nil, fmt.Errorf("RunAgent only supports detached mode; use ExecAgent for synchronous runs")
	}

	rec := m.tracker.Get(agentID)
	if rec == nil {
		return nil, fmt.Errorf("agent %q not found", agentID)
	}

	username := "bunker-" + agentID
	u, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("lookup user %s: %w", username, err)
	}

	runID, err := generateUUIDv4()
	if err != nil {
		return nil, fmt.Errorf("generate run_id: %w", err)
	}
	unitSuffix := strings.SplitN(runID, "-", 2)[0]
	unitName := fmt.Sprintf("bunker-run-%s-%s", agentID, unitSuffix)

	limits := (*v1.ResourceLimits)(nil)
	if rec.Limits != nil {
		limits = rec.Limits
	}

	cmdArgs := buildRunAgentArgs(agentID, u.Uid, u.Gid, unitName, req.GetCommand(), req.GetArgs(), req.GetEnv(), limits)
	cmd := exec.CommandContext(ctx, "systemd-run", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemd-run failed: %w (output: %s)", err, string(out))
	}

	return &v1.RunAgentResponse{
		RunId:    runID,
		Status:   "running",
		ExitCode: -1,
		UnitName: unitName,
	}, nil
}

// buildRunAgentArgs constructs the systemd-run argument list for a detached
// agent run. It is a pure function so it can be unit-tested without an actual
// system user or systemd.
func buildRunAgentArgs(agentID, uid, gid, unitName, command string, args []string, envOverrides map[string]string, limits *v1.ResourceLimits) []string {
	userHome := "/home/bunker-" + agentID
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")

	cmdArgs := []string{
		"--system",
		"--unit=" + unitName,
		"--uid=" + uid,
		"--gid=" + gid,
		"--property=PAMName=login",
	}

	if limits != nil {
		if limits.CpuQuota > 0 {
			cmdArgs = append(cmdArgs, fmt.Sprintf("--property=CPUQuota=%d%%", int(limits.CpuQuota*100)))
		}
		if limits.MemoryMaxBytes > 0 {
			cmdArgs = append(cmdArgs, fmt.Sprintf("--property=MemoryMax=%d", limits.MemoryMaxBytes))
		}
		if limits.DiskMaxBytes > 0 {
			cmdArgs = append(cmdArgs, fmt.Sprintf("--property=LimitFSIZE=%d", limits.DiskMaxBytes))
		}
	}

	env := []string{
		"PATH=" + agentBinPath + ":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + userHome,
		"USER=" + "bunker-" + agentID,
		"DOCKER_HOST=unix://" + dockerSockPath,
		"TMPDIR=" + tmpDir,
	}
	for k, v := range envOverrides {
		prefix := k + "="
		found := false
		for i, e := range env {
			if strings.HasPrefix(e, prefix) {
				env[i] = prefix + v
				found = true
				break
			}
		}
		if !found {
			env = append(env, prefix+v)
		}
	}
	for _, e := range env {
		cmdArgs = append(cmdArgs, "--setenv="+e)
	}

	cmdArgs = append(cmdArgs, command)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}
