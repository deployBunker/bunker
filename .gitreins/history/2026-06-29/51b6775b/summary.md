# Verdict: wi-016

**Task:** bunker exec command
**Evaluated:** 2026-06-29T08:19:56.709678
**Result:** ✓ PASS

## Criteria

- ✓ **bunker exec command exists in internal/cli/exec.go with agent-id, command, and optional args**
  - internal/cli/exec.go: Use="exec <agent-id> <command> [args...]", Args=cobra.MinimumNArgs(2), commandArgs=args[2:] captures optional args
- ✓ **bunker exec is wired into cmd/bunker/main.go**
  - cmd/bunker/main.go: root.AddCommand(cli.NewExecCommand()) wires the exec subcommand
- ✓ **ExecAgent RPC is implemented in internal/server/service.go via SSH to agent user**
  - internal/server/service.go:142-247 — ExecAgent method uses exec.CommandContext(ctx, "ssh", ..., fmt.Sprintf("bunker-%s@localhost", agentID), ...) to SSH as agent user
- ✓ **Server streams stdout/stderr concurrently and returns exit code**
  - internal/server/service.go:176-224 — two goroutines (sync.WaitGroup) stream stdout via stream.Send(Stdout) and stderr via stream.Send(Stderr) concurrently; lines 231-240 capture exitCode from exec.ExitError; line 247 sends final ExitCode
- ✓ **9 tests in exec_test.go cover help, no-server, success, exit-code, server-error, agent-not-found, stderr-output, timeout-flag, missing-args**
  - internal/cli/exec_test.go:69-240 — 9 TestExecCommand_* functions (Help, NoServer, Success, ExitCode, ServerError, AgentNotFound, StderrOutput, TimeoutFlag, MissingArgs) all pass go test

## Summary

Judge Result: wi-016

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ bunker exec command exists in internal/cli/exec.go with agent-id, command, and optional args: internal/cli/exec.go: Use="exec <agent-id> <command> [args...]", Args=cobra.MinimumNArgs(2), commandArgs=args[2:] captures optional args
  ✓ bunker exec is wired into cmd/bunker/main.go: cmd/bunker/main.go: root.AddCommand(cli.NewExecCommand()) wires the exec subcommand
  ✓ ExecAgent RPC is implemented in internal/server/service.go via SSH to agent user: internal/server/service.go:142-247 — ExecAgent method uses exec.CommandContext(ctx, "ssh", ..., fmt.Sprintf("bunker-%s@localhost", agentID), ...) to SSH as agent user
  ✓ Server streams stdout/stderr concurrently and returns exit code: internal/server/service.go:176-224 — two goroutines (sync.WaitGroup) stream stdout via stream.Send(Stdout) and stderr via stream.Send(Stderr) concurrently; lines 231-240 capture exitCode from exec.ExitError; line 247 sends final ExitCode
  ✓ 9 tests in exec_test.go cover help, no-server, success, exit-code, server-error, agent-not-found, stderr-output, timeout-flag, missing-args: internal/cli/exec_test.go:69-240 — 9 TestExecCommand_* functions (Help, NoServer, Success, ExitCode, ServerError, AgentNotFound, StderrOutput, TimeoutFlag, MissingArgs) all pass go test

Overall: PASS ✓
