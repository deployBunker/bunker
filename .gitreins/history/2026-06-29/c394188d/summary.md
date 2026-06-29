# Verdict: wi-016

**Task:** bunker exec command
**Evaluated:** 2026-06-29T08:21:09.306982
**Result:** ✓ PASS

## Criteria

- ✓ **bunker exec command exists in internal/cli/exec.go with agent-id, command, and optional args**
  - internal/cli/exec.go:18-29 — cobra.Command with Use: "exec <agent-id> <command> [args...]" and Args: cobra.MinimumNArgs(2) extracts agentID=args[0], command=args[1], commandArgs=args[2:]
- ✓ **bunker exec is wired into cmd/bunker/main.go**
  - cmd/bunker/main.go:33 — root.AddCommand(cli.NewExecCommand()) is registered in the root command
- ✓ **ExecAgent RPC is implemented in internal/server/service.go via SSH to agent user**
  - internal/server/service.go:155-170 — ExecAgent method uses exec.CommandContext(ctx, "ssh", ..., fmt.Sprintf("bunker-%s@localhost", agentID), "--", req.Msg.Command)
- ✓ **Server streams stdout/stderr concurrently and returns exit code**
  - internal/server/service.go:180-253 — Two goroutines with sync.WaitGroup stream stdout and stderr concurrently via stream.Send; final stream.Send with ExitCode after cmd.Wait() captures exit code
- ✓ **9 tests in exec_test.go cover help, no-server, success, exit-code, server-error, agent-not-found, stderr-output, timeout-flag, missing-args**
  - internal/cli/exec_test.go — 9 tests: TestExecCommand_Help (87), TestExecCommand_NoServer (104), TestExecCommand_Success (118), TestExecCommand_ExitCode (140), TestExecCommand_ServerError (160), TestExecCommand_AgentNotFound (174), TestExecCommand_StderrOutput (188), TestExecCommand_TimeoutFlag (204), TestExecCommand_MissingArgs (220). All pass.

## Summary

Judge Result: wi-016

Tier 2 (Agentic Evaluator): COMPLETE
  ✓ bunker exec command exists in internal/cli/exec.go with agent-id, command, and optional args: internal/cli/exec.go:18-29 — cobra.Command with Use: "exec <agent-id> <command> [args...]" and Args: cobra.MinimumNArgs(2) extracts agentID=args[0], command=args[1], commandArgs=args[2:]
  ✓ bunker exec is wired into cmd/bunker/main.go: cmd/bunker/main.go:33 — root.AddCommand(cli.NewExecCommand()) is registered in the root command
  ✓ ExecAgent RPC is implemented in internal/server/service.go via SSH to agent user: internal/server/service.go:155-170 — ExecAgent method uses exec.CommandContext(ctx, "ssh", ..., fmt.Sprintf("bunker-%s@localhost", agentID), "--", req.Msg.Command)
  ✓ Server streams stdout/stderr concurrently and returns exit code: internal/server/service.go:180-253 — Two goroutines with sync.WaitGroup stream stdout and stderr concurrently via stream.Send; final stream.Send with ExitCode after cmd.Wait() captures exit code
  ✓ 9 tests in exec_test.go cover help, no-server, success, exit-code, server-error, agent-not-found, stderr-output, timeout-flag, missing-args: internal/cli/exec_test.go — 9 tests: TestExecCommand_Help (87), TestExecCommand_NoServer (104), TestExecCommand_Success (118), TestExecCommand_ExitCode (140), TestExecCommand_ServerError (160), TestExecCommand_AgentNotFound (174), TestExecCommand_StderrOutput (188), TestExecCommand_TimeoutFlag (204), TestExecCommand_MissingArgs (220). All pass.

Overall: PASS ✓
