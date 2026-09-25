package cli

// DF-BUNKER-41 / DF-BUNKER-42: the flag grammar for `bunker run` and
// `bunker env`, mirroring the DF-BUNKER-31 exec grammar in
// exec_flaggrammar_test.go.
//
//   - run: the run flag set (--server, --timeout, --detach, --name, --env)
//     plus the root persistent flags (--config/--daemon-config) is accepted
//     in BOTH positions (pre- and post-agent-id) and BOTH forms (space,
//     inline). Unknown flags refuse LOCALLY (zero RPCs) in the pre-agent-id
//     position. A peeled --config is APPLIED, not just accepted.
//   - env: --server/--timeout are peeled from EVERY position — before the
//     subcommand, between the subcommand and the agent-id, and after the
//     payload — so the payload validator sees exactly the documented
//     tokens. Unknown flags refuse LOCALLY (zero RPCs).
//
// Success rows drive a real mock server and inspect the sent request;
// error rows assert the EXACT local error and that the server received
// zero requests.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	"github.com/spf13/pflag"
)

// mockRunFlagServer embeds mockRunServer and adds ExecAgent request capture,
// so a single test row can count BOTH the detach path (RunAgent) and the
// sync path (ExecAgent) against one request counter and inspect whichever
// request the command actually sent.
type mockRunFlagServer struct {
	mockRunServer
	captureExecReq func(*v1.ExecAgentRequest)
}

func (m *mockRunFlagServer) ExecAgent(
	ctx context.Context,
	req *connect.Request[v1.ExecAgentRequest],
	stream *connect.ServerStream[v1.ExecAgentResponse],
) error {
	if m.captureExecReq != nil {
		m.captureExecReq(req.Msg)
	}
	return m.mockRunServer.ExecAgent(ctx, req, stream)
}

