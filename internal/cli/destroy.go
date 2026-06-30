package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewDestroyCommand returns the `bunker destroy` cobra command.
func NewDestroyCommand() *cobra.Command {
	var (
		serverName string
		force      bool
	)

	cmd := &cobra.Command{
		Use:   "destroy <agent-id>",
		Short: "Destroy an agent",
		Long: `Destroy an agent on the active bunkerd server.

Examples:
  bunker destroy abc12345
  bunker destroy abc12345 --force
  bunker destroy abc12345 --server staging`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

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
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.DestroyAgentRequest{
				AgentId: agentID,
				Force:   force,
			})

			// Auth token
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC
			resp, err := client.DestroyAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("destroy agent: %w", err)
			}

			// 5. Print result
			if resp.Msg.Status == "not_found" {
				fmt.Printf("Agent %s not found.\n", agentID)
				return nil
			}
			fmt.Printf("Agent %s destroyed.\n", agentID)

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&force, "force", false, "Force destroy even if agent is running")

	return cmd
}
