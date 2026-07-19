# Package: `internal/hermes`

## Public API

- `SkillManager` — manages per-agent coding-hermes skill scaffolding.
- `NewSkillManager(cfg, logger)` — creates a manager using `cfg.Agent.BaseDataDir` for skill workspaces.
- `(*SkillManager) InitAgentSkills(ctx, agentID)` — creates `.coding-hermes/tasks.md` and `version.yaml` for an agent, best-effort installs core skills via `hermes skill install`.
- `(*SkillManager) CleanupAgentSkills(agentID)` — removes the agent's skill workspace directory.
- `(*SkillManager) ReadAgentTasks(agentID)` / `UpdateAgentTasks(agentID, content)` — read/write the agent's tasks.md.
- `(*SkillManager) GetAgentSkillInfo(agentID)` — returns structured metadata about the agent's skill setup.
- `AgentSkillInfo` — struct with AgentID, SkillsDir, TasksExist, Version, CoreSkills, InitializedAt.

## Conventions

- Agent skill workspaces live under `<BaseDataDir>/skills/<agentID>/`.
- Core skills installed: `coding-hermes`, `coding-hermes-cron`, `hilo-usage`, `gitreins`.
- `hermes skill install` is best-effort — if the hermes CLI is not available, the agent can use skill files manually.
- Tasks.md is only written if it does not already exist (idempotent initialisation).

## Dependencies

- `internal/config` — Config struct with `Agent.BaseDataDir`.
- Standard library: `context`, `fmt`, `log/slog`, `os`, `os/exec`, `path/filepath`, `strings`, `time`.

## Test Patterns

- `skills_test.go` covers: InitAgentSkills idempotency, CleanupAgentSkills with missing directory, missing agentID errors.
- `integration_test.go` has 5 safe CI integration tests: skill lifecycle, task queue format, core skills list, tracker integration, cleanup idempotency.

## Pitfalls

1. **`hermes skill install` is a subprocess call — it inherits the caller's environment.** If `HERMES_HOME` is not set correctly, skills install to the wrong directory. The manager explicitly sets `HERMES_HOME` to the agent's skill workspace.
2. **`InitAgentSkills` is idempotent only for tasks.md and version.yaml.** Core skills are always reinstalled (best-effort). Repeated calls don't duplicate tasks.md.
3. **No locking around skill file writes.** Two concurrent calls to `InitAgentSkills` for the same agent could race on `version.yaml`. In practice, the spawn lifecycle serialises per-agent.
4. **Agent skill workspace is NOT chowned to the agent user.** Files are created by the bunkerd process and the agent user may not own them. If the agent needs to write its own tasks.md, the directory permissions must allow it.
