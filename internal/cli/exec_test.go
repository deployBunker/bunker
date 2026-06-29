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

// newExecTestServer starts an httptest server with a chi router mounting
// the connect handler for the given BunkerdHandler implementation.
func newExecTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

// mockExecServer implements BunkerdHandler with configurable
// ExecAgent response stream. All other methods return Unimplemented.
type mockExecServer struct {
	mockBunkerdServer
	execResponses []*v1.ExecAgentResponse
	execErr       error
}

func (m *mockExecServer) ExecAgent(
	ctx context.Context,
	req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse],
) error {
	if m.execErr != nil {
		return m.execErr
	}
	for _, resp := range m.execResponses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// writeExecTestConfig writes a CLIConfig with a single server entry
// pointing at the given URL, and sets it as the active server.
func writeExecTestConfig(t *testing.T, home, serverURL string) {
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

func TestExecCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewExecCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "Execute a command") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--timeout") {
		t.Errorf("help output missing --timeout flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--server") {
		t.Errorf("help output missing --server flag, got:\n%s", output)
	}
}

func TestExecCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "ps"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no active server")
	} else if !strings.Contains(err.Error(), "bunker connect") {
		t.Fatalf("expected 'bunker connect' error, got: %v", err)
	}
}

func TestExecCommand_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("CONTAINER ID")}},
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("  IMAGE")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "ps"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
}

func TestExecCommand_ExitCode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stderr{Stderr: []byte("Error: No such container")}},
			{ExitCode: 1},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "rm", "missing"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for non-zero exit code")
	} else if !strings.Contains(err.Error(), "exit code 1") {
		t.Fatalf("expected 'exit code 1' error, got: %v", err)
	}
}

func TestExecCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execErr: connect.NewError(connect.CodeInternal, nil),
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "ps"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for server failure")
	}
}

func TestExecCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execErr: connect.NewError(connect.CodeNotFound, nil),
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"missing", "docker", "ps"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for not found agent")
	}
}

func TestExecCommand_StderrOutput(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stderr{Stderr: []byte("warning: something")}},
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("done")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "build", "."})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
}

func TestExecCommand_TimeoutFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"--timeout", "60", "abc123", "sleep", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command with timeout flag failed: %v", err)
	}
}

func TestExecCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123"}) // missing command
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing command argument")
	}
}
