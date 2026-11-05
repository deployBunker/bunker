package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
		serverName         string
		agentID            string
		prometheusTextfile string
		metricsURL         string
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
			if prometheusTextfile != "" {
				if err := writePrometheusTextfile(prometheusTextfile, metricsURL); err != nil {
					cmd.SilenceUsage = true
					fmt.Fprintf(cmd.ErrOrStderr(), "bunker metrics textfile: %v\n", err)
					return fmt.Errorf("write Prometheus textfile %q: %w", prometheusTextfile, err)
				}
				return nil
			}

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
	cmd.Flags().StringVar(&prometheusTextfile, "prometheus-textfile", "", "Write a Prometheus scrape to a textfile atomically")
	cmd.Flags().StringVar(&metricsURL, "metrics-url", "http://127.0.0.1:8080/metrics", "Prometheus metrics scrape URL")

	return cmd
}

func writePrometheusTextfile(path, metricsURL string) error {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(metricsURL)
	if err != nil {
		return fmt.Errorf("scrape %q: %w", metricsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scrape %q: unexpected HTTP status %s", metricsURL, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read scrape response from %q: %w", metricsURL, err)
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close scrape response from %q: %w", metricsURL, err)
	}
	bodyText := strings.TrimRight(string(body), "\n") + "\n"
	bodyText += "# HELP bunker_metrics_last_success_timestamp_seconds Unix time of the last successful bunkerd /metrics scrape by the Bunker textfile shim.\n"
	bodyText += "# TYPE bunker_metrics_last_success_timestamp_seconds gauge\n"
	bodyText += fmt.Sprintf("bunker_metrics_last_success_timestamp_seconds %d\n", time.Now().Unix())

	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".bunkerd-metrics-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary textfile beside %q: %w", path, err)
	}
	tempName := temp.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tempName)
		}
	}()

	if _, err := io.WriteString(temp, bodyText); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary textfile for %q: %w", path, err)
	}
	if err := temp.Chmod(0644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temporary textfile for %q: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary textfile for %q: %w", path, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("publish textfile %q: %w", path, err)
	}
	published = true
	return nil
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
