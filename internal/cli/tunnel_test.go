package cli

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// writeTunnelKey writes the client-local SSH key for an agent under
// ~/.bunker/keys/<id> so the tunnel command passes its key-existence check.
func writeTunnelKey(t *testing.T, home, agentID string) string {
	t.Helper()
	keyPath, err := defaultSSHKeyPath(agentID)
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("test-private-key"), 0600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	return keyPath
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
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	keyPath := writeTunnelKey(t, tmpDir, "abc123")

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
		"-i " + keyPath,
		"-L 2376:/run/bunker/abc123/docker.sock",
		"bunker-abc123@127.0.0.1",
		"-N",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("captured args missing %q, got: %q", want, joined)
		}
	}
	for _, absent := range []string{"/etc/bunkerd/ssh/", "@bunker-mvp"} {
		if strings.Contains(joined, absent) {
			t.Errorf("captured args must not contain %q, got: %q", absent, joined)
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
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")

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

func TestTunnelCommand_MissingKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when client-local SSH key is missing")
	}
	if !strings.Contains(err.Error(), "SSH key not found") {
		t.Errorf("expected 'SSH key not found' error, got: %v", err)
	}
}

func TestTunnelCommand_SshHostFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)
	writeTunnelKey(t, tmpDir, "abc123")

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "--ssh-host", "somehost.example"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "bunker-abc123@somehost.example") {
		t.Errorf("expected --ssh-host override in target, got: %q", joined)
	}
	if strings.Contains(joined, "@bunker-mvp") || strings.Contains(joined, "@127.0.0.1") {
		t.Errorf("target host not overridden, got: %q", joined)
	}
}

func TestTunnelCommand_SshKeyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newTunnelTestServer(t, &mockTunnelServer{
		getAgentResp: &v1.GetAgentResponse{
			Agent: &v1.AgentSummary{
				AgentId:          "abc123",
				DockerHostTunnel: "ssh -o StrictHostKeyChecking=no -i /etc/bunkerd/ssh/abc123 -L 2376:/run/bunker/abc123/docker.sock bunker-abc123@bunker-mvp -N",
			},
		},
	})
	defer server.Close()
	writeTunnelTestConfig(t, tmpDir, server.URL)

	customKey := filepath.Join(tmpDir, "custom-key")
	if err := os.WriteFile(customKey, []byte("custom-private-key"), 0600); err != nil {
		t.Fatalf("write custom key: %v", err)
	}

	var capturedArgs []string
	oldExec := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = args
		return exec.CommandContext(ctx, "echo", "mock tunnel")
	}
	defer func() { execCommandContext = oldExec }()

	cmd := NewTunnelCommand()
	cmd.SetArgs([]string{"abc123", "--ssh-key", customKey})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "-i "+customKey) {
		t.Errorf("expected --ssh-key override in args, got: %q", joined)
	}
	if strings.Contains(joined, "/etc/bunkerd/ssh/") {
		t.Errorf("args must not contain server-side key path, got: %q", joined)
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
