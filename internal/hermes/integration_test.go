package hermes

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/resource"
)

func testConfigWithTempDir(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agent.BaseDataDir = t.TempDir()
	return cfg
}

func testLoggerQuiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestIntegration_SkillLifecycle(t *testing.T) {
	cfg := testConfigWithTempDir(t)
	sm := NewSkillManager(cfg, testLoggerQuiet())
	agentID := "integration-lifecycle"
	ctx := context.Background()

	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	codingHermesDir := filepath.Join(sm.skillsDir, agentID, ".coding-hermes")
	if _, err := os.Stat(codingHermesDir); os.IsNotExist(err) {
		t.Fatal(".coding-hermes directory not created")
	}

	tasksPath := filepath.Join(codingHermesDir, "tasks.md")
	if _, err := os.Stat(tasksPath); os.IsNotExist(err) {
		t.Fatal("tasks.md not created")
	}

	versionPath := filepath.Join(codingHermesDir, "version.yaml")
	if _, err := os.Stat(versionPath); os.IsNotExist(err) {
		t.Fatal("version.yaml not created")
	}

	content, err := sm.ReadAgentTasks(agentID)
	if err != nil {
		t.Fatalf("ReadAgentTasks failed: %v", err)
	}
	if !strings.Contains(content, "[ ] Welcome task") {
		t.Fatalf("expected pending welcome task, got:\n%s", content)
	}

	updated := strings.Replace(content, "[ ] Welcome task", "[x] Welcome task", 1)
	if err := sm.UpdateAgentTasks(agentID, updated); err != nil {
		t.Fatalf("UpdateAgentTasks failed: %v", err)
	}

	content, err = sm.ReadAgentTasks(agentID)
	if err != nil {
		t.Fatalf("ReadAgentTasks after update failed: %v", err)
	}
	if !strings.Contains(content, "[x] Welcome task") {
		t.Fatalf("expected completed welcome task, got:\n%s", content)
	}
	if strings.Contains(content, "[ ] Welcome task") {
		t.Fatal("pending welcome task still present after update")
	}

	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("CleanupAgentSkills failed: %v", err)
	}
	if _, err := os.Stat(codingHermesDir); !os.IsNotExist(err) {
		t.Fatal("agent skill workspace still exists after cleanup")
	}
}

func TestIntegration_TaskQueueFormat(t *testing.T) {
	cfg := testConfigWithTempDir(t)
	sm := NewSkillManager(cfg, testLoggerQuiet())
	agentID := "integration-format"
	ctx := context.Background()

	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}
	t.Cleanup(func() { _ = sm.CleanupAgentSkills(agentID) })

	content, err := sm.ReadAgentTasks(agentID)
	if err != nil {
		t.Fatalf("ReadAgentTasks failed: %v", err)
	}

	requiredSections := []string{
		"## Quality Gates",
		"GitReins Tier 1",
		"GitReins Tier 2",
		"Hilo",
		"## Task States",
		"[ ]",
		"[x]",
	}
	for _, section := range requiredSections {
		if !strings.Contains(content, section) {
			t.Errorf("tasks.md missing required section %q", section)
		}
	}
}

func TestIntegration_CoreSkillsListed(t *testing.T) {
	cfg := testConfigWithTempDir(t)
	sm := NewSkillManager(cfg, testLoggerQuiet())

	info, err := sm.GetAgentSkillInfo("integration-skills")
	if err != nil {
		t.Fatalf("GetAgentSkillInfo failed: %v", err)
	}

	expected := []string{"coding-hermes", "coding-hermes-cron", "hilo-usage", "gitreins"}
	if len(info.CoreSkills) != len(expected) {
		t.Fatalf("expected %d core skills, got %d", len(expected), len(info.CoreSkills))
	}
	for i, skill := range expected {
		if info.CoreSkills[i] != skill {
			t.Fatalf("expected core skill %q at index %d, got %q", skill, i, info.CoreSkills[i])
		}
	}
}

func TestIntegration_AgentRecordAfterSpawn(t *testing.T) {
	cfg := testConfigWithTempDir(t)
	logger := testLoggerQuiet()
	tracker := resource.NewTracker(10, logger)
	sm := NewSkillManager(cfg, logger)

	agentID := "integration-tracked-agent"
	rec := &resource.AgentRecord{
		AgentID:   agentID,
		Status:    "running",
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := tracker.Register(rec); err != nil {
		t.Fatalf("tracker.Register failed: %v", err)
	}

	if tracker.Get(agentID) == nil {
		t.Fatal("agent record not found in tracker after register")
	}

	ctx := context.Background()
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	info, err := sm.GetAgentSkillInfo(agentID)
	if err != nil {
		t.Fatalf("GetAgentSkillInfo failed: %v", err)
	}
	if !info.TasksExist {
		t.Fatal("expected skill workspace tasks to exist after init")
	}
	if info.AgentID != agentID {
		t.Fatalf("expected agent_id %q, got %q", agentID, info.AgentID)
	}

	tracker.Unregister(agentID)
	if tracker.Get(agentID) != nil {
		t.Fatal("agent record still in tracker after unregister")
	}

	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("CleanupAgentSkills failed: %v", err)
	}
	if _, err := os.Stat(info.SkillsDir); !os.IsNotExist(err) {
		t.Fatal("skill workspace still exists after cleanup")
	}
}

func TestIntegration_CleanupIdempotent(t *testing.T) {
	cfg := testConfigWithTempDir(t)
	sm := NewSkillManager(cfg, testLoggerQuiet())
	agentID := "integration-idempotent"
	ctx := context.Background()

	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("first CleanupAgentSkills failed: %v", err)
	}
	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("second CleanupAgentSkills failed: %v", err)
	}
}
