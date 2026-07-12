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

// mockRunServer implements BunkerdHandler with configurable RunAgent and
// ExecAgent responses.
type mockRunServer struct {
	mockBunkerdServer
	runResp       *v1.RunAgentResponse
	runErr        error
	execResponses []*v1.ExecAgentResponse
	execErr       error
	captureRunReq func(*v1.RunAgentRequest)
}

func (m *mockRunServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	if m.captureRunReq != nil {
		m.captureRunReq(req.Msg)
	}
	if m.runErr != nil {
		return nil, m.runErr
	}
	return connect.NewResponse(m.runResp), nil
}

func (m *mockRunServer) ExecAgent(
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

func newRunTestServer(t *testing.T, handler bunkerv1connect.BunkerdHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(handler)
	r.Mount(path, h)
	return httptest.NewServer(r)
}

func TestRunCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewRunCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		if err := cmd.Execute(); err != nil {
			t.Logf("help Execute returned: %v", err)
		}
	})

	if !strings.Contains(output, "Run a command in an agent") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--detach") {
		t.Errorf("help output missing --detach flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--server") {
		t.Errorf("help output missing --server flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--env") {
		t.Errorf("help output missing --env flag, got:\n%s", output)
	}
}

func TestRunCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "docker", "ps"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no active server")
	} else if !strings.Contains(err.Error(), "bunker connect") {
		t.Fatalf("expected 'bunker connect' error, got: %v", err)
	}
}

func TestRunCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing command argument")
	}
}

func TestRunCommand_MissingAgentID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing agent-id")
	}
}

func TestRunCommand_DetachedSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var got *v1.RunAgentRequest
	server := newRunTestServer(t, &mockRunServer{
		runResp: &v1.RunAgentResponse{
			RunId:    "deadbeef",
			Status:   "running",
			ExitCode: -1,
			UnitName: "bunker-run-abc123-deadbeef",
		},
		captureRunReq: func(req *v1.RunAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "--detach", "--", "docker", "compose", "up"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run --detach failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.GetAgentId() != "abc123" {
		t.Errorf("agent_id = %q, want abc123", got.GetAgentId())
	}
	if got.GetCommand() != "docker" {
		t.Errorf("command = %q, want docker", got.GetCommand())
	}
	wantArgs := []string{"compose", "up"}
	if len(got.GetArgs()) != len(wantArgs) {
		t.Errorf("args = %v, want %v", got.GetArgs(), wantArgs)
	}
	if !got.GetDetach() {
		t.Error("detach = false, want true")
	}
}

func TestRunCommand_DetachedWithEnvAndName(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var got *v1.RunAgentRequest
	server := newRunTestServer(t, &mockRunServer{
		runResp: &v1.RunAgentResponse{
			RunId:    "12345678",
			UnitName: "bunker-run-abc-12345678--api",
			Status:   "running",
			ExitCode: -1,
		},
		captureRunReq: func(req *v1.RunAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc", "--env", "FOO=bar", "--env", "BAZ=qux", "--detach", "--name", "api", "--", "python", "serve.py"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run --detach failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.GetName() != "api" {
		t.Errorf("name = %q, want api", got.GetName())
	}
	if got.GetEnv()["FOO"] != "bar" {
		t.Errorf("env[FOO] = %q, want bar", got.GetEnv()["FOO"])
	}
	if got.GetEnv()["BAZ"] != "qux" {
		t.Errorf("env[BAZ] = %q, want qux", got.GetEnv()["BAZ"])
	}
}

func TestRunCommand_SyncStreamsOutput(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newRunTestServer(t, &mockRunServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("USER PID")}},
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("root 1")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "--", "ps", "aux"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run sync failed: %v", err)
	}
}

func TestRunCommand_SyncExitCode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newRunTestServer(t, &mockRunServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stderr{Stderr: []byte("command not found")}},
			{ExitCode: 127},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "--", "nonexistent-cmd"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for non-zero exit code")
	} else if !strings.Contains(err.Error(), "exit code 127") {
		t.Fatalf("expected 'exit code 127' error, got: %v", err)
	}
}

func TestRunCommand_DetachedServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newRunTestServer(t, &mockRunServer{
		runErr: connect.NewError(connect.CodeInternal, nil),
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "--detach", "--", "docker", "compose", "up"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for server failure")
	}
}

func TestRunCommand_DetachedAgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newRunTestServer(t, &mockRunServer{
		runErr: connect.NewError(connect.CodeNotFound, nil),
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"missing", "--detach", "--", "docker", "compose", "up"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for not found agent")
	}
}

func TestRunCommand_DockerFlagPassthrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var got *v1.RunAgentRequest
	server := newRunTestServer(t, &mockRunServer{
		runResp: &v1.RunAgentResponse{
			RunId:    "deadbeef",
			UnitName: "bunker-run-abc123-deadbeef",
			Status:   "running",
			ExitCode: -1,
		},
		captureRunReq: func(req *v1.RunAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"abc123", "--detach", "--", "docker", "run", "--rm", "-d", "nginx"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run --detach failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	wantArgs := []string{"run", "--rm", "-d", "nginx"}
	if len(got.GetArgs()) != len(wantArgs) {
		t.Errorf("args = %v, want %v", got.GetArgs(), wantArgs)
	}
	for i, want := range wantArgs {
		if got.GetArgs()[i] != want {
			t.Errorf("args[%d] = %q, want %q", i, got.GetArgs()[i], want)
		}
	}
}
