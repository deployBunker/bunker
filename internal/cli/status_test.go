package cli

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"

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

// GetAgentKey is unimplemented in this mock (GAP-128 handler surface).
func (m *statusMockServer) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
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
func (m *statusMockServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *statusMockServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *statusMockServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
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
func (m *statusMockServer) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
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
		cmd.SetArgs([]string{"--server", "default", "--help"})
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
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error (non-zero exit) when no servers are configured")
	}
	if !strings.Contains(err.Error(), "no servers configured") {
		t.Errorf("error should contain 'no servers configured', got: %v", err)
	}
}

func TestStatusCommand_NoServersConfigured_WithAll(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error (non-zero exit) with --all and no servers configured")
	}
	if !strings.Contains(err.Error(), "no servers configured") {
		t.Errorf("error should contain 'no servers configured', got: %v", err)
	}
}

// TestExitCode_NoServersConfigured is a table-driven exit-code guard (GAP-037):
// every server-facing command must fail loudly (non-zero exit) when no servers
// are configured. `bunker status` used to print a message and exit 0, which
// made scripts/CI treat failure as success; list/spawn/info already exited 1.
func TestExitCode_NoServersConfigured(t *testing.T) {
	// info takes a positional agent ID; the other commands are flag-only.
	// The invariant under test: every command errors (non-zero exit) — either
	// on the no-server check or, for info, on arg validation — never a silent
	// exit-0 success like status used to produce.
	newCmds := map[string]struct {
		newCmd func() *cobra.Command
		args   []string
	}{
		"status": {NewStatusCommand, nil},
		"list":   {NewListCommand, nil},
		"spawn":  {NewSpawnCommand, nil},
		"info":   {NewInfoCommand, []string{"demo-agent"}},
	}
	for name, tc := range newCmds {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			cmd := tc.newCmd()
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("%s: expected non-zero exit (error) with no servers configured", name)
			}
			if !strings.Contains(err.Error(), "no target bound") &&
				!strings.Contains(err.Error(), "no servers configured") &&
				!strings.Contains(err.Error(), "no active server") {
				t.Errorf("%s: error should name the unbound/unconfigured target, got: %v", name, err)
			}
		})
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
	cmd.SetArgs([]string{"--server", "default", "--all"})
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
	cmd.SetArgs([]string{"--server", "default", "--all-servers"})
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

