// Package server — bunkerd RPC service implementations.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/apikey"
	"github.com/deployBunker/bunker/internal/auth"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// bunkerdService implements bunkerv1connect.BunkerdHandler.
type bunkerdService struct {
	cfg          *config.Config
	logger       *slog.Logger
	agentMgr     agentManager
	tracker      *resource.Tracker
	tunnelMgr    *tunnel.TunnelManager
	tailscaleMgr *tailscale.TailscaleManager
	keyMgr       *apikey.Manager
	jwtAuth      *auth.JWTAuth
}

// agentManager is the subset of *agent.AgentManager used by bunkerdService.
// It exists primarily to make service-layer tests not require root.
type agentManager interface {
	Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error)
	Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error)
	RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error)
	Stop()
}

// ServerInfo returns information about the bunkerd server.
func (s *bunkerdService) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	hostname, _ := os.Hostname()
	resp := &v1.ServerInfoResponse{
		Hostname:      hostname,
		Version:       "0.1.0",
		UptimeSeconds: 0,
		AgentCount:    s.tracker.Count(),
		MaxAgents:     s.tracker.MaxAgents(),
	}
	return connect.NewResponse(resp), nil
}

// ServerMetrics returns resource usage metrics for the server.
func (s *bunkerdService) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	records := s.tracker.List()
	summaries := make([]*v1.AgentSummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, rec.ToAgentSummary())
	}

	resp := &v1.ServerMetricsResponse{
		Agents: summaries,
	}

	// Try to read cgroup metrics
	if metrics, err := resource.ReadCgroupMetrics(); err == nil {
		resp.CpuUsagePercent = metrics.CPUUsagePercent
		resp.MemoryUsedBytes = metrics.MemoryUsedBytes
		resp.MemoryTotalBytes = metrics.MemoryLimitBytes
	}

	// Try to read filesystem disk stats
	if used, total, err := readDiskStats(); err == nil {
		resp.DiskUsedBytes = used
		resp.DiskTotalBytes = total
	}

	// Count docker sockets (proxy for running docker daemons)
	resp.DockerContainersTotal = countDockerSockets()

	return connect.NewResponse(resp), nil
}

// SpawnAgent creates a new isolated agent environment.
func (s *bunkerdService) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	resp, err := s.agentMgr.Spawn(ctx, req.Msg)
	if err != nil {
		s.logger.Error("spawn agent failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Generate an agent-scoped opaque API sub-key when JWT auth is enabled.
	// The key is stored in the apikey manager and can be used by the agent
	// (or its owner) to call the Agent service scoped to this agent_id.
	if s.cfg.Auth.Enabled && s.cfg.Auth.JWTSecret != "" && s.keyMgr != nil {
		ttl := s.cfg.Agent.DefaultTTL
		if ttl <= 0 {
			ttl = 6 * time.Hour
		}
		if req.Msg.GetTtl() != "" {
			if parsed, err := time.ParseDuration(req.Msg.GetTtl()); err == nil && parsed > 0 {
				ttl = parsed
			}
		}
		if apiToken, _, err := s.keyMgr.Generate(resp.AgentId, ttl); err == nil {
			resp.ApiKey = apiToken
		} else {
			s.logger.Warn("failed to generate agent api key", "agent_id", resp.AgentId, "error", err)
		}
	}

	return connect.NewResponse(resp), nil
}

// DestroyAgent tears down an agent environment.
func (s *bunkerdService) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	resp, err := s.agentMgr.Destroy(ctx, req.Msg.AgentId, req.Msg.Force)
	if err != nil {
		s.logger.Error("destroy agent failed", "agent_id", req.Msg.AgentId, "error", err)
		// Map "not_found" to NotFound, other errors to Internal
		if resp != nil && resp.Status == "not_found" {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

// ListAgents returns all agents.
func (s *bunkerdService) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	records := s.tracker.List()
	summaries := make([]*v1.AgentSummary, 0, len(records))
	for _, rec := range records {
		summaries = append(summaries, rec.ToAgentSummary())
	}
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents:     summaries,
		TotalCount: uint32(len(summaries)),
	}), nil
}

