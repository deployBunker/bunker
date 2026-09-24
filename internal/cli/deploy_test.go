package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// deployMockServer is a test implementation of BunkerdHandler for deploy command tests.
type deployMockServer struct {
	agent *v1.AgentSummary
	err   error
}

// GetAgentKey is unimplemented in this mock (GAP-128 handler surface).
func (m *deployMockServer) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// deployMockServer key-lifecycle RPCs are unimplemented in this mock (GAP-132 handler
// surface; the key CLI tests supply their own mocks).
func (m *deployMockServer) RotateJWTSecret(context.Context, *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) RevokeKey(context.Context, *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) KeyList(context.Context, *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) RenewalDriftReport(context.Context, *connect.Request[v1.RenewalDriftRequest]) (*connect.Response[v1.RenewalDriftResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{}), nil
}
func (m *deployMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *deployMockServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *deployMockServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *deployMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: m.agent}), nil
}
func (m *deployMockServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *deployMockServer) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func TestDeployCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "deploy") {
		t.Errorf("help output missing deploy, got:\n%s", output)
	}
	if !strings.Contains(output, "--ssh-port") {
		t.Errorf("help output missing --ssh-port flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--ssh-host") {
		t.Errorf("help output missing --ssh-host flag, got:\n%s", output)
	}
}

func TestDeployCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{"mydir"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing destination argument")
	}
}

func TestDeployCommand_BadDestFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{"mydir", "nocolon"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for bad destination format")
	}
	if err != nil && !strings.Contains(err.Error(), "destination must be") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_MissingRemotePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{"mydir", "abc123:"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing remote path")
	}
	if err != nil && !strings.Contains(err.Error(), "remote path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_LocalPathIsFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{localFile, "abc123:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when local path is a file not a directory")
	}
	if err != nil && !strings.Contains(err.Error(), "bunker cp") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_LocalDirNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{"/does/not/exist", "abc123:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing local directory")
	}
}

func TestDeployCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{tmpDir, "abc123:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no server configured")
	}
	if err != nil && !strings.Contains(err.Error(), "no target bound") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeployCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &deployMockServer{
		err: connect.NewError(connect.CodeNotFound, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{tmpDir, "nonexistent:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for agent not found")
	}
}

func TestDeployCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &deployMockServer{
		err: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{tmpDir, "error-agent:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server error")
	}
}

