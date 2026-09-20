package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewDeployCommand returns the `bunker deploy` cobra command.
func NewDeployCommand() *cobra.Command {
	var (
		serverName string
		sshPort    uint32
		sshKey     string
		sshHost    string
	)

	cmd := &cobra.Command{
		Use:   "deploy <local-dir> <agent-id>:/path",
		Short: "Deploy a directory to an agent's environment via SCP",
		Long: `Copy an entire local directory recursively into an agent's container using SCP.

The agent connection details (SSH user, host) are resolved from the bunkerd
API. The SSH host defaults to the hostname of the server config URL (the
address the client used to reach bunkerd) and can be overridden with
--ssh-host. The SSH key is read from ~/.bunker/keys/<agent-id> (saved at
spawn time) unless overridden with --ssh-key.

Unlike 'bunker cp', this command uses SCP's recursive mode (-r) to copy the
entire directory tree. After the copy, file ownership is set to the agent user.

Examples:
  bunker deploy ./myapp abc12345:/home/bunker-abc12345/myapp
  bunker deploy ./configs def67890:/etc/app --ssh-port 2222
  bunker deploy . abc12345:/workspace --ssh-host 203.0.113.10`,

		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPath := args[0]
			dest := args[1]

			// Parse destination: <agent-id>:/path
			colonIdx := strings.Index(dest, ":")
			if colonIdx == -1 {
				return fmt.Errorf("destination must be <agent-id>:/path, got %q", dest)
			}
			agentID := dest[:colonIdx]
			remotePath := dest[colonIdx+1:]
			if remotePath == "" {
				return fmt.Errorf("remote path is required in destination %q", dest)
			}
			if agentID == "" {
				return fmt.Errorf("agent ID is required in destination %q", dest)
			}

			// Validate local directory exists
			localInfo, err := os.Stat(localPath)
			if err != nil {
				return fmt.Errorf("local path %q: %w", localPath, err)
			}
			if !localInfo.IsDir() {
				return fmt.Errorf("%q is not a directory — use 'bunker cp' to copy a single file", localPath)
			}

			// Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Determine server
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

			// Call GetAgent to resolve connection details
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

			// The recursive transfer and the chown that follows it are
			// long-lived LOCAL children: they must not outlive this CLI
			// (GAP-084). Same shared contract as `bunker cp` (proc.go).
			childCtx, stopChildren := newChildSignalContext()
			defer stopChildren()

			// Execute recursive SCP. The agent user is resolved BEFORE scp so
			// the failure path can name it in the ownership hint below.
			sshUser := strings.SplitN(userAtHost, "@", 2)[0]
			scpArgs := buildSCPArgs(keyPath, port, localPath, userAtHost, remotePath, true)
			scpCtx, cancelSCP := context.WithTimeout(childCtx, copyChildBudget)
			scpCmd := newLongLivedCommand(scpCtx, "scp", scpArgs...)
			scpCmd.Stdout = cmd.OutOrStdout()
			scpCmd.Stderr = cmd.ErrOrStderr()

			scpErr := runDetachedChildCommand(scpCmd)
			cancelSCP()
			if scpErr != nil {
				if childCtx.Err() != nil {
					// Signalled: the group teardown already reaped the
					// transfer; report the interruption, not "context
					// canceled", and skip the ownership probe.
					return fmt.Errorf("scp -r: interrupted by a shutdown signal — the copy was stopped")
				}
				// scp's own stderr already went to the user (streamed
				// above); it names no next step, so run ONE bounded probe
				// over the same SSH path to explain an existing
				// host-owned destination (the recursive-copy mirror of
				// DF-BUNKER-17). stat works on a directory, so the
				// destination itself is what gets probed. Best-effort: a
				// probe error/timeout only means "no hint". The raw scp
				// error text is kept either way.
				if hint := cpDestinationHint(keyPath, port, userAtHost, remotePath, sshUser); hint != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "Hint: %s\n", hint)
				}
				return fmt.Errorf("scp -r: %w", scpErr)
			}

			// Fix ownership: chown the directory recursively to the agent user
			chownCtx, cancelChown := context.WithTimeout(childCtx, copyChildBudget)
			chownCmd := newLongLivedCommand(chownCtx, "ssh",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "LogLevel=ERROR",
				"-o", "ConnectTimeout=10",
				"-o", "IdentitiesOnly=yes",
				"-i", keyPath,
				"-p", fmt.Sprintf("%d", port),
				userAtHost,
				fmt.Sprintf("chown -R %s:%s %s", sshUser, sshUser, remotePath),
			)
			chownCmd.Stdout = cmd.OutOrStdout()
			chownCmd.Stderr = cmd.ErrOrStderr()

			chownErr := runDetachedChildCommand(chownCmd)
			cancelChown()
			if chownErr != nil {
				if childCtx.Err() != nil {
					return fmt.Errorf("chown after scp: interrupted by a shutdown signal")
				}
				return fmt.Errorf("chown after scp: %w", chownErr)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Deployed %s to %s:%s\n", localPath, agentID, remotePath)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().Uint32Var(&sshPort, "ssh-port", 0, "SSH port (default: 22)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")

	return cmd
}
