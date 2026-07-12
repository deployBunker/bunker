package cli

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// infoMockServer is a test implementation of BunkerdHandler that returns
// controlled GetAgent responses.
type infoMockServer struct {
	agent *v1.AgentSummary
	err   error
}

func (m *infoMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{}), nil
}
func (m *infoMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: m.agent}), nil
}
func (m *infoMockServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *infoMockServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func TestInfoCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewInfoCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "bunker info") && !strings.Contains(output, "info AGENT_ID") {
		t.Errorf("help output missing usage, got:\n%s", output)
	}
}

func TestInfoCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing agent ID")
	}
}

func TestInfoCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no server configured")
	}
	if err != nil && !strings.Contains(err.Error(), "no active server") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestInfoCommand_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &infoMockServer{
		agent: &v1.AgentSummary{
			AgentId:          "e2e-test-42",
			Status:           "running",
			CreatedAt:        "2026-07-01T12:00:00Z",
			ExpiresAt:        "2026-07-01T18:00:00Z",
			PublicUrl:        "https://e2e-test-42.trycloudflare.com",
			PortRangeStart:   10000,
			PortRangeEnd:     10009,
			DockerHostTunnel: "ssh -L 2376:/run/bunker/e2e-test-42/docker.sock bunker-e2e-test-42@host -N",
			SshfsMount:       "sshfs bunker-e2e-test-42@host:/home/bunker-e2e-test-42 /mnt/bunker/e2e-test-42",
			TailnetIp:        "100.64.0.42",
			Limits: &v1.ResourceLimits{
				CpuQuota:            2.0,
				MemoryMaxBytes:      2 * 1024 * 1024 * 1024,  // 2 GB
				DiskMaxBytes:        20 * 1024 * 1024 * 1024, // 20 GB
				MaxDockerContainers: 10,
			},
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	// Write config with this server URL
	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"e2e-test-42"})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	// Verify key fields are present
	checks := []string{
		"e2e-test-42",
		"running",
		"2026-07-01T12:00:00Z",
		"2026-07-01T18:00:00Z",
		"trycloudflare.com",
		"10000-10009",
		"docker.sock",
		"sshfs",
		"100.64.0.42",
		"2.0 cores",
		"2.0 GB",
		"20.0 GB",
		"Max Containers: 10",
	}
	for _, check := range checks {
		if !strings.Contains(output, check) {
			t.Errorf("output missing %q, got:\n%s", check, output)
		}
	}
}

func TestInfoCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &infoMockServer{
		err: connect.NewError(connect.CodeNotFound, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"nonexistent"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for agent not found")
	}
}

func TestInfoCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &infoMockServer{
		err: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"error-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server error")
	}
}

func TestInfoCommand_MinimalAgent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Test with minimal fields (no limits, no network extras)
	mock := &infoMockServer{
		agent: &v1.AgentSummary{
			AgentId:        "minimal-agent",
			Status:         "starting",
			CreatedAt:      "2026-07-01T12:00:00Z",
			PortRangeStart: 20000,
			PortRangeEnd:   20009,
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewInfoCommand()
	cmd.SetArgs([]string{"minimal-agent"})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "minimal-agent") {
		t.Errorf("output missing agent ID, got:\n%s", output)
	}
	if !strings.Contains(output, "starting") {
		t.Errorf("output missing status, got:\n%s", output)
	}
	// Should NOT contain limit fields or network extras
	if strings.Contains(output, "CPU Quota") {
		t.Error("output should not contain CPU Quota for minimal agent")
	}
	if strings.Contains(output, "Public URL") {
		t.Error("output should not contain Public URL for minimal agent")
	}
}
