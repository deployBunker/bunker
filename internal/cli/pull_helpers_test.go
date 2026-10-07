package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// setupPullConfig prepares a HOME with a CLI config pointing at a mock server
// whose agent has a resolvable sshfs mount, plus the agent's local SSH key —
// the same setup cp's failure tests use (cp_test.go).
func setupPullConfig(t *testing.T) {
	t.Helper()
	t.Setenv(SessionTargetEnvVar, "custom-server")
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	keyPath := filepath.Join(tmpDir, ".config", "bunker", "keys", "test-agent")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("PRIVATE KEY"), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	mock := &cpMockServer{
		agent: &v1.AgentSummary{
			AgentId:    "test-agent",
			Status:     "running",
			SshfsMount: cpTestSSHFSMount,
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
}

// setupPullScpStub writes an `scp` stub that records its argv to a file and
// exits 0, puts it first on PATH, and returns the record file path.
func setupPullScpStub(t *testing.T) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "scp-argv.txt")

	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 0\n", record)
	if err := os.WriteFile(filepath.Join(binDir, "scp"), []byte(script), 0755); err != nil {
		t.Fatalf("write scp stub: %v", err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("SCP_ARGV_RECORD", record)
	return record
}

// runPullCommand executes the pull command, capturing stdout/stderr.
func runPullCommand(t *testing.T, flags []string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewPullCommand()
	var out, errBuf strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(append(flags, args...))
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

// readRecordedArgv reads the argv the scp stub recorded.
func readRecordedArgv(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("scp stub never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}
