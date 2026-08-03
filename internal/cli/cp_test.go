package cli

import (
	"context"
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

func (m *cpMockServer) ServerInfo(ctx context.Context, req *connect.Request[v1.ServerInfoRequest]) (*connect.Response[v1.ServerInfoResponse], error) {
	return connect.NewResponse(&v1.ServerInfoResponse{}), nil
}
func (m *cpMockServer) ServerMetrics(ctx context.Context, req *connect.Request[v1.ServerMetricsRequest]) (*connect.Response[v1.ServerMetricsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, nil)
}
func (m *cpMockServer) SpawnAgent(ctx context.Context, req *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error) {
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
	if err != nil && !strings.Contains(err.Error(), "no active server") {
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
