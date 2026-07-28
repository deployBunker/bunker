package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewStatusCommand returns the `bunker status` cobra command.
// Without flags it shows the active server. With --all it iterates
// every registered server and prints an aggregated health overview.
func NewStatusCommand() *cobra.Command {
	var (
		serverName string
		allServers bool
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status of bunkerd servers",
		Long: `Display status and resource metrics for bunkerd servers.

Without flags, shows the active server. Use --all to query every registered
server and produce an aggregated cross-host overview.

Examples:
  bunker status
  bunker status --server staging
  bunker status --all
  bunker status --all-servers`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// --all-servers is an alias for --all.
			if cmd.Flags().Changed("all-servers") {
				allServers = true
			}

			if allServers {
				return printAllServerStatus(cfg)
			}

			// Single-server mode.
			if serverName == "" {
				serverName = cfg.ActiveServer
			}

			// No servers configured at all.
			if len(cfg.Servers) == 0 {
				fmt.Println("No servers configured.")
				return nil
			}

			if serverName == "" {
				return fmt.Errorf("no active server; run 'bunker connect' first")
			}

			entry, ok := cfg.Servers[serverName]
			if !ok {
				return fmt.Errorf("server %q not found in config", serverName)
			}

			result := queryServer(entry)
			fmt.Print(formatServerStatus(result))
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().BoolVar(&allServers, "all", false, "Query all registered servers")
	cmd.Flags().Bool("all-servers", false, "Alias for --all")

	return cmd
}

// serverStatus holds the collected data for a single server query.
type serverStatus struct {
	entry   ServerEntry
	info    *v1.ServerInfoResponse
	metrics *v1.ServerMetricsResponse
	err     error
}

// queryServer contacts a single bunkerd server and collects its info and
// metrics. If ServerInfo fails the server is considered offline. If
// ServerMetrics fails (e.g. not implemented on older servers) the metrics
// fields are left nil and shown as "N/A".
func queryServer(entry ServerEntry) serverStatus {
	st := serverStatus{entry: entry}

	client := newBunkerdClient(entry)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token := resolveToken(entry)

	// 1. ServerInfo — required for "online" status.
	infoReq := connect.NewRequest(&v1.ServerInfoRequest{})
	if token != "" {
		infoReq.Header().Set("Authorization", "Bearer "+token)
	}
	infoResp, err := client.ServerInfo(ctx, infoReq)
	if err != nil {
		st.err = fmt.Errorf("server info: %w", err)
		return st
	}
	st.info = infoResp.Msg

	// 2. ServerMetrics — best-effort; failure is non-fatal.
	metricsReq := connect.NewRequest(&v1.ServerMetricsRequest{})
	if token != "" {
		metricsReq.Header().Set("Authorization", "Bearer "+token)
	}
	metricsResp, err := client.ServerMetrics(ctx, metricsReq)
	if err == nil {
		st.metrics = metricsResp.Msg
	}
	// On error, st.metrics stays nil → formatted as "N/A".

	return st
}

// printAllServerStatus queries every server in the config and prints
// a per-server status overview.
func printAllServerStatus(cfg *CLIConfig) error {
	if len(cfg.Servers) == 0 {
		fmt.Println("No servers configured.")
		return nil
	}

	// Collect sorted server names for deterministic output.
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Println()
	fmt.Printf("══════════ Bunker Server Status (%d servers) ══════════\n", len(names))
	fmt.Println()

	for i, name := range names {
		entry := cfg.Servers[name]
		result := queryServer(entry)
		fmt.Print(formatServerStatus(result))
		if i < len(names)-1 {
			fmt.Println()
		}
	}

	return nil
}

// formatServerStatus renders a single serverStatus as a human-readable section.
func formatServerStatus(st serverStatus) string {
	var b strings.Builder
	name := st.entry.Name
	if name == "" {
		name = st.entry.URL
	}

	// OFFLINE case.
	if st.err != nil {
		b.WriteString(fmt.Sprintf("── %s ──\n", name))
		b.WriteString(fmt.Sprintf("  URL:      %s\n", st.entry.URL))
		b.WriteString(fmt.Sprintf("  Status:   OFFLINE\n"))
		b.WriteString(fmt.Sprintf("  Error:    %v\n", st.err))
		return b.String()
	}

	// ONLINE case.
	info := st.info
	hostname := info.GetHostname()
	if hostname == "" {
		hostname = name
	}

	b.WriteString(fmt.Sprintf("── %s ──\n", name))
	b.WriteString(fmt.Sprintf("  Hostname: %s\n", hostname))
	b.WriteString(fmt.Sprintf("  URL:      %s\n", st.entry.URL))
	b.WriteString(fmt.Sprintf("  Version:  %s\n", info.GetVersion()))
	b.WriteString(fmt.Sprintf("  Status:   ONLINE\n"))
	b.WriteString(fmt.Sprintf("  Uptime:   %s\n", formatUptime(info.GetUptimeSeconds())))
	b.WriteString(fmt.Sprintf("  Agents:   %d/%d\n", info.GetAgentCount(), info.GetMaxAgents()))

	// Metrics (best-effort).
	if st.metrics != nil {
		m := st.metrics
		b.WriteString(fmt.Sprintf("  CPU:      %.1f%%\n", m.GetCpuUsagePercent()))
		b.WriteString(fmt.Sprintf("  Memory:   %s / %s\n", humanBytes(m.GetMemoryUsedBytes()), humanBytes(m.GetMemoryTotalBytes())))
		diskUsed := m.GetDiskUsedBytes()
		diskTotal := m.GetDiskTotalBytes()
		diskPct := diskUsagePercent(diskUsed, diskTotal)
		diskWarn := diskWarning(diskPct)
		b.WriteString(fmt.Sprintf("  Disk:     %s%s\n", diskWarn, formatDisk(diskUsed, diskTotal)))
		if m.GetDockerContainersTotal() > 0 {
			b.WriteString(fmt.Sprintf("  Docker:   %d containers\n", m.GetDockerContainersTotal()))
		}
		// Show agent tunnel URLs if present.
		for _, a := range m.GetAgents() {
			if a.GetPublicUrl() != "" {
				b.WriteString(fmt.Sprintf("  Tunnel:   %s → %s\n", a.GetAgentId(), a.GetPublicUrl()))
			}
		}
	} else {
		b.WriteString("  CPU:      N/A\n")
		b.WriteString("  Memory:   N/A\n")
		b.WriteString("  Disk:     N/A\n")
	}

	return b.String()
}

// formatUptime converts seconds into a human-readable duration string.
func formatUptime(seconds uint64) string {
	if seconds == 0 {
		return "unknown"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	mins := (seconds % 3600) / 60
	secs := seconds % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, mins, secs)
	}
	if mins > 0 {
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}
