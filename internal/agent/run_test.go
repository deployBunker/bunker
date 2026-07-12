package agent

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

func TestBuildRunAgentArgs(t *testing.T) {
	limits := &v1.ResourceLimits{
		CpuQuota:       1.5,
		MemoryMaxBytes: 1024 * 1024 * 512,
		DiskMaxBytes:   1024 * 1024 * 1024,
	}
	args := buildRunAgentArgs(
		"test-agent",
		"1001",
		"1001",
		"bunker-run-test-agent-abc",
		"docker",
		[]string{"compose", "up"},
		map[string]string{"DATABASE_URL": "postgres://db"},
		limits,
	)

	wantSubstrings := []string{
		"--system",
		"--unit=bunker-run-test-agent-abc",
		"--uid=1001",
		"--gid=1001",
		"--property=PAMName=login",
		"--property=CPUQuota=150%",
		"--property=MemoryMax=536870912",
		"--property=LimitFSIZE=1073741824",
		"--setenv=PATH=/home/bunker-test-agent/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"--setenv=HOME=/home/bunker-test-agent",
		"--setenv=USER=bunker-test-agent",
		"--setenv=DOCKER_HOST=unix:///run/bunker/test-agent/docker.sock",
		"--setenv=TMPDIR=/run/bunker/test-agent/tmp",
		// BUNKER_ENV_FILE is exported so other tools (and tests) can locate it.
		"--setenv=BUNKER_ENV_FILE=/run/bunker/test-agent/env",
		"--setenv=DATABASE_URL=postgres://db",
		// Shell wrapper that sources the env file before exec'ing the real
		// command so `bunker env set` injections persist into detached runs.
		". /run/bunker/test-agent/env 2>/dev/null && exec \"$@\"",
		"docker",
		"compose",
		"up",
	}

	got := " " + strings.Join(args, " ") + " "
	for _, want := range wantSubstrings {
		if !strings.Contains(got, " "+want+" ") {
			t.Errorf("expected args to contain %q, got:\n%v", want, args)
		}
	}
}

// TestBuildRunAgentArgs_EnvFileWrapperBeforeCommand verifies the env-file
// wrapper shell comes BEFORE the real command in the systemd-run argv so
// `bunker env set` env vars are sourced at exec time.
func TestBuildRunAgentArgs_EnvFileWrapperBeforeCommand(t *testing.T) {
	args := buildRunAgentArgs(
		"test-agent", "1001", "1001",
		"bunker-run-test-agent-abc",
		"my-binary", nil, nil, nil,
	)
	got := strings.Join(args, " ")
	srcIdx := strings.Index(got, ". /run/bunker/test-agent/env")
	cmdIdx := strings.Index(got, "my-binary")
	if srcIdx < 0 {
		t.Fatalf("expected env-file source string in args, got: %v", args)
	}
	if cmdIdx < 0 {
		t.Fatalf("expected user command in args, got: %v", args)
	}
	if srcIdx >= cmdIdx {
		t.Fatalf("env-file source must appear BEFORE user command, got: %v", args)
	}
}

// TestBuildRunAgentArgs_NoArgsDoesNotPanic ensures we don't crash if the user
// provides a command with zero positional args.
func TestBuildRunAgentArgs_NoArgsDoesNotPanic(t *testing.T) {
	args := buildRunAgentArgs("a", "1000", "1000", "u", "sh", nil, nil, nil)
	if len(args) == 0 {
		t.Fatal("expected non-empty args")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "sh") {
		t.Errorf("expected command 'sh' to appear in args: %v", args)
	}
}

func TestBuildRunAgentArgs_OverrideDefaultEnv(t *testing.T) {
	args := buildRunAgentArgs(
		"test-agent",
		"1001",
		"1001",
		"bunker-run-test-agent-abc",
		"sh",
		[]string{"-c", "echo hi"},
		map[string]string{"TMPDIR": "/custom/tmp", "DOCKER_HOST": "unix:///custom/docker.sock"},
		nil,
	)

	got := " " + strings.Join(args, " ") + " "
	if strings.Contains(got, " --setenv=TMPDIR=/run/bunker/test-agent/tmp ") {
		t.Error("default TMPDIR should be overridden")
	}
	if !strings.Contains(got, " --setenv=TMPDIR=/custom/tmp ") {
		t.Error("custom TMPDIR should be present")
	}
	if !strings.Contains(got, " --setenv=DOCKER_HOST=unix:///custom/docker.sock ") {
		t.Error("custom DOCKER_HOST should be present")
	}
}

func TestRunAgent_RequiresAgent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &AgentManager{
		cfg:     config.DefaultConfig(),
		tracker: resource.NewTracker(10, logger),
	}
	_, err := m.RunAgent(context.Background(), &v1.RunAgentRequest{
		AgentId: "missing",
		Command: "docker",
		Detach:  true,
	})
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
}

func TestRunAgent_RequiresCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &AgentManager{
		cfg:     config.DefaultConfig(),
		tracker: resource.NewTracker(10, logger),
	}
	_, err := m.RunAgent(context.Background(), &v1.RunAgentRequest{
		AgentId: "test-agent",
		Detach:  true,
	})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}
