package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// NewMetricsCommand returns the `bunker metrics` cobra command.
func NewMetricsCommand() *cobra.Command {
	var (
		serverName string
		agentID    string
	)

	cmd := &cobra.Command{
		Use:   "metrics [agent-id]",
		Short: "Show live resource metrics for an agent or server",
		Long: `Display live resource usage metrics for a specific agent or the entire server.

If an agent-id is provided, shows per-agent metrics (CPU, memory, disk).
If no agent-id is provided, shows server-wide metrics for all agents.

Examples:
  bunker metrics                    # server-wide metrics
  bunker metrics abc12345           # per-agent metrics
  bunker metrics abc12345 --server staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
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

			// 3. Build HTTP client
			httpClient := &http.Client{Timeout: 30 * time.Second}
			if entry.TLSInsecure {
				httpClient.Transport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}

			// 4. Build request
			client := bunkerv1connect.NewBunkerdClient(httpClient, entry.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Auth token
			token := entry.Token
			if token == "" {
				token = viper.GetString("token")
			}

			if len(args) > 0 {
				agentID = args[0]
			}

			if agentID != "" {
				return printAgentMetrics(ctx, client, token, agentID)
			}
			return printServerMetrics(ctx, client, token)
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}

func printServerMetrics(ctx context.Context, client bunkerv1connect.BunkerdClient, token string) error {
	req := connect.NewRequest(&v1.ServerMetricsRequest{})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	resp, err := client.ServerMetrics(ctx, req)
	if err != nil {
		return fmt.Errorf("server metrics: %w", err)
	}

	msg := resp.Msg
	fmt.Println()
	fmt.Println("══════════ Server Metrics ══════════")
	fmt.Println()
	if msg.CpuUsagePercent > 0 {
		fmt.Printf("  CPU Usage:      %.2f%%\n", msg.CpuUsagePercent)
	}
	if msg.MemoryUsedBytes > 0 {
		fmt.Printf("  Memory Used:    %s / %s\n", humanBytes(msg.MemoryUsedBytes), humanBytes(msg.MemoryTotalBytes))
	}
	if msg.DiskUsedBytes > 0 {
		fmt.Printf("  Disk Used:      %s / %s\n", humanBytes(msg.DiskUsedBytes), humanBytes(msg.DiskTotalBytes))
	}
	if msg.DockerContainersTotal > 0 {
		fmt.Printf("  Docker Containers: %d\n", msg.DockerContainersTotal)
	}
	fmt.Println()

	if len(msg.Agents) == 0 {
		fmt.Println("No agents tracked.")
		return nil
	}

	fmt.Printf("  %-14s %-10s %-10s %s\n", "Agent ID", "Status", "CPU %", "Memory")
	fmt.Printf("  %-14s %-10s %-10s %s\n", "────────", "──────", "────", "──────")
	for _, a := range msg.Agents {
		cpuStr := "-"
		memStr := "-"
		if a.Limits != nil {
			if a.Limits.CpuQuota > 0 {
				cpuStr = fmt.Sprintf("%.1f", a.Limits.CpuQuota)
			}
			if a.Limits.MemoryMaxBytes > 0 {
				memStr = humanBytes(a.Limits.MemoryMaxBytes)
			}
		}
		fmt.Printf("  %-14s %-10s %-10s %s\n", a.AgentId, a.Status, cpuStr, memStr)
	}
	fmt.Println()
	fmt.Printf("Total: %d agents\n", len(msg.Agents))

	return nil
}

func printAgentMetrics(ctx context.Context, client bunkerv1connect.BunkerdClient, token, agentID string) error {
	req := connect.NewRequest(&v1.AgentMetricsRequest{AgentId: agentID})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	resp, err := client.AgentMetrics(ctx, req)
	if err != nil {
		return fmt.Errorf("agent metrics: %w", err)
	}

	msg := resp.Msg
	fmt.Println()
	fmt.Printf("══════════ Agent Metrics: %s ══════════\n", agentID)
	fmt.Println()
	fmt.Printf("  Status:         %s\n", msg.Status)
	if msg.CpuUsagePercent > 0 {
		fmt.Printf("  CPU Usage:      %.2f%%\n", msg.CpuUsagePercent)
	}
	if msg.MemoryUsedBytes > 0 {
		fmt.Printf("  Memory Used:    %s\n", humanBytes(msg.MemoryUsedBytes))
	}
	if msg.MemoryLimitBytes > 0 {
		fmt.Printf("  Memory Limit:   %s\n", humanBytes(msg.MemoryLimitBytes))
	}
	if msg.DiskUsedBytes > 0 {
		fmt.Printf("  Disk Used:      %s\n", humanBytes(msg.DiskUsedBytes))
	}
	if msg.DiskLimitBytes > 0 {
		fmt.Printf("  Disk Limit:     %s\n", humanBytes(msg.DiskLimitBytes))
	}
	if msg.DockerContainers > 0 {
		fmt.Printf("  Docker Containers: %d\n", msg.DockerContainers)
	}
	if msg.Uptime != "" {
		fmt.Printf("  Uptime:         %s\n", msg.Uptime)
	}
	fmt.Println()

	return nil
}

// humanBytes converts bytes to a human-readable string (KB, MB, GB, TB).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