// TestRunFlagGrammar is the DF-BUNKER-41 table.
func TestRunFlagGrammar(t *testing.T) {
	rows := []struct {
		name          string
		args          []string // {CONFIG} -> custom config path
		sessionEnv    string   // BUNKER_SESSION_TARGET value ("" = leave unset)
		wantErr       string   // expected error substring; "" = expect success
		wantNoRequest bool     // expect the server to receive NO request
		detach        bool     // expect the detach request path (RunAgent)
		wantCommand   string
		wantArgs      []string
		wantTimeout   uint32
		wantDetach    bool
		wantName      string
		wantEnv       map[string]string
		wantCfgApp    bool // after Execute: configPathOverride must be the custom config path
	}{
		// -- Acceptance A: the DF-BUNKER-41 bug — --server before the agent-id.
		{
			name:        "server space form before agent-id reaches server",
			args:        []string{"--server", "default", "agent1", "--", "echo", "hi"},
			sessionEnv:  "default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
		},
		{
			name:        "server inline form before agent-id reaches server",
			args:        []string{"--server=default", "agent1", "--", "echo", "hi"},
			sessionEnv:  "default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
		},
		{
			name:        "server space form after agent-id reaches server",
			args:        []string{"agent1", "--server", "default", "--", "echo", "hi"},
			sessionEnv:  "default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
		},
		{
			name:        "server inline form after agent-id reaches server",
			args:        []string{"agent1", "--server=default", "--", "echo", "hi"},
			sessionEnv:  "default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
		},
		{
			name:        "config space form before agent-id is applied",
			args:        []string{"--config", "{CONFIG}", "agent1", "--", "echo", "hi"},
			sessionEnv:  "custom-default", // only resolvable via the custom config
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
			wantCfgApp:  true,
		},
		{
			name:        "config inline form before agent-id is applied",
			args:        []string{"--config={CONFIG}", "agent1", "--", "echo", "hi"},
			sessionEnv:  "custom-default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
			wantCfgApp:  true,
		},
		{
			name:        "config after agent-id is applied",
			args:        []string{"agent1", "--config", "{CONFIG}", "--", "echo", "hi"},
			sessionEnv:  "custom-default",
			wantCommand: "echo",
			wantArgs:    []string{"hi"},
			wantTimeout: runDefaultTimeoutSeconds,
			wantCfgApp:  true,
		},
		{
			name: "all run flags before agent-id in space form",
			args: []string{
				"--server", "default", "--timeout", "61", "--detach",
				"--name", "api", "--env", "FOO=bar", "--env=BAZ=qux",
				"agent1", "--", "python", "serve.py",
			},
			sessionEnv:  "default",
			detach:      true,
			wantCommand: "python",
			wantArgs:    []string{"serve.py"},
			wantTimeout: 61,
			wantDetach:  true,
			wantName:    "api",
			wantEnv:     map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "mixed flags in both positions",
			args: []string{
				"--config", "{CONFIG}", "--detach", "--name=api",
				"agent1", "--server", "custom-default", "--timeout", "45",
				"--env", "FOO=bar", "--", "python", "serve.py",
			},
			sessionEnv:  "custom-default",
			detach:      true,
			wantCommand: "python",
			wantArgs:    []string{"serve.py"},
			wantTimeout: 45,
			wantDetach:  true,
			wantName:    "api",
			wantEnv:     map[string]string{"FOO": "bar"},
			wantCfgApp:  true,
		},

		// -- "--" before the agent-id terminates flag parsing: the next
		//    token is the agent-id even if flag-shaped (exec parity).
		{
			name:        "separator before agent-id makes --server a plain token",
			args:        []string{"--", "--server", "agent1", "echo", "hi"},
			sessionEnv:  "default",
			wantCommand: "agent1",
			wantArgs:    []string{"echo", "hi"},
			wantTimeout: runDefaultTimeoutSeconds,
		},

		// -- Unknown flags refuse LOCALLY, naming the flag.
		{
			name:          "unknown flag before agent-id refuses naming it",
			args:          []string{"--bogus-flag", "agent1", "--", "echo", "hi"},
			wantErr:       `run takes no flags before <agent-id> (got "--bogus-flag")`,
			wantNoRequest: true,
		},
		{
			name:          "prefix of a known flag is not silently accepted",
			args:          []string{"--serv", "prod", "agent1", "--", "echo", "hi"},
			wantErr:       `run takes no flags before <agent-id> (got "--serv")`,
			wantNoRequest: true,
		},
		{
			name:          "lone flag hits the 2-argument contract first",
			args:          []string{"--timeout"},
			wantErr:       "requires at least 2 arg(s), only received 1",
			wantNoRequest: true,
		},
		{
			name:          "timeout with a bad value refuses locally",
			args:          []string{"--timeout=abc", "agent1", "--", "echo", "hi"},
			wantErr:       "--timeout: expected integer",
			wantNoRequest: true,
		},

		// -- GAP-093 refusals are intact and stay local.
		{
			name:          "no flags and no session target refuses no-target-bound",
			args:          []string{"agent1", "--", "echo", "hi"},
			wantErr:       "no target bound",
			wantNoRequest: true,
		},

		// -- Post-agent-id position: lenient grammar preserved — Docker
		//    flags and other unknown tokens are the command, never eaten.
		{
			name:        "docker run --rm never eaten",
			args:        []string{"agent1", "--detach", "--", "docker", "run", "--rm", "-d", "nginx"},
			sessionEnv:  "default",
			detach:      true,
			wantCommand: "docker",
			wantArgs:    []string{"run", "--rm", "-d", "nginx"},
			wantTimeout: runDefaultTimeoutSeconds,
			wantDetach:  true,
		},
		{
			name:        "unknown token after agent-id is the command",
			args:        []string{"agent1", "--", "--bogus-flag", "x"},
			sessionEnv:  "default",
			wantCommand: "--bogus-flag",
			wantArgs:    []string{"x"},
			wantTimeout: runDefaultTimeoutSeconds,
		},

		// -- Existing contract errors preserved.
		{
			name:          "command required after agent-id",
			args:          []string{"--server", "default", "agent1"},
			wantErr:       "command required after agent-id",
			wantNoRequest: true,
		},
		{
			name:          "requires at least 2 args",
			args:          []string{"agent1"},
			wantErr:       "requires at least 2 arg(s), only received 1",
			wantNoRequest: true,
		},
		{
			name:          "flags but no agent-id",
			args:          []string{"--server", "default"},
			wantErr:       "agent-id required after flags",
			wantNoRequest: true,
		},
	}

	for _, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			resetPathOverrides(t)

			var got *v1.ExecAgentRequest
			var gotRun *v1.RunAgentRequest
			requests := 0
			server := newRunTestServer(t, &mockRunFlagServer{
				mockRunServer: mockRunServer{
					runResp: &v1.RunAgentResponse{
						RunId:    "deadbeef",
						UnitName: "bunker-run-agent1-deadbeef",
						Status:   "running",
						ExitCode: -1,
					},
					captureRunReq: func(req *v1.RunAgentRequest) {
						requests++
						gotRun = req
					},
					execResponses: []*v1.ExecAgentResponse{
						{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
						{ExitCode: 0},
					},
				},
				captureExecReq: func(req *v1.ExecAgentRequest) {
					requests++
					got = req
				},
			})
			defer server.Close()

			// Default config at the default location; custom config at an
			// explicit path with a DIFFERENT active server so a success
			// proves --config was applied, not merely accepted.
			writeExecTestConfig(t, tmpDir, server.URL)
			cfgPath := filepath.Join(tmpDir, "custom.yaml")
			writeExecTestConfigAt(t, cfgPath, "custom-default", server.URL)

			args := append([]string(nil), tt.args...)
			if tt.sessionEnv != "" {
				t.Setenv(SessionTargetEnvVar, tt.sessionEnv)
			}
			for i, a := range args {
				if strings.Contains(a, "{CONFIG}") {
					args[i] = strings.ReplaceAll(a, "{CONFIG}", cfgPath)
				}
			}

			cmd := NewRunCommand()
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
				t.Fatalf("run command failed: %v", err)
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
			if tt.detach {
				if gotRun == nil {
					t.Fatal("RunAgent request not captured")
				}
				if gotRun.Command != tt.wantCommand {
					t.Errorf("command = %q, want %q", gotRun.Command, tt.wantCommand)
				}
				if len(gotRun.Args) != len(tt.wantArgs) {
					t.Fatalf("args = %v, want %v", gotRun.Args, tt.wantArgs)
				}
				for i, want := range tt.wantArgs {
					if gotRun.Args[i] != want {
						t.Errorf("args[%d] = %q, want %q", i, gotRun.Args[i], want)
					}
				}
				if gotRun.TimeoutSeconds != tt.wantTimeout {
					t.Errorf("TimeoutSeconds = %d, want %d", gotRun.TimeoutSeconds, tt.wantTimeout)
				}
				if gotRun.Detach != tt.wantDetach {
					t.Errorf("Detach = %v, want %v", gotRun.Detach, tt.wantDetach)
				}
				if gotRun.Name != tt.wantName {
					t.Errorf("Name = %q, want %q", gotRun.Name, tt.wantName)
				}
				for k, want := range tt.wantEnv {
					if gotRun.Env[k] != want {
						t.Errorf("env[%q] = %q, want %q", k, gotRun.Env[k], want)
					}
				}
			} else {
				if got == nil {
					t.Fatal("ExecAgent request not captured")
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
			}
			// Wiring proof: the peeler must APPLY the persistent flag via
			// the cli setter (read back before resetPathOverrides cleanup).
			if tt.wantCfgApp && configPathOverride != cfgPath {
				t.Errorf("configPathOverride = %q, want %q (peeled --config not applied)",
					configPathOverride, cfgPath)
			}
		})
	}
}

