package cli

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	captureReq    func(*v1.ExecAgentRequest)
}

func (m *mockExecServer) ExecAgent(
	ctx context.Context,
	req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse],
) error {
	if m.captureReq != nil {
		m.captureReq(req.Msg)
	}
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
		if err := cmd.Execute(); err != nil {
			t.Logf("help Execute returned: %v", err)
		}
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

// TestExecCommand_ExitCode7 verifies that a remote exit code propagates as an
// *ExitError carrying the exact code, so cmd/bunker/main.go can os.Exit(7)
// instead of the default 1.
func TestExecCommand_ExitCode7(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{ExitCode: 7},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "sh", "-c", "exit 7"})
	err := cmd.Execute()
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitError, got: %v", err)
	}
	if exitErr.Code != 7 {
		t.Errorf("exit code = %d, want 7", exitErr.Code)
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

func TestExecCommand_DockerFlagPassthrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var got *v1.ExecAgentRequest
	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("hello")}},
			{ExitCode: 0},
		},
		captureReq: func(req *v1.ExecAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "--", "docker", "run", "--rm", "hello-world"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.Command != "docker" {
		t.Errorf("command = %q, want docker", got.Command)
	}
	wantArgs := []string{"run", "--rm", "hello-world"}
	if len(got.Args) != len(wantArgs) {
		t.Errorf("args = %v, want %v", got.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if got.Args[i] != want {
			t.Errorf("args[%d] = %q, want %q", i, got.Args[i], want)
		}
	}
}

func TestExecCommand_DockerFlagsWithoutDoubleDash(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("hello")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "run", "--rm", "hello-world"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
}

func TestExecCommand_FlagTimeoutBeforeCommand(t *testing.T) {
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
	cmd.SetArgs([]string{"--timeout", "60", "abc123", "--", "docker", "ps"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command with timeout flag failed: %v", err)
	}
}

func TestExecCommand_RawFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	var got *v1.ExecAgentRequest
	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
			{ExitCode: 0},
		},
		captureReq: func(req *v1.ExecAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "--", "--raw", "docker", "ps", "--format", "{{.Names}}"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if !got.Raw {
		t.Errorf("Raw = %v, want true", got.Raw)
	}
	if got.Command != "docker" {
		t.Errorf("command = %q, want docker", got.Command)
	}
	wantArgs := []string{"ps", "--format", "{{.Names}}"}
	if len(got.Args) != len(wantArgs) {
		t.Errorf("args = %v, want %v", got.Args, wantArgs)
	}
	for i, want := range wantArgs {
		if got.Args[i] != want {
			t.Errorf("args[%d] = %q, want %q", i, got.Args[i], want)
		}
	}
}

func TestExecCommand_ScriptFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	scriptFile := filepath.Join(tmpDir, "script.sh")
	scriptBody := "#!/bin/sh\necho hello-from-script"
	if err := os.WriteFile(scriptFile, []byte(scriptBody), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var got *v1.ExecAgentRequest
	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("hello-from-script")}},
			{ExitCode: 0},
		},
		captureReq: func(req *v1.ExecAgentRequest) {
			got = req
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "--script", scriptFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command failed: %v", err)
	}
	if got == nil {
		t.Fatal("request not captured")
	}
	if got.ScriptContent != scriptBody {
		t.Errorf("ScriptContent = %q, want %q", got.ScriptContent, scriptBody)
	}
	if got.Raw {
		t.Error("Raw = true, want false when using script mode")
	}
}

func TestExecCommand_ScriptFlag_MissingFile(t *testing.T) {
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
	cmd.SetArgs([]string{"abc123", "--script", "/does/not/exist.sh"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing script file")
	}
}

// TestExecCommand_FlagSeparator verifies that a "--" separator between bunker
// flags and the remote command is stripped from the parsed request, and that
// the command/args after it are sent verbatim. Regression test for GAP-002:
// "bunker exec <id> --server srv -- docker ps" previously sent Command="--",
// which made the remote shell die with "sh: 0: Illegal option --".
func TestExecCommand_FlagSeparator(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCommand string
		wantArgs    []string
		wantTimeout uint32
		wantServer  string // server alias to register in the test config; "" = default only
	}{
		{
			name:        "flags before separator",
			args:        []string{"ms-a1", "--server", "srv", "--", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30, // --timeout flag default
			wantServer:  "srv",
		},
		{
			name:        "timeout flag before separator",
			args:        []string{"abc", "--timeout", "60", "--", "docker", "build", "-t", "myapp", "."},
			wantCommand: "docker",
			wantArgs:    []string{"build", "-t", "myapp", "."},
			wantTimeout: 60,
		},
		{
			name:        "leading separator",
			args:        []string{"abc", "--", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30, // --timeout flag default
		},
		{
			name:        "no separator",
			args:        []string{"abc", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 30, // --timeout flag default
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			var got *v1.ExecAgentRequest
			server := newExecTestServer(t, &mockExecServer{
				execResponses: []*v1.ExecAgentResponse{
					{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
					{ExitCode: 0},
				},
				captureReq: func(req *v1.ExecAgentRequest) {
					got = req
				},
			})
			defer server.Close()

			if tt.wantServer != "" {
				cfg := &CLIConfig{
					Servers: map[string]ServerEntry{
						"default": {
							Name:        "default",
							URL:         server.URL,
							ConnectedAt: "2026-06-28T00:00:00Z",
						},
						tt.wantServer: {
							Name:        tt.wantServer,
							URL:         server.URL,
							ConnectedAt: "2026-06-28T00:00:00Z",
						},
					},
					ActiveServer: "default",
				}
				if err := SaveCLIConfig(cfg); err != nil {
					t.Fatalf("SaveCLIConfig: %v", err)
				}
			} else {
				writeExecTestConfig(t, tmpDir, server.URL)
			}

			cmd := NewExecCommand()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("exec command failed: %v", err)
			}
			if got == nil {
				t.Fatal("request not captured")
			}
			if got.Command != tt.wantCommand {
				t.Errorf("command = %q, want %q", got.Command, tt.wantCommand)
			}
			if len(got.Args) != len(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", got.Args, tt.wantArgs)
			}
			for i, want := range tt.wantArgs {
				if got.Args[i] != want {
					t.Errorf("args[%d] = %q, want %q", i, got.Args[i], want)
				}
			}
			if got.TimeoutSeconds != tt.wantTimeout {
				t.Errorf("TimeoutSeconds = %d, want %d", got.TimeoutSeconds, tt.wantTimeout)
			}
		})
	}
}