// GetAgent returns a single agent by ID.
func (s *bunkerdService) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	return connect.NewResponse(&v1.GetAgentResponse{
		Agent: rec.ToAgentSummary(),
	}), nil
}

// AgentMetrics returns resource usage for a specific agent.
func (s *bunkerdService) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}

	resp := &v1.AgentMetricsResponse{
		AgentId: rec.AgentID,
		Status:  rec.Status,
	}
	if rec.Limits != nil {
		resp.MemoryLimitBytes = rec.Limits.MemoryMaxBytes
		resp.DiskLimitBytes = rec.Limits.DiskMaxBytes
	}

	// Try to read cgroup metrics (best-effort)
	if metrics, err := resource.ReadCgroupMetrics(); err == nil {
		resp.CpuUsagePercent = metrics.CPUUsagePercent
		resp.MemoryUsedBytes = metrics.MemoryUsedBytes
	}

	return connect.NewResponse(resp), nil
}

// ExecAgent executes a command in the agent's environment via SSH.
func (s *bunkerdService) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	agentID := req.Msg.AgentId
	if agentID == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_id is required"))
	}

	// Look up agent record
	rec := s.tracker.Get(agentID)
	if rec == nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", agentID))
	}

	// Build command to execute
	// The agent's dockerd listens on a per-agent Unix socket.  We need
	// DOCKER_HOST in the remote environment so that `docker` CLI commands
	// (and anything else that talks to Docker) reach the right socket.
	//
	// Two mechanisms:
	//   1. authorized_keys `environment=` prefix (set at spawn time) — works
	//      when sshd has PermitUserEnvironment=yes.
	//   2. `ssh -o SetEnv=DOCKER_HOST=...` — explicit client-side env push
	//      that works regardless of sshd config.  This is the fallback and
	//      the primary mechanism we rely on.
	//
	// We also set the variable in ~/.profile for interactive shells.
	userHome := "/home/bunker-" + agentID

	// Determine execution mode: raw (no shell) or shell-wrapped. Scripts are
	// uploaded and executed by the shell wrapper, so they share the same path.
	var cmd *exec.Cmd
	if req.Msg.GetRaw() {
		cmd = buildExecSSHRawCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args)
	} else if req.Msg.GetScriptContent() != "" {
		cmd = buildExecSSHScriptCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.GetScriptContent())
	} else {
		cmd = buildExecSSHCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome, req.Msg.Command, req.Msg.Args)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stdout pipe: %w", err))
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stderr pipe: %w", err))
	}

	if err := cmd.Start(); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("start ssh: %w", err))
	}

	// Stream stdout and stderr concurrently
	var wg sync.WaitGroup
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(stdoutDone)
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				if err := stream.Send(&v1.ExecAgentResponse{
					Output: &v1.ExecAgentResponse_Stdout{Stdout: buf[:n]},
				}); err != nil {
					s.logger.Warn("send stdout", "error", err)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer close(stderrDone)
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				if err := stream.Send(&v1.ExecAgentResponse{
					Output: &v1.ExecAgentResponse_Stderr{Stderr: buf[:n]},
				}); err != nil {
					s.logger.Warn("send stderr", "error", err)
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command completion
	exitCode := int32(0)
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = int32(exitErr.ExitCode())
		} else {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("ssh wait: %w", err))
		}
	}

	// Wait for streamers to finish
	wg.Wait()

	// Send final exit code
	if err := stream.Send(&v1.ExecAgentResponse{
		ExitCode: exitCode,
	}); err != nil {
		s.logger.Warn("send exit code", "error", err)
	}

	return nil
}

