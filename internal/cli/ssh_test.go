package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func TestSSHCommand_Help(t *testing.T) {
	cmd := NewSSHCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute --help: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "ssh <agent-id>") {
		t.Errorf("help missing usage: %q", out)
	}
	if !strings.Contains(out, "Open an interactive SSH session") {
		t.Errorf("help missing short description: %q", out)
	}
}

func TestSSHCommand_MissingArgs(t *testing.T) {
	cmd := NewSSHCommand()
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing agent-id")
	}
}

func TestSSHCommand_NoServer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cfg := &CLIConfig{Servers: map[string]ServerEntry{}}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewSSHCommand()
	cmd.SetArgs([]string{"test-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for no active server")
	}
	if !strings.Contains(err.Error(), "no active server") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSSHCommand_AgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	mock := &cpMockServer{agent: nil}
	srv := newTestServer(t, mock)
	defer srv.Close()

	cfg := &CLIConfig{
		ActiveServer: "test",
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewSSHCommand()
	cmd.SetArgs([]string{"missing-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestSSHCommand_MissingSSHKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

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
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewSSHCommand()
	cmd.SetArgs([]string{"test-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing SSH key")
	}
	if !strings.Contains(err.Error(), "SSH key not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSSHCommand_EmptySshfsMount(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

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
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewSSHCommand()
	cmd.SetArgs([]string{"test-agent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty sshfs mount")
	}
	if !strings.Contains(err.Error(), "sshfs_mount is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSSHCommand_ValidKeyRunsSSH(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Place the agent key where defaultSSHKeyPath expects it.
	keysDir := filepath.Join(tmpDir, ".bunker", "keys")
	if err := os.MkdirAll(keysDir, 0755); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	if err := os.WriteFile(filepath.Join(keysDir, "test-agent"), []byte("key"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	// Fake ssh binary: records its args, exits 0.
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	argsFile := filepath.Join(tmpDir, "ssh-args.txt")
	fakeSSH := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSH_ARGS_FILE\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(fakeSSH), 0755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_ARGS_FILE", argsFile)

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
		Servers:      map[string]ServerEntry{"test": {URL: srv.URL}},
	}
	if err := SaveCLIConfig(cfg); err != nil {
		t.Fatalf("SaveCLIConfig: %v", err)
	}

	cmd := NewSSHCommand()
	var sb strings.Builder
	cmd.SetOut(&sb)
	cmd.SetErr(&sb)
	cmd.SetArgs([]string{"test-agent", "--", "true"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh: %v", err)
	}

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read fake ssh args: %v", err)
	}
	got := strings.TrimSpace(string(argsBytes))
	wantKey := filepath.Join(keysDir, "test-agent")
	for _, want := range []string{
		"-i",
		wantKey,
		"IdentitiesOnly=yes",
		"bunker-test-agent@127.0.0.1", // host resolved from server URL
		"true",                        // remote command passed through
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh args %q missing %q", got, want)
		}
	}
}

func TestBuildSSHArgs(t *testing.T) {
	got := buildSSHArgs("/home/u/.bunker/keys/abc123", 22, "bunker-abc123@10.0.0.5", nil)
	want := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", "/home/u/.bunker/keys/abc123",
		"-p", "22",
		"bunker-abc123@10.0.0.5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildSSHArgs mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildSSHArgs_RemoteCommand(t *testing.T) {
	got := buildSSHArgs("/k", 2222, "u@h", []string{"docker", "ps"})
	want := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", "/k",
		"-p", "2222",
		"u@h",
		"docker", "ps",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildSSHArgs mismatch:\n got: %v\nwant: %v", got, want)
	}
}
