package cli

import (
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestEnvFlagGrammarEveryPosition is the DF-BUNKER-42 position table:
// --server/--timeout — space and inline forms, flag before, between, and
// after the payload arguments — must parse as the flag, never as a payload
// token, so no valid placement produces "requires exactly one ...". Error
// rows assert the exact local refusal AND (for arity rows) that the
// leftover arguments the validator saw are named in the message, and that
// the server received zero requests: every refusal happens before any RPC.
func TestEnvFlagGrammarEveryPosition(t *testing.T) {
	rows := []struct {
		name          string
		args          []string
		wantErrParts  []string // all must appear in the error; empty = success
		wantNoRequest bool     // expect the server to receive NO request
		wantAgentID   string
		wantShellPart string // a substring the constructed shell command must contain
	}{
		// -- Every position x both forms reaches the server.
		{
			name:          "set: server space after payload",
			args:          []string{"set", "abcd", "FOO=bar", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: server inline after payload",
			args:          []string{"set", "abcd", "FOO=bar", "--server=default"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: server space between subcommand and agent-id",
			args:          []string{"set", "--server", "default", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: server inline between subcommand and agent-id",
			args:          []string{"set", "--server=default", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: server space before subcommand",
			args:          []string{"--server", "default", "set", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: server inline before subcommand",
			args:          []string{"--server=default", "set", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: timeout space after payload",
			args:          []string{"set", "abcd", "FOO=bar", "--timeout", "5", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "set: timeout inline between subcommand and agent-id",
			args:          []string{"set", "--timeout=5", "abcd", "FOO=bar", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "get: server space after payload",
			args:          []string{"get", "abcd", "DATABASE_URL", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "get: server inline after payload",
			args:          []string{"get", "abcd", "DATABASE_URL", "--server=default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "get: timeout space after payload",
			args:          []string{"get", "abcd", "DATABASE_URL", "--timeout", "5", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "get: timeout inline between subcommand and agent-id",
			args:          []string{"get", "--timeout=5", "abcd", "DATABASE_URL", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "timeout space before subcommand",
			args:          []string{"--timeout", "5", "--server", "default", "set", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "flags in every position at once",
			args:          []string{"--server", "default", "set", "--timeout=5", "abcd", "FOO=bar", "--timeout", "45"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},

		// -- Genuine arity mistakes still refuse, but the error NAMES the
		//    leftover arguments the validator saw, so a flag-shaped token
		//    that reached the payload can no longer vanish behind a bare
		//    count complaint. All locally, zero RPCs.
		{
			name:          "set arity error names the leftover arguments",
			args:          []string{"set", "abcd", "FOO=bar", "EXTRA", "--server", "default"},
			wantErrParts:  []string{"env set requires exactly one KEY=VALUE argument", `"FOO=bar"`, `"EXTRA"`},
			wantNoRequest: true,
		},
		{
			name:          "get arity error names the leftover arguments",
			args:          []string{"get", "abcd", "A", "B", "--server", "default"},
			wantErrParts:  []string{"env get requires exactly one KEY argument", `"A"`, `"B"`},
			wantNoRequest: true,
		},
		{
			name:          "set arity error names a flag swallowed past --",
			args:          []string{"set", "abcd", "FOO=bar", "--", "--server", "default"},
			wantErrParts:  []string{"env set requires exactly one KEY=VALUE argument", "--server"},
			wantNoRequest: true,
		},
		{
			name:          "get arity error names the empty leftover",
			args:          []string{"get", "abcd", "--server", "default"},
			wantErrParts:  []string{"env get requires exactly one KEY argument", "[]"},
			wantNoRequest: true,
		},
		{
			name:          "flag between payload tokens is peeled before the arity check",
			args:          []string{"set", "abcd", "FOO=bar", "--server", "default", "EXTRA"},
			wantErrParts:  []string{"env set requires exactly one KEY=VALUE argument", "EXTRA"},
			wantNoRequest: true,
		},
		{
			name:          "malformed payload reported without needing a server",
			args:          []string{"set", "abcd", "NOEQUALS", "--server", "default"},
			wantErrParts:  []string{"expected KEY=VALUE"},
			wantNoRequest: true,
		},

		// -- Unknown flags still refuse locally, naming the token.
		{
			name:          "unknown flag after payload refuses naming it",
			args:          []string{"set", "abcd", "FOO=bar", "--bogus", "--server", "default"},
			wantErrParts:  []string{`env takes no flags (got "--bogus")`},
			wantNoRequest: true,
		},
		{
			name:          "unknown flag before subcommand refuses naming it",
			args:          []string{"--bogus", "set", "abcd", "FOO=bar"},
			wantErrParts:  []string{`env takes no flags (got "--bogus")`},
			wantNoRequest: true,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			mock := &mockEnvServer{
				mockExecServer: mockExecServer{
					execResponses: []*v1.ExecAgentResponse{
						{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
						{ExitCode: 0},
					},
				},
			}
			server := newExecTestServer(t, mock)
			defer server.Close()
			writeExecTestConfig(t, tmpDir, server.URL)

			cmd := NewEnvCommand()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			if len(tt.wantErrParts) > 0 {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrParts)
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("error %q missing %q", err.Error(), part)
					}
				}
			} else if err != nil {
				t.Fatalf("env command failed: %v", err)
			}

			if tt.wantNoRequest {
				if mock.execReq != nil {
					t.Fatal("server received a request, want 0")
				}
				return
			}
			if mock.execReq == nil {
				t.Fatal("ExecAgent request not captured")
			}
			if got := mock.execReq.GetAgentId(); got != tt.wantAgentID {
				t.Errorf("agent_id = %q, want %q", got, tt.wantAgentID)
			}
			shell := mock.execReq.GetArgs()[1]
			if !strings.Contains(shell, tt.wantShellPart) {
				t.Errorf("shell command %q missing %q", shell, tt.wantShellPart)
			}
		})
	}
}
