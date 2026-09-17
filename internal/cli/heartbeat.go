package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// heartbeatTTLSemantics is the single-sentence statement of what an
// acknowledged heartbeat does to the agent's expiry. It is printed on EVERY
// acknowledged heartbeat (DF-BUNKER-17) and documented verbatim in README.md
// (heartbeat section), so the behaviour is no longer only discoverable in
// --help. The heartbeat request carries no duration (proto
// HeartbeatAgentRequest has agent_id only), so the daemon decides the TTL —
// hence "the daemon's default TTL" rather than a client-chosen duration.
const heartbeatTTLSemantics = "the agent's expiry was extended to the daemon's default TTL (6h unless the daemon config sets agent.default_ttl); an existing longer expiry is never shortened"

// NewHeartbeatCommand returns the `bunker heartbeat` cobra command.
func NewHeartbeatCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "heartbeat AGENT_ID",
		Short: "Send a heartbeat to extend an agent's TTL",
		Long: `Send a heartbeat to the bunkerd server for the given agent.

There is no duration flag: the request carries only the agent ID, so the
daemon always applies its own default TTL (6h unless the daemon config sets
agent.default_ttl) and never shortens an existing longer expiry — a 7d agent
is not reset to 6h by a heartbeat. Every acknowledged heartbeat prints the
resulting expiry and states exactly what happened:

  TTL: ` + heartbeatTTLSemantics + `

This is useful for keeping long-running agents alive without changing the
original spawn request.`,
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
				// Always print the resulting expiry: it is the only way the
				// operator can see WHICH expiry their heartbeat produced,
				// and the daemon reports it on every acknowledgement.
				expires := msg.ExpiresAt
				if expires == "" {
					expires = "(not reported by the daemon)"
				}
				fmt.Printf("Expires at: %s\n", expires)
				// State the semantics too: without a duration flag the TTL
				// the daemon applies is otherwise invisible (DF-BUNKER-17).
				fmt.Printf("TTL: %s\n", heartbeatTTLSemantics)
			} else {
				fmt.Printf("Heartbeat not acknowledged for agent %s\n", msg.AgentId)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