func TestDeployCommand_EmptySshfsMount(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &deployMockServer{
		agent: &v1.AgentSummary{
			AgentId: "test-agent",
			Status:  "running",
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewDeployCommand()
	cmd.SetArgs([]string{tmpDir, "test-agent:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for empty sshfs mount")
	}
}

// deployTestSSHFSMount is the agent mount used by the deploy failure-path
// tests. The mount's host is rewritten to the address the client actually
// reached (the mock server's 127.0.0.1), exactly as the cp tests observe.
const deployTestSSHFSMount = "sshfs -o IdentityFile=/etc/bunkerd/ssh/test-agent -o idmap=user -o allow_other bunker-test-agent@bunker-host:/home/bunker-test-agent /mnt/bunker/test-agent"

// setupDeployFailureTest prepares a HOME with a CLI config pointing at a mock
// server whose agent has the standard sshfs mount, plus the agent's local SSH
// key and a local directory to deploy, so the command reaches the scp step.
// It returns the local directory.
func setupDeployFailureTest(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	keyPath := filepath.Join(tmpDir, ".bunker", "keys", "test-agent")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("PRIVATE KEY"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	localDir := filepath.Join(tmpDir, "payload")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "app.conf"), []byte("payload"), 0644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	mock := &deployMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "test-agent",
			Status:     "running",
			SshfsMount: deployTestSSHFSMount,
		},
	}
	srv := newTestServer(t, mock)
	t.Cleanup(srv.Close)

	cfg := &CLIConfig{
		ActiveServer: "custom-server",
		Servers: map[string]ServerEntry{
			"custom-server": {URL: srv.URL},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	return localDir
}

// runDeployCommand executes the deploy command capturing stdout and stderr.
func runDeployCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewDeployCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

// TestDeployCommand_ScpFailureOwnershipHint covers the failure-tolerance
// contract of the post-scp ownership probe on the recursive deploy path
// (mirror of DF-BUNKER-17 for bunker cp): the raw scp error is always kept,
// and the hint appears exactly when the probe reports an existing destination
// directory owned by someone other than the agent user. Every probe outcome
// that is not "other owner" must stay silent rather than invent advice.
func TestDeployCommand_ScpFailureOwnershipHint(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "custom-server")
	const remotePath = "/tmp/payload"

	tests := []struct {
		name     string
		probeOut string
		probeErr error
		wantHint bool
	}{
		{name: "existing host-owned destination gets a hint", probeOut: "root:root", wantHint: true},
		{name: "existing other-user destination gets a hint", probeOut: "ubuntu:ubuntu", wantHint: true},
		{name: "existing agent-owned destination stays silent", probeOut: "bunker-test-agent:bunker-test-agent", wantHint: false},
		{name: "probe failure (unreachable host) stays silent", probeErr: errors.New("ssh: connect to host bunker-host port 22: Connection refused"), wantHint: false},
		{name: "probe timeout stays silent", probeErr: context.DeadlineExceeded, wantHint: false},
		{name: "empty probe output (destination absent) stays silent", probeOut: "", wantHint: false},
		{name: "unparseable probe output stays silent", probeOut: "stat: cannot statx", wantHint: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localDir := setupDeployFailureTest(t)
			scpFailureBinDir(t)

			var probedKey, probedHost, probedPath string
			var probedPort uint32
			old := cpOwnershipProbeFn
			cpOwnershipProbeFn = func(keyPath string, port uint32, userAtHost, remotePath string) (string, error) {
				probedKey, probedPort, probedHost, probedPath = keyPath, port, userAtHost, remotePath
				return tt.probeOut, tt.probeErr
			}
			t.Cleanup(func() { cpOwnershipProbeFn = old })

			out, errOut, err := runDeployCommand(t, localDir, "test-agent:"+remotePath)

			if err == nil {
				t.Fatalf("deploy succeeded against a failing scp (out=%q)", out)
			}
			// The raw scp error text must survive, unchanged.
			if !strings.Contains(err.Error(), "scp -r:") {
				t.Errorf("error %q lost the original scp error", err)
			}
			if !strings.Contains(errOut, "Permission denied") {
				t.Errorf("stderr %q lost the streamed scp noise", errOut)
			}
			// The probe must have run over the agent's own SSH path (key,
			// default port, user@host) and against the DESTINATION DIRECTORY
			// itself. The mount's host is rewritten to the server URL's
			// hostname, which the mock server makes 127.0.0.1.
			keyPath, _ := defaultSSHKeyPath("test-agent")
			if probedKey != keyPath || probedHost != "bunker-test-agent@127.0.0.1" || probedPort != 22 {
				t.Errorf("probe used key=%q port=%d host=%q, want key=%q port=22 host=bunker-test-agent@127.0.0.1",
					probedKey, probedPort, probedHost, keyPath)
			}
			if probedPath != remotePath {
				t.Errorf("probe path = %q, want the destination directory %q", probedPath, remotePath)
			}
			if strings.Contains(out, "Deployed ") {
				t.Errorf("stdout reports success on a failed deploy: %q", out)
			}
			if tt.wantHint {
				if !strings.Contains(errOut, "Hint:") {
					t.Fatalf("stderr %q missing the ownership hint", errOut)
				}
				if !strings.Contains(errOut, tt.probeOut) {
					t.Errorf("stderr %q does not name the current owner %q", errOut, tt.probeOut)
				}
				if !strings.Contains(errOut, "bunker-test-agent") {
					t.Errorf("stderr %q does not name the agent user", errOut)
				}
				if !strings.Contains(errOut, "remove it") {
					t.Errorf("stderr %q does not carry the remedy", errOut)
				}
				return
			}
			if strings.Contains(errOut, "Hint:") {
				t.Errorf("stderr %q printed a hint it could not justify", errOut)
			}
		})
	}
}