// TestRunAcceptsEveryDeclaredFlag walks the flags the run command actually
// DECLARES (help surface) and requires each one to be peelable from the
// pre-agent-id position end-to-end. A flag declared for help but missing
// from the run flag grammar fails here: it would be rejected with
// "run takes no flags before <agent-id>" despite being advertised.
func TestRunAcceptsEveryDeclaredFlag(t *testing.T) {
	cmd := NewRunCommand()
	var declared []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		declared = append(declared, f.Name)
	})
	if len(declared) == 0 {
		t.Fatal("run declares no flags")
	}

	for _, name := range declared {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			resetPathOverrides(t)

			server := newRunTestServer(t, &mockRunFlagServer{
				mockRunServer: mockRunServer{
					runResp: &v1.RunAgentResponse{
						RunId:    "deadbeef",
						UnitName: "bunker-run-agent1-deadbeef",
						Status:   "running",
						ExitCode: -1,
					},
					execResponses: []*v1.ExecAgentResponse{
						{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
						{ExitCode: 0},
					},
				},
			})
			defer server.Close()

			writeExecTestConfig(t, tmpDir, server.URL)
			cfgPath := filepath.Join(tmpDir, "custom.yaml")
			writeExecTestConfigAt(t, cfgPath, "custom-default", server.URL)

			var flagArgs []string
			switch name {
			case "server":
				flagArgs = []string{"--server", "default"}
			case "timeout":
				flagArgs = []string{"--timeout", "30"}
			case "detach":
				flagArgs = []string{"--detach"}
			case "name":
				flagArgs = []string{"--name", "api"}
			case "env":
				flagArgs = []string{"--env", "FOO=bar"}
			case "config":
				flagArgs = []string{"--config", cfgPath}
			case "daemon-config":
				flagArgs = []string{"--daemon-config", cfgPath}
			default:
				t.Fatalf("run declares flag %q but this walk does not know it: "+
					"add it here AND to the run flag grammar in run.go", name)
			}

			// --config rows can only succeed when the peeled override is
			// APPLIED: the target server name exists only in the custom file.
			if name == "config" {
				t.Setenv(SessionTargetEnvVar, "custom-default")
			} else {
				t.Setenv(SessionTargetEnvVar, "default")
			}

			cmd = NewRunCommand()
			cmd.SetArgs(append(flagArgs, "agent1", "--", "echo", "hi"))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("declared flag --%s rejected before the agent-id: %v", name, err)
			}
		})
	}
}

