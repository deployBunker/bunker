package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewRestartCommand returns the `bunker restart` cobra command.
//
// Restart is the recovery path for a wedged agent session: it stops and starts
// the agent in one call and resets the heartbeat expiry, so a session that is
// half-dead can be brought back without destroying the agent (and its user,
// home, container and port range).
func NewRestartCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "restart <agent-id>",
		Short: "Restart an agent (stop + start, TTL reset)",
		Long: `Restart an agent on the active bunkerd server.

Stops and starts the agent in one call to recover a wedged session. The
agent's heartbeat expiry is reset to now plus the daemon's default TTL, and
the refreshed expiry is printed. Nothing is destroyed: the Linux user, home
directory, container and allocated port range are kept.

Examples:
  bunker restart abc12345
  bunker restart abc12345 --server staging`,

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
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.RestartAgentRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			resp, err := client.RestartAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("restart agent: %w", err)
			}

			// 4. Print result
			switch resp.Msg.GetStatus() {
			case "restarted":
				fmt.Printf("Agent %s restarted.\n", agentID)
				if resp.Msg.GetExpiresAt() != "" {
					fmt.Printf("Expires at: %s\n", resp.Msg.GetExpiresAt())
				}
			case "not_found":
				return fmt.Errorf("agent %s not found", agentID)
			default:
				return fmt.Errorf("restart agent: unexpected status %q", resp.Msg.GetStatus())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
