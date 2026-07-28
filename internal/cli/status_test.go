package cli

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// statusMockServer is a test implementation of BunkerdHandler with
// configurable ServerInfo and ServerMetrics responses.
type statusMockServer struct {
	info       *v1.ServerInfoResponse
	metrics    *v1.ServerMetricsResponse
	infoErr    error
	metricsErr error
}

func (m *statusMockServer) ServerInfo(
	ctx context.Context,
	req *connect.Request[v1.ServerInfoRequest],
) (*connect.Response[v1.ServerInfoResponse], error) {
	if m.infoErr != nil {
		return nil, m.infoErr
	}
	if m.info == nil {
		return connect.NewResponse(&v1.ServerInfoResponse{}), nil
	}
	return connect.NewResponse(m.info), nil
}

func (m *statusMockServer) ServerMetrics(
	ctx context.Context,
	req *connect.Request[v1.ServerMetricsRequest],
) (*connect.Response[v1.ServerMetricsResponse], error) {
	if m.metricsErr != nil {
		return nil, m.metricsErr
	}
	if m.metrics == nil {
		return connect.NewResponse(&v1.ServerMetricsResponse{}), nil
	}
	return connect.NewResponse(m.metrics), nil
}

func (m *statusMockServer) SpawnAgent(
	ctx context.Context, req *connect.Request[v1.SpawnAgentRequest],
) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) DestroyAgent(
	ctx context.Context, req *connect.Request[v1.DestroyAgentRequest],
) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) ListAgents(
	ctx context.Context, req *connect.Request[v1.ListAgentsRequest],
) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) GetAgent(
	ctx context.Context, req *connect.Request[v1.GetAgentRequest],
) (*connect.Response[v1.GetAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) AgentMetrics(
	ctx context.Context, req *connect.Request[v1.AgentMetricsRequest],
) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) ExecAgent(
	ctx context.Context,
	req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse],
) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) RunAgent(
	ctx context.Context, req *connect.Request[v1.RunAgentRequest],
) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *statusMockServer) HeartbeatAgent(
	ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest],
) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// newStatusTestServer starts an httptest server mounting the given handler.
func newStatusTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// --- Tests ---

func TestStatusCommand_Help(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewStatusCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "status") {
		t.Errorf("help output missing 'status', got:\n%s", output)
	}
	if !strings.Contains(output, "--all") {
		t.Errorf("help output missing --all flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--all-servers") {
		t.Errorf("help output missing --all-servers flag, got:\n%s", output)
	}
}

func TestStatusCommand_NoServersConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewStatusCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "No servers configured") {
		t.Errorf("output should contain 'No servers configured', got:\n%s", output)
	}
}

func TestStatusCommand_NoServersConfigured_WithAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--all"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "No servers configured") {
		t.Errorf("output should contain 'No servers configured', got:\n%s", output)
	}
}

func TestStatusCommand_SingleServer_Online(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname:      "bunker-prod-01",
			Version:       "v1.2.0",
			UptimeSeconds: 86400 + 3600 + 60,
			AgentCount:    5,
			MaxAgents:     20,
		},
		metrics: &v1.ServerMetricsResponse{
			CpuUsagePercent:       42.5,
			MemoryUsedBytes:       4 * 1024 * 1024 * 1024,
			MemoryTotalBytes:      16 * 1024 * 1024 * 1024,
			DiskUsedBytes:         80 * 1024 * 1024 * 1024,
			DiskTotalBytes:        200 * 1024 * 1024 * 1024,
			DockerContainersTotal: 12,
		},
	}
	srv := newStatusTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	checks := []string{
		"default",
		"bunker-prod-01",
		"v1.2.0",
		"ONLINE",
		"1d 1h 1m",
		"5/20",
		"42.5%",
		"4.0 GB",
		"16.0 GB",
		"40% (80.0 GB/200.0 GB)",
		"12 containers",
	}
	for _, want := range checks {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	// 40% disk should NOT have a warning indicator
	if strings.Contains(output, "Disk:     !") || strings.Contains(output, "Disk:     ⚠") {
		t.Errorf("disk at 40%% should not have warning indicator, got:\n%s", output)
	}
}

func TestStatusCommand_SingleServer_MetricsNA(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname:      "old-server",
			Version:       "v0.1.0",
			UptimeSeconds: 300,
			AgentCount:    2,
			MaxAgents:     10,
		},
		metricsErr: connect.NewError(connect.CodeUnimplemented, nil),
	}
	srv := newStatusTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	// Info should still show.
	if !strings.Contains(output, "ONLINE") {
		t.Errorf("output should show ONLINE, got:\n%s", output)
	}
	if !strings.Contains(output, "old-server") {
		t.Errorf("output should show hostname, got:\n%s", output)
	}
	if !strings.Contains(output, "2/10") {
		t.Errorf("output should show agent count, got:\n%s", output)
	}
	// Metrics should show N/A.
	if !strings.Contains(output, "CPU:      N/A") {
		t.Errorf("output should show CPU N/A, got:\n%s", output)
	}
	if !strings.Contains(output, "Memory:   N/A") {
		t.Errorf("output should show Memory N/A, got:\n%s", output)
	}
	if !strings.Contains(output, "Disk:     N/A") {
		t.Errorf("output should show Disk N/A, got:\n%s", output)
	}
}

