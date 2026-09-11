package agent

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/deployBunker/bunker/internal/config"
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

	cmdArgs := buildRunAgentArgs(agentID, u.Uid, u.Gid, unitName, req.GetCommand(), req.GetArgs(), req.GetEnv(), limits, m.cfg != nil && m.cfg.Containment.Disclosure)
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
//
// The supplied command is wrapped in `sh -c '. <envFile> 2>/dev/null && exec "$@"' --`
// so that env vars injected via `bunker env set` are visible at exec time
// (envFile is sourced inside the wrapper shell). The `--` separator prevents
// subsequent args from being interpreted as $0 by the wrapper.
//
// disclosure (GAP-067): when true, BUNKER_SANDBOX=1 is added to the unit's
// environment so detached sessions disclose the managed sandbox like every
// other exec mode. When false the argv is byte-identical to pre-GAP-067.
func buildRunAgentArgs(agentID, uid, gid, unitName, command string, args []string, envOverrides map[string]string, limits *v1.ResourceLimits, disclosure bool) []string {
	userHome := "/home/bunker-" + agentID
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	envFile := fmt.Sprintf("/run/bunker/%s/env", agentID)

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
		"BUNKER_ENV_FILE=" + envFile,
	}
	// GAP-067 containment disclosure: detached sessions get the same
	// BUNKER_SANDBOX=1 as shell/raw/script execs. Set only when the daemon
	// runs with containment.disclosure enabled; absent otherwise. The
	// disclosure value is ADMIN-CONTROLLED: when enabled, an agent-supplied
	// env override for the same key can neither suppress nor rewrite it
	// (overridden below); when disabled there is nothing to protect.
	if disclosure {
		env = append(env, config.ContainmentSandboxEnv)
	}
	for k, v := range envOverrides {
		if disclosure && k == config.ContainmentSandboxEnvKey {
			// Admin disclosure value wins: ignore ANY agent-supplied
			// BUNKER_SANDBOX entry (rewrite or suppression attempt) when
			// disclosure is enabled.
			continue
		}
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

	// Wrap command in a shell that sources the env file (when present) and then
	// execs the real command. `--` ends the wrapper's own argv so subsequent
	// args are passed as $1, $2, ... to the inner exec. The [ -f ] guard keeps
	// a fresh agent (no env file yet) from short-circuiting the exec, and
	// set -a exports injected vars to the exec'd process.
	cmdArgs = append(cmdArgs, "sh", "-c", fmt.Sprintf("set -a; [ -f %s ] && . %s 2>/dev/null; set +a; exec \"$@\"", envFile, envFile), "--", command)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}
