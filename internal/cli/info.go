package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/deployBunker/bunker/internal/config"
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

			// Rewrite the SSH command strings for display so the host is the one
			// the client can actually reach (server config URL hostname) and the
			// key path is the client-local one. The raw strings stay in the API.
			serverHost := sshHostFromMount(a.SshfsMount)
			if serverHost == "" {
				if _, h, ok := sshUserHostFromTunnel(a.DockerHostTunnel); ok {
					serverHost = h
				}
			}
			resolvedHost := resolveSSHHost(entry, serverHost, "")
			keyPath, _ := defaultSSHKeyPath(a.AgentId)

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
				fmt.Printf("  Docker Tunnel:    %s\n", rewriteTunnelCommand(a.DockerHostTunnel, serverHost, resolvedHost, keyPath))
			}
			if a.SshfsMount != "" {
				fmt.Printf("  SSHFS Mount:      %s\n", rewriteSSHFSMount(a.SshfsMount, serverHost, resolvedHost, keyPath))
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
					// DF-BUNKER-54: DiskMaxBytes arrives on the host as
					// LimitFSIZE (RLIMIT_FSIZE) — a PER-FILE size cap. It is
					// not a total-disk quota; nothing caps the agent's
					// aggregate usage (real quotas are GAP-161).
					fmt.Printf("    %s:   %s %s\n", maxFileSizeLabel, humanBytes(limits.DiskMaxBytes), perFileCapQualifier)
				}
				if limits.MaxDockerContainers > 0 {
					fmt.Printf("    Max Containers: %d\n", limits.MaxDockerContainers)
				}
			}
			// GAP-116 effective safety set: the preset the agent was spawned
			// under and the systemd knob set actually applied. A daemon (or
			// record) predating GAP-116 reports an empty preset — shown as
			// the built-in default name, no knob block (honest absence beats
			// a fabricated set).
			presetName := a.GetSafetyPreset()
			if presetName == "" {
				presetName = config.SafetyPresetDefault
			}
			fmt.Printf("  Safety Preset:    %s\n", presetName)
			if props := a.GetSystemdProperties(); len(props) > 0 {
				fmt.Println("  Safety Knobs:")
				for _, p := range props {
					fmt.Printf("    %-14s  %s\n", p.GetName()+":", p.GetValue())
				}
			}
			// DF-BUNKER-34: the orphan-uid verdict. Non-empty means the
			// agent's user record is GONE from the host while processes
			// still run under its uid — the state that made a destroyed
			// agent look healthy for 20+ hours. Rendered as a warning block,
			// never as a plain status line.
			if orphan := a.GetOrphanUidDetail(); orphan != "" {
				fmt.Println()
				fmt.Println("  ⚠  ORPHANED UID DETECTED:")
				fmt.Printf("    %s\n", orphan)
				fmt.Println("    Stop these processes on the host before destroying or renewing this agent;")
				fmt.Println("    a destroy that proceeds past them orphans them (they hold the home and possibly ports).")
			}
			fmt.Println()

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")

	return cmd
}
