package cli

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

type heartbeatMockServer struct {
	called    bool
	agentID   string
	ack       bool
	expiresAt string
}

func (m *heartbeatMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	m.called = true
	m.agentID = req.Msg.AgentId
	return connect.NewResponse(&v1.HeartbeatAgentResponse{
		AgentId:      req.Msg.AgentId,
		Acknowledged: m.ack,
		ExpiresAt:    m.expiresAt,
	}), nil
}

func (m *heartbeatMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func TestHeartbeatCommand_SendsAgentID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &heartbeatMockServer{ack: true, expiresAt: "2026-06-30T12:00:00Z"}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"mock": {Name: "mock", URL: srv.URL, Token: "test-token"},
		},
		ActiveServer: "mock",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("save test config: %v", err)
	}

	cmd := NewHeartbeatCommand()
	cmd.SetArgs([]string{"ttl-agent"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute heartbeat: %v", err)
	}

	if !mock.called {
		t.Fatal("HeartbeatAgent was not called")
	}
	if mock.agentID != "ttl-agent" {
		t.Fatalf("expected agentID ttl-agent, got %q", mock.agentID)
	}
}
