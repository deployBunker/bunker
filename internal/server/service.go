// Package server — bunkerd RPC service implementations.
package server

import (
	"context"
	"log/slog"
	"os"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
)

// bunkerdService implements bunkerv1connect.BunkerdHandler.
type bunkerdService struct {
	cfg      *config.Config
	logger   *slog.Logger
	agentMgr *agent.AgentManager
}

// ServerInfo returns information about the bunkerd server.
func (s *bunkerdService) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	hostname, _ := os.Hostname()
	resp := &v1.ServerInfoResponse{
		Hostname: hostname,
		Version:  "0.1.0",
		UptimeSeconds: 0, // TODO: track start time
		AgentCount: 0,    // TODO: track agent count
		MaxAgents:  100,
	}
	return connect.NewResponse(resp), nil
}

// ServerMetrics returns resource usage metrics for the server.
func (s *bunkerdService) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	resp := &v1.ServerMetricsResponse{
		Agents: nil,
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
	// TODO: WI-005 — Resource tracking
	return connect.NewResponse(&v1.ListAgentsResponse{
		Agents:    nil,
		TotalCount: 0,
	}), nil
}

// GetAgent returns a single agent by ID.
func (s *bunkerdService) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	// TODO: WI-005 — Resource tracking
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// AgentMetrics returns resource usage for a specific agent.
func (s *bunkerdService) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	// TODO: WI-005 — Resource tracking
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
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
	logger *slog.Logger
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
	// TODO: WI-005 — Resource tracking
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// Heartbeat sends a heartbeat from the authenticated agent.
func (s *agentService) Heartbeat(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		ExpiresAt:    time.Now().Add(6 * time.Hour).Format(time.RFC3339),
		Acknowledged: true,
	}), nil
}
