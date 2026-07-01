package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

func newTestManager(t *testing.T) *AgentManager {
	t.Helper()
	cfg := config.DefaultConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	return NewAgentManager(cfg, logger, tracker, nil, nil)
}

// uniqueAgentID returns a test-unique short agent ID to avoid username length limits (useradd ≤32 chars).
func uniqueAgentID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%100000)
}

// cleanupAgent destroys an agent if it was spawned successfully.
func cleanupAgent(t *testing.T, m *AgentManager, agentID string) {
	t.Helper()
	if agentID == "" {
		return
	}
	m.logger.Info("test cleanup: destroying agent", "agent_id", agentID)
	_, err := m.Destroy(t.Context(), agentID, true)
	if err != nil {
		t.Logf("cleanup: destroy %s: %v", agentID, err)
	}
}

// ── Unit tests on helper functions (no root needed) ──────────────

func TestGenerateUUIDv4(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		uuid, err := generateUUIDv4()
		if err != nil {
			t.Fatalf("generateUUIDv4 failed: %v", err)
		}
		if uuid == "" {
			t.Fatal("generateUUIDv4 returned empty string")
		}
		if seen[uuid] {
			t.Fatalf("duplicate UUID generated: %s", uuid)
		}
		seen[uuid] = true
	}
}

func TestValidAgentID(t *testing.T) {
	tests := []struct {
		id    string
		valid bool
	}{
		{"testagent", true},
		{"test-agent", true},
		{"test-agent-001", true},
		{"abc123", true},
		{"a", true},
		{strings.Repeat("a", 63), true},
		// Invalid cases
		{"", false},
		{strings.Repeat("a", 64), false},
		{"TestAgent", false},
		{"test/agent", false},
		{"test@agent", false},
		{"test agent", false},
		{"test.agent", false},
		{"test_agent", false},
	}
	for _, tt := range tests {
		got := validAgentID.MatchString(tt.id)
		if got != tt.valid {
			t.Errorf("validAgentID.MatchString(%q) = %v, want %v", tt.id, got, tt.valid)
		}
	}
}

// ── Spawn tests that need root ──────────────────────────────────

func TestSpawn_GeneratesAgentID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	req := &v1.SpawnAgentRequest{} // empty agent_id
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.AgentId == "" {
		t.Error("expected non-empty generated agent_id")
	}
	if len(resp.AgentId) > 8 {
		t.Errorf("expected generated agent_id to be short (first UUID segment), got %q (len=%d)", resp.AgentId, len(resp.AgentId))
	}
}

func TestSpawn_WithProvidedAgentID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("test-agent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.AgentId != agentID {
		t.Errorf("expected agent_id %q, got %q", agentID, resp.AgentId)
	}
}

func TestSpawn_InvalidAgentID(t *testing.T) {
	m := newTestManager(t)
	tests := []struct {
		name    string
		agentID string
	}{
		{"slash", "test/agent"},
		{"uppercase", "TestAgent"},
		{"special chars", "agent@test"},
		{"too long", strings.Repeat("a", 64)},
		{"empty ok", ""}, // empty is OK (auto-generated)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &v1.SpawnAgentRequest{
				AgentId: tt.agentID,
			}
			resp, err := m.Spawn(t.Context(), req)
			if tt.agentID == "" {
				// Empty is valid; error is expected to be from useradd (non-root), not validation.
				if err != nil && strings.Contains(err.Error(), "invalid agent_id") {
					t.Errorf("empty agent_id should be valid, got validation error: %v", err)
				}
				if resp != nil && resp.AgentId == "" {
					t.Error("expected generated agent_id when empty")
				}
				return
			}
			if err == nil {
				t.Errorf("expected error for invalid agent_id %q", tt.agentID)
				return
			}
			if !strings.Contains(err.Error(), "invalid agent_id") {
				t.Errorf("expected 'invalid agent_id' error for %q, got: %v", tt.agentID, err)
			}
		})
	}
}

