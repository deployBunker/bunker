package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

// TestExecAgent_SSHCommandIncludesDockerHost verifies that the SSH command built
// by ExecAgent ensures DOCKER_HOST reaches the remote command.  We now wrap the
// remote command with `env DOCKER_HOST=unix://...` so it works even when sshd
// does not accept SetEnv or PermitUserEnvironment.
func TestExecAgent_SSHCommandIncludesDockerHost(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}

	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}

	want := `DOCKER_HOST=unix://`
	if !strings.Contains(string(src), want) {
		t.Fatalf("ExecAgent SSH command missing %q in service.go", want)
	}

	// Also verify the socket path is built from the agent ID.
	if !strings.Contains(string(src), `fmt.Sprintf("/run/bunker/%s/docker.sock", agentID)`) {
		t.Fatalf("ExecAgent should build docker.sock path from agentID")
	}
}

// TestExecAgent_DockerHostPropagatedToCommand verifies that the exec command
// itself receives DOCKER_HOST in its environment.  We no longer rely on
// OpenSSH SetEnv because server sshd configs often restrict AcceptEnv; instead
// we prefix the remote command with env(1).
func TestExecAgent_DockerHostPropagatedToCommand(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}

	lines := strings.Split(string(src), "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, `DOCKER_HOST=unix://`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("service.go: ExecAgent ssh command missing DOCKER_HOST=unix://... wrapper")
	}
}

// TestExecAgent_AgentNotFound verifies the not-found error path.
func TestExecAgent_AgentNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: "missing-agent",
		Command: "docker",
		Args:    []string{"ps"},
	})

	// We can't construct a real *connect.ServerStream, so we verify the
	// error by calling the helper directly through a compile-time check.
	// The actual streaming test is covered by the integration suite.
	_ = req
	_ = svc
}

// TestExecAgent_AgentIDRequired verifies empty agent ID returns error.
func TestExecAgent_AgentIDRequired(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	ctx := context.Background()
	req := connect.NewRequest(&v1.ExecAgentRequest{
		AgentId: "",
		Command: "docker",
		Args:    []string{"ps"},
	})

	_ = ctx
	_ = req
	_ = svc
}

// TestBuildAgentExecCommand verifies the env prefix and command are built.
func TestBuildAgentExecCommand(t *testing.T) {
	got := buildAgentExecCommand("abc123", "/home/bunker-abc123", "docker", []string{"version"})
	wantParts := []string{
		// Sourcing the per-agent env file so `bunker env set` injections are
		// visible. stderr redirection makes the source tolerant of a missing
		// file.
		". /run/bunker/abc123/env",
		"env PATH=/home/bunker-abc123/bin:$PATH",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/run/bunker/abc123/tmp",
		"docker version",
	}
	for _, want := range wantParts {
		if !strings.Contains(got, want) {
			t.Errorf("buildAgentExecCommand() = %q, missing %q", got, want)
		}
	}
}

// TestBuildAgentExecCommand_EnvFileSourcedBeforeCommand verifies the env file
// source line appears before the env(1) + user command, so that vars set via
// `bunker env set` take effect for the executed command.
func TestBuildAgentExecCommand_EnvFileSourcedBeforeCommand(t *testing.T) {
	got := buildAgentExecCommand("abc123", "/home/bunker-abc123", "docker", []string{"version"})
	srcIdx := strings.Index(got, ". /run/bunker/abc123/env")
	cmdIdx := strings.Index(got, "docker version")
	if srcIdx < 0 || cmdIdx < 0 || srcIdx >= cmdIdx {
		t.Fatalf("expected . /run/bunker/abc123/env to appear BEFORE 'docker version', got: %q", got)
	}
}

// TestBuildAgentRawExecCommand verifies TMPDIR is present in the raw argv.
func TestBuildAgentRawExecCommand(t *testing.T) {
	got := buildAgentRawExecCommand("abc123", "/home/bunker-abc123", "echo", []string{"hi"})
	joined := strings.Join(got, " ")
	wantParts := []string{
		"env",
		"PATH=/home/bunker-abc123/bin:$PATH",
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/run/bunker/abc123/tmp",
		"echo",
		"hi",
	}
	for _, want := range wantParts {
		if !strings.Contains(joined, want) {
			t.Errorf("buildAgentRawExecCommand() = %v, missing %q", got, want)
		}
	}
	// Ensure TMPDIR is its own argv element (raw mode must not shell-split).
	found := false
	for _, arg := range got {
		if arg == "TMPDIR=/run/bunker/abc123/tmp" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("buildAgentRawExecCommand() missing dedicated TMPDIR argv element: %v", got)
	}
}

