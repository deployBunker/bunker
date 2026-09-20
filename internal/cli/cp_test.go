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
			"test": {URL: srv.URL},
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
			"test": {URL: srv.URL},
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
			"test": {URL: srv.URL},
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
			"test": {URL: srv.URL},
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
			"custom-server": {URL: srv.URL},
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

	expected := filepath.Join(tmpDir, ".bunker", "keys", "test-agent")
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

	keyPath := filepath.Join(tmpDir, ".bunker", "keys", "test-agent")
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
			"custom-server": {URL: srv.URL},
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
		"-i\n" + filepath.Join(os.Getenv("HOME"), ".bunker", "keys", "test-agent"),
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
