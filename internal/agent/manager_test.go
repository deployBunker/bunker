package agent

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

func newTestManager(t *testing.T) *AgentManager {
	t.Helper()
	cfg := config.DefaultConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tracker := resource.NewTracker(cfg.Agent.MaxAgents, logger)
	return NewAgentManager(cfg, logger, tracker)
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
	req := &v1.SpawnAgentRequest{
		AgentId: "test-agent",
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	if resp.AgentId != "test-agent" {
		t.Errorf("expected agent_id 'test-agent', got %q", resp.AgentId)
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
				// If we got a response (unlikely without root), agent_id should be set.
				if resp != nil && resp.AgentId == "" {
					t.Error("expected generated agent_id when empty")
				}
				return
			}
			// For invalid IDs, we should get a validation error before any system calls.
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
	req := &v1.SpawnAgentRequest{
		AgentId: "testagent-001",
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer func() {
		// Best-effort cleanup.
		m.logger.Info("test cleanup: removing user", "agent_id", resp.AgentId)
	}()

	if resp.AgentId != "testagent-001" {
		t.Errorf("expected testagent-001, got %q", resp.AgentId)
	}

	// Verify user was created by checking /etc/passwd.
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Fatalf("read /etc/passwd: %v", err)
	}
	if !strings.Contains(string(passwd), "bunker-testagent-001") {
		t.Error("user 'bunker-testagent-001' not found in /etc/passwd")
	}
}

func TestSpawn_GeneratesSSHKeys(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges for useradd")
	}
	m := newTestManager(t)
	req := &v1.SpawnAgentRequest{
		AgentId: "testagent-002",
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

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
	req := &v1.SpawnAgentRequest{
		AgentId: "testagent-003",
	}
	resp, err := m.Spawn(t.Context(), req)
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}

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

	if resp.AgentId != "testagent-003" {
		t.Errorf("AgentId: got %q, want 'testagent-003'", resp.AgentId)
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
		{"valid", "test-agent", true},   // user doesn't exist, so userdel fails
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
