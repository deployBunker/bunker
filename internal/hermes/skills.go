// Package hermes provides integration with the Coding-Hermes skill system.
// It manages per-agent skill installation, task queue initialization, and
// lifecycle hooks that trigger coding-hermes foreman ticks.
package hermes

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/deployBunker/bunker/internal/config"
)

// SkillManager manages coding-hermes skills for agents.
type SkillManager struct {
	cfg    *config.Config
	logger *slog.Logger
	// skillsDir is the base directory where agent skill workspaces live.
	skillsDir string
}

// NewSkillManager creates a new SkillManager.
func NewSkillManager(cfg *config.Config, logger *slog.Logger) *SkillManager {
	skillsDir := filepath.Join(cfg.Agent.BaseDataDir, "skills")
	return &SkillManager{
		cfg:       cfg,
		logger:    logger,
		skillsDir: skillsDir,
	}
}

// InitAgentSkills sets up the coding-hermes skill scaffolding for an agent.
// It creates the agent's skill workspace directory, writes a default
// tasks.md file, and installs the core skills (coding-hermes, coding-hermes-cron,
// hilo-usage, gitreins) into the agent's isolated environment.
func (sm *SkillManager) InitAgentSkills(ctx context.Context, agentID string) error {
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}

	agentSkillsDir := filepath.Join(sm.skillsDir, agentID)
	sm.logger.Info("initializing coding-hermes skills", "agent_id", agentID, "dir", agentSkillsDir)

	// Create the agent skill workspace.
	if err := os.MkdirAll(agentSkillsDir, 0750); err != nil {
		return fmt.Errorf("create agent skills dir: %w", err)
	}

	// Create .coding-hermes directory for task queue.
	codingHermesDir := filepath.Join(agentSkillsDir, ".coding-hermes")
	if err := os.MkdirAll(codingHermesDir, 0750); err != nil {
		return fmt.Errorf("create .coding-hermes dir: %w", err)
	}

	// Write default tasks.md with a welcome task.
	tasksPath := filepath.Join(codingHermesDir, "tasks.md")
	if _, err := os.Stat(tasksPath); os.IsNotExist(err) {
		tasksContent := fmt.Sprintf(`# Coding Hermes Task Queue — Agent %s

## Active Sprint

### [ ] Welcome task — verify skill integration
- **Priority:** high
- **Model:** deepseek-v4-flash
- **Files:** .coding-hermes/tasks.md
- **AC:** Agent can read and update its own task queue

## Quality Gates
- **GitReins Tier 1**: secrets, lint, tests
- **GitReins Tier 2**: LLM code review per task
- **Hilo**: dependency graph analysis
- **Build**: go build ./... && go vet ./...

## Task States
- [ ] — pending
- [x] — complete
`, agentID)
		if err := os.WriteFile(tasksPath, []byte(tasksContent), 0640); err != nil {
			return fmt.Errorf("write tasks.md: %w", err)
		}
	}

	// Write version.yaml for foreman tracking.
	versionPath := filepath.Join(codingHermesDir, "version.yaml")
	if _, err := os.Stat(versionPath); os.IsNotExist(err) {
		versionContent := fmt.Sprintf("version: 0.1.0\nagent_id: %s\ncreated_at: %s\n",
			agentID, time.Now().Format(time.RFC3339))
		if err := os.WriteFile(versionPath, []byte(versionContent), 0640); err != nil {
			return fmt.Errorf("write version.yaml: %w", err)
		}
	}

	// Install core skills via hermes CLI (if available).
	// This is best-effort; if hermes is not installed, the agent can still
	// use the skill files manually.
	if err := sm.installCoreSkills(ctx, agentID, agentSkillsDir); err != nil {
		sm.logger.Warn("core skill install failed (hermes may not be available)", "agent_id", agentID, "error", err)
	}

	sm.logger.Info("coding-hermes skills initialized", "agent_id", agentID)
	return nil
}

