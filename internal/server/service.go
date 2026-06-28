// Package server — bunkerd RPC service implementations.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

// ExecAgent executes a command in the agent's environment.
func (s *bunkerdService) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	// TODO: WI-016 — bunker exec
	return connect.NewError(connect.CodeUnimplemented, nil)
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
