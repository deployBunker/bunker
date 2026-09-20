package cli

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// errLifecycleNotFound stands in for the server's not-found error on the wire.
var errLifecycleNotFound = errors.New("agent not found")

// This file pins the GAP-071 CLI surface: `bunker stop`, `bunker start` and
// `bunker restart` exist, print usage, call the matching RPC and report the
// server's answer (including the not-found error) without inventing anything.

// mockLifecycleServer implements BunkerdHandler with configurable Stop/Start/
// Restart responses; every other method returns Unimplemented.
type mockLifecycleServer struct {
	mockBunkerdServer

	stopResp    *v1.StopAgentResponse
	startResp   *v1.StartAgentResponse
	restartResp *v1.RestartAgentResponse
	err         error

	stopCalled    bool
	startCalled   bool
	restartCalled bool
	gotAgentID    string
}

func (m *mockLifecycleServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	m.stopCalled = true
	m.gotAgentID = req.Msg.GetAgentId()
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.stopResp), nil
}

func (m *mockLifecycleServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	m.startCalled = true
	m.gotAgentID = req.Msg.GetAgentId()
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.startResp), nil
}

func (m *mockLifecycleServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
	m.restartCalled = true
	m.gotAgentID = req.Msg.GetAgentId()
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(m.restartResp), nil
}

// newLifecycleTestServer mounts a BunkerdHandler on an httptest server.
func newLifecycleTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// TestLifecycleCommands_Help asserts each command exists and prints usage.
func TestLifecycleCommands_Help(t *testing.T) {
	tests := []struct {
		name     string
		short    string
		use      string
		longLine string
		wantDoc  string
	}{
		{
			name:     "stop",
			short:    "Stop an agent (keeping its state)",
			use:      "stop <agent-id>",
			longLine: "Stop an agent on the active bunkerd server.",
			wantDoc:  "bunker start",
		},
		{
			name:     "start",
			short:    "Start a stopped agent",
			use:      "start <agent-id>",
			longLine: "Start a stopped agent on the active bunkerd server.",
			wantDoc:  "bunker stop",
		},
		{
			name:     "restart",
			short:    "Restart an agent (stop + start, TTL reset)",
			use:      "restart <agent-id>",
			longLine: "Restart an agent on the active bunkerd server.",
			wantDoc:  "heartbeat expiry is reset",
		},
	}

	commands := map[string]func() *cobra.Command{
		"stop":    NewStopCommand,
		"start":   NewStartCommand,
		"restart": NewRestartCommand,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			cmd := commands[tt.name]()
			output := captureStdout(t, func() {
				cmd.SetArgs([]string{"--server", "default", "--help"})
				_ = cmd.Execute()
			})
			// Cobra renders Long (falling back to Short) in --help, so the
			// usage assertion targets the text a user actually sees.
			if !strings.Contains(output, tt.longLine) {
				t.Errorf("--help output missing %q, got:\n%s", tt.longLine, output)
			}
			if !strings.Contains(output, "--server") {
				t.Errorf("--help output missing --server, got:\n%s", output)
			}
			if cmd.Short != tt.short {
				t.Errorf("Short = %q, want %q", cmd.Short, tt.short)
			}
			if cmd.Use != tt.use {
				t.Errorf("Use = %q, want %q", cmd.Use, tt.use)
			}
			if !strings.Contains(cmd.Long, tt.wantDoc) {
				t.Errorf("help text must mention %q, got:\n%s", tt.wantDoc, cmd.Long)
			}
		})
	}
}

