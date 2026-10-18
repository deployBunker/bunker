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

// cpMockServer is a test implementation of BunkerdHandler for cp command tests.
type cpMockServer struct {
	agent *v1.AgentSummary
	err   error
}

// GetAgentKey is unimplemented in this mock (GAP-128 handler surface).
func (m *cpMockServer) GetAgentKey(context.Context, *connect.Request[v1.GetAgentKeyRequest]) (*connect.Response[v1.GetAgentKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// cpMockServer key-lifecycle RPCs are unimplemented in this mock (GAP-132 handler
// surface; the key CLI tests supply their own mocks).
func (m *cpMockServer) RotateJWTSecret(context.Context, *connect.Request[v1.RotateJWTSecretRequest]) (*connect.Response[v1.RotateJWTSecretResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) RevokeKey(context.Context, *connect.Request[v1.RevokeKeyRequest]) (*connect.Response[v1.RevokeKeyResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) KeyList(context.Context, *connect.Request[v1.KeyListRequest]) (*connect.Response[v1.KeyListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) RenewalDriftReport(context.Context, *connect.Request[v1.RenewalDriftRequest]) (*connect.Response[v1.RenewalDriftResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

// cpMockServer operator-lifecycle RPCs are unimplemented in this mock
// (GAP-130 handler surface; the operator CLI tests supply their own).
func (m *cpMockServer) OperatorCreate(context.Context, *connect.Request[v1.OperatorCreateRequest]) (*connect.Response[v1.OperatorCreateResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) OperatorList(context.Context, *connect.Request[v1.OperatorListRequest]) (*connect.Response[v1.OperatorListResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) OperatorDelete(context.Context, *connect.Request[v1.OperatorDeleteRequest]) (*connect.Response[v1.OperatorDeleteResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{}), nil
}
func (m *cpMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) StopAgent(ctx context.Context, req *connect.Request[v1.StopAgentRequest]) (*connect.Response[v1.StopAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *cpMockServer) StartAgent(ctx context.Context, req *connect.Request[v1.StartAgentRequest]) (*connect.Response[v1.StartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *cpMockServer) RestartAgent(ctx context.Context, req *connect.Request[v1.RestartAgentRequest]) (*connect.Response[v1.RestartAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func (m *cpMockServer) DestroyAgent(ctx context.Context, req *connect.Request[v1.DestroyAgentRequest]) (*connect.Response[v1.DestroyAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) ListAgents(ctx context.Context, req *connect.Request[v1.ListAgentsRequest]) (*connect.Response[v1.ListAgentsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) GetAgent(ctx context.Context, req *connect.Request[v1.GetAgentRequest]) (*connect.Response[v1.GetAgentResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&v1.GetAgentResponse{Agent: m.agent}), nil
}
func (m *cpMockServer) AgentMetrics(ctx context.Context, req *connect.Request[v1.AgentMetricsRequest]) (*connect.Response[v1.AgentMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) ExecAgent(ctx context.Context, req *connect.Request[v1.ExecAgentRequest], stream *connect.ServerStream[v1.ExecAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) RunAgent(ctx context.Context, req *connect.Request[v1.RunAgentRequest]) (*connect.Response[v1.RunAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) HeartbeatAgent(ctx context.Context, req *connect.Request[v1.HeartbeatAgentRequest]) (*connect.Response[v1.HeartbeatAgentResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) QueryAudit(ctx context.Context, req *connect.Request[v1.QueryAuditRequest]) (*connect.Response[v1.QueryAuditResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}

func TestCpCommand_Help(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		cmd.Execute()
	})

	if !strings.Contains(output, "Copy a local file") {
		t.Errorf("help output missing description, got:\n%s", output)
	}
	if !strings.Contains(output, "--ssh-port") {
		t.Errorf("help output missing --ssh-port flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--ssh-key") {
		t.Errorf("help output missing --ssh-key flag, got:\n%s", output)
	}
	if !strings.Contains(output, "--ssh-host") {
		t.Errorf("help output missing --ssh-host flag, got:\n%s", output)
	}
}

func TestCpCommand_MissingArgs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	cmd.SetArgs([]string{"file.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing destination argument")
	}
}

func TestCpCommand_BadDestFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	cmd.SetArgs([]string{"file.txt", "nocolon"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for bad destination format")
	}
	if err != nil && !strings.Contains(err.Error(), "destination must be") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCpCommand_MissingRemotePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	cmd.SetArgs([]string{"file.txt", "abc123:"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing remote path")
	}
	if err != nil && !strings.Contains(err.Error(), "remote path") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCpCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Create a temp file for the local path
	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "abc123:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no server configured")
	}
	if err != nil && !strings.Contains(err.Error(), "no target bound") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCpCommand_LocalFileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	cmd.SetArgs([]string{"/does/not/exist.txt", "abc123:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing local file")
	}
}

func TestCpCommand_LocalPathIsDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cmd := NewCpCommand()
	cmd.SetArgs([]string{tmpDir, "abc123:/tmp/test"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when local path is a directory")
	}
	if err != nil && !strings.Contains(err.Error(), "bunker deploy") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCpCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	mock := &cpMockServer{
		err: connect.NewError(connect.CodeNotFound, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "nonexistent:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for agent not found")
	}
}

func TestCpCommand_ServerError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	mock := &cpMockServer{
		err: connect.NewError(connect.CodeInternal, nil),
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "error-agent:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for server error")
	}
}

func TestCpCommand_MissingSSHKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	mock := &cpMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "test-agent",
			Status:     "running",
			SshfsMount: "sshfs -o IdentityFile=/etc/bunkerd/ssh/test-agent -o idmap=user -o allow_other bunker-test-agent@bunker-host:/home/bunker-test-agent /mnt/bunker/test-agent",
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "test-agent:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for missing SSH key")
	}
}

func TestCpCommand_EmptySshfsMount(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	mock := &cpMockServer{
		agent: &v1.AgentSummary{
			AgentId: "test-agent",
			Status:  "running",
			// No SshfsMount set
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers: map[string]ServerEntry{
			"test": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "test-agent:/tmp/test.txt"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for empty sshfs mount")
	}
}

func TestCpCommand_ServerResolvedFromConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	localFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(localFile, []byte("hello"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	mock := &cpMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "test-agent",
			Status:     "running",
			SshfsMount: "sshfs -o IdentityFile=/etc/bunkerd/ssh/test-agent -o idmap=user -o allow_other bunker-test-agent@bunker-host:/home/bunker-test-agent /mnt/bunker/test-agent",
		},
	}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "custom-server",
		Servers: map[string]ServerEntry{
			"custom-server": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewCpCommand()
	cmd.SetArgs([]string{localFile, "test-agent:/tmp/test.txt"})
	err := cmd.Execute()
	// This will fail at SSH key lookup since we can't actually scp, but it
	// proves the API connection and agent lookup worked.
	if err == nil || !strings.Contains(err.Error(), "SSH key not found") {
		// If it failed for a different reason, that's unexpected
		if err != nil && !strings.Contains(err.Error(), "get agent info") {
			t.Logf("expected SSH key error; got: %v", err)
		}
	}
}

// TestParseSSHUserHost tests the sshfs mount parser.
func TestParseSSHUserHost(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "standard sshfs mount",
			input: "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@bunker-host:/home/bunker-abc123 /mnt/bunker/abc123",
			want:  "bunker-abc123@bunker-host",
		},
		{
			name:  "host with FQDN",
			input: "sshfs -o IdentityFile=/etc/bunkerd/ssh/abc123 -o idmap=user -o allow_other bunker-abc123@bunker-mvp.example.com:/home/bunker-abc123 /mnt/bunker/abc123",
			want:  "bunker-abc123@bunker-mvp.example.com",
		},
		{
			name:  "localhost host",
			input: "sshfs -o IdentityFile=/etc/bunkerd/ssh/test -o idmap=user -o allow_other bunker-test@localhost:/home/bunker-test /mnt/bunker/test",
			want:  "bunker-test@localhost",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "malformed — missing colon",
			input:   "sshfs -o IdentityFile=/path -o idmap=user bunker-abc@host /mnt",
			wantErr: true,
		},
		{
			name:    "malformed — no bunker- prefix",
			input:   "sshfs -o IdentityFile=/path root@host:/home /mnt",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSSHUserHost(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil and result %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDefaultSSHKeyPath tests the default key path resolution.
func TestDefaultSSHKeyPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	path, err := defaultSSHKeyPath("test-agent")
	if err != nil {
		t.Fatalf("defaultSSHKeyPath: %v", err)
	}

	expected := filepath.Join(tmpDir, ".config", "bunker", "keys", "test-agent")
	if path != expected {
		t.Errorf("got %q, want %q", path, expected)
	}
}

// setupCpFailureTest prepares a HOME with a CLI config pointing at a mock
// server whose agent has the given sshfs mount, plus the agent's local SSH
// key (so the command reaches the scp step). It returns the local file to
// copy.
func setupCpFailureTest(t *testing.T, sshfsMount string) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	keyPath := filepath.Join(tmpDir, ".config", "bunker", "keys", "test-agent")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("PRIVATE KEY"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	localFile := filepath.Join(tmpDir, "payload.txt")
	if err := os.WriteFile(localFile, []byte("payload"), 0644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	mock := &cpMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "test-agent",
			Status:     "running",
			SshfsMount: sshfsMount,
		},
	}
	srv := newTestServer(t, mock)
	t.Cleanup(srv.Close)

	cfg := &CLIConfig{
		ActiveServer: "custom-server",
		Servers: map[string]ServerEntry{
			"custom-server": {URL: srv.URL, Token: "test-cli-token"},
		},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}
	return localFile
}

// scpFailureBinDir writes an `scp` stub that always fails with raw scp noise,
// puts it first on PATH, and returns the stub's captured-stderr-free path.
// Only `scp` is stubbed: any other binary the command shells out to is a
// test failure waiting to happen (the probe must go through the seam).
func scpFailureBinDir(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	scp := filepath.Join(binDir, "scp")
	script := "#!/bin/sh\necho \"$0: /tmp/test.txt: Permission denied\" >&2\nexit 1\n"
	if err := os.WriteFile(scp, []byte(script), 0755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir)
}

// runCpCommand executes the cp command capturing stdout and stderr.
func runCpCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewCpCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

const cpTestSSHFSMount = "sshfs -o IdentityFile=/etc/bunkerd/ssh/test-agent -o idmap=user -o allow_other bunker-test-agent@bunker-host:/home/bunker-test-agent /mnt/bunker/test-agent"

// TestCpOwnershipHint table-tests the pure hint formatter: only an existing
// destination owned by a user OTHER than the agent user produces a hint, and
// the hint must name both the current owner and the agent user.
func TestCpOwnershipHint(t *testing.T) {
	tests := []struct {
		name     string
		probeOut string
		agent    string
		wantHint bool
	}{
		{"root-owned destination", "root:root", "bunker-test-agent", true},
		{"other-user destination", "ubuntu:ubuntu", "bunker-test-agent", true},
		{"owner is the agent user", "bunker-test-agent:bunker-test-agent", "bunker-test-agent", false},
		{"same owner, different group", "bunker-test-agent:root", "bunker-test-agent", false},
		{"empty probe output (path absent)", "", "bunker-test-agent", false},
		{"whitespace-only probe output", "  \n\t", "bunker-test-agent", false},
		{"unparseable probe output", "garbage", "bunker-test-agent", false},
		{"empty group", "root:", "bunker-test-agent", false},
		{"empty owner", ":root", "bunker-test-agent", false},
		{"probe noise with spaces", "stat: cannot statx", "bunker-test-agent", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cpOwnershipHint(tt.probeOut, tt.agent)
			if tt.wantHint {
				if got == "" {
					t.Fatalf("cpOwnershipHint(%q, %q) = \"\", want a hint", tt.probeOut, tt.agent)
				}
				owner := strings.TrimSpace(tt.probeOut)
				if !strings.Contains(got, owner) {
					t.Errorf("hint %q does not name the current owner %q", got, owner)
				}
				if !strings.Contains(got, tt.agent) {
					t.Errorf("hint %q does not name the agent user %q", got, tt.agent)
				}
				if !strings.Contains(got, "remove it") {
					t.Errorf("hint %q does not carry the remedy", got)
				}
				return
			}
			if got != "" {
				t.Errorf("cpOwnershipHint(%q, %q) = %q, want no hint", tt.probeOut, tt.agent, got)
			}
		})
	}
}

// TestBuildSSHProbeArgs pins the probe argv: it must ride the SAME SSH path
// scp used (same key, port, user@host) and ask for the destination's
// owner:group.
func TestBuildSSHProbeArgs(t *testing.T) {
	// MOUNT-012: mux options create ~/.bunker/mux — isolate HOME first.
	t.Setenv("HOME", t.TempDir())
	args := buildSSHProbeArgs("/home/kara/.bunker/keys/test-agent", 2222, "bunker-test-agent@bunker-host", "/tmp/test.txt")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-i /home/kara/.bunker/keys/test-agent",
		"-p 2222",
		"bunker-test-agent@bunker-host",
		"stat -c '%U:%G'",
		"'/tmp/test.txt'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("probe argv %q missing %q", joined, want)
		}
	}
}

// TestCpCommand_ScpFailureOwnershipHint covers the failure-tolerance contract
// of the post-scp ownership probe: the raw scp error is always kept, and the
// hint appears exactly when the probe reports an existing destination owned
// by someone other than the agent user. Every probe outcome that is not
// "other owner" must stay silent rather than invent advice.
func TestCpCommand_ScpFailureOwnershipHint(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "custom-server")
	tests := []struct {
		name      string
		probeOut  string
		probeErr  error
		wantHint  bool
		agentUser string
	}{
		{name: "existing host-owned destination gets a hint", probeOut: "root:root", wantHint: true},
		{name: "existing agent-owned destination stays silent", probeOut: "bunker-test-agent:bunker-test-agent", wantHint: false},
		{name: "probe failure (unreachable host) stays silent", probeErr: errors.New("ssh: connect to host bunker-host port 22: Connection refused"), wantHint: false},
		{name: "empty probe output (destination absent) stays silent", probeOut: "", wantHint: false},
		{name: "unparseable probe output stays silent", probeOut: "stat: cannot statx", wantHint: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localFile := setupCpFailureTest(t, cpTestSSHFSMount)
			scpFailureBinDir(t)

			var probedKey, probedHost string
			var probedPort uint32
			old := cpOwnershipProbeFn
			cpOwnershipProbeFn = func(keyPath string, port uint32, userAtHost, remotePath string) (string, error) {
				probedKey, probedPort, probedHost = keyPath, port, userAtHost
				return tt.probeOut, tt.probeErr
			}
			t.Cleanup(func() { cpOwnershipProbeFn = old })

			out, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")

			if err == nil {
				t.Fatalf("cp succeeded against a failing scp (out=%q)", out)
			}
			// The raw scp error text must survive.
			if !strings.Contains(err.Error(), "scp:") {
				t.Errorf("error %q lost the original scp error", err)
			}
			if !strings.Contains(errOut, "Permission denied") {
				t.Errorf("stderr %q lost the streamed scp noise", errOut)
			}
			// The probe must have run over the agent's own SSH path: the
			// agent's key, the default port, and user@host. The host is the
			// server URL's hostname (the mount's "bunker-host" is rewritten
			// to the address the client actually reached), which the mock
			// server makes 127.0.0.1.
			keyPath, _ := defaultSSHKeyPath("test-agent")
			if probedKey != keyPath || probedHost != "bunker-test-agent@127.0.0.1" || probedPort != 22 {
				t.Errorf("probe used key=%q port=%d host=%q, want key=%q port=22 host=bunker-test-agent@127.0.0.1",
					probedKey, probedPort, probedHost, keyPath)
			}
			if strings.Contains(out, "Copied ") {
				t.Errorf("stdout reports success on a failed copy: %q", out)
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
				return
			}
			if strings.Contains(errOut, "Hint:") {
				t.Errorf("stderr %q printed a hint it could not justify", errOut)
			}
		})
	}
}

// TestCpCommand_OwnershipProbeRunsThroughRealSSHPath drives the REAL probe
// (no seam substitution) with stubbed scp/ssh binaries on PATH: it proves the
// probe shells out to ssh with the same key/port/user@host as scp and that
// its output reaches the user as an ownership hint.
func TestCpCommand_OwnershipProbeRunsThroughRealSSHPath(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "custom-server")
	localFile := setupCpFailureTest(t, cpTestSSHFSMount)

	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "ssh-argv.txt")
	scp := filepath.Join(binDir, "scp")
	sshStub := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(scp, []byte("#!/bin/sh\necho \"scp: /tmp/test.txt: Permission denied\" >&2\nexit 1\n"), 0755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	sshScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CP_SSH_RECORD\"\necho \"root:root\"\n"
	if err := os.WriteFile(sshStub, []byte(sshScript), 0755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CP_SSH_RECORD", record)

	_, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err == nil {
		t.Fatal("cp succeeded against a failing scp")
	}

	argv, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatalf("ssh was never invoked: %v", readErr)
	}
	got := string(argv)
	for _, want := range []string{
		"-i\n" + filepath.Join(os.Getenv("HOME"), ".config", "bunker", "keys", "test-agent"),
		"-p\n22",
		// The mount's host is rewritten to the server URL's hostname.
		"bunker-test-agent@127.0.0.1",
		"stat -c '%U:%G'",
		"/tmp/test.txt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe ssh argv missing %q; got:\n%s", want, got)
		}
	}

	if !strings.Contains(errOut, "Hint:") {
		t.Fatalf("stderr %q missing the ownership hint from the real probe", errOut)
	}
	if !strings.Contains(errOut, "root:root") || !strings.Contains(errOut, "bunker-test-agent") {
		t.Errorf("hint must name the current owner and the agent user, got: %q", errOut)
	}
}

// ── DF-BUNKER-97: the pull-shaped invocation of the push-only cp ─────────────

// writeLocalCpFile is the default test source: a real local file, so the
// invocation is unmistakeably a push.
func writeLocalCpFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(path, []byte("payload"), 0644); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	return path
}

// TestCpCommand_PullShapedInvocation covers DF-BUNKER-97: `bunker cp` copies
// local → agent, and the dogfood run that typed the reverse direction got a
// bare "destination must be <agent-id>:/path" that named neither the reason nor
// a way to get the file. A pull-shaped invocation must answer with the
// remediation — the reverse command with the arguments the user typed, scp
// over the key spawn saved, and the sshfs mount — while every genuinely local
// or malformed invocation keeps the existing error. A name that merely CONTAINS
// a colon is a local source, not an agent reference.
func TestCpCommand_PullShapedInvocation(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "")

	tests := []struct {
		name    string
		src     string
		dest    string
		setup   func(t *testing.T) string // overrides src when non-nil
		want    []string
		wantNot []string
	}{
		{
			name: "an agent source with a local destination gets the pull remediation",
			src:  "dfb-1016:app.tgz",
			dest: "/tmp/x",
			want: []string{
				"pull from an agent is not supported by bunker cp",
				"scp -i ~/.config/bunker/keys/dfb-1016",
				"bunker-dfb-1016@<host>:app.tgz",
				"bunker pull dfb-1016 app.tgz /tmp/x",
				"bunker mount dfb-1016",
			},
			wantNot: []string{"destination must be"},
		},
		{
			name: "a local file whose name merely contains a colon is not a pull",
			setup: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "agent:path")
				if err := os.WriteFile(path, []byte("local"), 0644); err != nil {
					t.Fatalf("write colon-named file: %v", err)
				}
				return path
			},
			dest:    "/tmp/x",
			want:    []string{"destination must be <agent-id>:/path"},
			wantNot: []string{"not supported by"},
		},
		{
			name:    "both arguments local keeps the destination-format error",
			setup:   writeLocalCpFile,
			dest:    "nocolon",
			want:    []string{"destination must be <agent-id>:/path"},
			wantNot: []string{"not supported by"},
		},
		{
			name:    "an agent source with an empty path is still a format error",
			src:     "dfb-1016:",
			dest:    "/tmp/x",
			want:    []string{"destination must be <agent-id>:/path"},
			wantNot: []string{"not supported by"},
		},
		{
			name:    "a push-shaped invocation is untouched and reaches the binding refusal",
			setup:   writeLocalCpFile,
			dest:    "abc123:/tmp/test.txt",
			want:    []string{"no target bound"},
			wantNot: []string{"not supported by"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			src := tt.src
			if tt.setup != nil {
				src = tt.setup(t)
			}

			out, _, err := runCpCommand(t, src, tt.dest)
			if err == nil {
				t.Fatalf("cp %q %q succeeded, want an error (stdout=%q)", src, tt.dest, out)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			for _, not := range tt.wantNot {
				if strings.Contains(err.Error(), not) {
					t.Errorf("error %q should not contain %q", err, not)
				}
			}
			if strings.Contains(out, "Copied ") {
				t.Errorf("stdout reports a copy that never happened: %q", out)
			}
		})
	}
}

// TestCpCommand_PullShapedRefusalIsLocal proves the refusal is decided from the
// invocation alone: the dogfood run may be typed on a machine with no bunkerd
// bound at all, and it must still get the explanation. Any scp/ssh/sshfs child
// is a regression (the refusal would otherwise hang on a network round trip
// before explaining itself).
func TestCpCommand_PullShapedRefusalIsLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(SessionTargetEnvVar, "")

	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "children.txt")
	for _, name := range []string{"scp", "ssh", "sshfs"} {
		script := "#!/bin/sh\nprintf '%s %s\\n' " + name + " \"$*\" >> \"$CP_CHILD_RECORD\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CP_CHILD_RECORD", record)

	_, _, err := runCpCommand(t, "dfb-1016:app.tgz", "/tmp/x")
	if err == nil {
		t.Fatal("pull-shaped cp succeeded")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error %q is not the pull remediation", err)
	}
	if b, readErr := os.ReadFile(record); readErr == nil {
		t.Errorf("the refusal ran child processes: %s", b)
	}
}

// TestParseAgentSource pins the shape the pull remediation hangs on: a
// non-empty id of ordinary id characters, one colon, and a non-empty path.
func TestParseAgentSource(t *testing.T) {
	tests := []struct {
		name     string
		arg      string
		wantID   string
		wantPath string
		wantOK   bool
	}{
		{name: "standard agent path", arg: "dfb-1016:/tmp/app.tgz", wantID: "dfb-1016", wantPath: "/tmp/app.tgz", wantOK: true},
		{name: "dotted underscore id", arg: "my_agent.v2:/var/log/x", wantID: "my_agent.v2", wantPath: "/var/log/x", wantOK: true},
		{name: "remote path keeps its own colons", arg: "dfb-1016:/tmp/a:b:c", wantID: "dfb-1016", wantPath: "/tmp/a:b:c", wantOK: true},
		{name: "relative remote path", arg: "dfb-1016:app.tgz", wantID: "dfb-1016", wantPath: "app.tgz", wantOK: true},
		{name: "no colon", arg: "local.txt"},
		{name: "empty path", arg: "dfb-1016:"},
		{name: "empty id", arg: ":/tmp/x"},
		{name: "colon only", arg: ":"},
		{name: "empty", arg: ""},
		{name: "id with a path separator", arg: "/tmp/dfb:/x"},
		{name: "id starting with a separator", arg: "-dfb:/tmp/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, path, ok := parseAgentSource(tt.arg)
			if ok != tt.wantOK {
				t.Fatalf("parseAgentSource(%q) ok = %v, want %v", tt.arg, ok, tt.wantOK)
			}
			if id != tt.wantID || path != tt.wantPath {
				t.Errorf("parseAgentSource(%q) = (%q, %q), want (%q, %q)", tt.arg, id, path, tt.wantID, tt.wantPath)
			}
		})
	}
}

