package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewExecCommand returns the `bunker exec` cobra command.
func NewExecCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
		timeout    uint32
	)

	cmd := &cobra.Command{
		Use:   "exec <agent-id> <command> [args...]",
		Short: "Execute a command in an agent's environment",
		Long: `Execute a command inside an agent's isolated environment via the bunkerd server.

The command runs as the agent's user (bunker-<agent-id>) with the agent's
Docker socket and environment variables available.

Examples:
  bunker exec abc12345 docker ps
  bunker exec abc12345 docker run --rm hello-world
  bunker exec abc12345 ls -la /home
  bunker exec abc12345 --timeout 60 docker build -t myapp .`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID = args[0]
			command := args[1]
			commandArgs := args[2:]

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
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

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.ExecAgentRequest{
				AgentId:        agentID,
				Command:        command,
				Args:           commandArgs,
				TimeoutSeconds: timeout,
			})

			// Auth token
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC (streaming)
			stream, err := client.ExecAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("exec agent: %w", err)
			}

			// 5. Stream output
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
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&timeout, "timeout", 30, "Command timeout in seconds")

	return cmd
}
