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

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/config"
	"github.com/deployBunker/bunker/internal/imagespec"
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
		imageSpecFile string
		preset        string
		mountDriver   string
	)

	cmd := &cobra.Command{
		Use:   "spawn [agent-id]",
		Short: "Create a new agent",
		Long: `Create a new agent on the active bunkerd server and return
a connection bundle with SSH keys, Docker host, and networking details.

The optional positional [agent-id] is an alias for --agent-id; it must
match [a-z0-9-]{1,64} (lowercase letters, digits, hyphens only).

The bundle's Expires timestamp is printed in the daemon host's local
timezone (with offset, e.g. -05:00), while TTL durations (--ttl) are
computed in UTC — a correct 6h TTL shows as a local-time timestamp
exactly 6h ahead of the daemon host's current local time, not a UTC
clock reading.

--ttl is validated locally, before the RPC: an invalid value (e.g. 6x)
fails fast with the accepted format and no "Creating agent..." progress
line. Accepted format is <digits><unit>, unit h, m, or d: 6h, 90m, 7d.

--disk is a PER-FILE size cap, not a total-disk quota: it is applied on
the host as systemd LimitFSIZE (RLIMIT_FSIZE), which bounds the size of
any single file the agent writes. Nothing caps the agent's total disk
usage (per-user filesystem quotas are a separate, unimplemented item,
GAP-161), and a finite value makes .NET apps that ftruncate a large
sparse file at first boot crash-loop — see internal/agent/SKILL.md.

Examples:
  bunker spawn
  bunker spawn demo-agent --ttl 1h
  bunker spawn --cpu 2.0 --memory 4294967296
  bunker spawn --network cloudflare --trycloudflare
  bunker spawn --server staging --ttl 24h
  bunker spawn --image-spec spec.json`,
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

			// 0.5 Load + validate the image spec LOCALLY so bad specs fail
			// fast without a round-trip (GAP-064). The file is JSON:
			// {"base": "...", "packages": [{"manager": "apt|go|npm", "packages": [...]}]}.
			var imageSpecPB *v1.ImageSpec
			if imageSpecFile != "" {
				raw, err := os.ReadFile(imageSpecFile)
				if err != nil {
					return fmt.Errorf("read image spec: %w", err)
				}
				spec, err := imagespec.Parse(raw)
				if err != nil {
					return fmt.Errorf("invalid image spec %s: %w", imageSpecFile, err)
				}
				imageSpecPB = spec.ToProto()
			}

			// 0.6 Validate --ttl LOCALLY, before the progress line and
			// before the RPC, reusing the daemon's OWN parser
			// (agent.ParseAgentTTL) so the accepted set can never drift
			// from what bunkerd accepts (DF-BUNKER-17). Empty keeps
			// today's behavior byte-for-byte: the daemon applies its
			// default TTL. Without this, a typo cost a "Creating agent..."
			// progress line and a round trip inside the 300s RPC.
			if ttl != "" {
				if _, err := agent.ParseAgentTTL(ttl); err != nil {
					return fmt.Errorf("invalid --ttl %q: %w (accepted format: <digits><unit> where unit is h, m, or d — e.g. 6h, 90m, 7d)", ttl, err)
				}
			}

			// 0.7 Validate --preset LOCALLY, before the progress line and
			// the RPC (GAP-116): an unknown preset name fails fast with the
			// accepted vocabulary, exactly like --ttl fails fast with the
			// duration format. Empty defers to BUNKERD_SAFETY_PRESET, then
			// the daemon's config global, then the built-in default — the
			// daemon re-validates the resolved value regardless.
			if preset != "" && !config.ValidSafetyPreset(preset) {
				return fmt.Errorf("invalid --preset %q (valid: %v)", preset, config.ValidSafetyPresets())
			}

			// 1. Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// 2. Determine server
			// Fail-closed binding (GAP-093): mutating commands never fall
			// back to the shared active_server default.
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			serverName = resolved

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
				AgentId:      agentID,
				Ttl:          ttl,
				ImageSpec:    imageSpecPB,
				SafetyPreset: preset,
				// MOUNT-006: the requested mount driver. Empty = the
				// server's sshfs default; an unknown name is refused by
				// the server (CodeInvalidArgument) — never a silent
				// fallback to sshfs.
				MountDriver: mountDriver,
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

			// GAP-128: the spawn response carries NO private key by default
			// (key material must be explicitly requested on the wire). The CLI
			// fetches it through the master-credential-gated GetAgentKey RPC
			// instead, so operators keep a working local key copy without the
			// spawn body ever carrying the secret. Failure is non-fatal: the
			// agent exists and is usable, so we warn and continue. The
			// fetch+save is shared with renew's re-spawn leg (DF-BUNKER-65);
			// this leg only owns the printing.
			keyPath := ""
			savedKeyPath := ""
			if r.SshPrivateKey == "" {
				if p, ferr := fetchAndSaveAgentKey(ctx, client, r.AgentId, token); ferr != nil {
					fmt.Printf("  (warn: could not fetch SSH key: %v)\n", ferr)
				} else if p != "" {
					r.SshPrivateKey = "fetched" // non-empty: the bundle prints the key line
					savedKeyPath = p
				}
			}

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

			// Client-local key path (same one saved by the GetAgentKey fetch
			// above); empty when the server returned no private key and the
			// fetch produced none.
			if keyPath == "" && r.SshPrivateKey != "" {
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
				// The key was saved by fetchAndSaveAgentKey (GetAgentKey path)
				// or arrived inline in the spawn response; save the inline case.
				if savedKeyPath == "" {
					keyDir, _ := configFilePath()
					keyDir = filepath.Join(filepath.Dir(keyDir), "keys")
					_ = os.MkdirAll(keyDir, 0700)
					keyPath = filepath.Join(keyDir, r.AgentId)
					_ = os.WriteFile(keyPath, []byte(r.SshPrivateKey), 0600)
				} else {
					keyPath = savedKeyPath
				}
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
			if r.Image != "" {
				fmt.Printf("  Image:        %s\n", r.Image)
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
	cmd.Flags().Uint64Var(&diskMax, "disk", 0, "Per-file size cap in bytes (LimitFSIZE/RLIMIT_FSIZE — not a total-disk quota; 0 = no cap)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Time-to-live (6h, 24h, 7d)")
	cmd.Flags().StringVar(&networkMode, "network", "", "Network mode: cloudflare, tailscale, direct")
	cmd.Flags().BoolVar(&trycloudflare, "trycloudflare", false, "Use anonymous TryCloudflare tunnel")
	cmd.Flags().StringVar(&domain, "domain", "", "Custom domain for Cloudflare tunnel")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host shown in the bundle (default: hostname from server config URL)")
	cmd.Flags().StringVar(&imageSpecFile, "image-spec", "", "JSON file with an image customization spec (base + apt/go/npm package adds)")
	cmd.Flags().StringVar(&preset, "preset", "", "Safety preset for this agent: open, standard, hardened (default: BUNKERD_SAFETY_PRESET, then the server's config, then open)")
	cmd.Flags().StringVar(&mountDriver, "mount-driver", "", "Mount driver for this agent (default: sshfs; an unknown name is refused by the server)")

	return cmd
}
