package cli

// SURF-012 CLI tests: the three persistent-container flags, their mutual
// exclusion, the flag/token combinations that must be refused LOCALLY (server
// receives zero requests), the request wiring, and the empty-listing notice.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func TestPersistentModeFromFlags(t *testing.T) {
	cases := []struct {
		name                      string
		persistent, list, cleanup bool
		want                      v1.PersistentMode
		wantErr                   bool
	}{
		{"none", false, false, false, v1.PersistentMode_PERSISTENT_MODE_NONE, false},
		{"exec", true, false, false, v1.PersistentMode_PERSISTENT_MODE_EXEC, false},
		{"list", false, true, false, v1.PersistentMode_PERSISTENT_MODE_LIST, false},
		{"cleanup", false, false, true, v1.PersistentMode_PERSISTENT_MODE_CLEANUP, false},
		{"exec+list", true, true, false, v1.PersistentMode_PERSISTENT_MODE_NONE, true},
		{"exec+cleanup", true, false, true, v1.PersistentMode_PERSISTENT_MODE_NONE, true},
		{"list+cleanup", false, true, true, v1.PersistentMode_PERSISTENT_MODE_NONE, true},
		{"all three", true, true, true, v1.PersistentMode_PERSISTENT_MODE_NONE, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := persistentModeFromFlags(c.persistent, c.list, c.cleanup)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected a mutual-exclusion error, got mode %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("mode = %v, want %v", got, c.want)
			}
		})
	}
}

// TestExecPersistentFlags drives the REAL command against a mock server: a
// success row asserts the ExecAgentRequest carries the mode (and the command,
// where one exists); an error row asserts the exact local refusal AND that the
// server saw zero requests, which is what makes the refusal "local" rather
// than a round trip that happened to fail.
func TestExecPersistentFlags(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantErr       string
		wantNoRequest bool
		wantMode      v1.PersistentMode
		wantCommand   string
		wantArgs      []string
	}{
		{
			name:        "persistent before agent-id",
			args:        []string{"--persistent", "agent1", "--", "go", "build", "./..."},
			wantMode:    v1.PersistentMode_PERSISTENT_MODE_EXEC,
			wantCommand: "go",
			wantArgs:    []string{"build", "./..."},
		},
		{
			name:        "persistent after agent-id",
			args:        []string{"agent1", "--persistent", "--", "go", "build"},
			wantMode:    v1.PersistentMode_PERSISTENT_MODE_EXEC,
			wantCommand: "go",
			wantArgs:    []string{"build"},
		},
		{
			name:        "list-persistent takes no command",
			args:        []string{"agent1", "--list-persistent"},
			wantMode:    v1.PersistentMode_PERSISTENT_MODE_LIST,
			wantCommand: "",
		},
		{
			name:        "cleanup-persistent takes no command",
			args:        []string{"--cleanup-persistent", "agent1"},
			wantMode:    v1.PersistentMode_PERSISTENT_MODE_CLEANUP,
			wantCommand: "",
		},
		{
			name:        "no flag keeps the one-shot default",
			args:        []string{"agent1", "--", "uname", "-a"},
			wantMode:    v1.PersistentMode_PERSISTENT_MODE_NONE,
			wantCommand: "uname",
			wantArgs:    []string{"-a"},
		},
		{
			name:          "persistent and list-persistent are mutually exclusive",
			args:          []string{"agent1", "--persistent", "--list-persistent"},
			wantErr:       "mutually exclusive",
			wantNoRequest: true,
		},
		{
			name:          "list-persistent and cleanup-persistent are mutually exclusive",
			args:          []string{"--list-persistent", "--cleanup-persistent", "agent1"},
			wantErr:       "mutually exclusive",
			wantNoRequest: true,
		},
		{
			name:          "persistent rejects raw mode",
			args:          []string{"agent1", "--persistent", "--raw", "--", "echo", "hi"},
			wantErr:       "--persistent cannot be combined with --raw",
			wantNoRequest: true,
		},
		{
			name:          "list-persistent rejects a trailing command",
			args:          []string{"agent1", "--list-persistent", "docker", "ps"},
			wantErr:       "--list-persistent takes no command",
			wantNoRequest: true,
		},
		{
			name:          "cleanup-persistent rejects a trailing command",
			args:          []string{"agent1", "--cleanup-persistent", "echo"},
			wantErr:       "--cleanup-persistent takes no command",
			wantNoRequest: true,
		},
		{
			name:          "command still required without a lifecycle flag",
			args:          []string{"agent1"},
			wantErr:       "command required after agent-id",
			wantNoRequest: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			t.Setenv(SessionTargetEnvVar, "default")
			resetPathOverrides(t)

			requests := 0
			var gotReq *v1.ExecAgentRequest
			server := newExecTestServer(t, &mockExecServer{
				execResponses: []*v1.ExecAgentResponse{
					{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("ok")}},
					{ExitCode: 0},
				},
				captureReq: func(req *v1.ExecAgentRequest) { requests++; gotReq = req },
			})
			defer server.Close()
			writeExecTestConfig(t, tmpDir, server.URL)

			cmd := NewExecCommand()
			cmd.SetArgs(c.args)
			err := cmd.Execute()

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", c.wantErr)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if c.wantNoRequest {
				if requests != 0 {
					t.Fatalf("local refusal must not reach the server, but %d request(s) arrived", requests)
				}
				return
			}
			if requests != 1 {
				t.Fatalf("server received %d request(s), want 1", requests)
			}
			if gotReq.GetPersistentMode() != c.wantMode {
				t.Errorf("PersistentMode = %v, want %v", gotReq.GetPersistentMode(), c.wantMode)
			}
			if gotReq.GetCommand() != c.wantCommand {
				t.Errorf("Command = %q, want %q", gotReq.GetCommand(), c.wantCommand)
			}
			if len(c.wantArgs) > 0 {
				got := gotReq.GetArgs()
				if len(got) != len(c.wantArgs) {
					t.Fatalf("Args = %q, want %q", got, c.wantArgs)
				}
				for i := range c.wantArgs {
					if got[i] != c.wantArgs[i] {
						t.Fatalf("Args = %q, want %q", got, c.wantArgs)
					}
				}
			}
		})
	}
}