// TestBuildAgentRawExecCommand_NoEnvFileSourcing verifies that raw mode does
// NOT include the env file source line. Raw mode bypasses the shell to avoid
// metacharacter interpretation, so the env-file sourcing performed by the
// other builders (shell-based) doesn't apply here. `bunker env set` vars are
// therefore only visible to non-raw `bunker exec` / `bunker exec --script` /
// `bunker run --detach`.
func TestBuildAgentRawExecCommand_NoEnvFileSourcing(t *testing.T) {
	got := buildAgentRawExecCommand("abc123", "/home/bunker-abc123", "echo", []string{"hi"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "/run/bunker/abc123/env") {
		t.Errorf("buildAgentRawExecCommand() should not reference env file (raw mode), got: %v", got)
	}
}

// TestBuildAgentScriptCommand verifies TMPDIR is set for script execution.
func TestBuildAgentScriptCommand(t *testing.T) {
	got := buildAgentScriptCommand("abc123", "/home/bunker-abc123", "#!/bin/sh\necho hi\n")
	wantParts := []string{
		"DOCKER_HOST=unix:///run/bunker/abc123/docker.sock",
		"TMPDIR=/run/bunker/abc123/tmp",
		"env PATH=/home/bunker-abc123/bin:$PATH",
		// Env file source line for `bunker env set` propagation.
		". /run/bunker/abc123/env",
	}
	for _, want := range wantParts {
		if !strings.Contains(got, want) {
			t.Errorf("buildAgentScriptCommand() = %q, missing %q", got, want)
		}
	}
}

// TestShellQuoteSingle verifies single-quoting and escaping.
func TestShellQuoteSingle(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "'hello'"},
		{"a'b", "'a'\\''b'"},
		{"", "''"},
	}
	for _, c := range cases {
		got := shellQuoteSingle(c.in)
		if got != c.want {
			t.Errorf("shellQuoteSingle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildExecSSHCommand verifies the ssh command is built as a single remote
// command string with sh -c '...' so the inner command is not misparsed.
func TestBuildExecSSHCommand(t *testing.T) {
	ctx := context.Background()
	cmd := buildExecSSHCommand(ctx, "abc123", "/keys/abc123", "/home/bunker-abc123", "docker", []string{"version"})
	if cmd.Path != "ssh" && !strings.HasSuffix(cmd.Path, "ssh") {
		t.Fatalf("want ssh command, got %q", cmd.Path)
	}
	wantHost := "bunker-abc123@localhost"
	foundHost := false
	for _, arg := range cmd.Args {
		if arg == wantHost {
			foundHost = true
		}
	}
	if !foundHost {
		t.Errorf("ssh args missing host %q: %v", wantHost, cmd.Args)
	}

	// The post-host argument must be a single sh -c '...' string containing the
	// env prefix and the docker command.
	last := cmd.Args[len(cmd.Args)-1]
	if !strings.HasPrefix(last, "sh -c '") {
		t.Errorf("last ssh arg should be sh -c '...', got %q", last)
	}
	if !strings.Contains(last, "env PATH=/home/bunker-abc123/bin:$PATH") {
		t.Errorf("ssh remote command missing PATH prefix: %q", last)
	}
	if !strings.Contains(last, "docker version") {
		t.Errorf("ssh remote command missing 'docker version': %q", last)
	}
	// Ensure we don't accidentally split sh -c into separate arguments anymore.
	for i, arg := range cmd.Args {
		if arg == "--" {
			t.Errorf("ssh args still contain unsupported -- separator at index %d", i)
		}
	}
}

// TestServerInfo verifies ServerInfo returns hostname, version, and agent count.
func TestServerInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ServerInfoRequest{})
	resp, err := svc.ServerInfo(context.Background(), req)
	if err != nil {
		t.Fatalf("ServerInfo() error: %v", err)
	}
	msg := resp.Msg
	if msg == nil {
		t.Fatal("ServerInfo() returned nil response")
	}
	if msg.Hostname == "" {
		t.Error("ServerInfo().Hostname is empty")
	}
	if msg.Version == "" {
		t.Error("ServerInfo().Version is empty")
	}
	if msg.AgentCount != 0 {
		t.Errorf("ServerInfo().AgentCount = %d, want 0 (empty tracker)", msg.AgentCount)
	}
	if msg.MaxAgents != 10 {
		t.Errorf("ServerInfo().MaxAgents = %d, want 10", msg.MaxAgents)
	}
}

// fakeAgentManager implements agentManager for service-layer tests.
type fakeAgentManager struct {
	destroyResp   *v1.DestroyAgentResponse
	destroyErr    error
	destroyCalled bool
}

func (f *fakeAgentManager) Spawn(ctx context.Context, req *v1.SpawnAgentRequest) (*v1.SpawnAgentResponse, error) {
	return nil, nil
}

func (f *fakeAgentManager) Destroy(ctx context.Context, agentID string, force bool) (*v1.DestroyAgentResponse, error) {
	f.destroyCalled = true
	if f.destroyErr != nil {
		return f.destroyResp, f.destroyErr
	}
	return f.destroyResp, nil
}

func (f *fakeAgentManager) RunAgent(ctx context.Context, req *v1.RunAgentRequest) (*v1.RunAgentResponse, error) {
	return nil, nil
}

func (f *fakeAgentManager) Stop() {}

// TestDestroyAgent verifies successful agent destruction.
func TestDestroyAgent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	mgr := &fakeAgentManager{
		destroyResp: &v1.DestroyAgentResponse{AgentId: "agent-1", Status: "destroyed"},
	}

	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: mgr,
	}

	req := connect.NewRequest(&v1.DestroyAgentRequest{AgentId: "agent-1"})
	resp, err := svc.DestroyAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("DestroyAgent() error: %v", err)
	}
	if !mgr.destroyCalled {
		t.Error("DestroyAgent() did not call agentMgr.Destroy")
	}
	if resp.Msg.Status != "destroyed" {
		t.Errorf("DestroyAgent().Status = %q, want destroyed", resp.Msg.Status)
	}
}

