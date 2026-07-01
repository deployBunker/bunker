// Package hermes provides integration with the Coding-Hermes skill system.
package hermes

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deployBunker/bunker/internal/config"
)

func testConfig() *config.Config {
	return testConfigDir(filepath.Join(os.TempDir(), "bunker-test"))
}

func testConfigDir(baseDir string) *config.Config {
	return &config.Config{
		Agent: config.AgentConfig{
			BaseDataDir: baseDir,
		},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestNewSkillManager(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	if sm == nil {
		t.Fatal("NewSkillManager returned nil")
	}
	expectedDir := filepath.Join(cfg.Agent.BaseDataDir, "skills")
	if sm.skillsDir != expectedDir {
		t.Fatalf("expected skillsDir %q, got %q", expectedDir, sm.skillsDir)
	}
}

func TestInitAgentSkills(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-123"

	// Clean up before test.
	agentDir := filepath.Join(sm.skillsDir, agentID)
	os.RemoveAll(agentDir)

	ctx := t.Context()
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	// Verify .coding-hermes directory exists.
	codingHermesDir := filepath.Join(agentDir, ".coding-hermes")
	if _, err := os.Stat(codingHermesDir); os.IsNotExist(err) {
		t.Fatalf(".coding-hermes directory not created")
	}

	// Verify tasks.md exists and contains agent ID.
	tasksPath := filepath.Join(codingHermesDir, "tasks.md")
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("tasks.md not found: %v", err)
	}
	if !strings.Contains(string(data), agentID) {
		t.Fatalf("tasks.md does not contain agent ID %q", agentID)
	}
	if !strings.Contains(string(data), "Welcome task") {
		t.Fatalf("tasks.md does not contain welcome task")
	}

	// Verify version.yaml exists.
	versionPath := filepath.Join(codingHermesDir, "version.yaml")
	if _, err := os.Stat(versionPath); os.IsNotExist(err) {
		t.Fatalf("version.yaml not created")
	}

	// Clean up after test.
	os.RemoveAll(agentDir)
}

func TestInitAgentSkills_Idempotent(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-idempotent"
	agentDir := filepath.Join(sm.skillsDir, agentID)
	os.RemoveAll(agentDir)

	ctx := t.Context()
	// First init.
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("first InitAgentSkills failed: %v", err)
	}

	// Modify tasks.md.
	tasksPath := filepath.Join(agentDir, ".coding-hermes", "tasks.md")
	if err := os.WriteFile(tasksPath, []byte("# Modified"), 0640); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}

	// Second init should NOT overwrite existing files.
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("second InitAgentSkills failed: %v", err)
	}
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		t.Fatalf("read tasks.md: %v", err)
	}
	if string(data) != "# Modified" {
		t.Fatalf("tasks.md was overwritten on second init")
	}

	os.RemoveAll(agentDir)
}

func TestInitAgentSkills_EmptyAgentID(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	ctx := t.Context()
	if err := sm.InitAgentSkills(ctx, ""); err == nil {
		t.Fatal("expected error for empty agent_id")
	}
}

func TestCleanupAgentSkills(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-cleanup"
	agentDir := filepath.Join(sm.skillsDir, agentID)

	// Create a dummy directory.
	os.MkdirAll(filepath.Join(agentDir, ".coding-hermes"), 0750)

	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("CleanupAgentSkills failed: %v", err)
	}

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("agent skills dir still exists after cleanup")
	}
}

func TestCleanupAgentSkills_EmptyAgentID(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	if err := sm.CleanupAgentSkills(""); err == nil {
		t.Fatal("expected error for empty agent_id")
	}
}

func TestCleanupAgentSkills_NonExistent(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "non-existent-agent"
	// Should not error if directory doesn't exist.
	if err := sm.CleanupAgentSkills(agentID); err != nil {
		t.Fatalf("CleanupAgentSkills failed for non-existent agent: %v", err)
	}
}

