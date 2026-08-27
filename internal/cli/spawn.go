package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// agentIDRe matches the CLI-side agent ID rule: lowercase letters, digits,
// and hyphens, 1-64 characters. The server enforces its own (stricter, 1-63)
// rule as a backstop; the CLI validates first so typos fail fast without a
// round-trip (DOGFOOD-008).
var agentIDRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

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
		sshHost       string
	)

	cmd := &cobra.Command{
		Use:   "spawn [agent-id]",
		Short: "Create a new agent",
		Long: `Create a new agent on the active bunkerd server and return
a connection bundle with SSH keys, Docker host, and networking details.

The optional positional [agent-id] is an alias for --agent-id; it must
match [a-z0-9-]{1,64} (lowercase letters, digits, hyphens only).

Examples:
  bunker spawn
  bunker spawn demo-agent --ttl 1h
  bunker spawn --cpu 2.0 --memory 4294967296
  bunker spawn --network cloudflare --trycloudflare
  bunker spawn --server staging --ttl 24h`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 0. Bind + validate the agent ID locally, BEFORE any config
			// load or network I/O: the positional [agent-id] is an alias
			// for --agent-id (DOGFOOD-008). Validation must fire even when
			// no server is configured or reachable.
			if len(args) > 0 {
				if agentID != "" {
					return fmt.Errorf("agent id given both as positional argument %q and --agent-id %q; use one or the other", args[0], agentID)
				}
				agentID = args[0]
			}
			if agentID != "" && !agentIDRe.MatchString(agentID) {
				return fmt.Errorf("invalid agent id %q: must match ^[a-z0-9-]{1,64}$ (lowercase letters, digits, hyphens only, 1-64 characters)", agentID)
			}

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
			// 300s: fresh-agent rootless-docker install downloads ~93MB and takes
			// 60-90s+; the old 30s hardcode killed every spawn mid-install
			// (spawn agent: deadline_exceeded; server rolled back with userdel
			// failure, leaving half-spawned users). Server request_timeout is 300s.
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
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
			// Agent creation can take ~20s server-side (useradd, dockerd start,
			// key write); print a progress line immediately so the user doesn't
			// see a silent wait and Ctrl-C into a half-created agent.
			fmt.Println("Creating agent...")
			resp, err := client.SpawnAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("spawn agent: %w", err)
			}

			// 5. Print connection bundle
			r := resp.Msg

			// Resolve the SSH host shown in the bundle: the client reached
			// bunkerd via the server URL, so its hostname is the address a
			// remote client can actually reach; fall back to the hostname the
			// server embedded in the commands, with --ssh-host as an override.
			serverHost := sshHostFromMount(r.SshfsMount)
			if serverHost == "" {
				if _, h, ok := sshUserHostFromTunnel(r.DockerHostTunnel); ok {
					serverHost = h
				}
			}
			resolvedHost := resolveSSHHost(entry, serverHost, sshHost)

			// Client-local key path (same one saved below); empty when the
			// server returned no private key.
			keyPath := ""
			if r.SshPrivateKey != "" {
				if p, err := defaultSSHKeyPath(r.AgentId); err == nil {
					keyPath = p
				}
			}

			fmt.Println("Agent created:", r.AgentId)
			fmt.Println()
			fmt.Println("══════════ Connection Bundle ══════════")
			fmt.Println()
			if r.DockerHostSsh != "" {
				fmt.Printf("  Docker SSH:   %s\n", rewriteDockerHostSsh(r.DockerHostSsh, serverHost, resolvedHost))
			}
			if r.SshPrivateKey != "" {
				fmt.Println("  SSH Key:      (saved to ~/.bunker/keys/)")
				// Save private key
				keyDir, _ := configFilePath()
				keyDir = filepath.Join(filepath.Dir(keyDir), "keys")
				_ = os.MkdirAll(keyDir, 0700)
				keyPath := filepath.Join(keyDir, r.AgentId)
				_ = os.WriteFile(keyPath, []byte(r.SshPrivateKey), 0600)
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
			if r.SshfsMount != "" {
				fmt.Printf("  SSHFS Mount:  %s\n", rewriteSSHFSMount(r.SshfsMount, serverHost, resolvedHost, keyPath))
			}
			if r.DockerHostTunnel != "" {
				fmt.Printf("  Docker Tunnel: %s\n", rewriteTunnelCommand(r.DockerHostTunnel, serverHost, resolvedHost, keyPath))
			}
			fmt.Println()
			fmt.Println("═ Use `bunker exec` to run commands in this agent ═")

			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent ID (auto-generated if empty; positional [agent-id] is an alias)")
	cmd.Flags().Float64Var(&cpuQuota, "cpu", 0, "CPU quota in cores (e.g. 2.0)")
	cmd.Flags().Uint64Var(&memoryMax, "memory", 0, "Memory limit in bytes")
	cmd.Flags().Uint64Var(&diskMax, "disk", 0, "Disk limit in bytes")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Time-to-live (6h, 24h, 7d)")
	cmd.Flags().StringVar(&networkMode, "network", "", "Network mode: cloudflare, tailscale, direct")
	cmd.Flags().BoolVar(&trycloudflare, "trycloudflare", false, "Use anonymous TryCloudflare tunnel")
	cmd.Flags().StringVar(&domain, "domain", "", "Custom domain for Cloudflare tunnel")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host shown in the bundle (default: hostname from server config URL)")

	return cmd
}