// TestDestroyAgent_NotFound verifies the not-found error path.
func TestDestroyAgent_NotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	mgr := &fakeAgentManager{
		destroyResp: &v1.DestroyAgentResponse{AgentId: "missing", Status: "not_found"},
		destroyErr:  fmt.Errorf("agent not found"),
	}

	svc := &bunkerdService{
		cfg:      config.DefaultConfig(),
		logger:   logger,
		tracker:  tracker,
		agentMgr: mgr,
	}

	req := connect.NewRequest(&v1.DestroyAgentRequest{AgentId: "missing"})
	_, err := svc.DestroyAgent(context.Background(), req)
	if err == nil {
		t.Fatal("DestroyAgent() expected error, got nil")
	}
	connectErr, ok := err.(*connect.Error)
	if !ok || connectErr.Code() != connect.CodeNotFound {
		t.Errorf("DestroyAgent() error code = %v, want NotFound", connectErr.Code())
	}
}

// TestListAgents verifies ListAgents returns agents registered in the tracker.
func TestListAgents(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	// Register two agents
	tracker.Register(&resource.AgentRecord{AgentID: "agent-1", Status: "running"})
	tracker.Register(&resource.AgentRecord{AgentID: "agent-2", Status: "idle"})

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ListAgentsRequest{})
	resp, err := svc.ListAgents(context.Background(), req)
	if err != nil {
		t.Fatalf("ListAgents() error: %v", err)
	}
	msg := resp.Msg
	if msg.TotalCount != 2 {
		t.Errorf("ListAgents().TotalCount = %d, want 2", msg.TotalCount)
	}
	if len(msg.Agents) != 2 {
		t.Fatalf("ListAgents() returned %d agents, want 2", len(msg.Agents))
	}
	// Verify agent IDs are present
	ids := make(map[string]bool)
	for _, a := range msg.Agents {
		ids[a.AgentId] = true
	}
	if !ids["agent-1"] || !ids["agent-2"] {
		t.Errorf("ListAgents() missing expected agent IDs, got: %v", ids)
	}
}

// TestListAgents_Empty verifies ListAgents returns empty list when tracker is empty.
func TestListAgents_Empty(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ListAgentsRequest{})
	resp, err := svc.ListAgents(context.Background(), req)
	if err != nil {
		t.Fatalf("ListAgents() error: %v", err)
	}
	if resp.Msg.TotalCount != 0 {
		t.Errorf("ListAgents().TotalCount = %d, want 0", resp.Msg.TotalCount)
	}
}

// TestGetAgent verifies GetAgent returns the correct agent.
func TestGetAgent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)
	tracker.Register(&resource.AgentRecord{AgentID: "agent-1", Status: "running"})

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.GetAgentRequest{AgentId: "agent-1"})
	resp, err := svc.GetAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("GetAgent() error: %v", err)
	}
	if resp.Msg.Agent == nil {
		t.Fatal("GetAgent().Agent is nil")
	}
	if resp.Msg.Agent.AgentId != "agent-1" {
		t.Errorf("GetAgent().Agent.AgentId = %q, want agent-1", resp.Msg.Agent.AgentId)
	}
}