// installCoreSkills installs the required coding-hermes skills into the agent workspace.
func (sm *SkillManager) installCoreSkills(ctx context.Context, agentID, agentSkillsDir string) error {
	coreSkills := []string{
		"coding-hermes",
		"coding-hermes-cron",
		"hilo-usage",
		"gitreins",
	}

	for _, skill := range coreSkills {
		cmd := exec.CommandContext(ctx, "hermes", "skill", "install", skill)
		cmd.Dir = agentSkillsDir
		cmd.Env = append(os.Environ(),
			"HERMES_HOME="+agentSkillsDir,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Log but don't fail — hermes CLI may not be installed.
			sm.logger.Debug("skill install attempt",
				"skill", skill,
				"agent_id", agentID,
				"error", err,
				"output", strings.TrimSpace(string(out)),
			)
		} else {
			sm.logger.Info("skill installed", "skill", skill, "agent_id", agentID)
		}
	}
	return nil
}

// CleanupAgentSkills removes the skill workspace for an agent.
// Called during agent destroy to ensure no orphaned skill directories remain.
func (sm *SkillManager) CleanupAgentSkills(agentID string) error {
	if agentID == "" {
		return fmt.Errorf("agent_id is required")
	}

	agentSkillsDir := filepath.Join(sm.skillsDir, agentID)
	sm.logger.Info("cleaning up coding-hermes skills", "agent_id", agentID, "dir", agentSkillsDir)

	if err := os.RemoveAll(agentSkillsDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove agent skills dir: %w", err)
	}
	return nil
}

// GetAgentTasksPath returns the path to the agent's tasks.md file.
func (sm *SkillManager) GetAgentTasksPath(agentID string) string {
	return filepath.Join(sm.skillsDir, agentID, ".coding-hermes", "tasks.md")
}

// ReadAgentTasks reads the current task queue for an agent.
func (sm *SkillManager) ReadAgentTasks(agentID string) (string, error) {
	path := sm.GetAgentTasksPath(agentID)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read tasks: %w", err)
	}
	return string(data), nil
}

// UpdateAgentTasks writes the task queue for an agent.
func (sm *SkillManager) UpdateAgentTasks(agentID string, content string) error {
	path := sm.GetAgentTasksPath(agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("create tasks dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		return fmt.Errorf("write tasks: %w", err)
	}
	return nil
}

// AgentSkillInfo returns metadata about an agent's skill setup.
type AgentSkillInfo struct {
	AgentID        string    `json:"agent_id"`
	SkillsDir      string    `json:"skills_dir"`
	TasksExist     bool      `json:"tasks_exist"`
	Version        string    `json:"version"`
	CoreSkills     []string  `json:"core_skills"`
	InitializedAt  time.Time `json:"initialized_at"`
}

// GetAgentSkillInfo returns information about the agent's skill workspace.
func (sm *SkillManager) GetAgentSkillInfo(agentID string) (*AgentSkillInfo, error) {
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	agentSkillsDir := filepath.Join(sm.skillsDir, agentID)
	info := &AgentSkillInfo{
		AgentID:   agentID,
		SkillsDir: agentSkillsDir,
		CoreSkills: []string{
			"coding-hermes",
			"coding-hermes-cron",
			"hilo-usage",
			"gitreins",
		},
	}

	// Check if tasks.md exists.
	tasksPath := filepath.Join(agentSkillsDir, ".coding-hermes", "tasks.md")
	if _, err := os.Stat(tasksPath); err == nil {
		info.TasksExist = true
	}

	// Read version.yaml if present.
	versionPath := filepath.Join(agentSkillsDir, ".coding-hermes", "version.yaml")
	if data, err := os.ReadFile(versionPath); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "version: ") {
				info.Version = strings.TrimPrefix(line, "version: ")
			}
			if strings.HasPrefix(line, "created_at: ") {
				if t, err := time.Parse(time.RFC3339, strings.TrimPrefix(line, "created_at: ")); err == nil {
					info.InitializedAt = t
				}
			}
		}
	}

	return info, nil
}
