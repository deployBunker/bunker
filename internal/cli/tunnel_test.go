package cli

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// newTunnelTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newTunnelTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockTunnelServer implements BunkerdHandler with configurable GetAgent response.
type mockTunnelServer struct {
	mockBunkerdServer
	getAgentResp *v1.GetAgentResponse
	getAgentErr  error
}

func (m *mockTunnelServer) GetAgent(
	ctx context.Context,
	req *connect.Request[v1.GetAgentRequest],
) (*connect.Response[v1.GetAgentResponse], error) {
	if m.getAgentErr != nil {
		return nil, m.getAgentErr
	}
	return connect.NewResponse(m.getAgentResp), nil
}

// writeTunnelTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeTunnelTestConfig(t *testing.T, home, serverURL string) {
	t.Helper()
	cfg := &CLIConfig{
		Servers: map[string]ServerEntry{
			"default": {
				Name:        "default",
				URL:         serverURL,
				ConnectedAt: "2026-06-28T00:00:00Z",
			},
		},
		ActiveServer: "default",
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

func TestTunnelCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "SSH tunnel") {
		t.Errorf("help output missing SSH tunnel description, got:\n%s", output)
	}
	if !strings.Contains(output, "agent-id") {
		t.Errorf("help output missing agent-id argument, got:\n%s", output)
	}
	if !strings.Contains(output, "local-port") {
		t.Errorf("help output missing local-port argument, got:\n%s", output)
	}
}

func TestTunnelCommand_NoActiveServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no active server")
	}
}

func TestTunnelCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentErr: connect.NewError(connect.CodeNotFound, nil),
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"missing"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for not found agent")
	}
}

func TestTunnelCommand_NoTunnelCommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{AgentId: "abc123"},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when agent has no tunnel command")
	}
	if !strings.Contains(err.Error(), "no docker host tunnel command") {
		t.Errorf("expected 'no docker host tunnel command' error, got: %v", err)
	}
}

func TestTunnelCommand_ExecutesStoredCommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /keys/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@localhost -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	// Capture the command that the tunnel command would execute.
	var capturedName string
	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedName = name
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if capturedName != "ssh" {
		t.Errorf("expected command name ssh, got %q", capturedName)
	}
	joined := strings.Join(capturedArgs, " ")
	for _, want := range []string{
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
		"-i /keys/abc123",
		"-L 2376:/run/bunker/abc123/docker.sock",
		"bunker-abc123@localhost",
		"-N",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("captured args missing %q, got: %q", want, joined)
		}
	}
}

func TestTunnelCommand_CustomPort(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /keys/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@localhost -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "2377"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "-L 2377:/run/bunker/abc123/docker.sock") {
		t.Errorf("expected custom port 2377 in -L spec, got: %q", joined)
	}
}

func TestTunnelCommand_InvalidPort(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "not-a-port"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for invalid port")
	}
}

// Ensure imported packages are used (keeps compiler happy in test builds).
var _ = os.Stdout