// TestStopCommand_Outputs pins the three stop outcomes.
func TestStopCommand_Outputs(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		err        error
		wantOutput string
		wantErr    bool
	}{
		{name: "stopped", status: "stopped", wantOutput: "Agent abc12345 stopped."},
		{name: "already_stopped", status: "already_stopped", wantOutput: "Agent abc12345 is already stopped."},
		{name: "not_found_status", status: "not_found", wantErr: true},
		{name: "rpc_error", err: connect.NewError(connect.CodeNotFound, errLifecycleNotFound), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			mock := &mockLifecycleServer{
				stopResp: &v1.StopAgentResponse{AgentId: "abc12345", Status: tt.status},
				err:      tt.err,
			}
			srv := newLifecycleTestServer(t, mock)
			defer srv.Close()
			writeDestroyTestConfig(t, tmpDir, srv.URL)

			cmd := NewStopCommand()
			cmd.SetArgs([]string{"--server", "default", "abc12345"})
			var err error
			output := captureStdout(t, func() { err = cmd.Execute() })

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !mock.stopCalled || mock.gotAgentID != "abc12345" {
				t.Errorf("StopAgent call = %v (id %q), want one call for abc12345", mock.stopCalled, mock.gotAgentID)
			}
			if !strings.Contains(output, tt.wantOutput) {
				t.Errorf("output missing %q, got:\n%s", tt.wantOutput, output)
			}
		})
	}
}

// TestStartCommand_Outputs pins the start outcomes.
func TestStartCommand_Outputs(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantOutput string
	}{
		{name: "started", status: "started", wantOutput: "Agent abc12345 started."},
		{name: "already_running", status: "already_running", wantOutput: "Agent abc12345 is already running."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			mock := &mockLifecycleServer{startResp: &v1.StartAgentResponse{AgentId: "abc12345", Status: tt.status}}
			srv := newLifecycleTestServer(t, mock)
			defer srv.Close()
			writeDestroyTestConfig(t, tmpDir, srv.URL)

			cmd := NewStartCommand()
			cmd.SetArgs([]string{"--server", "default", "abc12345"})
			var err error
			output := captureStdout(t, func() { err = cmd.Execute() })
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !mock.startCalled {
				t.Error("StartAgent was never called")
			}
			if !strings.Contains(output, tt.wantOutput) {
				t.Errorf("output missing %q, got:\n%s", tt.wantOutput, output)
			}
		})
	}
}

// TestRestartCommand_PrintsRefreshedExpiry pins the restart output: the status
// line plus the refreshed expiry the RPC returned.
func TestRestartCommand_PrintsRefreshedExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockLifecycleServer{restartResp: &v1.RestartAgentResponse{
		AgentId:   "abc12345",
		Status:    "restarted",
		ExpiresAt: "2026-09-18T06:00:00-05:00",
	}}
	srv := newLifecycleTestServer(t, mock)
	defer srv.Close()
	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewRestartCommand()
	cmd.SetArgs([]string{"--server", "default", "abc12345"})
	var err error
	output := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !mock.restartCalled {
		t.Error("RestartAgent was never called")
	}
	if !strings.Contains(output, "Agent abc12345 restarted.") {
		t.Errorf("output missing the restart line, got:\n%s", output)
	}
	if !strings.Contains(output, "2026-09-18T06:00:00-05:00") {
		t.Errorf("output must print the refreshed expiry, got:\n%s", output)
	}
}

// TestRestartCommand_NotFoundExitsNonZero pins the failure contract: the
// server's not-found error surfaces as a non-zero exit (a returned error) with
// the server's message, never a silent success.
func TestRestartCommand_NotFoundExitsNonZero(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockLifecycleServer{err: connect.NewError(connect.CodeNotFound, errLifecycleNotFound)}
	srv := newLifecycleTestServer(t, mock)
	defer srv.Close()
	writeDestroyTestConfig(t, tmpDir, srv.URL)

	cmd := NewRestartCommand()
	cmd.SetArgs([]string{"--server", "default", "ghost"})
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	if err == nil {
		t.Fatal("a not_found agent must make the command fail")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q must carry the server's not-found message", err.Error())
	}
}

// TestLifecycleCommands_RequireAnAgentID pins the arity contract.
func TestLifecycleCommands_RequireAnAgentID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	for name, cmd := range map[string]interface {
		SetArgs([]string)
		Execute() error
	}{
		"stop":    NewStopCommand(),
		"start":   NewStartCommand(),
		"restart": NewRestartCommand(),
	} {
		t.Run(name, func(t *testing.T) {
			cmd.SetArgs([]string{})
			if err := cmd.Execute(); err == nil {
				t.Error("a missing agent id must be a usage error")
			}
		})
	}
}