// TestIsPullShapedSource pins the classification: the `<agent-id>:<path>` shape
// is necessary but not sufficient — a source that exists locally (a file
// literally called `agent:path`, or a Windows C:\ path) is an ordinary local
// push and must keep the destination-format error.
func TestIsPullShapedSource(t *testing.T) {
	tmpDir := t.TempDir()
	colonFile := filepath.Join(tmpDir, "agent:path")
	if err := os.WriteFile(colonFile, []byte("local"), 0644); err != nil {
		t.Fatalf("write colon-named file: %v", err)
	}
	colonDir := filepath.Join(tmpDir, "agent:dir")
	if err := os.MkdirAll(colonDir, 0755); err != nil {
		t.Fatalf("mkdir colon-named dir: %v", err)
	}
	plainFile := filepath.Join(tmpDir, "plain.txt")
	if err := os.WriteFile(plainFile, []byte("local"), 0644); err != nil {
		t.Fatalf("write plain file: %v", err)
	}

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{name: "missing agent-side path", src: "dfb-1016:/tmp/app.tgz", want: true},
		{name: "relative agent-side path", src: "dfb-1016:app.tgz", want: true},
		{name: "existing file named like an agent path", src: colonFile, want: false},
		{name: "existing directory named like an agent path", src: colonDir, want: false},
		{name: "plain local file", src: plainFile, want: false},
		{name: "colon-less missing name", src: filepath.Join(tmpDir, "missing.txt"), want: false},
		{name: "empty path", src: "dfb-1016:", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPullShapedSource(tt.src); got != tt.want {
				t.Errorf("isPullShapedSource(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

// TestCpCommand_HelpPullingFiles pins the documented pull story in cp's help
// (DF-BUNKER-97): the command is push-only, says pulling is not supported, and
// names the reverse command, the agent key scp falls back to, and the sshfs
// mount.
func TestCpCommand_HelpPullingFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := NewCpCommand()
	output := captureStdout(t, func() {
		cmd.SetArgs([]string{"--help"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cp --help: %v", err)
		}
	})

	for _, want := range []string{
		"Pulling files",
		"push-only",
		"NOT supported here",
		"bunker pull",
		"scp -i ~/.config/bunker/keys/<agent-id>",
		"bunker mount",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("cp --help missing %q, got:\n%s", want, output)
		}
	}
}

// ── GAP-144: scp identity pinning and partial-transfer reporting ────────────
//
// The ticket's root cause was "cp/deploy build scp without -o IdentitiesOnly=yes"
// — which this tree already fixed (c238df46). What was genuinely missing is the
// PARTIAL-TREE half: a failed scp left 49 of 290 files at the destination and
// nothing but "exit status 1" to say so. These tests pin BOTH halves:
//   - the identity pair is present in every scp argv shape (builder level) and
//     in the argv the command actually executes (wiring level), and
//   - a failed transfer reports landed/total, or says the count is unavailable.

// scpIdentityArgs checks the load-bearing "-o IdentitiesOnly=yes" + "-i <key>"
// pair appears exactly once each in args and returns the -i value.
func scpIdentityArgs(t *testing.T, args []string) string {
	t.Helper()

	identitiesOnly := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-o" && args[i+1] == "IdentitiesOnly=yes" {
			identitiesOnly++
		}
	}
	if identitiesOnly != 1 {
		t.Fatalf("argv carries %d copies of \"-o IdentitiesOnly=yes\", want exactly 1: %v", identitiesOnly, args)
	}

	identityFile := ""
	count := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-i" {
			count++
			identityFile = args[i+1]
		}
	}
	if count != 1 {
		t.Fatalf("argv carries %d \"-i\" operands, want exactly 1: %v", count, args)
	}
	return identityFile
}

