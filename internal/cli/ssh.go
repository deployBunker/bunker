package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewSSHCommand returns the `bunker ssh` cobra command.
func NewSSHCommand() *cobra.Command {
	var (
		serverName string
		sshPort    uint32
		sshKey     string
		sshHost    string
	)

	cmd := &cobra.Command{
		Use:   "ssh <agent-id> [command...]",
		Short: "Open an interactive SSH session into an agent",
		Long: `Open an interactive SSH session into an agent's host environment.

The agent connection details (SSH user, host) are resolved from the bunkerd
API. The SSH host defaults to the hostname of the server config URL (the
address the client used to reach bunkerd) and can be overridden with
--ssh-host. The SSH key is read from ~/.bunker/keys/<agent-id> (saved at
spawn time) unless overridden with --ssh-key.

Without a command argument an interactive shell session is opened. Any extra
arguments after the agent ID are executed as a remote command and the session
exits when it finishes.

Examples:
  bunker ssh abc12345
  bunker ssh abc12345 -- docker ps
  bunker ssh abc12345 --ssh-port 2222
  bunker ssh abc12345 --ssh-key ~/.ssh/custom_key
  bunker ssh abc12345 --ssh-host 203.0.113.10 -- hostname`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			remoteCmd := args[1:]

			// Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Determine server
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

			// Call GetAgent to resolve connection details. The timeout is
			// scoped to the API call only — the interactive session below
			// must not inherit it.
			client := newBunkerdClient(entry)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}

			resp, err := client.GetAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("get agent info: %w", err)
			}

			agent := resp.Msg.GetAgent()
			if agent == nil {
				return fmt.Errorf("agent %q not found", agentID)
			}

			// Resolve SSH user and host from the sshfs_mount command.
			userAtHost, err := resolveUserAtHost(entry, agent.GetSshfsMount(), sshHost)
			if err != nil {
				return fmt.Errorf("resolve agent host: %w", err)
			}

			// Resolve SSH key path
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

			// Determine SSH port
			port := uint32(22)
			if sshPort > 0 {
				port = sshPort
			}

			// Execute ssh with the session streams attached. No context
			// deadline: an interactive session must live until the user
			// exits (Ctrl+C on the terminal reaches the ssh child directly).
			sshCmd := exec.Command("ssh", buildSSHArgs(keyPath, port, userAtHost, remoteCmd)...)
			sshCmd.Stdin = cmd.InOrStdin()
			sshCmd.Stdout = cmd.OutOrStdout()
			sshCmd.Stderr = cmd.ErrOrStderr()

			if err := sshCmd.Run(); err != nil {
				return fmt.Errorf("ssh: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&sshPort, "ssh-port", 0, "SSH port (default: 22)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")

	return cmd
}

// buildSSHArgs builds the ssh argument list for an agent session. The options
// mirror `bunker cp`: the server-local key path is replaced with the
// client-local one, IdentitiesOnly=yes is forced (a loaded ssh-agent would
// otherwise offer every key first and the server's MaxAuthTries would kill
// the connection before the correct -i key is tried), and any remote command
// arguments are appended verbatim.
func buildSSHArgs(keyPath string, port uint32, userAtHost string, remoteCmd []string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", port),
		userAtHost,
	}
	args = append(args, remoteCmd...)
	return args
}
