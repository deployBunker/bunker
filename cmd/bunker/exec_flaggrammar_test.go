package main

// DF-BUNKER-31: `bunker exec` must accept the same global flag grammar as
// every other verb. These tests drive the REAL root command tree
// (newRootCommand), so main.go's PersistentPreRun --config/--daemon-config
// transfer participates exactly as in production, and the requests land on
// a stub bunkerd connect server.

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"
	"github.com/spf13/pflag"
	yaml "go.yaml.in/yaml/v3"

	"github.com/deployBunker/bunker/internal/cli"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// rootExecStubServer implements bunkerv1connect.BunkerdHandler: ExecAgent
// records the request and streams a trivial success; every other method is
// Unimplemented.
type rootExecStubServer struct {
	requests int
	command  string
	args     []string
}

func (s *rootExecStubServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	s.requests++
	s.command = req.Msg.Command
	s.args = append([]string(nil), req.Msg.Args...)
	if err := stream.Send(&v1.ExecAgentResponse{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}}); err != nil {
		return err
	}
	return stream.Send(&v1.ExecAgentResponse{ExitCode: 0})
}

func (s *rootExecStubServer) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) ServerInfo(context.Context, *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) ServerMetrics(context.Context, *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) SpawnAgent(context.Context, *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) DestroyAgent(context.Context, *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) StopAgent(context.Context, *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) StartAgent(context.Context, *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) RestartAgent(context.Context, *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) ListAgents(context.Context, *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) GetAgent(context.Context, *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) AgentMetrics(context.Context, *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) RunAgent(context.Context, *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) HeartbeatAgent(context.Context, *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (s *rootExecStubServer) QueryAudit(context.Context, *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// newRootExecStub starts an httptest server serving the stub handler.
func newRootExecStub(t *testing.T) (*httptest.Server, *rootExecStubServer) {
	t.Helper()
	stub := &rootExecStubServer{}
	r := chi.NewRouter()
	path, h := bunkerv1connect.NewBunkerdHandler(stub)
	r.Mount(path, h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, stub
}

// writeRootTestConfig writes a CLI config with a single server under the
// given name (via the real cli save path, honoring HOME/BUNKER_HOME).
func writeRootTestConfig(t *testing.T, serverName, serverURL string) {
	t.Helper()
	cfg := &cli.CLIConfig{
		Servers: map[string]cli.ServerEntry{
			serverName: {
				Name:        serverName,
				URL:         serverURL,
				ConnectedAt: "2026-09-20T00:00:00Z",
			},
		},
		ActiveServer: serverName,
	}
	if err := cli.SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
}

// runRootExec executes the real root command with the given args, capturing
// the streamed stdout so it does not leak into the test log.
func runRootExec(t *testing.T, args ...string) error {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	execErr := func() (execErr error) {
		defer func() {
			os.Stdout = old
		}()
		root := newRootCommand()
		var discard bytes.Buffer
		root.SetOut(&discard)
		root.SetErr(&discard)
		root.SetArgs(args)
		return root.Execute()
	}()
	w.Close()
	_, _ = io.Copy(io.Discard, r)
	r.Close()
	return execErr
}

// TestRootExecConfigFlagBeforeAgentID is the DF-BUNKER-31 acceptance case
// end-to-end: `bunker exec --config <cfg> <agent> -- <cmd>` — the exact
// invocation every dogfood run since 2026-09-16 tripped on — must reach the
// server. The custom config names a DIFFERENT server than the default
// config, so success proves --config was applied, not merely tolerated.
func TestRootExecConfigFlagBeforeAgentID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("BUNKER_HOME", "")
	cli.ResetConfigPathOverride()
	cli.ResetDaemonConfigPathOverride()
	t.Cleanup(func() {
		cli.ResetConfigPathOverride()
		cli.ResetDaemonConfigPathOverride()
	})

	srv, stub := newRootExecStub(t)

	writeRootTestConfig(t, "default", srv.URL)
	customCfg := filepath.Join(tmpDir, "custom.yaml")
	writeRootTestConfigAt(t, customCfg, "custom-default", srv.URL)

	// Only resolvable through the CUSTOM config: if the peeled --config is
	// not applied, target resolution fails and no request is sent.
	t.Setenv(cli.SessionTargetEnvVar, "custom-default")

	if err := runRootExec(t, "exec", "--config", customCfg, "agent1", "--", "docker", "ps"); err != nil {
		t.Fatalf("bunker exec --config <cfg> <agent> -- cmd failed: %v", err)
	}
	if stub.requests != 1 {
		t.Fatalf("server received %d request(s), want 1", stub.requests)
	}
	if stub.command != "docker" {
		t.Errorf("command = %q, want docker", stub.command)
	}
	if len(stub.args) != 1 || stub.args[0] != "ps" {
		t.Errorf("args = %v, want [ps]", stub.args)
	}
}

// writeRootTestConfigAt writes a CLI config at an explicit path.
func writeRootTestConfigAt(t *testing.T, path, serverName, serverURL string) {
	t.Helper()
	cfg := &cli.CLIConfig{
		Servers: map[string]cli.ServerEntry{
			serverName: {
				Name:        serverName,
				URL:         serverURL,
				ConnectedAt: "2026-09-20T00:00:00Z",
			},
		},
		ActiveServer: serverName,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config %s: %v", path, err)
	}
}

// TestRootPersistentFlagsAreExecPeelable pins the global-flag contract at
// the root: every persistent flag the root command registers must be
// peelable by `bunker exec` from the pre-agent-id position, end-to-end.
// A NEW persistent flag added to the root fails here until it is added to
// execFlagSpecs (internal/cli/exec.go) — the same guard exists in the cli
// package for exec's own declared flags (TestExecAcceptsEveryDeclaredFlag).
func TestRootPersistentFlagsAreExecPeelable(t *testing.T) {
	root := newRootCommand()
	var persistent []string
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		persistent = append(persistent, f.Name)
	})
	want := []string{"config", "daemon-config"}
	if len(persistent) != len(want) {
		t.Fatalf("root registers persistent flags %v, want exactly %v; "+
			"a new persistent flag must also be added to execFlagSpecs in "+
			"internal/cli/exec.go so `bunker exec` keeps accepting the full "+
			"global flag grammar (DF-BUNKER-31)", persistent, want)
	}
	seen := map[string]bool{}
	for _, n := range persistent {
		seen[n] = true
	}
	for _, n := range want {
		if !seen[n] {
			t.Fatalf("root persistent flags %v missing %q", persistent, n)
		}
	}

	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			t.Setenv("BUNKER_HOME", "")
			cli.ResetConfigPathOverride()
			cli.ResetDaemonConfigPathOverride()
			t.Cleanup(func() {
				cli.ResetConfigPathOverride()
				cli.ResetDaemonConfigPathOverride()
			})

			srv, stub := newRootExecStub(t)
			writeRootTestConfig(t, "default", srv.URL)
			customCfg := filepath.Join(tmpDir, "custom.yaml")
			writeRootTestConfigAt(t, customCfg, "custom-default", srv.URL)

			flagArgs := []string{"exec", "--" + name, customCfg, "agent1", "--", "docker", "ps"}
			if name == "config" {
				// Only resolvable via the custom config: proves the peeled
				// value is applied, not just accepted.
				t.Setenv(cli.SessionTargetEnvVar, "custom-default")
			} else {
				t.Setenv(cli.SessionTargetEnvVar, "default")
			}
			if err := runRootExec(t, flagArgs...); err != nil {
				t.Fatalf("bunker exec --%s <cfg> <agent> -- cmd failed: %v", name, err)
			}
			if stub.requests != 1 {
				t.Fatalf("server received %d request(s), want 1", stub.requests)
			}
			if stub.command != "docker" {
				t.Errorf("command = %q, want docker", stub.command)
			}
		})
	}
}