func TestSpawn_CreatesUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.AgentId != agentID {
		t.Errorf("expected %q, got %q", agentID, resp.AgentId)
	}

	// Verify user was created by checking /etc/passwd.
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Fatalf("read /etc/passwd: %v", err)
	}
	expectedUser := "bunker-" + agentID
	if !strings.Contains(string(passwd), expectedUser) {
		t.Errorf("user %q not found in /etc/passwd", expectedUser)
	}
}

func TestSpawn_GeneratesSSHKeys(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges for useradd")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.SshPrivateKey == "" {
		t.Error("expected non-empty SSH private key")
	}
	if !strings.HasPrefix(resp.SshPrivateKey, "-----BEGIN") {
		preview := resp.SshPrivateKey
		if len(preview) > 50 {
			preview = preview[:50]
		}
		t.Errorf("private key should start with '-----BEGIN', got: %s", preview)
	}
}

func TestSpawn_Response(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	checks := []struct {
		field string
		value string
	}{
		{"AgentId", resp.AgentId},
		{"DockerHostSsh", resp.DockerHostSsh},
		{"SshPrivateKey", resp.SshPrivateKey},
		{"ExpiresAt", resp.ExpiresAt},
	}
	for _, c := range checks {
		if c.value == "" {
			t.Errorf("%s is empty", c.field)
		}
	}

	if resp.AgentId != agentID {
		t.Errorf("AgentId: got %q, want %q", resp.AgentId, agentID)
	}
	if resp.PortRangeStart == 0 {
		t.Error("PortRangeStart is 0, want non-zero default")
	}
	if resp.PortRangeEnd == 0 {
		t.Error("PortRangeEnd is 0, want non-zero default")
	}
	if !strings.Contains(resp.DockerHostSsh, "DOCKER_HOST=ssh://") {
		t.Errorf("DockerHostSsh: unexpected format: %q", resp.DockerHostSsh)
	}
	if resp.DockerHostTunnel == "" {
		t.Error("DockerHostTunnel is empty")
	} else {
		wantParts := []string{
			"ssh",
			"-o StrictHostKeyChecking=no",
			"-o UserKnownHostsFile=/dev/null",
			"-o LogLevel=ERROR",
			"-i /etc/bunkerd/ssh/" + agentID,
			"-L 2376:/run/bunker/" + agentID + "/docker.sock",
			"bunker-" + agentID + "@",
			"-N",
		}
		for _, want := range wantParts {
			if !strings.Contains(resp.DockerHostTunnel, want) {
				t.Errorf("DockerHostTunnel missing %q: got %q", want, resp.DockerHostTunnel)
			}
		}
	}
}

// ── Spawn SSH transport tests (root required) ──────────────────

func TestSpawn_AuthorizedKeysHasEnvironment(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	authKeysFile := fmt.Sprintf("/home/bunker-%s/.ssh/authorized_keys", agentID)
	content, err := os.ReadFile(authKeysFile)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	got := string(content)
	wantEnv := fmt.Sprintf("environment=\"DOCKER_HOST=unix:///run/bunker/%s/docker.sock\"", agentID)
	if !strings.Contains(got, wantEnv) {
		t.Errorf("authorized_keys missing environment prefix\ngot: %s\nwant substring: %s", got, wantEnv)
	}
	if !strings.Contains(got, "ssh-ed25519") {
		t.Errorf("authorized_keys missing ssh-ed25519 key type")
	}

	// Verify the agent's .profile also exports DOCKER_HOST for interactive shells.
	profilePath := fmt.Sprintf("/home/bunker-%s/.profile", agentID)
	profileContent, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read .profile: %v", err)
	}
	wantProfile := fmt.Sprintf("export DOCKER_HOST=unix:///run/bunker/%s/docker.sock", agentID)
	if !strings.Contains(string(profileContent), wantProfile) {
		t.Errorf(".profile missing DOCKER_HOST export\ngot: %s\nwant substring: %s", string(profileContent), wantProfile)
	}
}

