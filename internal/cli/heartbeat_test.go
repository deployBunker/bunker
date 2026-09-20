package cli

import (
	"context"
	"strings"
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

func (m *heartbeatMockServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
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

func (m *heartbeatMockServer) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *heartbeatMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func TestHeartbeatCommand_SendsAgentID(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "mock")
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

// TestHeartbeatCommand_OutputStatesTTLSemantics pins the DF-BUNKER-17
// contract: an acknowledged heartbeat always prints the resulting expiry AND
// states the TTL semantics (the daemon's default TTL is applied, an existing
// longer expiry is never shortened) — the behaviour used to live only in
// --help.
func TestHeartbeatCommand_OutputStatesTTLSemantics(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "mock")
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
	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute heartbeat: %v", execErr)
	}

	for _, want := range []string{
		"Heartbeat acknowledged for agent ttl-agent", // pre-existing wording, kept
		"Expires at: 2026-06-30T12:00:00Z",           // the resulting expiry
		"TTL: " + heartbeatTTLSemantics,              // the semantics, stated
		"never shortened",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("heartbeat output missing %q, got:\n%s", want, out)
		}
	}
}

// TestHeartbeatCommand_OutputWhenDaemonOmitsExpiry makes the expiry line
// unconditional: an acknowledgement that carries no expires_at must still
// show the line (with a placeholder) rather than silently dropping it.
func TestHeartbeatCommand_OutputWhenDaemonOmitsExpiry(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "mock")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &heartbeatMockServer{ack: true, expiresAt: ""}
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
	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("execute heartbeat: %v", execErr)
	}

	if !strings.Contains(out, "Expires at: (not reported by the daemon)") {
		t.Errorf("heartbeat output missing the expiry line, got:\n%s", out)
	}
	if !strings.Contains(out, "TTL: "+heartbeatTTLSemantics) {
		t.Errorf("heartbeat output missing the TTL semantics, got:\n%s", out)
	}
}

// TestHeartbeatCommand_HelpStatesSemantics keeps --help and the runtime
// output in lockstep (the semantics line is shared via heartbeatTTLSemantics).
func TestHeartbeatCommand_HelpStatesSemantics(t *testing.T) {
	cmd := NewHeartbeatCommand()
	out := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		_ = cmd.Execute()
	})
	if !strings.Contains(out, heartbeatTTLSemantics) {
		t.Errorf("heartbeat --help does not state the TTL semantics, got:\n%s", out)
	}
	if !strings.Contains(out, "never shortened") {
		t.Errorf("heartbeat --help does not mention the never-shrink rule, got:\n%s", out)
	}
}