// RunAgent starts a command in the agent environment as a persistent systemd
// transient unit. The unit survives the RPC session ending. Non-detached
// (synchronous) runs are handled by the CLI via ExecAgent streaming.
func (s *bunkerdService) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	if req.Msg.GetAgentId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_id is required"))
	}
	if req.Msg.GetCommand() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("command is required"))
	}
	if !req.Msg.GetDetach() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("non-detached runs are not supported by RunAgent; use ExecAgent"))
	}
	if rec := s.tracker.Get(req.Msg.GetAgentId()); rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.GetAgentId()))
	}
	resp, err := s.agentMgr.RunAgent(ctx, req.Msg)
	if err != nil {
		s.logger.Error("run agent failed", "agent_id", req.Msg.GetAgentId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

// HeartbeatAgent acknowledges an agent heartbeat.
func (s *bunkerdService) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	// Extend TTL on heartbeat unless the agent has a zero TTL.
	ttl := 6 * time.Hour
	if s.cfg.Agent.DefaultTTL > 0 {
		ttl = s.cfg.Agent.DefaultTTL
	}
	rec.ExpiresAt = time.Now().Add(ttl)
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}

// buildAgentExecCommand constructs the shell command that runs inside the agent
// via SSH.  It prefixes the user command with env(1) so PATH, DOCKER_HOST, and
// TMPDIR are set regardless of sshd PermitUserEnvironment/AcceptEnv settings.
func buildAgentExecCommand(agentID, userHome, command string, args []string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	remoteCmd := command
	if len(args) > 0 {
		remoteCmd += " " + strings.Join(args, " ")
	}
	return fmt.Sprintf("env PATH=%s:$PATH DOCKER_HOST=unix://%s TMPDIR=%s %s", agentBinPath, dockerSockPath, tmpDir, remoteCmd)
}

// shellQuoteSingle returns s wrapped in single quotes, with embedded single
// quotes escaped for POSIX sh.  Example: hello'world -> 'hello'\\”world'.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// buildAgentRawExecCommand constructs the remote argv for raw mode. The command
// is executed directly by the SSH server without an intermediate shell, so args
// are passed as-is and shell injection/metacharacters are not interpreted.
func buildAgentRawExecCommand(agentID, userHome, command string, args []string) []string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	// sshd's ForceCommand or default shell may still receive a string, but
	// passing a command with args and using ssh's internal exec channel (when
	// the remote shell is not forced) will execve directly. We keep a tiny
	// wrapper here: env(1) so we can set DOCKER_HOST and TMPDIR before the real binary.
	return append([]string{
		"env",
		"PATH=" + agentBinPath + ":$PATH",
		"DOCKER_HOST=unix://" + dockerSockPath,
		"TMPDIR=" + tmpDir,
		command,
	}, args...)
}

// buildAgentScriptCommand writes scriptContent to a remote file and returns the
// shell command that executes it. The file is written via ssh heredoc.
func buildAgentScriptCommand(agentID, userHome, scriptContent string) string {
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	tmpDir := filepath.Join("/run", "bunker", agentID, "tmp")
	agentBinPath := filepath.Join(userHome, "bin")
	scriptPath := filepath.Join(userHome, ".bunker", "exec-script.sh")
	// Use POSIX heredoc to create + chmod + execute the script in one SSH call.
	// We quote the EOF delimiter to prevent expansion of the script body.
	escaped := strings.ReplaceAll(scriptContent, "'", "'\\''")
	return fmt.Sprintf(
		"mkdir -p %q && cat > %q <<'EOFSCRIPT'\n%s\nEOFSCRIPT\nchmod +x %q && env PATH=%s:$PATH DOCKER_HOST=unix://%s TMPDIR=%s %q",
		filepath.Dir(scriptPath), scriptPath, escaped, scriptPath, agentBinPath, dockerSockPath, tmpDir, scriptPath,
	)
}

// buildExecSSHCommand returns an exec.Cmd that runs buildAgentExecCommand
// inside the agent via ssh.  The remote script is passed as a single quoted
// "sh -c '...'" argument to OpenSSH so that multi-token commands such as
// "docker version" are not misparsed by the inner shell.
func buildExecSSHCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string) *exec.Cmd {
	wrappedCmd := buildAgentExecCommand(agentID, userHome, command, args)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildExecSSHRawCommand returns an exec.Cmd that runs command+args directly
// without a shell wrapper. Each arg is passed as a separate ssh argument; sshd
// will attempt to exec the requested program directly.
func buildExecSSHRawCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string) *exec.Cmd {
	remoteArgv := buildAgentRawExecCommand(agentID, userHome, command, args)
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, remoteArgv...)
}

