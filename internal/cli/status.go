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
				// Read-only convenience default, but the resolved target is
				// always printed (GAP-093) so a read is never mistaken for a
				// read of a different server.
				serverName = ReadOnlyTarget(serverName, cfg.ActiveServer)
				if serverName != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "bunker: reading server %q\n", serverName)
				}
			}

			// No servers configured at all — fail loudly (non-zero exit) so
			// scripts/CI don't treat failure as success (GAP-037). Matches
			// list/spawn/info, which already exit 1 in the same state.
			if len(cfg.Servers) == 0 {
				return fmt.Errorf("no servers configured; run 'bunker connect' first")
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
		return fmt.Errorf("no servers configured; run 'bunker connect' first")
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
	b.WriteString(formatTmpIsolation(info.GetTmpIsolation(), info.GetTmpIsolationDetail()))
	b.WriteString(formatResidue(info.GetResidue()))

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
		// Show a prominent WARNING banner when disk exceeds 90%.
		if diskAlert(diskPct) {
			b.WriteString(fmt.Sprintf("\n  ╔══════════════════════════════════════════════╗\n"))
			b.WriteString(fmt.Sprintf("  ║  ⚠  WARNING: Disk usage at %.1f%% — critically high.  ║\n", diskPct))
			b.WriteString(fmt.Sprintf("  ║  Agent spawns may fail due to disk space.          ║\n"))
			b.WriteString(fmt.Sprintf("  ╚══════════════════════════════════════════════╝\n\n"))
		}
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

// formatTmpIsolation renders the /tmp isolation line of the ONLINE status
// section (DF-BUNKER-9). The daemon reports the /tmp policy it actually
// enforces; a CLI must never let the README's private-/tmp promise stand
// unverified:
//
//	private     -> "  /tmp:     private (per-session pam_namespace instance)"
//	host-shared -> the line plus a prominent WARNING banner mirroring the
//	               disk-warning banner, because the README promise does NOT
//	               hold on this host
//	unknown     -> "  /tmp:     unknown — <detail>"
//	(empty)     -> "  /tmp:     not reported by this daemon — …" (a daemon
//	               that predates capability reporting, e.g. any tagged
//	               v0.1.x build)
func formatTmpIsolation(level, detail string) string {
	switch level {
	case "private":
		return "  /tmp:     private (per-session pam_namespace instance)\n"
	case "host-shared":
		var b strings.Builder
		b.WriteString("  /tmp:     HOST-SHARED — agent sessions see the host /tmp\n")
		b.WriteString("\n")
		b.WriteString("  ╔══════════════════════════════════════════════════════════╗\n")
		b.WriteString("  ║  ⚠  WARNING: Private /tmp is NOT active on this host.           ║\n")
		b.WriteString("  ║  Agent exec sessions share the host /tmp; the README's        ║\n")
		b.WriteString("  ║  'Private /tmp per agent' promise does not hold here.         ║\n")
		b.WriteString("  ╚══════════════════════════════════════════════════════════╝\n")
		if detail != "" {
			b.WriteString(fmt.Sprintf("  Reason:   %s\n", detail))
		}
		return b.String()
	case "unknown":
		if detail != "" {
			return fmt.Sprintf("  /tmp:     unknown — %s\n", detail)
		}
		return "  /tmp:     unknown\n"
	default:
		// Empty (or any unrecognized value): the daemon predates capability
		// reporting — e.g. any tagged v0.1.x release built before the
		// isolation feature (GAP-075) landed. Nothing here proves /tmp is
		// private, so the CLI says so instead of staying silent.
		return "  /tmp:     not reported by this daemon — it predates capability reporting; build/run a daemon from the same commit as the CLI (private /tmp is not guaranteed)\n"
	}
}

// formatResidue renders the DF-BUNKER-21 residue inventory reported by
// ServerInfo. The counts are what an operator needs after a failed spawn: the
// QA host held 11 orphan bunker-* users, ~2.5GB of homes and fresh linger
// entries with 0 registered agents, and no status surface said so.
//
// Three states, mirroring formatTmpIsolation's honesty contract:
//
//	ok        -> the four counts, one line
//	partial / unavailable -> the counts PLUS the probe status and the exact
//	             planes that could not be read (a "0" that came from an
//	             unreadable directory must never read as "host is clean")
//	(absent)  -> the daemon predates residue reporting; say so instead of
//	             printing zeroes the daemon never probed
func formatResidue(inv *v1.ResidueInventory) string {
	if inv == nil {
		return "  Residue:  not reported by this daemon — it predates residue inventory reporting (DF-BUNKER-21); agent users/homes/keys/linger entries left behind on this host are NOT visible here\n"
	}

	var b strings.Builder
	b.WriteString("  Residue:  " + residueCount(inv.GetOrphanUsers(), "orphan user", "orphan users") +
		", " + residueCount(inv.GetOrphanHomes(), "orphan home", "orphan homes") +
		", " + residueCount(inv.GetOrphanKeys(), "orphan key", "orphan keys") +
		", " + residueCount(inv.GetStaleLingerEntries(), "stale linger entry", "stale linger entries") +
		" (" + residueCount(inv.GetRegisteredAgents(), "registered agent", "registered agents") + ")\n")

	switch inv.GetStatus() {
	case "ok":
		// every plane probed: nothing to add
	case "", "unknown":
		// A daemon that reports the counts but no status cannot be trusted to
		// have probed every plane; say so rather than implying "ok".
		b.WriteString("  Probe:    status not reported by this daemon — the counts above may be a lower bound\n")
	default:
		b.WriteString(fmt.Sprintf("  Probe:    %s — the counts above are a LOWER BOUND (not every plane could be read)\n", inv.GetStatus()))
		if detail := inv.GetDetail(); detail != "" {
			b.WriteString(fmt.Sprintf("            %s\n", detail))
		}
	}

	if inv.GetOrphanUsers()+inv.GetOrphanHomes()+inv.GetOrphanKeys()+inv.GetStaleLingerEntries() > 0 {
		b.WriteString("            residue present: this host holds agent users/homes/keys/linger entries with no registered agent behind them\n")
	}
	return b.String()
}

// residueCount renders "N <noun>", pluralised when N != 1, so the status line
// never reads "1 orphan users".
func residueCount(n uint32, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
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
