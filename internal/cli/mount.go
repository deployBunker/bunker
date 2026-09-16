package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// sshfsMaxAttempts is the total number of sshfs attempts (first try plus
// retries) before the mount command gives up.
const sshfsMaxAttempts = 3

// sshfsAttemptTimeout bounds each individual sshfs attempt. Every attempt
// gets a fresh budget so earlier failed attempts do not starve later ones.
const sshfsAttemptTimeout = 30 * time.Second

// sshfsRetryDelay is the wait between sshfs attempts. Package-level so
// tests can zero it out (no real sleeps).
var sshfsRetryDelay = 2 * time.Second

// sshfsRun runs the sshfs binary with args, streaming its combined output
// to the terminal (os.Stdout/os.Stderr) exactly as before while also
// forwarding it to the caller-provided capture writers. Package-level
// seam so tests can stub the execution.
var sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = io.MultiWriter(stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)
	return cmd.Run()
}

// sshfsMaxOutputTail bounds the captured-output tail embedded in error text.
const sshfsMaxOutputTail = 500

// sshfsPermanentFragments are lowercase output fragments that indicate a
// permanent sshfs failure. Retrying cannot help, so the command fails
// immediately on the first attempt without any session-limit hint.
var sshfsPermanentFragments = []string{
	"permission denied",
	"no such file or directory",
	"mountpoint is not empty",
	"fuse: device not found",
	"transport endpoint is not connected",
}

// sshfsTransientFragments are lowercase output fragments that indicate a
// transient connection failure worth retrying. These were observed against
// healthy agents whose host limits parallel SSH sessions (sshd MaxStartups):
// each sshfs attempt opens a fresh connection while tunnels stay alive.
var sshfsTransientFragments = []string{
	"connection reset by peer",
	"remote host has disconnected",
	"connection closed",
}

// classifySSHFSFailure inspects the captured sshfs output and the process
// error and returns a class ("permanent", "transient", or "unknown") plus a
// short human-readable reason. Permanent causes take precedence: real ssh
// transcripts often contain both a permanent fragment and a transient one
// (e.g. "Permission denied ... Connection closed by host"), and retrying an
// auth failure would only add noise.
func classifySSHFSFailure(output string, err error) (string, string) {
	lower := strings.ToLower(output)
	for _, frag := range sshfsPermanentFragments {
		if strings.Contains(lower, frag) {
			return "permanent", frag
		}
	}
	for _, frag := range sshfsTransientFragments {
		if strings.Contains(lower, frag) {
			return "transient", frag
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "transient", "unexpected EOF"
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
			if ws, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return "transient", fmt.Sprintf("killed by signal %s", ws.Signal())
			}
		}
	}
	return "unknown", ""
}

// trimSSHFSOutput trims captured sshfs output to a sane tail for embedding
// in error text.
func trimSSHFSOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(none)"
	}
	if len(s) > sshfsMaxOutputTail {
		s = "...(truncated) " + s[len(s)-sshfsMaxOutputTail:]
	}
	return s
}