// buildExecSSHScriptCommand returns an exec.Cmd that uploads scriptContent via
// heredoc and executes it on the agent.
func buildExecSSHScriptCommand(ctx context.Context, agentID, sshKeyPath, userHome, scriptContent string) *exec.Cmd {
	wrappedCmd := buildAgentScriptCommand(agentID, userHome, scriptContent)
	sshRemoteCmd := fmt.Sprintf("sh -c %s", shellQuoteSingle(wrappedCmd))
	return buildSSHBaseCommand(ctx, agentID, sshKeyPath, sshRemoteCmd)
}

// buildSSHBaseCommand builds the common ssh command with the given remote args.
func buildSSHBaseCommand(ctx context.Context, agentID, sshKeyPath string, remoteArgs ...string) *exec.Cmd {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-i", sshKeyPath,
		fmt.Sprintf("bunker-%s@localhost", agentID),
	}
	args = append(args, remoteArgs...)
	return exec.CommandContext(ctx, "ssh", args...)
}

// agentService implements bunkerv1connect.AgentHandler.
type agentService struct {
	logger  *slog.Logger
	tracker *resource.Tracker
}

// GetInfo returns info about the authenticated agent.
func (s *agentService) GetInfo(ctx context.Context, req *connect.Request[v1.GetInfoRequest]) (*connect.Response[v1.GetInfoResponse], error) {
	// Extract agent_id from the auth context (JWT claims or scoped sub-key).
	agentID := ""
	if claims, ok := auth.ClaimsFromContext(ctx); ok && claims.AgentID != "" {
		agentID = claims.AgentID
	}

	resp := &v1.GetInfoResponse{
		Status: "running",
	}
	if agentID != "" {
		resp.AgentId = agentID
		// If we have a tracker record, populate more fields
		if rec := s.tracker.Get(agentID); rec != nil {
			status := rec.Status
			if status == "" {
				status = "running"
			}
			resp.Status = status
			resp.PublicUrl = rec.PublicURL
			resp.ExpiresAt = rec.ExpiresAt.Format(time.RFC3339)
			if rec.Limits != nil {
				resp.Limits = rec.Limits
			}
		}
	}
	return connect.NewResponse(resp), nil
}

// Metrics returns resource usage for the authenticated agent.
func (s *agentService) Metrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	resp := &v1.AgentMetricsResponse{
		AgentId: req.Msg.AgentId,
		Status:  "running",
	}
	if metrics, err := resource.ReadCgroupMetrics(); err == nil {
		resp.CpuUsagePercent = metrics.CPUUsagePercent
		resp.MemoryUsedBytes = metrics.MemoryUsedBytes
	}
	return connect.NewResponse(resp), nil
}

// Heartbeat sends a heartbeat from the authenticated agent.
func (s *agentService) Heartbeat(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	rec := s.tracker.Get(req.Msg.AgentId)
	if rec == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", req.Msg.AgentId))
	}
	// Extend TTL on heartbeat.
	rec.ExpiresAt = time.Now().Add(6 * time.Hour)
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    rec.ExpiresAt.Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}

// readDiskStats returns used and total bytes for the root filesystem.
func readDiskStats() (used, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0, err
	}
	total = stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used = total - free
	return used, total, nil
}

// countDockerSockets counts the number of docker socket files in /run/bunker/*/docker.sock.
// This is a proxy for the number of running dockerd instances.
func countDockerSockets() uint32 {
	entries, err := os.ReadDir("/run/bunker")
	if err != nil {
		return 0
	}
	var count uint32
	for _, entry := range entries {
		if entry.IsDir() {
			sockPath := filepath.Join("/run/bunker", entry.Name(), "docker.sock")
			if info, statErr := os.Stat(sockPath); statErr == nil && info.Mode()&os.ModeSocket != 0 {
				count++
			}
		}
	}
	return count
}