// TestStatusCommand_TmpIsolationReporting verifies the /tmp isolation line of
// the ONLINE status section (DF-BUNKER-9) end-to-end through the mock server,
// for every reported level: private, host-shared (with its WARNING banner),
// unknown (with the detail) and empty (a daemon that predates capability
// reporting, e.g. any tagged v0.1.x build).
func TestStatusCommand_TmpIsolationReporting(t *testing.T) {
	tests := []struct {
		name        string
		tmpLevel    string
		tmpDetail   string
		wantSubstrs []string
		notWant     []string
	}{
		{
			name:     "private",
			tmpLevel: "private",
			wantSubstrs: []string{
				"  /tmp:     private (per-session pam_namespace instance)",
			},
			notWant: []string{"HOST-SHARED", "WARNING"},
		},
		{
			name:      "host-shared",
			tmpLevel:  "host-shared",
			tmpDetail: "pam_namespace.so is not installed on this host",
			wantSubstrs: []string{
				"  /tmp:     HOST-SHARED — agent sessions see the host /tmp",
				"WARNING: Private /tmp is NOT active on this host.",
				"'Private /tmp per agent' promise does not hold here.",
				"Reason:   pam_namespace.so is not installed on this host",
			},
		},
		{
			name:      "unknown",
			tmpLevel:  "unknown",
			tmpDetail: "daemon is not running as root — /tmp isolation state cannot be verified",
			wantSubstrs: []string{
				"  /tmp:     unknown — daemon is not running as root — /tmp isolation state cannot be verified",
			},
			notWant: []string{"HOST-SHARED", "WARNING"},
		},
		{
			name:     "empty field (daemon predates capability reporting)",
			tmpLevel: "",
			wantSubstrs: []string{
				"  /tmp:     not reported by this daemon — it predates capability reporting; build/run a daemon from the same commit as the CLI (private /tmp is not guaranteed)",
			},
			notWant: []string{"HOST-SHARED", "WARNING"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			mock := &statusMockServer{
				info: &v1.ServerInfoResponse{
					Hostname:           "iso-host",
					Version:            "v1.0.0",
					UptimeSeconds:      60,
					AgentCount:         1,
					MaxAgents:          5,
					TmpIsolation:       tt.tmpLevel,
					TmpIsolationDetail: tt.tmpDetail,
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

			for _, want := range tt.wantSubstrs {
				if !strings.Contains(output, want) {
					t.Errorf("output missing %q, got:\n%s", want, output)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(output, notWant) {
					t.Errorf("output should not contain %q, got:\n%s", notWant, output)
				}
			}
		})
	}
}

// TestStatusCommand_AllServers_TmpIsolation verifies the /tmp line appears in
// --all output too: every server section (online ones) flows through
// formatServerStatus regardless of single-server vs --all mode.
func TestStatusCommand_AllServers_TmpIsolation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mock := &statusMockServer{
		info: &v1.ServerInfoResponse{
			Hostname:           "iso-all-host",
			Version:            "v1.0.0",
			TmpIsolation:       "host-shared",
			TmpIsolationDetail: "the agent isolation group does not exist on this host",
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
	cmd.SetArgs([]string{"--server", "default", "--all"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})

	for _, want := range []string{
		"1 servers",
		"iso-all-host",
		"  /tmp:     HOST-SHARED — agent sessions see the host /tmp",
		"WARNING: Private /tmp is NOT active on this host.",
		"Reason:   the agent isolation group does not exist on this host",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestFormatTmpIsolation pins formatTmpIsolation's exact lines without going
// through the command, including the empty-vs-unknown distinction.
func TestFormatTmpIsolation(t *testing.T) {
	tests := []struct {
		name   string
		level  string
		detail string
		want   string
	}{
		{
			name:  "private",
			level: "private",
			want:  "  /tmp:     private (per-session pam_namespace instance)\n",
		},
		{
			name:   "host-shared with detail",
			level:  "host-shared",
			detail: "the pam_namespace drop-in configuration is missing",
			want: "  /tmp:     HOST-SHARED — agent sessions see the host /tmp\n" +
				"\n" +
				"  ╔══════════════════════════════════════════════════════════╗\n" +
				"  ║  ⚠  WARNING: Private /tmp is NOT active on this host.           ║\n" +
				"  ║  Agent exec sessions share the host /tmp; the README's        ║\n" +
				"  ║  'Private /tmp per agent' promise does not hold here.         ║\n" +
				"  ╚══════════════════════════════════════════════════════════╝\n" +
				"  Reason:   the pam_namespace drop-in configuration is missing\n",
		},
		{
			name:  "unknown without detail",
			level: "unknown",
			want:  "  /tmp:     unknown\n",
		},
		{
			name:   "unknown with detail",
			level:  "unknown",
			detail: "probe failed",
			want:   "  /tmp:     unknown — probe failed\n",
		},
		{
			name:  "empty level",
			level: "",
			want:  "  /tmp:     not reported by this daemon — it predates capability reporting; build/run a daemon from the same commit as the CLI (private /tmp is not guaranteed)\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTmpIsolation(tt.level, tt.detail)
			if got != tt.want {
				t.Errorf("formatTmpIsolation(%q, %q) =\n%q\nwant\n%q", tt.level, tt.detail, got, tt.want)
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

// ── DF-BUNKER-21 (AC5): `bunker status` reports residue counts ──────────────

// residueInfo builds a ServerInfo with a scripted residue inventory.
func residueInfo(inv *v1.ResidueInventory) *v1.ServerInfoResponse {
	return &v1.ServerInfoResponse{
		Hostname:      "residue-host",
		Version:       "v1.0.0",
		UptimeSeconds: 120,
		AgentCount:    inv.GetRegisteredAgents(),
		MaxAgents:     10,
		Residue:       inv,
	}
}

// runStatusSingle boots a mock server with the given ServerInfo, points the CLI
// config at it and returns `bunker status` output.
func runStatusSingle(t *testing.T, info *v1.ServerInfoResponse) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	srv := newStatusTestServer(t, &statusMockServer{info: info})
	defer srv.Close()
	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers:      map[string]ServerEntry{"default": {Name: "default", URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	cmd := NewStatusCommand()
	return captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
}

// TestStatusCommand_ResidueInventoryReporting is the AC5 acceptance: an operator
// running `bunker status` against a host carrying leaked agent state sees the
// four counts, including the QA-BUNKER-19 shape (residue with nothing
// registered).
func TestStatusCommand_ResidueInventoryReporting(t *testing.T) {
	output := runStatusSingle(t, residueInfo(&v1.ResidueInventory{
		OrphanUsers:        11,
		OrphanHomes:        9,
		OrphanKeys:         3,
		StaleLingerEntries: 4,
		RegisteredAgents:   0,
		Status:             "ok",
	}))

	for _, want := range []string{
		"  Residue:  11 orphan users, 9 orphan homes, 3 orphan keys, 4 stale linger entries (0 registered agents)",
		"residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
	// A clean host must NOT carry the residue-present note.
	clean := runStatusSingle(t, residueInfo(&v1.ResidueInventory{RegisteredAgents: 2, Status: "ok"}))
	if !strings.Contains(clean, "  Residue:  0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (2 registered agents)") {
		t.Errorf("clean inventory line missing, got:\n%s", clean)
	}
	if strings.Contains(clean, "residue present") {
		t.Errorf("a clean host must not warn about residue, got:\n%s", clean)
	}
}

// A daemon that predates residue reporting must be named as such: printing
// zeroes it never probed would be a fabricated "host is clean".
func TestStatusCommand_ResidueNotReportedByDaemon(t *testing.T) {
	output := runStatusSingle(t, residueInfo(nil))

	if !strings.Contains(output, "  Residue:  not reported by this daemon — it predates residue inventory reporting (DF-BUNKER-21)") {
		t.Errorf("output missing the not-reported line, got:\n%s", output)
	}
	if strings.Contains(output, "0 orphan users") {
		t.Errorf("the CLI must not print counts the daemon never probed, got:\n%s", output)
	}
}

// A partial probe prints the counts AND the reason they are a lower bound.
func TestStatusCommand_ResiduePartialProbe(t *testing.T) {
	output := runStatusSingle(t, residueInfo(&v1.ResidueInventory{
		OrphanUsers:      2,
		RegisteredAgents: 0,
		Status:           "partial",
		Detail:           "keys: open /etc/bunkerd/ssh: permission denied",
	}))

	for _, want := range []string{
		"  Residue:  2 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (0 registered agents)",
		"  Probe:    partial — the counts above are a LOWER BOUND (not every plane could be read)",
		"            keys: open /etc/bunkerd/ssh: permission denied",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q, got:\n%s", want, output)
		}
	}
}

// TestStatusCommand_AllServers_Residue proves the line reaches --all output too.
func TestStatusCommand_AllServers_Residue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newStatusTestServer(t, &statusMockServer{
		info: residueInfo(&v1.ResidueInventory{OrphanUsers: 1, StaleLingerEntries: 1, Status: "ok"}),
	})
	defer srv.Close()
	cfg := &CLIConfig{
		ActiveServer: "default",
		Servers:      map[string]ServerEntry{"default": {Name: "default", URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	cmd := NewStatusCommand()
	cmd.SetArgs([]string{"--server", "default", "--all"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	})
	if !strings.Contains(output, "  Residue:  1 orphan user, 0 orphan homes, 0 orphan keys, 1 stale linger entry (0 registered agents)") {
		t.Errorf("--all output missing the residue line, got:\n%s", output)
	}
}

// TestFormatResidue pins the exact rendering, including the statuses that are
// NOT "ok" (a daemon that reports counts but no status is not proof of a
// complete probe).
func TestFormatResidue(t *testing.T) {
	tests := []struct {
		name string
		inv  *v1.ResidueInventory
		want string
	}{
		{
			name: "absent (daemon predates residue reporting)",
			inv:  nil,
			want: "  Residue:  not reported by this daemon — it predates residue inventory reporting (DF-BUNKER-21); agent users/homes/keys/linger entries left behind on this host are NOT visible here\n",
		},
		{
			name: "ok",
			inv:  &v1.ResidueInventory{RegisteredAgents: 3, Status: "ok"},
			want: "  Residue:  0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (3 registered agents)\n",
		},
		{
			name: "residue present adds the note",
			inv:  &v1.ResidueInventory{OrphanUsers: 2, OrphanHomes: 1, StaleLingerEntries: 1, Status: "ok"},
			want: "  Residue:  2 orphan users, 1 orphan home, 0 orphan keys, 1 stale linger entry (0 registered agents)\n" +
				"            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them\n",
		},
		{
			name: "partial probe with detail",
			inv:  &v1.ResidueInventory{OrphanKeys: 1, Status: "partial", Detail: "homes: read /home: permission denied"},
			want: "  Residue:  0 orphan users, 0 orphan homes, 1 orphan key, 0 stale linger entries (0 registered agents)\n" +
				"  Probe:    partial — the counts above are a LOWER BOUND (not every plane could be read)\n" +
				"            homes: read /home: permission denied\n" +
				"            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them\n",
		},
		{
			name: "unavailable probe",
			inv:  &v1.ResidueInventory{Status: "unavailable", Detail: "users: probe unavailable"},
			want: "  Residue:  0 orphan users, 0 orphan homes, 0 orphan keys, 0 stale linger entries (0 registered agents)\n" +
				"  Probe:    unavailable — the counts above are a LOWER BOUND (not every plane could be read)\n" +
				"            users: probe unavailable\n",
		},
		{
			name: "counts without a status field",
			inv:  &v1.ResidueInventory{OrphanUsers: 1},
			want: "  Residue:  1 orphan user, 0 orphan homes, 0 orphan keys, 0 stale linger entries (0 registered agents)\n" +
				"  Probe:    status not reported by this daemon — the counts above may be a lower bound\n" +
				"            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatResidue(tt.inv); got != tt.want {
				t.Errorf("formatResidue() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