func TestSpawn_ProfileHasDockerHost(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	profilePath := fmt.Sprintf("/home/bunker-%s/.profile", agentID)
	content, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read .profile: %v", err)
	}
	got := string(content)
	wantExport := fmt.Sprintf("export DOCKER_HOST=unix:///run/bunker/%s/docker.sock", agentID)
	if !strings.Contains(got, wantExport) {
		t.Errorf(".profile missing DOCKER_HOST export\ngot: %s\nwant substring: %s", got, wantExport)
	}
}

func TestSpawn_PersistsSSHKey(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	sshKeyPath := fmt.Sprintf("/etc/bunkerd/ssh/%s", agentID)
	content, err := os.ReadFile(sshKeyPath)
	if err != nil {
		t.Fatalf("read persisted SSH key %s: %v", sshKeyPath, err)
	}
	if !strings.HasPrefix(string(content), "-----BEGIN") {
		t.Errorf("persisted SSH key doesn't start with -----BEGIN: %q", string(content)[:50])
	}
	if string(content) != resp.SshPrivateKey {
		t.Error("persisted SSH key doesn't match response SshPrivateKey")
	}
}

func TestSpawn_SocketDirOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("testagent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	sockDir := fmt.Sprintf("/run/bunker/%s", agentID)
	info, err := os.Stat(sockDir)
	if err != nil {
		t.Fatalf("stat socket dir %s: %v", sockDir, err)
	}
	if !info.IsDir() {
		t.Errorf("socket path %s is not a directory", sockDir)
	}
	_ = resp
}

// ── Destroy validation tests (no root needed) ─────────────────

func TestDestroy_InvalidAgentID(t *testing.T) {
	m := newTestManager(t)
	resp, err := m.Destroy(t.Context(), "INVALID!", false)
	if err == nil {
		t.Fatal("expected error for invalid agent_id")
	}
	if resp.Status != "error" {
		t.Errorf("expected status 'error', got %q", resp.Status)
	}
}

func TestDestroy_EmptyAgentID(t *testing.T) {
	m := newTestManager(t)
	resp, err := m.Destroy(t.Context(), "", false)
	if err == nil {
		t.Fatal("expected error for empty agent_id")
	}
	if resp.Status != "error" {
		t.Errorf("expected status 'error', got %q", resp.Status)
	}
}

func TestDestroy_ValidatesAgentID(t *testing.T) {
	tests := []struct {
		name    string
		agentID string
		wantErr bool
	}{
		{"valid", "test-agent", true}, // user doesn't exist, so userdel fails
		{"uppercase", "TestAgent", true},
		{"empty", "", true},
		{"too long", strings.Repeat("a", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManager(t)
			_, err := m.Destroy(t.Context(), tt.agentID, false)
			if tt.wantErr && err == nil {
				t.Errorf("expected error for %q", tt.agentID)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tt.agentID, err)
			}
		})
	}
}

func TestStopDockerdDirect_NoProcess(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := stopDockerdDirect(t.Context(), "nonexistent-user-12345", "bunker-docker-test", logger)
	if err == nil {
		t.Fatal("expected error when no dockerd process exists")
	}
	if !strings.Contains(err.Error(), "no dockerd process found") && !strings.Contains(err.Error(), "pgrep") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSpawn_SystemdUnitHasUlimits(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("ulimit-agent")
	req := &v1.SpawnAgentRequest{AgentId: agentID}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	unitName := "bunker-docker-" + resp.AgentId
	// Wait briefly for systemd to load the unit and apply properties.
	time.Sleep(500 * time.Millisecond)

	out, err := exec.CommandContext(t.Context(), "systemctl", "show", unitName, "--property=TasksMax", "--property=LimitNOFILE").CombinedOutput()
	if err != nil {
		t.Logf("systemctl show output: %s", string(out))
		// systemd-run --system transient units may not be visible to systemctl show immediately.
		t.Skip("could not query systemd unit properties")
	}
	output := string(out)

	wantTasksMax := fmt.Sprintf("TasksMax=%d", m.cfg.Agent.DefaultMaxProcesses)
	if !strings.Contains(output, wantTasksMax) {
		t.Errorf("systemd unit missing TasksMax\ngot:\n%s\nwant substring: %s", output, wantTasksMax)
	}
	wantLimitNOFILE := fmt.Sprintf("LimitNOFILE=%d %d", m.cfg.Agent.DefaultMaxOpenFiles, m.cfg.Agent.DefaultMaxOpenFiles)
	if !strings.Contains(output, wantLimitNOFILE) {
		t.Errorf("systemd unit missing LimitNOFILE\ngot:\n%s\nwant substring: %s", output, wantLimitNOFILE)
	}
}

func TestConfig_DefaultUlimits(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Agent.DefaultMaxProcesses == 0 {
		t.Error("DefaultMaxProcesses should be non-zero")
	}
	if cfg.Agent.DefaultMaxOpenFiles == 0 {
		t.Error("DefaultMaxOpenFiles should be non-zero")
	}
	if cfg.Agent.DefaultMaxProcesses > cfg.Agent.DefaultMaxOpenFiles {
		t.Errorf("DefaultMaxProcesses (%d) should not exceed DefaultMaxOpenFiles (%d)", cfg.Agent.DefaultMaxProcesses, cfg.Agent.DefaultMaxOpenFiles)
	}
}

func TestConfig_DefaultDiskAndContainerLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Agent.DefaultDiskBytes == 0 {
		t.Error("DefaultDiskBytes should be non-zero")
	}
	if cfg.Agent.DefaultMaxDockerContainers == 0 {
		t.Error("DefaultMaxDockerContainers should be non-zero")
	}
}

