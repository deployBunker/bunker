package cli

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// metricsMockServer is a test implementation that supports ServerMetrics and AgentMetrics.
type metricsMockServer struct {
	serverMetrics *v1.ServerMetricsResponse
	agentMetrics  *v1.AgentMetricsResponse
	err           error
}

func (m *metricsMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{Hostname: "test-server", Version: "0.1.0"}), nil
}
func (m *metricsMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.serverMetrics), nil
}
func (m *metricsMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.agentMetrics), nil
}
func (m *metricsMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *metricsMockServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func newMetricsTestServer(t *testing.T, mock *metricsMockServer) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, handler := bunkerv1connect.NewBunkerdHandler(mock)
	r.Mount(path, handler)
	return httptest.NewServer(r)
}

func TestMetricsCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewMetricsCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "metrics") {
		t.Fatalf("expected help to mention 'metrics', got:\n%s", output)
	}
}

func TestMetricsCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no server configured")
	}
	if !strings.Contains(err.Error(), "no active server") {
		t.Fatalf("expected 'no active server' error, got: %v", err)
	}
}

func TestMetricsCommand_ServerMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &metricsMockServer{
		serverMetrics: &v1.ServerMetricsResponse{
			CpuUsagePercent:       12.5,
			MemoryUsedBytes:       1024 * 1024 * 512,
			MemoryTotalBytes:      1024 * 1024 * 1024 * 4,
			DiskUsedBytes:         1024 * 1024 * 1024,
			DiskTotalBytes:        1024 * 1024 * 1024 * 100,
			DockerContainersTotal: 3,
			Agents: []*v1.AgentSummary{
				{
					AgentId: "agent-1",
					Status:  "running",
					Limits:  &v1.ResourceLimits{CpuQuota: 2.0, MemoryMaxBytes: 1024 * 1024 * 1024},
				},
				{
					AgentId: "agent-2",
					Status:  "stopped",
					Limits:  &v1.ResourceLimits{CpuQuota: 1.0, MemoryMaxBytes: 512 * 1024 * 1024},
				},
			},
		},
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	// Register server in config
	if err := RegisterServer("test", ts.URL, "", true); err != nil {
		t.Fatalf("register server: %v", err)
	}

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"--server", "test"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "Server Metrics") {
		t.Fatalf("expected 'Server Metrics' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "agent-1") {
		t.Fatalf("expected 'agent-1' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "agent-2") {
		t.Fatalf("expected 'agent-2' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "12.50%") && !strings.Contains(output, "12.5%") {
		t.Fatalf("expected CPU usage in output, got:\n%s", output)
	}
}

func TestMetricsCommand_AgentMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &metricsMockServer{
		agentMetrics: &v1.AgentMetricsResponse{
			AgentId:          "abc123",
			Status:           "running",
			CpuUsagePercent:  45.2,
			MemoryUsedBytes:  1024 * 1024 * 256,
			MemoryLimitBytes: 1024 * 1024 * 1024,
			DiskUsedBytes:    1024 * 1024 * 1024 * 2,
			DiskLimitBytes:   1024 * 1024 * 1024 * 10,
			DockerContainers: 2,
			Uptime:           "3h42m",
		},
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	if err := RegisterServer("test", ts.URL, "", true); err != nil {
		t.Fatalf("register server: %v", err)
	}

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"abc123", "--server", "test"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "abc123") {
		t.Fatalf("expected agent ID in output, got:\n%s", output)
	}
	if !strings.Contains(output, "running") {
		t.Fatalf("expected status in output, got:\n%s", output)
	}
	if !strings.Contains(output, "45.2") {
		t.Fatalf("expected CPU usage in output, got:\n%s", output)
	}
}

func TestMetricsCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &metricsMockServer{
		err: connect.NewError(connect.CodeInternal, fmt.Errorf("database error")),
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	if err := RegisterServer("test", ts.URL, "", true); err != nil {
		t.Fatalf("register server: %v", err)
	}

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"--server", "test"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from server failure")
	}
	if !strings.Contains(err.Error(), "server metrics") {
		t.Fatalf("expected 'server metrics' in error, got: %v", err)
	}
}

func TestMetricsCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &metricsMockServer{
		err: connect.NewError(connect.CodeNotFound, fmt.Errorf("agent not found")),
	}
	ts := newMetricsTestServer(t, mock)
	defer ts.Close()

	if err := RegisterServer("test", ts.URL, "", true); err != nil {
		t.Fatalf("register server: %v", err)
	}

	cmd := NewMetricsCommand()
	cmd.SetArgs([]string{"missing-id", "--server", "test"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if !strings.Contains(err.Error(), "agent metrics") {
		t.Fatalf("expected 'agent metrics' in error, got: %v", err)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		input    uint64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{1024 * 1024 * 1024 * 1024, "1.0 TB"},
		{1536, "1.5 KB"},
		{1024 * 1024 * 512, "512.0 MB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := humanBytes(tt.input)
			if result != tt.expected {
				t.Fatalf("humanBytes(%d) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
