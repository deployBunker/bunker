package agent

import (
	"fmt"
	"log/slog"
	"os"
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