// TestExecListPersistent_StatesAnEmptyResult: docker prints nothing when an
// agent has no persistent containers, and a silent empty stdout is
// indistinguishable from a listing that quietly failed. The CLI states it.
func TestExecListPersistent_StatesAnEmptyResult(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	resetPathOverrides(t)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{{ExitCode: 0}},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	var stderr bytes.Buffer
	cmd := NewExecCommand()
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"agent1", "--list-persistent"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list-persistent: %v", err)
	}
	if !strings.Contains(stderr.String(), "no persistent containers for agent agent1") {
		t.Errorf("stderr = %q, want the empty-listing notice", stderr.String())
	}
}

// TestExecListPersistent_IsSilentWhenRowsArrive is the negative control for the
// notice above: with rows on stdout the CLI must not add anything of its own.
func TestExecListPersistent_IsSilentWhenRowsArrive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	resetPathOverrides(t)

	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{
			{Output: &v1.ExecAgentResponse_Stdout{Stdout: []byte("bunker-exec-agent1\timg\tUp 3 minutes\n")}},
			{ExitCode: 0},
		},
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	var stderr bytes.Buffer
	cmd := NewExecCommand()
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"agent1", "--list-persistent"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list-persistent: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when rows were listed", stderr.String())
	}
}

// TestExecHelp_DocumentsPersistentFlags keeps the help surface honest: the
// flags are declared, so they must be explained where an operator looks.
func TestExecHelp_DocumentsPersistentFlags(t *testing.T) {
	cmd := NewExecCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	if err := cmd.Help(); err != nil {
		t.Fatalf("help: %v", err)
	}
	help := buf.String()
	for _, want := range []string{
		"--persistent",
		"--list-persistent",
		"--cleanup-persistent",
		"bunker-exec-<agent-id>",
		"mutually exclusive",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("exec help missing %q", want)
		}
	}
}

// TestExecPersistentScriptMode pins that --script and --persistent compose:
// both the upload flag and the persistent mode must reach the server, since
// the server-side script path is a separate builder.
func TestExecPersistentScriptMode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv(SessionTargetEnvVar, "default")
	resetPathOverrides(t)

	scriptFile := filepath.Join(tmpDir, "run.sh")
	if err := os.WriteFile(scriptFile, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	var gotReq *v1.ExecAgentRequest
	server := newExecTestServer(t, &mockExecServer{
		execResponses: []*v1.ExecAgentResponse{{ExitCode: 0}},
		captureReq:    func(req *v1.ExecAgentRequest) { gotReq = req },
	})
	defer server.Close()
	writeExecTestConfig(t, tmpDir, server.URL)

	cmd := NewExecCommand()
	cmd.SetArgs([]string{"agent1", "--persistent", "--script", scriptFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("persistent script exec: %v", err)
	}
	if gotReq == nil {
		t.Fatal("server received no request")
	}
	if gotReq.GetPersistentMode() != v1.PersistentMode_PERSISTENT_MODE_EXEC {
		t.Errorf("PersistentMode = %v, want EXEC", gotReq.GetPersistentMode())
	}
	if !strings.Contains(gotReq.GetScriptContent(), "echo hi") {
		t.Errorf("ScriptContent = %q, want the uploaded script", gotReq.GetScriptContent())
	}
}
