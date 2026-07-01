package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewMountCommand returns the `bunker mount` cobra command.
func NewMountCommand() *cobra.Command {
	var (
		serverName string
		mountPoint string
	)

	cmd := &cobra.Command{
		Use:   "mount <agent-id> [mountpoint]",
		Short: "Mount an agent's home directory via SSHFS",
		Long: `Mount an agent's home directory to a local path using SSHFS.

If mountpoint is omitted, a default under /mnt/bunker/<agent-id> is used.
The command requires the agent to have been spawned with the SSHFS mount
details saved in the CLI state.

Examples:
  bunker mount abc12345
  bunker mount abc12345 /tmp/bunker-mnt`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			if len(args) > 1 {
				mountPoint = args[1]
			}
			if mountPoint == "" {
				mountPoint = filepath.Join("/mnt", "bunker", agentID)
			}

			// 1. Load CLI config
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

			// 2. Retrieve the SSHFS mount command from the server for the agent.
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
			mountCmd := info.Msg.GetAgent().GetSshfsMount()
			if mountCmd == "" {
				return fmt.Errorf("agent %s has no SSHFS mount command; ensure it was spawned with SSHFS support", agentID)
			}

			// 3. Ensure mount point exists.
			if err := os.MkdirAll(mountPoint, 0755); err != nil {
				return fmt.Errorf("create mount point %s: %w", mountPoint, err)
			}

			// 4. Build the SSHFS command by substituting the mount point.
			// The stored command ends with the default mount point. Replace it
			// with the user-provided mount point so the same command works for
			// arbitrary paths.
			parts := strings.Fields(mountCmd)
			if len(parts) < 2 {
				return fmt.Errorf("invalid SSHFS mount command: %s", mountCmd)
			}
			parts[len(parts)-1] = mountPoint

			// 5. Run sshfs with extra SSH options that are not encoded in the
			// stored command so the connection succeeds without host-key prompts.
			sshfsArgs := []string{
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
			}
			// Append everything except the leading `sshfs` and trailing mount
			// point, then the mount point last.
			sshfsArgs = append(sshfsArgs, parts[1:len(parts)-1]...)
			sshfsArgs = append(sshfsArgs, parts[len(parts)-1])

			sshfsCmd := exec.CommandContext(ctx, parts[0], sshfsArgs...)
			sshfsCmd.Stdout = os.Stdout
			sshfsCmd.Stderr = os.Stderr
			if err := sshfsCmd.Run(); err != nil {
				return fmt.Errorf("sshfs failed: %w", err)
			}

			fmt.Printf("Mounted %s at %s\n", agentID, mountPoint)
			fmt.Printf("Unmount with: fusermount -u %s\n", mountPoint)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	return cmd
}