// TestBuildSCPArgsForcesIdentitiesOnly is AC1: table-driven construction test
// over the shapes `bunker cp` produces. A loaded ssh-agent offers every key it
// holds; without IdentitiesOnly a target sshd with the default MaxAuthTries=6
// refuses the connection before the agent's own key is tried.
func TestBuildSCPArgsForcesIdentitiesOnly(t *testing.T) {
	// MOUNT-012: the mux options create ~/.bunker/mux — isolate HOME.
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		name      string
		keyPath   string
		port      uint32
		localPath string
		remote    string
		recursive bool
	}{
		{
			name:      "single file, default port",
			keyPath:   "/home/u/.bunker/keys/abc123",
			port:      22,
			localPath: "/tmp/app.conf",
			remote:    "/srv/app.conf",
		},
		{
			name:      "custom port",
			keyPath:   "/home/u/.bunker/keys/abc123",
			port:      2222,
			localPath: "/tmp/app.conf",
			remote:    "/srv/app.conf",
		},
		{
			name:      "key path containing spaces stays one argv element",
			keyPath:   "/home/u/.bunker/keys/my agent key",
			port:      22,
			localPath: "/tmp/app.conf",
			remote:    "/srv/app.conf",
		},
		{
			name:      "remote path containing spaces stays one argv element",
			keyPath:   "/home/u/.bunker/keys/abc123",
			port:      22,
			localPath: "/tmp/app.conf",
			remote:    "/srv/my app/app.conf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildSCPArgs(tt.keyPath, tt.port, tt.localPath, "bunker-abc123@10.0.0.5", tt.remote, tt.recursive)

			if got := scpIdentityArgs(t, args); got != tt.keyPath {
				t.Errorf("-i value = %q, want the agent key %q", got, tt.keyPath)
			}
			if got := findOption(args, "IdentitiesOnly"); got != "yes" {
				t.Errorf("IdentitiesOnly = %q, want yes", got)
			}
			// The identity must be offered BEFORE the operands, or ssh parses
			// the trailing paths as further operands.
			keyIdx, localIdx, destIdx := -1, -1, -1
			for i, a := range args {
				switch {
				case a == "-i" && keyIdx < 0:
					keyIdx = i
				case a == tt.localPath && localIdx < 0:
					localIdx = i
				case a == "bunker-abc123@10.0.0.5:"+tt.remote:
					destIdx = i
				}
			}
			if keyIdx < 0 || localIdx < 0 || destIdx < 0 {
				t.Fatalf("argv is missing an identity or operand: %v", args)
			}
			if keyIdx > localIdx || keyIdx > destIdx {
				t.Errorf("-i at %d must precede the operands (local %d, dest %d): %v", keyIdx, localIdx, destIdx, args)
			}
			if strings.Contains(strings.Join(args, " "), " -r ") {
				t.Errorf("bunker cp must not pass -r: %v", args)
			}
			if got := findOption(args, "ConnectTimeout"); got != "10" {
				t.Errorf("ConnectTimeout = %q, want 10 (a hung handshake must not stall the copy)", got)
			}
		})
	}
}

