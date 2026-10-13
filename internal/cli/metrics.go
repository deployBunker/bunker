package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

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
			// Read-only convenience default is allowed, but the resolved
			// target is always printed (GAP-093): a read is never mistaken
			// for a read of a different server.
			serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// Auth token
			token := resolveToken(entry)

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
		// DF-BUNKER-54: DiskLimitBytes is the agent's PER-FILE size cap
		// (LimitFSIZE/RLIMIT_FSIZE), not a total-disk quota — labelled as
		// such so no surface presents it as an enforced disk limit.
		fmt.Printf("  %s:   %s %s\n", maxFileSizeLabel, humanBytes(msg.DiskLimitBytes), perFileCapQualifier)
	}
	if msg.DockerContainers > 0 {
		fmt.Printf("  Docker Containers: %d\n", msg.DockerContainers)
	}
	if msg.Uptime != "" {
		fmt.Printf("  Uptime:         %s\n", msg.Uptime)
	}
	if msg.HostLevelFallback {
		fmt.Println("  NOTE: host-level fallback (agent cgroup unavailable — metrics are HOST values, not agent values)")
	}
	// GAP-121: one-liner pointer to the ceiling events, so the operator sees
	// the hit while reading usage and knows where the detail lives. Absent
	// (nil) on a pre-GAP-121 daemon, and unmeasured vs clean is preserved.
	if ev := msg.GetResourceEvents(); ev != nil {
		fmt.Printf("  Ceiling events: %s\n", resourceEventsBrief(ev))
	}
	fmt.Println()

	return nil
}

// resourceEventsBrief is the compact ceiling-event verdict used by
// `bunker metrics <agent-id>`; the full breakdown (and the peak sources)
// live in `bunker resource-events <agent-id>`.
func resourceEventsBrief(ev *v1.ResourceEvents) string {
	if !ev.GetAvailable() {
		return "UNAVAILABLE — not measured (see 'bunker resource-events <agent-id>')"
	}
	var parts []string
	if n := ev.GetMemory().GetMax(); n > 0 {
		parts = append(parts, fmt.Sprintf("memory.max ×%d", n))
	}
	if n := ev.GetMemory().GetOomKill(); n > 0 {
		parts = append(parts, fmt.Sprintf("oom_kill ×%d", n))
	}
	if n := ev.GetPids().GetMax(); n > 0 {
		parts = append(parts, fmt.Sprintf("pids.max ×%d", n))
	}
	if len(parts) == 0 {
		return "none recorded"
	}
	return strings.Join(parts, ", ")
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