// TestEnvFlagGrammar is the DF-BUNKER-42 table: --server/--timeout peeled
// from every position and both forms; the payload validator then sees
// exactly the documented tokens and the RPC reaches the server. Error rows
// assert the exact local error and zero server requests.
func TestEnvFlagGrammar(t *testing.T) {
	rows := []struct {
		name          string
		args          []string
		wantErr       string // expected error substring; "" = expect success
		wantNoRequest bool   // expect the server to receive NO request
		wantAgentID   string
		wantShellPart string // a substring the constructed shell command must contain
	}{
		// -- Every position × both forms reaches the server.
		{
			name:          "server before subcommand",
			args:          []string{"--server", "default", "set", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "server inline before subcommand",
			args:          []string{"--server=default", "set", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "server after payload",
			args:          []string{"set", "abcd", "FOO=bar", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "server between subcommand and agent-id",
			args:          []string{"set", "--server", "default", "abcd", "FOO=bar"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},
		{
			name:          "server after get payload",
			args:          []string{"get", "abcd", "DATABASE_URL", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "server after list payload",
			args:          []string{"list", "abcd", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "cat ",
		},
		{
			name:          "server after unset payload",
			args:          []string{"unset", "abcd", "DATABASE_URL", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "DATABASE_URL",
		},
		{
			name:          "timeout after payload",
			args:          []string{"list", "abcd", "--timeout", "61", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "cat ",
		},
		{
			name:          "timeout inline between subcommand and agent-id",
			args:          []string{"list", "--timeout=61", "abcd", "--server", "default"},
			wantAgentID:   "abcd",
			wantShellPart: "cat ",
		},
		{
			name:          "flags scattered in every position",
			args:          []string{"--server", "default", "set", "--timeout=45", "abcd", "FOO=bar", "--timeout", "61"},
			wantAgentID:   "abcd",
			wantShellPart: "FOO=bar",
		},

		// -- Malformed payloads still refuse locally without a working
		//    server (the deliberate validate-before-config property).
		{
			name:          "set with two payload tokens still refuses",
			args:          []string{"set", "abcd", "FOO=bar", "EXTRA=x", "--server", "default"},
			wantErr:       "env set requires exactly one KEY=VALUE argument",
			wantNoRequest: true,
		},
		{
			name:          "get with two payload tokens still refuses",
			args:          []string{"get", "abcd", "A", "B", "--server", "default"},
			wantErr:       "env get requires exactly one KEY argument",
			wantNoRequest: true,
		},
		{
			name:          "list with extra payload token still refuses",
			args:          []string{"list", "abcd", "EXTRA", "--server", "default"},
			wantErr:       "env list takes no extra arguments",
			wantNoRequest: true,
		},

		// -- Unknown flags refuse LOCALLY, naming the flag, zero RPCs.
		{
			name:          "unknown flag before subcommand refuses naming it",
			args:          []string{"--bogus", "set", "abcd", "FOO=bar"},
			wantErr:       `env takes no flags (got "--bogus")`,
			wantNoRequest: true,
		},
		{
			name:          "unknown flag after payload refuses naming it",
			args:          []string{"set", "abcd", "FOO=bar", "--bogus-flag"},
			wantErr:       `env takes no flags (got "--bogus-flag")`,
			wantNoRequest: true,
		},

		// -- Dangling value-taking flag refuses locally.
		{
			name:          "server without value needs an argument",
			args:          []string{"--server"},
			wantErr:       "flag needs an argument: --server",
			wantNoRequest: true,
		},
		{
			name:          "timeout with a bad value refuses locally",
			args:          []string{"--timeout=abc", "list", "abcd", "--server", "default"},
			wantErr:       "--timeout: expected integer",
			wantNoRequest: true,
		},

		// -- "--" terminates flag parsing; the rest is payload.
		{
			name:          "separator makes --server a payload token",
			args:          []string{"set", "abcd", "FOO=bar", "--", "--server", "default"},
			wantErr:       "env set requires exactly one KEY=VALUE argument",
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

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
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
