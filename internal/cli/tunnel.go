package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// execCommandContext is a package-level hook for testing the tunnel command
// without requiring a real ssh binary.
var execCommandContext = exec.CommandContext

// NewTunnelCommand returns the `bunker tunnel` cobra command.
func NewTunnelCommand() *cobra.Command {
	var (
		serverName string
		sshHost    string
		sshKey     string
	)

	cmd := &cobra.Command{
		Use:   "tunnel <agent-id> [local-port]",
		Short: "Open an SSH tunnel to an agent's Docker socket",
		Long: `Open an SSH tunnel that forwards a local TCP port to the agent's remote Docker socket.

The SSH host defaults to the hostname of the server config URL (the address
the client used to reach bunkerd) and can be overridden with --ssh-host. The
SSH key is read from ~/.bunker/keys/<agent-id> (saved at spawn time) unless
overridden with --ssh-key.

Examples:
  bunker tunnel abc12345
  bunker tunnel abc12345 2377
  bunker tunnel abc12345 --ssh-host 203.0.113.10

The tunnel runs in the foreground until interrupted. While it is running, the
agent's Docker socket is available on the local port:

  DOCKER_HOST=tcp://localhost:2376 docker version`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			var localPort uint32
			if len(args) > 1 {
				p, err := strconv.ParseUint(args[1], 10, 32)
				if err != nil {
					return fmt.Errorf("invalid local port %q: %w", args[1], err)
				}
				localPort = uint32(p)
			}

			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
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

			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			info, err := client.GetAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("get agent %s: %w", agentID, err)
			}
			tunnelCmd := info.Msg.GetAgent().GetDockerHostTunnel()
			if tunnelCmd == "" {
				return fmt.Errorf("agent %s has no docker host tunnel command", agentID)
			}

			// Resolve the SSH login target (user@host) embedded in the tunnel
			// command. The server bakes its self-reported hostname in here;
			// remote clients must use the host from their server config URL
			// instead (overridable with --ssh-host).
			_, serverHost, ok := sshUserHostFromTunnel(tunnelCmd)
			if !ok {
				return fmt.Errorf("cannot parse ssh target from tunnel command: %s", tunnelCmd)
			}
			resolvedHost := resolveSSHHost(entry, serverHost, sshHost)

			// Resolve the client-local SSH key path. The server-side key path
			// embedded in the tunnel command (/etc/bunkerd/ssh/<id>) is not
			// readable from a remote client.
			keyPath := sshKey
			if keyPath == "" {
				keyPath, err = defaultSSHKeyPath(agentID)
				if err != nil {
					return fmt.Errorf("resolve SSH key: %w", err)
				}
			}
			if _, statErr := os.Stat(keyPath); statErr != nil {
				return fmt.Errorf("SSH key not found at %q — spawn the agent first or use --ssh-key", keyPath)
			}

			// Rewrite the stored command: client-local key path + resolved
			// host. The -L forward spec (server-side socket path) is kept.
			parts := clientTunnelArgs(tunnelCmd, resolvedHost, keyPath)
			if len(parts) < 2 {
				return fmt.Errorf("invalid tunnel command: %s", tunnelCmd)
			}

			// If the user requested a specific local port, rewrite the -L spec.
			if localPort > 0 && len(args) > 1 {
				for i := 0; i < len(parts); i++ {
					if parts[i] == "-L" && i+1 < len(parts) {
						spec := parts[i+1]
						colonIdx := strings.Index(spec, ":")
						if colonIdx != -1 {
							parts[i+1] = fmt.Sprintf("%d:%s", localPort, spec[colonIdx+1:])
						}
						break
					}
				}
			}

			// Prepend SSH options that are not encoded in the stored command so
			// the connection succeeds without host-key prompts and stays quiet.
			seenOpts := make(map[string]bool)
			for i := 0; i < len(parts); i++ {
				if parts[i] == "-o" && i+1 < len(parts) {
					seenOpts[parts[i+1]] = true
					i++
				}
			}
			extraOpts := []string{}
			for _, opt := range []string{"UserKnownHostsFile=/dev/null", "LogLevel=ERROR"} {
				if !seenOpts[opt] {
					extraOpts = append(extraOpts, "-o", opt)
				}
			}
			// Prepend extras right after the `ssh` binary so the command stays valid.
			cmdArgs := append([]string{}, parts[1:]...)
			cmdArgs = append(extraOpts, cmdArgs...)

			if localPort == 0 {
				localPort = 2376
			}
			fmt.Fprintf(os.Stderr, "Opening tunnel to %s on local port %d. Press Ctrl-C to stop.\n", agentID, localPort)

			c := execCommandContext(ctx, parts[0], cmdArgs...)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	return cmd
}
