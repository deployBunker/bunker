package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewStartCommand returns the `bunker start` cobra command.
//
// Start resumes an agent that was paused with `bunker stop`: the agent's
// session units are re-armed (and, for an agent carrying an image spec, its
// kept container is started again) and its status returns to running. The
// agent's existing heartbeat expiry is left untouched — `bunker restart`
// is the command that resets the TTL.
func NewStartCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "start <agent-id>",
		Short: "Start a stopped agent",
		Long: `Start a stopped agent on the active bunkerd server.

Re-arms an agent paused with 'bunker stop': its Linux user and home directory
were kept, so the session is restored and the agent's status returns to
running. Starting an agent that is already running is a no-op.

Examples:
  bunker start abc12345
  bunker start abc12345 --server staging`,

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

			req := connect.NewRequest(&v1.StartAgentRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			resp, err := client.StartAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("start agent: %w", err)
			}

			// 4. Print result
			switch resp.Msg.GetStatus() {
			case "started":
				fmt.Printf("Agent %s started.\n", agentID)
			case "already_running":
				fmt.Printf("Agent %s is already running.\n", agentID)
			case "not_found":
				return fmt.Errorf("agent %s not found", agentID)
			default:
				return fmt.Errorf("start agent: unexpected status %q", resp.Msg.GetStatus())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