// NewMountCommand returns the `bunker mount` cobra command.
func NewMountCommand() *cobra.Command {
	var (
		serverName string
		mountPoint string
		sshKey     string
	)

	cmd := &cobra.Command{
		Use:   "mount <agent-id> [mountpoint]",
		Short: "Mount an agent's home directory via SSHFS",
		Long: `Mount an agent's home directory to a local path using SSHFS.

If mountpoint is omitted, a default under /mnt/bunker/<agent-id> is used.
The stored SSHFS command from the server is rewritten for this client: the
daemon-local key path is replaced with the client-local key saved at spawn
time (~/.bunker/keys/<agent-id>, or --ssh-key) and the daemon hostname is
resolved to the server address this client actually connects to.

Examples:
  bunker mount abc12345
  bunker mount abc12345 /tmp/bunker-mnt
  bunker mount abc12345 --ssh-key ~/.ssh/custom_key`,
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

			// 3. Resolve the client-side connection details. The stored
			// command is generated on the daemon host: it embeds the
			// daemon-local key path (/etc/bunkerd/ssh/<agent-id>) and the
			// daemon's own hostname, neither of which is usable from a
			// remote client. `bunker ssh` and `bunker cp` already solve
			// this — mount reuses the same helpers: the host is resolved
			// from the server entry URL (the address this client actually
			// used to reach bunkerd) and the key is the client-local copy
			// saved at spawn time (~/.bunker/keys/<agent-id>).
			userAtHost, err := resolveUserAtHost(entry, mountCmd, "")
			if err != nil {
				return fmt.Errorf("resolve agent host: %w", err)
			}
			_, resolvedHost, _ := strings.Cut(userAtHost, "@")
			serverHost := sshHostFromMount(mountCmd)

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

			// 4. Ensure mount point exists.
			if err := os.MkdirAll(mountPoint, 0755); err != nil {
				return fmt.Errorf("create mount point %s: %w", mountPoint, err)
			}

			// 5. Build the SSHFS command: rewrite the stored command for
			// this client (client-local key + resolved host), then
			// substitute the mount point. The stored command ends with the
			// daemon default mount point; replace it with the
			// user-provided one so the same command works for arbitrary
			// paths.
			clientMountCmd := rewriteSSHFSMount(mountCmd, serverHost, resolvedHost, keyPath)
			parts := strings.Fields(clientMountCmd)
			if len(parts) < 2 {
				return fmt.Errorf("invalid SSHFS mount command: %s", clientMountCmd)
			}
			parts[len(parts)-1] = mountPoint

			// 5. Run sshfs with extra SSH options that are not encoded in the
			// stored command so the connection succeeds without host-key prompts.
			sshfsArgs := []string{
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "IdentitiesOnly=yes",
			}
			// Append everything except the leading `sshfs` and trailing mount
			// point, then the mount point last.
			sshfsArgs = append(sshfsArgs, parts[1:len(parts)-1]...)
			sshfsArgs = append(sshfsArgs, parts[len(parts)-1])

			// 6. Run sshfs with bounded retry. Transient failures
			// (connection resets and similar) are retried with backoff
			// while permanent causes fail immediately. The final
			// exhausted-retry message reports the CLASSIFIED cause from
			// the captured output: the session-limit hint is only shown
			// when the captured output actually contains the transient
			// fragments that indicate it (previously the hint was
			// asserted unconditionally, misdiagnosing e.g. auth or
			// reachability failures as sshd MaxStartups limiting).
			var combined bytes.Buffer
			var lastErr error
			var lastClass, lastReason string
			for attempt := 1; attempt <= sshfsMaxAttempts; attempt++ {
				combined.Reset()
				stdoutCap := &bytes.Buffer{}
				stderrCap := &bytes.Buffer{}
				attemptCtx, attemptCancel := context.WithTimeout(context.Background(), sshfsAttemptTimeout)
				err := sshfsRun(attemptCtx, parts[0], sshfsArgs, stdoutCap, stderrCap)
				attemptCancel()
				_, _ = combined.Write(stdoutCap.Bytes())
				_, _ = combined.Write(stderrCap.Bytes())
				if err == nil {
					lastErr = nil
					break
				}
				lastErr = err
				lastClass, lastReason = classifySSHFSFailure(combined.String(), err)
				if lastClass != "transient" {
					// Permanent or unknown cause: retrying cannot help. Keep
					// the original cause and do NOT hint at session limiting.
					return fmt.Errorf("sshfs failed: %w (last output: %s)", err, trimSSHFSOutput(combined.String()))
				}
				if attempt < sshfsMaxAttempts {
					fmt.Fprintf(os.Stderr, "bunker: sshfs attempt %d/%d failed (%s); retrying in %s\n",
						attempt, sshfsMaxAttempts, lastReason, sshfsRetryDelay)
					time.Sleep(sshfsRetryDelay)
				}
			}
			if lastErr != nil {
				if lastReason == "" {
					lastClass, lastReason = classifySSHFSFailure(combined.String(), lastErr)
				}
				msg := fmt.Sprintf("sshfs failed after %d attempts (%s)", sshfsMaxAttempts, lastReason)
				if lastClass == "transient" && containsAnyFragment(combined.String(), sshfsTransientFragments) {
					msg += " — agent host may be limiting parallel SSH sessions; try again or close other tunnels"
				}
				return fmt.Errorf("%s: %w (last output: %s)", msg, lastErr, trimSSHFSOutput(combined.String()))
			}

			fmt.Printf("Mounted %s at %s\n", agentID, mountPoint)
			fmt.Printf("Unmount with: fusermount -u %s\n", mountPoint)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	return cmd
}

// containsAnyFragment reports whether s contains any of the (lowercase)
// fragments, so evidenced hints can be gated on the captured output.
func containsAnyFragment(s string, fragments []string) bool {
	lower := strings.ToLower(s)
	for _, frag := range fragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
