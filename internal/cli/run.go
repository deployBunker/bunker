package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/config"
	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// runDefaultTimeoutSeconds is the default --timeout budget for
// `bunker run`. SURF-017: the old 30s default was the same remote-build
// killer SURF-011 already fixed on `bunker exec` (a cold `go build ./...`
// measures ~173s on a remote box), one verb over. 1800s matches exec and
// covers the remote build/test round; an explicit --timeout still wins.
const runDefaultTimeoutSeconds uint32 = 1800

// runArgs holds the parsed arguments for a `bunker run` invocation.
type runArgs struct {
	agentID     string
	serverName  string
	timeout     uint32
	detach      bool
	name        string
	envVars     []string
	preset      string
	command     string
	commandArgs []string
}

// parseRunArgs extracts the agent ID, flags, and command from the raw args
// slice. The accepted flag set (--server, --timeout, --detach, --name,
// --env, plus the root persistent flags --config/--daemon-config) is peeled
// in BOTH the pre-agent-id position and the post-agent-id position, each in
// the space form (--server prod) and the inline form (--server=prod)
// (DF-BUNKER-41). A flag-like token outside the set refuses LOCALLY —
// before any config load or RPC — naming the token.
//
// A peeled --config / --daemon-config is APPLIED through the cli setters:
// with DisableFlagParsing cobra never parses the root persistent flags for
// run, so the root PersistentPreRun transfer sees an empty value and a
// peeled path would otherwise be accepted and silently ignored.
func parseRunArgs(args []string) (runArgs, error) {
	if len(args) < 2 {
		return runArgs{}, fmt.Errorf("requires at least 2 arg(s), only received %d", len(args))
	}

	var (
		serverName string
		timeout    uint32 = runDefaultTimeoutSeconds
		detach     bool
		name       string
		envVars    []string
		preset     string
	)
	grammar := flagGrammar{name: "run", specs: map[string]flagGrammarSpec{
		"--server": {apply: func(v string) error { serverName = v; return nil }},
		"--timeout": {apply: func(v string) error {
			n, err := parseUint32(v)
			if err != nil {
				return fmt.Errorf("--timeout: %v", err)
			}
			timeout = n
			return nil
		}},
		"--detach": {apply: func(string) error { detach = true; return nil }, boolean: true},
		"--name":   {apply: func(v string) error { name = v; return nil }},
		"--env":    {apply: func(v string) error { envVars = append(envVars, v); return nil }},
		// GAP-116: the run path's preset flag — the per-spawn equivalent of
		// `spawn --preset`. Validated LOCALLY (vocabulary check) below.
		"--preset":        {apply: func(v string) error { preset = v; return nil }},
		"--config":        {apply: func(v string) error { SetConfigPathOverride(v); return nil }},
		"--daemon-config": {apply: func(v string) error { SetDaemonConfigPathOverride(v); return nil }},
	}}

	// Flags BEFORE the agent-id: strict — an unknown flag-like token
	// refuses locally instead of silently becoming the agent-id (the
	// DF-BUNKER-41 bug: `run --server prod abc -- echo` parsed "--server"
	// as the agent-id and died with "no target bound").
	pre, err := grammar.peelHead(args)
	if err != nil {
		return runArgs{}, err
	}
	if len(pre) == 0 {
		return runArgs{}, fmt.Errorf("agent-id required after flags")
	}
	agentID := pre[0]

	// Flags AFTER the agent-id: lenient — the peel stops at the first
	// token that is not an accepted flag (the command), so Docker flags
	// such as --rm, --format, -d, and --name are never eaten. This is the
	// position that already worked; its grammar is unchanged.
	rest := grammar.peelLeading(pre[1:])
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return runArgs{}, fmt.Errorf("command required after agent-id")
	}

	// Validate env var format.
	for _, e := range envVars {
		if !strings.Contains(e, "=") {
			return runArgs{}, fmt.Errorf("invalid env var %q (expected KEY=VALUE)", e)
		}
	}

	// GAP-116: validate --preset LOCALLY so an unknown name fails fast
	// without a round-trip. Empty defers to BUNKERD_SAFETY_PRESET > config
	// global > built-in default on the daemon side (re-validated there).
	if preset != "" && !config.ValidSafetyPreset(preset) {
		return runArgs{}, fmt.Errorf("invalid --preset %q (valid: %v)", preset, config.ValidSafetyPresets())
	}

	return runArgs{
		agentID:     agentID,
		serverName:  serverName,
		timeout:     timeout,
		detach:      detach,
		name:        name,
		envVars:     envVars,
		preset:      preset,
		command:     rest[0],
		commandArgs: rest[1:],
	}, nil
}

