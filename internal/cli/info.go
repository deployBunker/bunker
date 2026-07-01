package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewInfoCommand returns the `bunker info` cobra command.
func NewInfoCommand() *cobra.Command {
	var serverName string

	cmd := &cobra.Command{
		Use:   "info AGENT_ID",
		Short: "Show detailed information about an agent",
		Long: `Display detailed information about a specific agent, including status,
resource limits, network configuration, and timestamps.

Examples:
  bunker info abc12345
  bunker info abc12345 --server staging`,
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

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC
			resp, err := client.GetAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("get agent: %w", err)
			}

			// 5. Print agent details
			a := resp.Msg.GetAgent()
			if a == nil {
				return fmt.Errorf("agent %q not found in response", agentID)
			}

			fmt.Println()
			fmt.Printf("══════════ Agent: %s ══════════\n", a.AgentId)
			fmt.Println()
			fmt.Printf("  Status:           %s\n", a.Status)
			if a.CreatedAt != "" {
				fmt.Printf("  Created At:       %s\n", a.CreatedAt)
			}
			if a.ExpiresAt != "" {
				fmt.Printf("  Expires At:       %s\n", a.ExpiresAt)
			}
			if a.TailnetIp != "" {
				fmt.Printf("  Tailnet IP:       %s\n", a.TailnetIp)
			}
			if a.PublicUrl != "" {
				fmt.Printf("  Public URL:       %s\n", a.PublicUrl)
			}
			if a.PortRangeStart > 0 || a.PortRangeEnd > 0 {
				fmt.Printf("  Port Range:       %d-%d\n", a.PortRangeStart, a.PortRangeEnd)
			}
			if a.DockerHostTunnel != "" {
				fmt.Printf("  Docker Tunnel:    %s\n", a.DockerHostTunnel)
			}
			if a.SshfsMount != "" {
				fmt.Printf("  SSHFS Mount:      %s\n", a.SshfsMount)
			}
			if limits := a.Limits; limits != nil {
				fmt.Println("  Limits:")
				if limits.CpuQuota > 0 {
					fmt.Printf("    CPU Quota:      %.1f cores\n", limits.CpuQuota)
				}
				if limits.MemoryMaxBytes > 0 {
					fmt.Printf("    Memory Limit:   %s\n", humanBytes(limits.MemoryMaxBytes))
				}
				if limits.DiskMaxBytes > 0 {
					fmt.Printf("    Disk Limit:     %s\n", humanBytes(limits.DiskMaxBytes))
				}
				if limits.MaxDockerContainers > 0 {
					fmt.Printf("    Max Containers: %d\n", limits.MaxDockerContainers)
				}
			}
			fmt.Println()

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
