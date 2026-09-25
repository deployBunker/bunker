package cli

// SURF-017: the SURF-011 fix was scoped to `bunker exec` — `bunker run`
// and `bunker env` still carried the 30s default and a bare deadline
// error, "the same remote-build killer one verb over". These tests pin
// the extended contract on both verbs:
//
//   - the default --timeout budget is 1800s (run end-to-end via the RPC
//     request, both verbs via the declared flag default — env's RPC
//     carries TimeoutSeconds 0 by design and relies on the client-side
//     deadline, so the flag default is its observable budget surface),
//   - a deadline-class failure on run/env surfaces the elapsed budget and
//     the "pass --timeout <seconds> to extend" hint, naming the verb,
//   - non-deadline failures stay bare.

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// TestSurf017TimeoutFlagDefaults pins the declared --timeout default of
// every remote-exec verb to the 1800s budget (exec's default is pinned
// by TestExecFlagGrammar; run and env are the SURF-017 additions).
func TestSurf017TimeoutFlagDefaults(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		cmd := NewRunCommand()
		f := cmd.Flags().Lookup("timeout")
		if f == nil {
			t.Fatal("run does not declare --timeout")
		}
		if f.DefValue != "1800" {
			t.Errorf("run --timeout default = %q, want \"1800\" (SURF-017)", f.DefValue)
		}
	})
	t.Run("env", func(t *testing.T) {
		cmd := NewEnvCommand()
		f := cmd.Flags().Lookup("timeout")
		if f == nil {
			t.Fatal("env does not declare --timeout")
		}
		if f.DefValue != "1800" {
			t.Errorf("env --timeout default = %q, want \"1800\" (SURF-017)", f.DefValue)
		}
	})
}

// TestRunDefaultTimeoutIs1800 drives the REAL `bunker run` command and
// asserts the sent RPC carries the 1800s default when no --timeout is
// passed (the run equivalent of TestExecFlagGrammar's default rows).
func TestRunDefaultTimeoutIs1800(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")

	var got *v1.ExecAgentRequest
	server := newRunTestServer(t, &mockRunFlagServer{
		mockRunServer: mockRunServer{
			execResponses: []*v1.ExecAgentResponse{
				{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
				{ExitCode: 0},
			},
		},
		captureExecReq: func(req *v1.ExecAgentRequest) { got = req },
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewRunCommand()
	cmd.SetArgs([]string{"agent1", "--", "echo", "hi"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run command failed: %v", err)
	}
	if got == nil {
		t.Fatal("ExecAgent request not captured")
	}
	if got.TimeoutSeconds != 1800 {
		t.Errorf("TimeoutSeconds = %d, want 1800 (SURF-017: run default must match exec)", got.TimeoutSeconds)
	}
}

// assertDeadlineHint checks the SURF-017 decoration contract on a failed
// CLI invocation: deadline-class errors name the verb, echo the elapsed
// budget, and point at --timeout; other errors stay bare.
func assertDeadlineHint(t *testing.T, verb, budget string, want bool, err error) {
	t.Helper()
	msg := err.Error()
	hasHint := strings.Contains(msg, verb+" deadline exceeded after "+budget+"s") &&
		strings.Contains(msg, "pass --timeout <seconds> to extend")
	if want && !hasHint {
		t.Fatalf("deadline error missing %s hint (budget %ss): %v", verb, budget, err)
	}
	if !want && hasHint {
		t.Fatalf("non-deadline error unexpectedly carries the hint: %v", err)
	}
}

// TestRunDeadlineHintEndToEnd drives the REAL `bunker run` command
// against a mock server whose RPC handlers fail, and asserts the
// SURF-017 decoration contract on both request paths (detach RunAgent
// and synchronous ExecAgent streaming).
func TestRunDeadlineHintEndToEnd(t *testing.T) {
	deadlineErr := func() error {
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded"))
	}
	rows := []struct {
		name      string
		args      []string
		runErr    error
		execErr   error
		execResps []*v1.ExecAgentResponse
		wantHint  bool
	}{
		{
			name:     "detach run deadline surfaces the hint with the budget",
			args:     []string{"agent1", "--detach", "--", "sleep", "10000"},
			runErr:   deadlineErr(),
			wantHint: true,
		},
		{
			name:     "sync run deadline before any stream message surfaces the hint",
			args:     []string{"agent1", "--", "go", "build", "./..."},
			execErr:  deadlineErr(),
			wantHint: true,
		},
		{
			name: "sync run deadline after a stream message surfaces the hint",
			args: []string{"agent1", "--", "go", "build", "./..."},
			execResps: []*v1.ExecAgentResponse{
				{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("partial")}},
			},
			execErr:  deadlineErr(),
			wantHint: true,
		},
		{
			name:     "non-deadline run failure stays bare",
			args:     []string{"agent1", "--", "echo", "hi"},
			execErr:  connect.NewError(connect.CodeInternal, errors.New("agent exploded")),
			wantHint: false,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			t.Setenv(SessionTargetEnvVar, "default")

			server := newRunTestServer(t, &mockRunServer{
				runResp: &v1.RunAgentResponse{
					RunId:    "deadbeef",
					UnitName: "bunker-run-agent1-deadbeef",
					Status:   "running",
					ExitCode: -1,
				},
				runErr:        tt.runErr,
				execResponses: tt.execResps,
				execErr:       tt.execErr,
			})
			defer server.Close()
			writeExecTestConfig(t, tmpDir, server.URL)

			cmd := NewRunCommand()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected run to fail, got nil")
			}
			assertDeadlineHint(t, "run", "1800", tt.wantHint, err)
		})
	}
}

// TestEnvDeadlineHintEndToEnd drives the REAL `bunker env` command
// against a mock server whose ExecAgent handler fails, and asserts the
// SURF-017 decoration contract on the error the user actually sees.
func TestEnvDeadlineHintEndToEnd(t *testing.T) {
	deadlineErr := connect.NewError(connect.CodeDeadlineExceeded, errors.New("context deadline exceeded"))
	rows := []struct {
		name      string
		execErr   error
		execResps []*v1.ExecAgentResponse
		wantHint  bool
	}{
		{
			name:     "env deadline before any stream message surfaces the hint",
			execErr:  deadlineErr,
			wantHint: true,
		},
		{
			name: "env deadline after a stream message surfaces the hint",
			execResps: []*v1.ExecAgentResponse{
				{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("partial")}},
			},
			execErr:  deadlineErr,
			wantHint: true,
		},
		{
			name:     "non-deadline env failure stays bare",
			execErr:  connect.NewError(connect.CodeInternal, errors.New("agent exploded")),
			wantHint: false,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)

			mock := &mockEnvServer{
				mockExecServer: mockExecServer{
					execResponses: tt.execResps,
					execErr:       tt.execErr,
				},
			}
			server := newExecTestServer(t, mock)
			defer server.Close()
			writeExecTestConfig(t, tmpDir, server.URL)

			cmd := NewEnvCommand()
			cmd.SetArgs([]string{"--server", "default", "list", "abcd"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected env to fail, got nil")
			}
			assertDeadlineHint(t, "env", "1800", tt.wantHint, err)
		})
	}
}