// NewRunCommand returns the `bunker run` cobra command.
func NewRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <agent-id> [flags] [--] <command> [args...]",
		Short: "Run a command in an agent's environment",
		Long: `Run a command in an agent's isolated environment via the bunkerd server.

Without --detach, the command runs synchronously and output is streamed to the
local terminal, just like 'bunker exec'.

With --detach, the command is started as a persistent systemd transient unit
that survives the SSH session ending. A run ID and systemd unit name are
printed on success.

Use -- to separate bunker flags from the command to execute, so that Docker
flags such as --rm, --format, -d, and --name are not intercepted by the CLI.

The run flags (--server, --timeout, --detach, --name, --env) and the global
persistent flags --config and --daemon-config are accepted in BOTH
positions, before and after the agent-id, in the space form (--server prod)
and the inline form (--server=prod):

  bunker run --server prod abc12345 -- echo hi
  bunker run abc12345 --server prod -- echo hi

Any other flag before the agent-id is rejected before anything is sent to
the server:

  run takes no flags before <agent-id> (got "--flag")

Examples:
  bunker run abc12345 -- docker ps
  bunker run --server prod abc12345 -- echo hi
  bunker run abc12345 --detach -- docker compose up
  bunker run abc12345 --detach --env DATABASE_URL=postgres://... -- ./worker
  bunker run --timeout 60 abc12345 -- ./longjob`,

		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "--help" || a == "-h" {
					return cmd.Help()
				}
			}
			parsed, err := parseRunArgs(args)
			if err != nil {
				return err
			}

			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			serverName := parsed.serverName
			// Fail-closed binding (GAP-093): mutating commands never fall
			// back to the shared active_server default.
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			serverName = resolved
			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(parsed.timeout)*time.Second)
			defer cancel()

			envMap := make(map[string]string)
			for _, e := range parsed.envVars {
				parts := strings.SplitN(e, "=", 2)
				envMap[parts[0]] = parts[1]
			}

			token := resolveToken(entry)

			if parsed.detach {
				req := connect.NewRequest(&v1.RunAgentRequest{
					AgentId:        parsed.agentID,
					Command:        parsed.command,
					Args:           parsed.commandArgs,
					Env:            envMap,
					Detach:         true,
					TimeoutSeconds: parsed.timeout,
					Name:           parsed.name,
					SafetyPreset:   parsed.preset,
				})
				if token != "" {
					req.Header().Set("Authorization", "Bearer "+token)
				}
				resp, err := client.RunAgent(ctx, req)
				if err != nil {
					return decorateDeadline("run", fmt.Errorf("run agent: %w", err), parsed.timeout)
				}
				fmt.Printf("Run ID: %s\n", resp.Msg.GetRunId())
				fmt.Printf("Unit: %s\n", resp.Msg.GetUnitName())
				return nil
			}

			// Synchronous mode: stream via ExecAgent like `bunker exec`.
			req := connect.NewRequest(&v1.ExecAgentRequest{
				AgentId:        parsed.agentID,
				Command:        parsed.command,
				Args:           parsed.commandArgs,
				TimeoutSeconds: parsed.timeout,
			})
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			stream, err := client.ExecAgent(ctx, req)
			if err != nil {
				return decorateDeadline("run", fmt.Errorf("exec agent: %w", err), parsed.timeout)
			}
			var exitCode int32
			for stream.Receive() {
				msg := stream.Msg()
				if msg.GetStdout() != nil {
					fmt.Print(string(msg.GetStdout()))
				}
				if msg.GetStderr() != nil {
					fmt.Fprint(cmd.ErrOrStderr(), string(msg.GetStderr()))
				}
				if msg.ExitCode != 0 {
					exitCode = msg.ExitCode
				}
			}
			if err := stream.Err(); err != nil {
				return decorateDeadline("run", fmt.Errorf("stream error: %w", err), parsed.timeout)
			}
			if exitCode != 0 {
				return &ExitError{Code: int(exitCode)}
			}
			return nil
		},
		DisableFlagParsing: true,
	}

	cmd.Flags().String("server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().Uint32("timeout", runDefaultTimeoutSeconds, "Command timeout in seconds")
	cmd.Flags().Bool("detach", false, "Run as a persistent systemd transient unit")
	cmd.Flags().String("name", "", "Optional name suffix for the run unit")
	cmd.Flags().StringArray("env", nil, "Environment variable in KEY=VALUE form (repeatable)")

	return cmd
}

func parseUint32(s string) (uint32, error) {
	var v uint32
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}