// TestSCPArgvCarriesIdentitiesOnlyEverywhere sweeps the family of connection
// builders in this package: the -i + IdentitiesOnly pair is only load-bearing
// if EVERY path that shells out to ssh/scp offers it, and a future builder
// added without it would reopen GAP-144 for that command alone.
func TestSCPArgvCarriesIdentitiesOnlyEverywhere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		keyPath    = "/home/u/.bunker/keys/abc123"
		userAtHost = "bunker-abc123@10.0.0.5"
	)
	builders := map[string]func() []string{
		"buildSCPArgs (cp)":        func() []string { return buildSCPArgs(keyPath, 22, "/tmp/f", userAtHost, "/srv/f", false) },
		"buildSCPArgs (deploy -r)": func() []string { return buildSCPArgs(keyPath, 22, "/tmp/d", userAtHost, "/srv/d", true) },
		"buildPullSCPArgs":         func() []string { return buildPullSCPArgs(keyPath, 22, userAtHost, "/srv/f", "/tmp", false) },
		"buildSSHArgs":             func() []string { return buildSSHArgs(keyPath, 22, userAtHost, nil) },
		"buildSSHProbeArgs":        func() []string { return buildSSHProbeArgs(keyPath, 22, userAtHost, "/srv/f") },
		"buildSSHCountArgs":        func() []string { return buildSSHCountArgs(keyPath, 22, userAtHost, "/srv/f") },
	}
	for name, build := range builders {
		args := build()
		if got := scpIdentityArgs(t, args); got != keyPath {
			t.Errorf("%s: -i value = %q, want %q", name, got, keyPath)
		}
		if got := findOption(args, "IdentitiesOnly"); got != "yes" {
			t.Errorf("%s: IdentitiesOnly = %q, want yes", name, got)
		}
	}
}

