package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// runArgs holds the parsed arguments for a `bunker run` invocation.
type runArgs struct {
	agentID     string
	serverName  string
	timeout     uint32
	detach      bool
	name        string
	envVars     []string
	command     string
	commandArgs []string
}

// parseRunArgs extracts the agent ID, flags, and command from the raw args slice.
// It supports --detach, --env KEY=VALUE, --env=KEY=VALUE, --timeout, --server, and --name.
func parseRunArgs(args []string) (runArgs, error) {
	if len(args) < 2 {
		return runArgs{}, fmt.Errorf("requires at least 2 arg(s), only received %d", len(args))
	}

	agentID := args[0]
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return runArgs{}, fmt.Errorf("command required after agent-id")
	}

	var (
		serverName string
		timeout    uint32 = 30
		detach     bool
		name       string
		envVars    []string
	)

	i := 0
	for i < len(rest) {
		switch {
		case rest[i] == "--server":
			if i+1 < len(rest) {
				serverName = rest[i+1]
				i += 2
				continue
			}
		case rest[i] == "--timeout":
			if i+1 < len(rest) {
				if v, err := parseUint32(rest[i+1]); err == nil {
					timeout = v
				}
				i += 2
				continue
			}
		case rest[i] == "--detach":
			detach = true
			i += 1
			continue
		case rest[i] == "--name":
			if i+1 < len(rest) {
				name = rest[i+1]
				i += 2
				continue
			}
		case strings.HasPrefix(rest[i], "--env="):
			envVars = append(envVars, strings.TrimPrefix(rest[i], "--env="))
			i += 1
			continue
		case rest[i] == "--env":
			if i+1 < len(rest) {
				envVars = append(envVars, rest[i+1])
				i += 2
				continue
			}
		}
		break
	}
	rest = rest[i:]
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

	return runArgs{
		agentID:     agentID,
		serverName:  serverName,
		timeout:     timeout,
		detach:      detach,
		name:        name,
		envVars:     envVars,
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

Examples:
  bunker run abc12345 -- docker ps
  bunker run abc12345 --detach -- docker compose up
  bunker run abc12345 --detach --env DATABASE_URL=postgres://... -- ./worker`,

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
			if serverName == "" {
				serverName = cfg.ActiveServer
			}
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
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
				})
				if token != "" {
					req.Header().Set("Authorization", "Bearer "+token)
				}
				resp, err := client.RunAgent(ctx, req)
				if err != nil {
					return fmt.Errorf("run agent: %w", err)
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
				return fmt.Errorf("exec agent: %w", err)
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
				return fmt.Errorf("stream error: %w", err)
			}
			if exitCode != 0 {
				return fmt.Errorf("exit code %d", exitCode)
			}
			return nil
		},
		DisableFlagParsing: true,
	}

	cmd.Flags().String("server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32("timeout", 30, "Command timeout in seconds")
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