func TestStatusCommand_OfflineServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &CLIConfig{
		ActiveServer: "dead",
		Servers: map[string]ServerEntry{
			"dead": {Name: "dead", URL: "http://127.0.0.1:19999"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute should not error for offline server, got: %v", err)
		}
	})

	if !strings.Contains(output, "OFFLINE") {
		t.Errorf("output should show OFFLINE, got:\n%s", output)
	}
	if !strings.Contains(output, "dead") {
		t.Errorf("output should contain server name, got:\n%s", output)
	}
}

func TestStatusCommand_AllServers_Mixed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Server 1: online with metrics
	mock1 := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname:      "alpha-host",
			Version:       "v1.0.0",
			UptimeSeconds: 7200,
			AgentCount:    3,
			MaxAgents:     10,
		},
		metrics: &v1.ServerMetricsResponse{
			CpuUsagePercent:  15.0,
			MemoryUsedBytes:  2 * 1024 * 1024 * 1024,
			MemoryTotalBytes: 8 * 1024 * 1024 * 1024,
			DiskUsedBytes:    30 * 1024 * 1024 * 1024,
			DiskTotalBytes:   100 * 1024 * 1024 * 1024,
		},
	}
	srv1 := newStatusTestServer(t, mock1)
	defer srv1.Close()

	// Server 2: online, no metrics support
	mock2 := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname:      "beta-host",
			Version:       "v0.9.0",
			UptimeSeconds: 60,
			AgentCount:    1,
			MaxAgents:     5,
		},
		metricsErr: connect.NewError(connect.CodeUnimplemented, nil),
	}
	srv2 := newStatusTestServer(t, mock2)
	defer srv2.Close()

	cfg := &CLIConfig{
		ActiveServer: "alpha",
		Servers: map[string]ServerEntry{
			"alpha": {Name: "alpha", URL: srv1.URL},
			"beta":  {Name: "beta", URL: srv2.URL},
			"dead":  {Name: "dead", URL: "http://127.0.0.1:19998"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--all"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	// Should show all 3 servers (sorted: alpha, beta, dead).
	checks := []string{
		"3 servers",
		"alpha",
		"alpha-host",
		"v1.0.0",
		"ONLINE",
		"3/10",
		"15.0%",
		"beta",
		"beta-host",
		"N/A",
		"dead",
		"OFFLINE",
	}
	for _, want := range checks {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

func TestStatusCommand_AllServers_AliasFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname: "solo",
			Version:  "v1.0.0",
		},
	}
	srv := newStatusTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--all-servers"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "1 servers") {
		t.Errorf("--all-servers flag should work as --all alias, got:\n%s", output)
	}
}

func TestStatusCommand_ServerNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers: map[string]ServerEntry{
			"default": {Name: "default", URL: "http://localhost:1234"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "nonexistent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-existent server")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should contain 'not found', got: %v", err)
	}
}

func TestStatusCommand_WithServerFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname: "flagged",
			Version:  "v2.0.0",
		},
	}
	srv := newStatusTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "other",
		Servers: map[string]ServerEntry{
			"other":  {Name: "other", URL: "http://127.0.0.1:19997"},
			"target": {Name: "target", URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "target"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	if !strings.Contains(output, "flagged") {
		t.Errorf("output should show 'target' server info, got:\n%s", output)
	}
}

func TestStatusCommand_HighDiskWarning(t *testing.T) {
	tests := []struct {
		name          string
		diskUsed      uint64
		diskTotal     uint64
		wantIndicator string
	}{
		{
			name:          "80% warn",
			diskUsed:      80 * 1024 * 1024 * 1024,
			diskTotal:     100 * 1024 * 1024 * 1024,
			wantIndicator: "! ",
		},
		{
			name:          "90% critical",
			diskUsed:      900 * 1024 * 1024 * 1024,
			diskTotal:     1000 * 1024 * 1024 * 1024,
			wantIndicator: "⚠ ",
		},
		{
			name:          "95% critical",
			diskUsed:      95,
			diskTotal:     100,
			wantIndicator: "⚠ ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			mock := &statusMockServer{
				info: &v1.ServerInfoResponse{
					Hostname:  "disk-host",
					Version:   "v1.0.0",
					MaxAgents: 10,
				},
				metrics: &v1.ServerMetricsResponse{
					DiskUsedBytes:  tt.diskUsed,
					DiskTotalBytes: tt.diskTotal,
				},
			}
			srv := newStatusTestServer(t, mock)
			defer srv.Close()

			cfg := &CLIConfig{
				ActiveServer: "default",
				Servers: map[string]ServerEntry{
					"default": {Name: "default", URL: srv.URL},
				},
			}
			if err := SaveCLIConfig(cfg); err != nil {
				t.Fatalf("SaveCLIConfig: %v", err)
			}

			cmd := NewStatusCommand()
			output := captureStdout(t, func() {
				if err := cmd.Execute(); err != nil {
					t.Fatalf("Execute: %v", err)
				}
			})

			want := "Disk:     " + tt.wantIndicator
			if !strings.Contains(output, want) {
				t.Errorf("output missing indicator %q, got:\n%s", want, output)
			}
		})
	}
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		seconds uint64
		want    string
	}{
		{0, "unknown"},
		{45, "45s"},
		{120, "2m 0s"},
		{3661, "1h 1m 1s"},
		{90061, "1d 1h 1m"},
		{172800, "2d 0h 0m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatUptime(tt.seconds)
			if got != tt.want {
				t.Errorf("formatUptime(%d) = %q, want %q", tt.seconds, got, tt.want)
			}
		})
	}
}
