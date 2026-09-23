package cli

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		cmd.SetArgs([]string{"--server", "default", "--help"})
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
	t.Setenv(SessionTargetEnvVar, "") // explicit: no session binding either

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"abc123", "docker", "ps"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when no active server")
	} else if !strings.Contains(err.Error(), "no target bound") {
		t.Fatalf("expected 'no target bound' refusal, got: %v", err)
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "docker", "ps"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "docker", "rm", "missing"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "sh", "-c", "exit 7"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "docker", "ps"})
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
	cmd.SetArgs([]string{"--server", "default", "missing", "docker", "ps"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "docker", "build", "."})
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
	cmd.SetArgs([]string{"--server", "default", "--timeout", "60", "abc123", "sleep", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("exec command with timeout flag failed: %v", err)
	}
}

func TestExecCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"--server", "default", "abc123"}) // missing command
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "--", "docker", "run", "--rm", "hello-world"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "docker", "run", "--rm", "hello-world"})
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
	cmd.SetArgs([]string{"--server", "default", "--timeout", "60", "abc123", "--", "docker", "ps"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "--", "--raw", "docker", "ps", "--format", "{{.Names}}"})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "--script", scriptFile})
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
	cmd.SetArgs([]string{"--server", "default", "abc123", "--script", "/does/not/exist.sh"})
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
			wantTimeout: execDefaultTimeoutSeconds, // --timeout flag default
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
			wantTimeout: execDefaultTimeoutSeconds, // --timeout flag default
		},
		{
			name:        "no separator",
			args:        []string{"abc", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: execDefaultTimeoutSeconds, // --timeout flag default
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
			// Fail-closed binding (GAP-093): the implicit-default cases need an
			// explicit binding now. Inject it via the session env var so the
			// flag-parsing behavior under test is untouched.
			if !slices.Contains(tt.args, "--server") {
				t.Setenv(SessionTargetEnvVar, "default")
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

// TestExecCommand_FlagsBeforeAgentID verifies that the four exec flags are
// accepted BOTH before and after the agent-id token, that the "=" inline
// forms parse, and that an unknown flag-like token before the agent-id
// fails locally with an actionable message instead of reaching the server
// and dying as a not_found stream error (DF-BUNKER-8: "bunker --server X
// exec abc -- ..." previously sent agent-id="--server").
func TestExecCommand_FlagsBeforeAgentID(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		registerSrv   string // extra server alias to register; "" = default only
		scriptBody    string // if set, written to a temp file replacing {SCRIPT}
		wantCommand   string
		wantArgs      []string
		wantTimeout   uint32
		wantRaw       bool
		wantScript    string // expected ScriptContent; "" = expect empty
		wantErr       string // expected error substring; "" = expect success
		wantNoRequest bool   // expect the server to receive NO request
	}{
		{
			name:        "server flag before agent-id",
			args:        []string{"--server", "srv2", "agent1", "--", "docker", "ps"},
			registerSrv: "srv2",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: execDefaultTimeoutSeconds,
		},
		{
			name:        "server=NAME inline before agent-id",
			args:        []string{"--server=srv2", "agent1", "--", "docker", "ps"},
			registerSrv: "srv2",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: execDefaultTimeoutSeconds,
		},
		{
			name:        "timeout flag before agent-id",
			args:        []string{"--timeout", "90", "abc", "--", "sleep", "1"},
			wantCommand: "sleep",
			wantArgs:    []string{"1"},
			wantTimeout: 90,
		},
		{
			name:        "timeout=900 inline before agent-id",
			args:        []string{"--timeout=900", "abc", "--", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 900,
		},
		{
			name:        "server and timeout both before agent-id",
			args:        []string{"--server", "srv2", "--timeout", "900", "abc", "--", "docker", "ps"},
			registerSrv: "srv2",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: 900,
		},
		{
			name:        "raw flag before agent-id",
			args:        []string{"--raw", "abc", "--", "docker", "ps", "--format", "{{.Names}}"},
			wantCommand: "docker",
			wantArgs:    []string{"ps", "--format", "{{.Names}}"},
			wantTimeout: execDefaultTimeoutSeconds,
			wantRaw:     true,
		},
		{
			name:        "script flag before agent-id",
			args:        []string{"--script", "{SCRIPT}", "abc"},
			scriptBody:  "#!/bin/sh\necho hello-before-agent-id",
			wantCommand: "",
			wantTimeout: execDefaultTimeoutSeconds,
			wantScript:  "#!/bin/sh\necho hello-before-agent-id",
		},
		{
			name:        "script=PATH inline before agent-id",
			args:        []string{"--script={SCRIPT}", "abc"},
			scriptBody:  "#!/bin/sh\necho hello-inline-script",
			wantCommand: "",
			wantTimeout: execDefaultTimeoutSeconds,
			wantScript:  "#!/bin/sh\necho hello-inline-script",
		},
		{
			name:        "separator before agent-id is not a flag",
			args:        []string{"--", "abc", "docker", "ps"},
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: execDefaultTimeoutSeconds,
		},
		{
			name:        "server flag after agent-id still works",
			args:        []string{"agent1", "--server", "srv2", "--", "docker", "ps"},
			registerSrv: "srv2",
			wantCommand: "docker",
			wantArgs:    []string{"ps"},
			wantTimeout: execDefaultTimeoutSeconds,
		},
		{
			name:          "unknown flag before agent-id fails locally",
			args:          []string{"--bogus", "abc", "--", "docker", "ps"},
			wantErr:       `exec takes no flags before <agent-id> (got "--bogus")`,
			wantNoRequest: true,
		},
		{
			name:          "flags but no agent-id",
			args:          []string{"--timeout", "60"},
			wantErr:       "agent-id required after flags",
			wantNoRequest: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			var got *v1.ExecAgentRequest
			requests := 0
			server := newExecTestServer(t, &mockExecServer{
				execResponses: []*v1.ExecAgentResponse{
					{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
					{ExitCode: 0},
				},
				captureReq: func(req *v1.ExecAgentRequest) {
					requests++
					got = req
				},
			})
			defer server.Close()

			cfg := &CLIConfig{
				Servers: map[string]ServerEntry{
					"default": {
						Name:        "default",
						URL:         server.URL,
						ConnectedAt: "2026-06-28T00:00:00Z",
					},
				},
				ActiveServer: "default",
			}
			if tt.registerSrv != "" {
				cfg.Servers[tt.registerSrv] = ServerEntry{
					Name:        tt.registerSrv,
					URL:         server.URL,
					ConnectedAt: "2026-06-28T00:00:00Z",
				}
			}
			if err := SaveCLIConfig(cfg); err != nil {
				t.Fatalf("SaveCLIConfig: %v", err)
			}

			args := append([]string(nil), tt.args...)
			// Fail-closed binding (GAP-093): cases without --server bind via
			// the session env so the flag-parse behavior under test is
			// untouched; wantErr cases may leave the session unbound on
			// purpose (their error is the point).
			if tt.wantErr == "" && !slices.Contains(args, "--server") {
				t.Setenv(SessionTargetEnvVar, tt.registerSrv)
				if tt.registerSrv == "" {
					t.Setenv(SessionTargetEnvVar, "default")
				}
			}
			if tt.scriptBody != "" {
				scriptFile := filepath.Join(tmpDir, "script.sh")
				if err := os.WriteFile(scriptFile, []byte(tt.scriptBody), 0o644); err != nil {
					t.Fatalf("write script: %v", err)
				}
				for i, a := range args {
					if idx := strings.Index(a, "{SCRIPT}"); idx >= 0 {
						args[i] = strings.ReplaceAll(a, "{SCRIPT}", scriptFile)
					}
				}
			}

			cmd := NewExecCommand()
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("exec command failed: %v", err)
			}

			if tt.wantNoRequest {
				if requests != 0 {
					t.Fatalf("server received %d request(s), want 0", requests)
				}
				return
			}
			if requests != 1 {
				t.Fatalf("server received %d request(s), want 1", requests)
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
			if got.Raw != tt.wantRaw {
				t.Errorf("Raw = %v, want %v", got.Raw, tt.wantRaw)
			}
			if tt.wantScript != "" && got.ScriptContent != tt.wantScript {
				t.Errorf("ScriptContent = %q, want %q", got.ScriptContent, tt.wantScript)
			}
		})
	}
}
