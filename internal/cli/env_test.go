package cli

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// mockEnvServer embeds mockExecServer so it satisfies both BunkerdHandler and
// captures the ExecAgent request (so tests can assert on the constructed
// shell command).
type mockEnvServer struct {
	mockExecServer
	execReq *v1.ExecAgentRequest
}

func (m *mockEnvServer) ExecAgent(
	ctx context.Context,
	req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse],
) error {
	// Capture first; then mirror mockExecServer.ExecAgent's send behavior so
	// tests that pre-load execResponses still work.
	m.execReq = req.Msg
	for _, resp := range m.execResponses {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return m.execErr
}

func TestEnvCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		if err := cmd.Execute(); err != nil {
			t.Logf("help Execute returned: %v", err)
		}
	})

	if !strings.Contains(output, "environment variables") {
		t.Errorf("help missing description, got:\n%s", output)
	}
	for _, sub := range []string{"set", "get", "list", "unset"} {
		if !strings.Contains(output, sub) {
			t.Errorf("help missing subcommand %q", sub)
		}
	}
}

func TestEnvCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "abc123", "FOO=bar"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no active server")
	}
	if !strings.Contains(err.Error(), "bunker connect") {
		t.Fatalf("expected 'bunker connect' error, got: %v", err)
	}
}

func TestEnvCommand_MissingSubcommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"abc123"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for missing subcommand")
	}
}

func TestEnvCommand_UnknownSubcommand(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Need an active server for the dispatch switch to be reached, otherwise
	// we hit the "no active server" path first.
	writeExecTestConfig(t, tmpDir, "http://127.0.0.1:1") // unreachable is fine

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"frobnicate", "abc123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown env subcommand") {
		t.Fatalf("expected unknown-subcommand error, got: %v", err)
	}
}

func TestEnvSet_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{ExitCode: 0},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "abcd", "DATABASE_URL=postgres://db.local/app"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env set failed: %v", err)
	}

	if mock.execReq == nil {
		t.Fatal("ExecAgent not invoked")
	}
	if got := mock.execReq.GetAgentId(); got != "abcd" {
		t.Errorf("agent_id = %q, want abcd", got)
	}
	if got := mock.execReq.GetCommand(); got != "sh" {
		t.Errorf("command = %q, want sh", got)
	}
	if len(mock.execReq.GetArgs()) != 2 || mock.execReq.GetArgs()[0] != "-c" {
		t.Fatalf("args = %v, want [-c <shell>]", mock.execReq.GetArgs())
	}

	shell := mock.execReq.GetArgs()[1]
	if !strings.Contains(shell, "/run/bunker/abcd/env") {
		t.Errorf("shell command should reference env file path: %s", shell)
	}
	if !strings.Contains(shell, "DATABASE_URL") {
		t.Errorf("shell command should contain KEY: %s", shell)
	}
	if !strings.Contains(shell, "postgres://db.local/app") {
		t.Errorf("shell command should contain VALUE: %s", shell)
	}
	// The construction uses `if [ -f ... ] && grep -qF ... ; then sed -i ...; else printf ...`.
	if !strings.Contains(shell, "grep -qF") {
		t.Errorf("set shell should use grep -qF for idempotent KEY detection: %s", shell)
	}
	if !strings.Contains(shell, "sed -i") {
		t.Errorf("set shell should use sed -i for in-place replacement: %s", shell)
	}
	if !strings.Contains(shell, "printf") {
		t.Errorf("set shell should use printf for safe append: %s", shell)
	}
}

func TestEnvSet_InvalidKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	// Leading digit is not allowed by POSIX env name rules.
	cmd.SetArgs([]string{"set", "abc", "1FOO=bar"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid env key")
	}
	if !strings.Contains(err.Error(), "invalid env key") {
		t.Fatalf("expected 'invalid env key' error, got: %v", err)
	}
}

func TestEnvSet_InvalidFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "abc", "NOEQUALS"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing '=' separator")
	}
	if !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("expected KEY=VALUE error, got: %v", err)
	}
}

func TestEnvSet_EmptyValue(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{ExitCode: 0},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "abcd", "EMPTY="})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env set with empty value failed: %v", err)
	}
	if !strings.Contains(mock.execReq.GetArgs()[1], "EMPTY=") {
		t.Errorf("shell command should still contain EMPTY=: %s", mock.execReq.GetArgs()[1])
	}
}