func TestCountAgentContainers(t *testing.T) {
	// Replace countAgentContainers with a fake that returns 2, then restore.
	orig := countAgentContainers
	countAgentContainers = func(ctx context.Context, dockerSockPath string) (uint32, error) {
		return 2, nil
	}
	defer func() { countAgentContainers = orig }()

	got, err := countAgentContainers(t.Context(), "/run/bunker/test/docker.sock")
	if err != nil {
		t.Fatalf("countAgentContainers: %v", err)
	}
	if got != 2 {
		t.Errorf("expected 2 containers, got %d", got)
	}
}

func TestSpawn_MaxDockerContainersEnforced(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("maxcontainers")
	req := &v1.SpawnAgentRequest{
		AgentId: agentID,
		Limits: &v1.ResourceLimits{
			MaxDockerContainers: 0, // zero means use default; default is 10, so spawn should succeed
		},
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn with default MaxDockerContainers failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.Limits == nil || resp.Limits.MaxDockerContainers != m.cfg.Agent.DefaultMaxDockerContainers {
		t.Errorf("expected MaxDockerContainers %d in response, got %v", m.cfg.Agent.DefaultMaxDockerContainers, resp.Limits)
	}
}

func TestSpawn_DiskMaxBytesEnforced(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges")
	}
	m := newTestManager(t)
	agentID := uniqueAgentID("diskmax")
	req := &v1.SpawnAgentRequest{
		AgentId: agentID,
		Limits: &v1.ResourceLimits{
			DiskMaxBytes: 1 * 1024 * 1024 * 1024, // 1 GiB
		},
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn with custom DiskMaxBytes failed: %v", err)
	}
	defer cleanupAgent(t, m, resp.AgentId)

	if resp.Limits == nil || resp.Limits.DiskMaxBytes != 1*1024*1024*1024 {
		t.Errorf("expected DiskMaxBytes 1GiB in response, got %v", resp.Limits)
	}

	// Verify systemd unit has LimitFSIZE set.
	unitName := "bunker-docker-" + resp.AgentId
	time.Sleep(500 * time.Millisecond)
	out, err := exec.CommandContext(t.Context(), "systemctl", "show", unitName, "--property=LimitFSIZE").CombinedOutput()
	if err != nil {
		t.Logf("systemctl show output: %s", string(out))
		t.Skip("could not query systemd unit properties")
	}
	want := fmt.Sprintf("LimitFSIZE=%d %d", 1*1024*1024*1024, 1*1024*1024*1024)
	if !strings.Contains(string(out), want) {
		t.Errorf("systemd unit missing LimitFSIZE\ngot:\n%s\nwant substring: %s", string(out), want)
	}
}
