package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewStopCommand returns the `bunker stop` cobra command.
//
// Stop pauses an agent WITHOUT destroying it: its session units and processes
// are stopped (CPU freed), while the Linux user, home directory, container and
// allocated port range are kept, so `bunker start` can resume it later.
func NewStopCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "stop <agent-id>",
		Short: "Stop an agent (keeping its state)",
		Long: `Stop an agent on the active bunkerd server.

The agent's session units and processes are stopped so it no longer consumes
CPU, but the agent itself is KEPT: its Linux user, home directory, container
and allocated port range all survive, and it can be resumed with
'bunker start <agent-id>'. Stopping an already-stopped agent is a no-op.

Examples:
  bunker stop abc12345
  bunker stop abc12345 --server staging`,

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
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

			// 3. Call the RPC
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.StopAgentRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			resp, err := client.StopAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("stop agent: %w", err)
			}

			// 4. Print result
			switch resp.Msg.GetStatus() {
			case "stopped":
				fmt.Printf("Agent %s stopped.\n", agentID)
			case "already_stopped":
				fmt.Printf("Agent %s is already stopped.\n", agentID)
			case "not_found":
				return fmt.Errorf("agent %s not found", agentID)
			default:
				return fmt.Errorf("stop agent: unexpected status %q", resp.Msg.GetStatus())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
