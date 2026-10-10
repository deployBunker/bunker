package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewPullCommand returns the `bunker pull` cobra command: the copy-FROM-agent
// direction of `bunker cp` (which pushes a local file INTO an agent).
func NewPullCommand() *cobra.Command {
	var (
		serverName string
		sshPort    uint32
		sshKey     string
		sshHost    string
		recursive  bool
	)

	cmd := &cobra.Command{
		Use:   "pull <agent-id> <remote-path> [local-dir]",
		Short: "Copy a file from an agent's environment via SCP",
		Long: `Copy a file from an agent's container to the local machine using SCP.

The agent connection details (SSH user, host) are resolved from the bunkerd
API, the same way 'bunker cp' resolves them. The SSH host defaults to the
hostname of the server config URL (the address the client used to reach
bunkerd) and can be overridden with --ssh-host. The SSH key is read from
~/.bunker/keys/<agent-id> (saved at spawn time) unless overridden with
--ssh-key.

If [local-dir] is omitted, the file is copied into the current working
directory (mirroring scp's default destination behavior). Use --recursive
(-r) to copy a remote directory tree.

A [local-dir] that does not exist is created as a directory so the file is
pulled INTO it. A [local-dir] that exists as a FILE is refused instead of
overwritten: scp would write onto that path (silently clobbering it) and
still report success, so pass a directory or remove the file first.

Examples:
  bunker pull abc12345 /home/bunker-abc12345/config.yaml
  bunker pull abc12345 /home/bunker-abc12345/logs ./out
  bunker pull abc12345 /var/log/app --recursive --ssh-port 2222
  bunker pull abc12345 /app/.env ./secrets --ssh-key ~/.ssh/custom_key`,

		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			remotePath := args[1]

			localDir := ""
			if len(args) == 3 {
				localDir = args[2]
			} else {
				wd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("resolve current directory: %w", err)
				}
				localDir = wd
			}

			// scp destination semantics are the hazard (DF-BUNKER-83):
			// an existing FILE at the destination is written ONTO (the
			// transfer still succeeds, so the success message masks the
			// loss), and a missing path is created as a file — the
			// pulled bytes must land INSIDE a directory. Refuse the
			// file case before touching the network; create a missing
			// directory so the file lands at <local-dir>/<basename>.
			if err := ensurePullDestination(localDir); err != nil {
				return err
			}

			// Load CLI config
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

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

			// Resolve SSH user and host from the sshfs mount command.
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

			// The transfer is a long-lived LOCAL child: it must not outlive
			// this CLI (GAP-084). Same contract as cp: signal-aware context,
			// own process group, parent-death backstop.
			childCtx, stopChildren := newChildSignalContext()
			defer stopChildren()

			// Execute SCP: pull means the REMOTE source comes first and the
			// LOCAL destination last.
			scpArgs := buildPullSCPArgs(keyPath, port, userAtHost, remotePath, localDir, recursive)
			scpCtx, cancelSCP := context.WithTimeout(childCtx, copyChildBudget)
			scpCmd := newLongLivedCommand(scpCtx, "scp", scpArgs...)
			scpCmd.Stdout = cmd.OutOrStdout()
			scpCmd.Stderr = cmd.ErrOrStderr()

			scpErr := runDetachedChildCommand(scpCmd)
			cancelSCP()
			if scpErr != nil {
				if childCtx.Err() != nil {
					return fmt.Errorf("scp: interrupted by a shutdown signal — the pull was stopped")
				}
				return fmt.Errorf("scp: %w", scpErr)
			}

			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Copied %s:%s to %s\n", agentID, remotePath, localDir); err != nil {
				return fmt.Errorf("write status: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().Uint32Var(&sshPort, "ssh-port", 0, "SSH port (default: 22)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Recursively copy remote directories")

	return cmd
}

// buildPullSCPArgs constructs the scp argv for pulling a file FROM an agent:
// the same connection options as buildSCPArgs, with the remote source
// (<user@host>:<remote-path>) FIRST and the local destination LAST.
func buildPullSCPArgs(keyPath string, port uint32, userAtHost, remotePath, localDir string, recursive bool) []string {
	args := append([]string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
	}, sshMultiplexArgs(keyPath, port, userAtHost)...)
	args = append(args,
		"-i", keyPath,
		"-P", fmt.Sprintf("%d", port),
	)
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, fmt.Sprintf("%s:%s", userAtHost, remotePath), localDir)
	return args
}

// ensurePullDestination makes sure the local destination argument of
// `bunker pull` is a directory BEFORE scp runs.
//
// An existing directory is left untouched (the long-standing behavior: the
// file lands inside it). A path that does not exist yet is created as a
// directory, so the pulled file is written INTO it instead of becoming that
// path itself. A path that exists as a non-directory (regular file, device,
// symlink to a file) is refused with an actionable error and never
// overwritten: scp would clobber it in place and exit 0, which is the silent
// data loss DF-BUNKER-83 reproduced.
func ensurePullDestination(localDir string) error {
	info, err := os.Stat(localDir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("local target exists and is not a directory: %s; pass a directory or remove the file", localDir)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if mkErr := os.MkdirAll(localDir, 0o755); mkErr != nil {
			return fmt.Errorf("create local directory %s: %w", localDir, mkErr)
		}
		return nil
	default:
		return fmt.Errorf("inspect local target %s: %w", localDir, err)
	}
}

// pullRequiresConnectImport is a compile-time guard removed in the same
// commit; see below.
var _ = strings.TrimSpace