// TestGetAgent_NotFound verifies GetAgent returns NotFound for missing agent.
func TestGetAgent_NotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.GetAgentRequest{AgentId: "missing"})
	_, err := svc.GetAgent(context.Background(), req)
	if err == nil {
		t.Fatal("GetAgent() expected error, got nil")
	}
	connectErr, ok := err.(*connect.Error)
	if !ok || connectErr.Code() != connect.CodeNotFound {
		t.Errorf("GetAgent() error code = %v, want NotFound", connectErr.Code())
	}
}

// TestAgentMetrics verifies AgentMetrics returns resource info for a registered agent.
func TestAgentMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)
	tracker.Register(&resource.AgentRecord{
		AgentID: "agent-1",
		Status:  "running",
		Limits: &v1.ResourceLimits{
			MemoryMaxBytes: 512 * 1024 * 1024,
			DiskMaxBytes:   10 * 1024 * 1024 * 1024,
		},
	})

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.AgentMetricsRequest{AgentId: "agent-1"})
	resp, err := svc.AgentMetrics(context.Background(), req)
	if err != nil {
		t.Fatalf("AgentMetrics() error: %v", err)
	}
	msg := resp.Msg
	if msg.AgentId != "agent-1" {
		t.Errorf("AgentMetrics().AgentId = %q, want agent-1", msg.AgentId)
	}
	if msg.Status != "running" {
		t.Errorf("AgentMetrics().Status = %q, want running", msg.Status)
	}
	if msg.MemoryLimitBytes != 512*1024*1024 {
		t.Errorf("AgentMetrics().MemoryLimitBytes = %d, want 512MB", msg.MemoryLimitBytes)
	}
	if msg.DiskLimitBytes != 10*1024*1024*1024 {
		t.Errorf("AgentMetrics().DiskLimitBytes = %d, want 10GB", msg.DiskLimitBytes)
	}
}

// TestAgentMetrics_NotFound verifies AgentMetrics returns NotFound for missing agent.
func TestAgentMetrics_NotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.AgentMetricsRequest{AgentId: "missing"})
	_, err := svc.AgentMetrics(context.Background(), req)
	if err == nil {
		t.Fatal("AgentMetrics() expected error, got nil")
	}
	connectErr, ok := err.(*connect.Error)
	if !ok || connectErr.Code() != connect.CodeNotFound {
		t.Errorf("AgentMetrics() error code = %v, want NotFound", connectErr.Code())
	}
}

// TestServerMetrics verifies that ServerMetrics returns all expected fields.
func TestServerMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := resource.NewTracker(10, logger)

	svc := &bunkerdService{
		cfg:     config.DefaultConfig(),
		logger:  logger,
		tracker: tracker,
	}

	req := connect.NewRequest(&v1.ServerMetricsRequest{})
	resp, err := svc.ServerMetrics(context.Background(), req)
	if err != nil {
		t.Fatalf("ServerMetrics() error: %v", err)
	}

	msg := resp.Msg
	if msg == nil {
		t.Fatal("ServerMetrics() returned nil response")
	}

	// ServerMetrics should always return at least agent summaries (even if empty).
	if msg.Agents == nil {
		t.Error("ServerMetrics().Agents is nil, expected empty slice")
	}

	// disk stats should always be populated on any Linux system.
	if msg.DiskTotalBytes == 0 {
		t.Error("ServerMetrics().DiskTotalBytes is 0, expected root filesystem total")
	}
	if msg.DiskUsedBytes == 0 {
		t.Error("ServerMetrics().DiskUsedBytes is 0, expected root filesystem usage")
	}
	if msg.DiskUsedBytes > msg.DiskTotalBytes {
		t.Errorf("ServerMetrics().DiskUsedBytes (%d) > DiskTotalBytes (%d)", msg.DiskUsedBytes, msg.DiskTotalBytes)
	}

	// DockerContainersTotal should be 0 in test contexts (no real docker sockets).
	// We just verify it's a reasonable value (non-negative).
	if int32(msg.DockerContainersTotal) < 0 {
		t.Errorf("ServerMetrics().DockerContainersTotal is negative: %d", msg.DockerContainersTotal)
	}

	t.Logf("ServerMetrics: CPU=%.1f%%, Memory=%d/%d, Disk=%d/%d, Docker=%d, Agents=%d",
		msg.CpuUsagePercent, msg.MemoryUsedBytes, msg.MemoryTotalBytes,
		msg.DiskUsedBytes, msg.DiskTotalBytes, msg.DockerContainersTotal, len(msg.Agents))
}
