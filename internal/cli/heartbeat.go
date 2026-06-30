package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewHeartbeatCommand returns the `bunker heartbeat` cobra command.
func NewHeartbeatCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "heartbeat AGENT_ID",
		Short: "Send a heartbeat to extend an agent's TTL",
		Long: `Send a heartbeat to the bunkerd server for the given agent.

The server extends the agent's TTL by the configured default TTL (or 6h)
when the heartbeat is acknowledged. This is useful for keeping long-running
agents alive without changing the original spawn request.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.HeartbeatAgentRequest{AgentId: agentID})
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			resp, err := client.HeartbeatAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}

			msg := resp.Msg
			if msg.Acknowledged {
				fmt.Printf("Heartbeat acknowledged for agent %s\n", msg.AgentId)
				if msg.ExpiresAt != "" {
					fmt.Printf("Expires at: %s\n", msg.ExpiresAt)
				}
			} else {
				fmt.Printf("Heartbeat not acknowledged for agent %s\n", msg.AgentId)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
