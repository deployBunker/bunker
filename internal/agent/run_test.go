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
		limits, false,
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
		// Shell wrapper that sources the env file (when present) before
		// exec'ing the real command so `bunker env set` injections persist
		// into detached runs. The [ -f ] guard keeps fresh agents working;
		// set -a exports injected vars to the exec'd process.
		"set -a; [ -f /run/bunker/test-agent/env ] && . /run/bunker/test-agent/env 2>/dev/null; set +a; exec \"$@\"",
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
		"my-binary", nil, nil, nil, false,
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
	args := buildRunAgentArgs("a", "1000", "1000", "u", "sh", nil, nil, nil, false)
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
		nil, false,
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

// TestBuildRunAgentArgs_ContainmentEnv verifies GAP-067: detached runs get
// BUNKER_SANDBOX=1 as a dedicated --setenv element when disclosure is on,
// and the argv is untouched when off. The disclosure value is
// ADMIN-CONTROLLED: an agent-supplied env override for the same key can
// neither suppress nor rewrite it when disclosure is enabled.
func TestBuildRunAgentArgs_ContainmentEnv(t *testing.T) {
	off := buildRunAgentArgs("test-agent", "1001", "1001", "u", "my-binary", nil, nil, nil, false)
	joinedOff := strings.Join(off, " ")
	if strings.Contains(joinedOff, "BUNKER_SANDBOX") {
		t.Errorf("flag-off detached argv must not contain BUNKER_SANDBOX: %v", off)
	}

	on := buildRunAgentArgs("test-agent", "1001", "1001", "u", "my-binary", nil, nil, nil, true)
	joinedOn := strings.Join(on, " ")
	want := "--setenv=" + config.ContainmentSandboxEnv
	if !strings.Contains(" "+joinedOn+" ", " "+want+" ") {
		t.Errorf("flag-on detached argv missing exact %q element: %v", want, on)
	}

	// Admin disclosure value wins: an agent trying to rewrite
	// BUNKER_SANDBOX when disclosure is enabled gets the canonical value.
	rewrite := buildRunAgentArgs("test-agent", "1001", "1001", "u", "my-binary", nil,
		map[string]string{"BUNKER_SANDBOX": "user-value"}, nil, true)
	joinedRewrite := strings.Join(rewrite, " ")
	if !strings.Contains(" "+joinedRewrite+" ", " "+want+" ") {
		t.Errorf("admin disclosure value must win over agent rewrite: %v", rewrite)
	}
	if strings.Contains(joinedRewrite, "--setenv=BUNKER_SANDBOX=user-value") {
		t.Errorf("agent rewrite of BUNKER_SANDBOX must be ignored: %v", rewrite)
	}

	// Attempted suppression (empty value) is ignored the same way — the
	// canonical value stays present.
	suppress := buildRunAgentArgs("test-agent", "1001", "1001", "u", "my-binary", nil,
		map[string]string{"BUNKER_SANDBOX": ""}, nil, true)
	joinedSuppress := strings.Join(suppress, " ")
	if !strings.Contains(" "+joinedSuppress+" ", " "+want+" ") {
		t.Errorf("admin disclosure value must survive suppression attempt: %v", suppress)
	}

	// With disclosure DISABLED there is nothing to protect: an agent-set
	// BUNKER_SANDBOX passes through as ordinary user env.
	userOff := buildRunAgentArgs("test-agent", "1001", "1001", "u", "my-binary", nil,
		map[string]string{"BUNKER_SANDBOX": "user-value"}, nil, false)
	if !strings.Contains(strings.Join(userOff, " "), "--setenv=BUNKER_SANDBOX=user-value") {
		t.Errorf("flag-off user env for BUNKER_SANDBOX should pass through: %v", userOff)
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