func TestEnvGet_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("postgres://db.local/app")}},
				{ExitCode: 0},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"get", "abcd", "DATABASE_URL"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env get failed: %v", err)
	}

	shell := mock.execReq.GetArgs()[1]
	// get should use awk to print just the value (after the first '=').
	if !strings.Contains(shell, "awk") {
		t.Errorf("get shell should use awk: %s", shell)
	}
	if !strings.Contains(shell, "DATABASE_URL") {
		t.Errorf("get shell should contain KEY: %s", shell)
	}
}

func TestEnvGet_InvalidKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"get", "abc", "BAD-KEY"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for invalid env key")
	}
}

func TestEnvList_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("FOO=bar\nBAZ=qux\n")}},
				{ExitCode: 0},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"list", "abcd"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env list failed: %v", err)
	}

	shell := mock.execReq.GetArgs()[1]
	if !strings.Contains(shell, "[ -f") {
		t.Errorf("list shell should test for file existence: %s", shell)
	}
	if !strings.Contains(shell, "cat ") {
		t.Errorf("list shell should cat the file: %s", shell)
	}
}

func TestEnvUnset_Success(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{ExitCode: 0},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"unset", "abcd", "DATABASE_URL"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env unset failed: %v", err)
	}

	shell := mock.execReq.GetArgs()[1]
	if !strings.Contains(shell, "sed -i") {
		t.Errorf("unset shell should use sed -i: %s", shell)
	}
	if !strings.Contains(shell, "/^DATABASE_URL=/d") {
		t.Errorf("unset shell should delete lines beginning with KEY=: %s", shell)
	}
}

func TestEnvUnset_InvalidKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"unset", "abc", "BAD KEY"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for invalid env key with space")
	}
}

func TestEnvSet_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execErr: connect.NewError(connect.CodeNotFound, nil),
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "missing", "FOO=bar"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for not-found agent")
	}
}

func TestEnvSet_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execErr: connect.NewError(connect.CodeInternal, nil),
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"set", "abcd", "FOO=bar"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for server failure")
	}
}

func TestEnvGet_ExitNonZero_NoError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Simulate `awk` exiting non-zero because the key is not set. env get
	// should not surface this as an error — it's a normal "key not present"
	// outcome for a missing variable.
	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{
				{ExitCode: 1},
			},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"get", "abcd", "MISSING_KEY"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env get on missing key should NOT return an error, got: %v", err)
	}
}

func TestEnvList_TimeoutFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &mockEnvServer{
		mockExecServer: mockExecServer{
			execResponses: []*v1.ExecAgentResponse{{ExitCode: 0}},
		},
	}
	server := newExecTestServer(t, mock)
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewEnvCommand()
	cmd.SetArgs([]string{"--timeout", "60", "list", "abcd"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("env list with --timeout failed: %v", err)
	}
}

func TestParseEnvAssignment(t *testing.T) {
	cases := []struct {
		in              string
		wantKey         string
		wantValue       string
		wantErrContains string
	}{
		{"FOO=bar", "FOO", "bar", ""},
		{"DATABASE_URL=postgres://db.local/app", "DATABASE_URL", "postgres://db.local/app", ""},
		{"EMPTY=", "EMPTY", "", ""},
		{"=novalue", "", "", "expected KEY=VALUE"},
		{"NOEQUALS", "", "", "expected KEY=VALUE"},
		{"1FOO=bar", "", "", "invalid env key"},
		{"BAD-KEY=x", "", "", "invalid env key"},
		{"FOO=line1\nline2", "", "", "must not contain newlines"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			k, v, err := parseEnvAssignment(c.in)
			if c.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", c.wantErrContains)
				}
				if !strings.Contains(err.Error(), c.wantErrContains) {
					t.Fatalf("error %v missing %q", err, c.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if k != c.wantKey {
				t.Errorf("key = %q, want %q", k, c.wantKey)
			}
			if v != c.wantValue {
				t.Errorf("value = %q, want %q", v, c.wantValue)
			}
		})
	}
}

func TestEnvFilePath(t *testing.T) {
	if got, want := envFilePath("abcd"), "/run/bunker/abcd/env"; got != want {
		t.Errorf("envFilePath(abcd) = %q, want %q", got, want)
	}
}