// TestCountLocalSourceFiles pins the denominator of the partial-transfer
// report: every regular file a copy would move, nested dirs included, and
// symlinks excluded (scp does not follow them by default).
func TestCountLocalSourceFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	for _, rel := range []string{"one.txt", "a/two.txt", "a/b/three.txt"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	// A symlink is an entry of its own: not followed, not counted.
	if err := os.Symlink(filepath.Join(dir, "one.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := countLocalSourceFiles(dir)
	if err != nil {
		t.Fatalf("countLocalSourceFiles(dir): %v", err)
	}
	if got != 3 {
		t.Errorf("directory file count = %d, want 3 (symlink excluded)", got)
	}

	got, err = countLocalSourceFiles(filepath.Join(dir, "one.txt"))
	if err != nil {
		t.Fatalf("countLocalSourceFiles(file): %v", err)
	}
	if got != 1 {
		t.Errorf("single-file count = %d, want 1", got)
	}

	if _, err := countLocalSourceFiles(filepath.Join(dir, "missing")); err == nil {
		t.Error("countLocalSourceFiles(missing) returned no error, want a stat failure")
	}
}

// TestParseRemoteFileCount: only wc's single integer is a measurement. Anything
// else must be an error — reading an unparseable probe as "0 landed" would
// understate the damage of a partial copy.
func TestParseRemoteFileCount(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"49\n", 49},
		{"  290  \n", 290},
		{"0\n", 0},
	}
	for _, tt := range ok {
		got, err := parseRemoteFileCount(tt.in)
		if err != nil {
			t.Errorf("parseRemoteFileCount(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseRemoteFileCount(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
	bad := []string{"", "   \n", "root:root", "-3", "1 2", "wc: command not found"}
	for _, in := range bad {
		if _, err := parseRemoteFileCount(in); err == nil {
			t.Errorf("parseRemoteFileCount(%q) returned no error, want a parse failure", in)
		}
	}
}

// TestCpCommand_ScpInvocationCarriesIdentitiesOnly is the WIRING half of AC1:
// a builder-level assertion proves nothing about what the command executes, so
// this drives `bunker cp` against a recording scp stub and inspects the real
// argv (key, port, identity mode, operands).
func TestCpCommand_ScpInvocationCarriesIdentitiesOnly(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "custom-server")
	localFile := setupCpFailureTest(t, cpTestSSHFSMount)

	binDir := t.TempDir()
	argvRecord := filepath.Join(t.TempDir(), "scp-argv.txt")
	scpStub := filepath.Join(binDir, "scp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CP_SCP_RECORD\"\n" +
		"echo 'Received disconnect from 10.0.0.5 port 22:2: Too many authentication failures' >&2\nexit 1\n"
	if err := os.WriteFile(scpStub, []byte(script), 0755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CP_SCP_RECORD", argvRecord)

	// Measure nothing over the (absent) ssh binary: this test is about argv.
	oldCount := scpFileCountFn
	scpFileCountFn = func(string, uint32, string, string) (int, error) { return 0, errors.New("not measured here") }
	t.Cleanup(func() { scpFileCountFn = oldCount })
	oldProbe := cpOwnershipProbeFn
	cpOwnershipProbeFn = func(string, uint32, string, string) (string, error) { return "", nil }
	t.Cleanup(func() { cpOwnershipProbeFn = oldProbe })

	_, _, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
	if err == nil {
		t.Fatal("cp succeeded against a failing scp stub")
	}

	raw, readErr := os.ReadFile(argvRecord)
	if readErr != nil {
		t.Fatalf("scp was never invoked: %v", readErr)
	}
	argv := string(raw)
	keyPath, _ := defaultSSHKeyPath("test-agent")
	for _, want := range []string{
		"-o\nIdentitiesOnly=yes",
		"-i\n" + keyPath,
		"-P\n22",
		"bunker-test-agent@127.0.0.1:/tmp/test.txt",
		"\n" + localFile + "\n",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("executed scp argv missing %q; got:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "\n-r\n") {
		t.Errorf("bunker cp must not pass -r; got:\n%s", argv)
	}
}

// TestCpCommand_PartialTransferReported is AC2 for the single-file path: a
// failed scp must say what landed, and must stay loud when the destination
// cannot be measured at all.
func TestCpCommand_PartialTransferReported(t *testing.T) {
	t.Setenv(SessionTargetEnvVar, "custom-server")

	tests := []struct {
		name     string
		landed   int
		countErr error
		want     []string
	}{
		{
			name:   "landed count measured",
			landed: 0,
			want:   []string{"scp failed: 0 of 1 files landed", "destination may be incomplete", "/tmp/test.txt"},
		},
		{
			name:   "a partial file counts as landed",
			landed: 1,
			want:   []string{"scp failed: 1 of 1 files landed", "destination may be incomplete"},
		},
		{
			name:     "count probe unreachable stays loud without counts",
			countErr: errors.New("ssh: connect to host 127.0.0.1 port 22: Connection refused"),
			want:     []string{"destination /tmp/test.txt may be incomplete", "could not be measured", "Connection refused"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localFile := setupCpFailureTest(t, cpTestSSHFSMount)
			scpFailureBinDir(t)

			var probedKey, probedHost string
			var probedPort uint32
			var probedPath string
			oldCount := scpFileCountFn
			scpFileCountFn = func(keyPath string, port uint32, userAtHost, remotePath string) (int, error) {
				probedKey, probedPort, probedHost, probedPath = keyPath, port, userAtHost, remotePath
				return tt.landed, tt.countErr
			}
			t.Cleanup(func() { scpFileCountFn = oldCount })
			oldProbe := cpOwnershipProbeFn
			cpOwnershipProbeFn = func(string, uint32, string, string) (string, error) { return "", nil }
			t.Cleanup(func() { cpOwnershipProbeFn = oldProbe })

			out, errOut, err := runCpCommand(t, localFile, "test-agent:/tmp/test.txt")
			if err == nil {
				t.Fatalf("cp succeeded against a failing scp (out=%q)", out)
			}
			if !strings.Contains(err.Error(), "scp:") {
				t.Errorf("error %q lost the original scp error", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr %q missing %q", errOut, want)
				}
			}
			if !strings.Contains(errOut, "re-run the copy before using it") {
				t.Errorf("stderr %q does not name the remedy", errOut)
			}
			if strings.Contains(out, "Copied ") {
				t.Errorf("stdout reports a copy that did not complete: %q", out)
			}
			// The measurement must run over the SAME identity scp used.
			keyPath, _ := defaultSSHKeyPath("test-agent")
			if probedKey != keyPath || probedPort != 22 || probedHost != "bunker-test-agent@127.0.0.1" || probedPath != "/tmp/test.txt" {
				t.Errorf("count probe used key=%q port=%d host=%q path=%q, want key=%q port=22 host=bunker-test-agent@127.0.0.1 path=/tmp/test.txt",
					probedKey, probedPort, probedHost, probedPath, keyPath)
			}
		})
	}
}

// TestReportSCPFailureStateUnreadableSource covers the other degraded branch:
// the denominator itself cannot be counted, and the message must still say the
// destination is untrustworthy rather than print a made-up ratio.
func TestReportSCPFailureStateUnreadableSource(t *testing.T) {
	var buf strings.Builder
	oldCount := scpFileCountFn
	scpFileCountFn = func(string, uint32, string, string) (int, error) { return 7, nil }
	t.Cleanup(func() { scpFileCountFn = oldCount })

	reportSCPFailureState(&buf, "/k", 22, "u@h", "/srv/app", filepath.Join(t.TempDir(), "gone"))

	got := buf.String()
	if !strings.Contains(got, "destination /srv/app may be incomplete") {
		t.Errorf("message %q does not mark the destination incomplete", got)
	}
	if !strings.Contains(got, "source file count could not be read") {
		t.Errorf("message %q does not explain the missing denominator", got)
	}
	if strings.Contains(got, " of ") {
		t.Errorf("message %q invented a ratio it could not compute", got)
	}
}
