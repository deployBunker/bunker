// Package server — bunkerd RPC service implementations.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	"github.com/deployBunker/bunker/internal/tailscale"
	"github.com/deployBunker/bunker/internal/tunnel"
)

// bunkerdService implements bunkerv1connect.BunkerdHandler.
type bunkerdService struct {
	cfg          *config.Config
	logger       *slog.Logger
	agentMgr     *agent.AgentManager
	tracker      *resource.Tracker
	tunnelMgr    *tunnel.TunnelManager
	tailscaleMgr *tailscale.TailscaleManager
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

	return connect.NewResponse(resp), nil
}

// SpawnAgent creates a new isolated agent environment.
func (s *bunkerdService) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	resp, err := s.agentMgr.Spawn(ctx, req.Msg)
	if err != nil {
		s.logger.Error("spawn agent failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
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
	dockerSockPath := fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", fmt.Sprintf("SetEnv=DOCKER_HOST=unix://%s", dockerSockPath),
		"-i", rec.SshPrivateKeyPath,
		fmt.Sprintf("bunker-%s@localhost", agentID),
		"--",
		req.Msg.Command,
	)
	if len(req.Msg.Args) > 0 {
		cmd.Args = append(cmd.Args, req.Msg.Args...)
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

// HeartbeatAgent acknowledges an agent heartbeat.
func (s *bunkerdService) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}

// agentService implements bunkerv1connect.AgentHandler.
type agentService struct {
	logger  *slog.Logger
	tracker *resource.Tracker
}

// GetInfo returns info about the authenticated agent.
func (s *agentService) GetInfo(ctx context.Context, req *connect.Request[v1.GetInfoRequest]) (*connect.Response[v1.GetInfoResponse], error) {
	// TODO: WI-003 — Agent context from auth
	return connect.NewResponse(&v1.GetInfoResponse{
		Status: "running",
	}), nil
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
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}