func TestGetAgentTasksPath(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-path"
	path := sm.GetAgentTasksPath(agentID)
	expected := filepath.Join(sm.skillsDir, agentID, ".coding-hermes", "tasks.md")
	if path != expected {
		t.Fatalf("expected path %q, got %q", expected, path)
	}
}

func TestReadAgentTasks(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-read"
	agentDir := filepath.Join(sm.skillsDir, agentID)
	os.RemoveAll(agentDir)

	ctx := t.Context()
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	content, err := sm.ReadAgentTasks(agentID)
	if err != nil {
		t.Fatalf("ReadAgentTasks failed: %v", err)
	}
	if !strings.Contains(content, "Welcome task") {
		t.Fatalf("ReadAgentTasks content unexpected: %s", content)
	}

	os.RemoveAll(agentDir)
}

func TestReadAgentTasks_NotFound(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-notfound"
	os.RemoveAll(filepath.Join(sm.skillsDir, agentID))

	_, err := sm.ReadAgentTasks(agentID)
	if err == nil {
		t.Fatal("expected error for non-existent tasks")
	}
}

func TestUpdateAgentTasks(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-update"
	agentDir := filepath.Join(sm.skillsDir, agentID)
	os.RemoveAll(agentDir)

	newContent := "## Updated tasks\n\n- [x] All done\n"
	if err := sm.UpdateAgentTasks(agentID, newContent); err != nil {
		t.Fatalf("UpdateAgentTasks failed: %v", err)
	}

	data, err := os.ReadFile(sm.GetAgentTasksPath(agentID))
	if err != nil {
		t.Fatalf("read updated tasks: %v", err)
	}
	if string(data) != newContent {
		t.Fatalf("expected %q, got %q", newContent, string(data))
	}

	os.RemoveAll(agentDir)
}

func TestGetAgentSkillInfo(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-info"
	agentDir := filepath.Join(sm.skillsDir, agentID)
	os.RemoveAll(agentDir)

	ctx := t.Context()
	if err := sm.InitAgentSkills(ctx, agentID); err != nil {
		t.Fatalf("InitAgentSkills failed: %v", err)
	}

	info, err := sm.GetAgentSkillInfo(agentID)
	if err != nil {
		t.Fatalf("GetAgentSkillInfo failed: %v", err)
	}
	if info.AgentID != agentID {
		t.Fatalf("expected agent_id %q, got %q", agentID, info.AgentID)
	}
	if !info.TasksExist {
		t.Fatal("expected TasksExist to be true")
	}
	if info.Version != "0.1.0" {
		t.Fatalf("expected version 0.1.0, got %q", info.Version)
	}
	if len(info.CoreSkills) != 4 {
		t.Fatalf("expected 4 core skills, got %d", len(info.CoreSkills))
	}
	if info.InitializedAt.IsZero() {
		t.Fatal("expected InitializedAt to be set")
	}

	os.RemoveAll(agentDir)
}

func TestGetAgentSkillInfo_EmptyAgentID(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	_, err := sm.GetAgentSkillInfo("")
	if err == nil {
		t.Fatal("expected error for empty agent_id")
	}
}

func TestGetAgentSkillInfo_NotInitialized(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	agentID := "test-agent-uninit"
	os.RemoveAll(filepath.Join(sm.skillsDir, agentID))

	info, err := sm.GetAgentSkillInfo(agentID)
	if err != nil {
		t.Fatalf("GetAgentSkillInfo failed: %v", err)
	}
	if info.TasksExist {
		t.Fatal("expected TasksExist to be false for uninitialized agent")
	}
	if info.Version != "" {
		t.Fatalf("expected empty version, got %q", info.Version)
	}
}

func TestAgentSkillInfo_CoreSkills(t *testing.T) {
	cfg := testConfig()
	sm := NewSkillManager(cfg, testLogger())
	info, _ := sm.GetAgentSkillInfo("any-agent")
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
