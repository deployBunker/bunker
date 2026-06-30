package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewSpawnCommand returns the `bunker spawn` cobra command.
func NewSpawnCommand() *cobra.Command {
	var (
		serverName    string
		agentID       string
		cpuQuota      float64
		memoryMax     uint64
		diskMax       uint64
		ttl           string
		networkMode   string // cloudflare, tailscale, direct
		trycloudflare bool
		domain        string
	)

	cmd := &cobra.Command{
		Use:   "spawn",
		Short: "Create a new agent",
		Long: `Create a new agent on the active bunkerd server and return
a connection bundle with SSH keys, Docker host, and networking details.

Examples:
  bunker spawn
  bunker spawn --cpu 2.0 --memory 4294967296
  bunker spawn --network cloudflare --trycloudflare
  bunker spawn --server staging --ttl 24h`,
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

			// 3. Build request
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.SpawnAgentRequest{
				AgentId: agentID,
				Ttl:     ttl,
			})

			// Limits
			if cpuQuota > 0 || memoryMax > 0 || diskMax > 0 {
				req.Msg.Limits = &v1.ResourceLimits{
					CpuQuota:       cpuQuota,
					MemoryMaxBytes: memoryMax,
					DiskMaxBytes:   diskMax,
				}
			}

			// Network
			if networkMode != "" || trycloudflare || domain != "" {
				req.Msg.Network = &v1.NetworkConfig{}
				switch networkMode {
				case "cloudflare":
					req.Msg.Network.Mode = v1.NetworkConfig_MODE_CLOUDFLARE_TUNNEL
				case "tailscale":
					req.Msg.Network.Mode = v1.NetworkConfig_MODE_TAILSCALE
				case "direct":
					req.Msg.Network.Mode = v1.NetworkConfig_MODE_DIRECT
				}
				req.Msg.Network.Trycloudflare = trycloudflare
				req.Msg.Network.Domain = domain
			}

			// Auth token
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			// 4. Call RPC
			resp, err := client.SpawnAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("spawn agent: %w", err)
			}

			// 5. Print connection bundle
			r := resp.Msg
			fmt.Println("Agent created:", r.AgentId)
			fmt.Println()
			fmt.Println("══════════ Connection Bundle ══════════")
			fmt.Println()
			if r.DockerHostSsh != "" {
				fmt.Printf("  Docker SSH:   %s\n", r.DockerHostSsh)
			}
			if r.SshPrivateKey != "" {
				fmt.Println("  SSH Key:      (saved to ~/.bunker/keys/)")
				// Save private key
				keyDir, _ := configFilePath()
				keyDir = filepath.Join(filepath.Dir(keyDir), "keys")
				os.MkdirAll(keyDir, 0700)
				keyPath := filepath.Join(keyDir, r.AgentId)
				os.WriteFile(keyPath, []byte(r.SshPrivateKey), 0600)
				fmt.Printf("                %s\n", keyPath)
			}
			if r.PublicUrl != "" {
				fmt.Printf("  Public URL:   %s\n", r.PublicUrl)
			}
			if r.TailnetIp != "" {
				fmt.Printf("  Tailnet IP:   %s\n", r.TailnetIp)
			}
			if r.PortRangeStart > 0 {
				fmt.Printf("  Port Range:   %d-%d\n", r.PortRangeStart, r.PortRangeEnd)
			}
			if r.ExpiresAt != "" {
				fmt.Printf("  Expires:      %s\n", r.ExpiresAt)
			}
			if r.ApiKey != "" {
				fmt.Printf("  API Key:      %s\n", r.ApiKey)
			}
			fmt.Println()
			fmt.Println("═ Use `bunker exec` to run commands in this agent ═")

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent ID (auto-generated if empty)")
	cmd.Flags().Float64Var(&cpuQuota, "cpu", 0, "CPU quota in cores (e.g. 2.0)")
	cmd.Flags().Uint64Var(&memoryMax, "memory", 0, "Memory limit in bytes")
	cmd.Flags().Uint64Var(&diskMax, "disk", 0, "Disk limit in bytes")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Time-to-live (6h, 24h, 7d)")
	cmd.Flags().StringVar(&networkMode, "network", "", "Network mode: cloudflare, tailscale, direct")
	cmd.Flags().BoolVar(&trycloudflare, "trycloudflare", false, "Use anonymous TryCloudflare tunnel")
	cmd.Flags().StringVar(&domain, "domain", "", "Custom domain for Cloudflare tunnel")

	return cmd
}
