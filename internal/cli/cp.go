package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// sshfsUserHostRE extracts "user@host" from an sshfs mount command like:
//
//	sshfs -o IdentityFile=/path -o idmap=user -o allow_other bunker-abc@myhost:/home/bunker-abc /mnt/bunker/abc
var sshfsUserHostRE = regexp.MustCompile(`\b(bunker-\S+@\S+):`)

// NewCpCommand returns the `bunker cp` cobra command.
func NewCpCommand() *cobra.Command {
	var (
		serverName string
		sshPort    uint32
		sshKey     string
		sshHost    string
	)

	cmd := &cobra.Command{
		Use:   "cp <local-file> <agent-id>:/path",
		Short: "Copy a file to an agent's environment via SCP",
		Long: `Copy a local file into an agent's container using SCP.

The agent connection details (SSH user, host) are resolved from the bunkerd
API. The SSH host defaults to the hostname of the server config URL (the
address the client used to reach bunkerd) and can be overridden with
--ssh-host. The SSH key is read from ~/.bunker/keys/<agent-id> (saved at
spawn time) unless overridden with --ssh-key. After the copy, file ownership
is set to the agent user.

The destination format is <agent-id>:/path — the colon and path are required.

When the copy fails because the destination already exists on the agent host
and is owned by another user (e.g. a host-owned /tmp path), the CLI prints an
ownership hint naming the current owner and the agent's user instead of only
scp's raw error.

Examples:
  bunker cp ./config.yaml abc12345:/home/bunker-abc12345/config.yaml
  bunker cp secret.env def67890:/app/.env --ssh-port 2222
  bunker cp ./script.sh abc12345:/home/bunker-abc12345/bin/script.sh --ssh-key ~/.ssh/custom_key
  bunker cp ./app.conf abc12345:/app.conf --ssh-host 203.0.113.10`,

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

			// Validate local file exists
			localInfo, err := os.Stat(localPath)
			if err != nil {
				return fmt.Errorf("local file %q: %w", localPath, err)
			}
			if localInfo.IsDir() {
				return fmt.Errorf("%q is a directory — use 'bunker deploy' to copy directories", localPath)
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

			// The transfer and the chown that follows it are long-lived LOCAL
			// children: they must not outlive this CLI (GAP-084). They run
			// under the shared long-lived-child contract (proc.go) — a
			// signal-aware context (SIGINT/SIGTERM/SIGHUP turns into a
			// cancellation instead of the CLI dying mid-syscall), an own
			// process group with a group-wide SIGTERM-then-SIGKILL teardown,
			// and the kernel parent-death backstop for the case NOTHING in
			// this process can run (kill -9).
			childCtx, stopChildren := newChildSignalContext()
			defer stopChildren()

			// Execute SCP. The agent user is resolved BEFORE scp so the
			// failure path can name it in the ownership hint below.
			sshUser := strings.SplitN(userAtHost, "@", 2)[0]
			scpArgs := buildSCPArgs(keyPath, port, localPath, userAtHost, remotePath, false)
			scpCtx, cancelSCP := context.WithTimeout(childCtx, copyChildBudget)
			scpCmd := newLongLivedCommand(scpCtx, "scp", scpArgs...)
			scpCmd.Stdout = cmd.OutOrStdout()
			scpCmd.Stderr = cmd.ErrOrStderr()

			scpErr := runDetachedChildCommand(scpCmd)
			cancelSCP()
			if scpErr != nil {
				if childCtx.Err() != nil {
					// The CLI was signalled: the group teardown above already
					// reaped the transfer, so report the interruption instead
					// of a misleading "context canceled" and skip the probe.
					return fmt.Errorf("scp: interrupted by a shutdown signal — the copy was stopped")
				}
				// scp's own stderr already went to the user (streamed
				// above); it names no next step, so run ONE bounded probe
				// over the same SSH path to explain an existing
				// host-owned destination (DF-BUNKER-17). Best-effort: a
				// probe error/timeout only means "no hint". The raw scp
				// error text is kept either way.
				if hint := cpDestinationHint(keyPath, port, userAtHost, remotePath, sshUser); hint != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "Hint: %s\n", hint)
				}
				return fmt.Errorf("scp: %w", scpErr)
			}

			// Fix ownership: chown the file to the agent user
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
				fmt.Sprintf("chown %s:%s %s", sshUser, sshUser, remotePath),
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

			fmt.Fprintf(cmd.OutOrStdout(), "Copied %s to %s:%s\n", localPath, agentID, remotePath)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().Uint32Var(&sshPort, "ssh-port", 0, "SSH port (default: 22)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")

	return cmd
}

// parseSSHUserHost extracts "user@host" from an sshfs mount command string.
// The sshfs_mount format is:
//
//	sshfs -o IdentityFile=<path> -o idmap=user -o allow_other bunker-<id>@<host>:/home/bunker-<id> /mnt/bunker/<id>
func parseSSHUserHost(sshfsMount string) (string, error) {
	if sshfsMount == "" {
		return "", fmt.Errorf("sshfs_mount is empty — agent may not be fully spawned yet")
	}
	match := sshfsUserHostRE.FindStringSubmatch(sshfsMount)
	if match == nil {
		return "", fmt.Errorf("cannot parse user@host from sshfs_mount: %q", sshfsMount)
	}
	return match[1], nil
}

// buildSCPArgs constructs the arguments for the scp command.
func buildSCPArgs(keyPath string, port uint32, localPath, userAtHost, remotePath string, recursive bool) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", keyPath,
		"-P", fmt.Sprintf("%d", port),
	}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, localPath, fmt.Sprintf("%s:%s", userAtHost, remotePath))
	return args
}

// cpProbeTimeout bounds the post-failure ownership probe. It is deliberately
// short: one bounded round trip, never a retry (this is a message-quality
// fix, not a retry fix — DF-BUNKER-17).
const cpProbeTimeout = 10 * time.Second

// copyChildBudget bounds one transfer step of `bunker cp` / `bunker deploy`
// (scp, then the chown that follows it). It preserves the envelope those
// commands effectively had before GAP-084 — the 30s GetAgent deadline was
// handed straight to exec, so it was the transfer's deadline too — but it is
// now an explicit per-child budget derived from the command's SIGNAL context
// instead of a side effect of the RPC's: an over-running step still fails, and
// a signal still reaps the child while it runs.
const copyChildBudget = 30 * time.Second

// cpOwnershipProbeFunc is the injectable runner seam for the ownership probe:
// tests substitute it to exercise every probe outcome without a live host.
type cpOwnershipProbeFunc func(keyPath string, port uint32, userAtHost, remotePath string) (string, error)

// cpOwnershipProbeFn queries the destination's ownership on the agent host.
// Package-level so it can be stubbed in tests.
var cpOwnershipProbeFn cpOwnershipProbeFunc = probeRemoteOwnership

// probeRemoteOwnership runs ONE bounded ssh command over the SAME SSH path scp
// just used (same key, port, and user@host) and returns its trimmed stdout.
// The command prints "<owner>:<group>" for an existing destination and
// nothing for a missing one, so a non-existent path yields ("", nil) and the
// caller simply prints no hint. The context is derived from Background, NOT
// from the command's own (possibly already-expired) RPC context: a slow scp
// can burn the caller's deadline, and a dead context here would silently
// disable the hint.
func probeRemoteOwnership(keyPath string, port uint32, userAtHost, remotePath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cpProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "ssh", buildSSHProbeArgs(keyPath, port, userAtHost, remotePath)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// buildSSHProbeArgs constructs the ssh argv for the ownership probe: the same
// connection options as buildSCPArgs (same key/port/userAtHost), with a stat
// command instead of a file transfer. The remote path is single-quoted for
// the remote POSIX shell; a missing path prints nothing and still exits 0.
func buildSSHProbeArgs(keyPath string, port uint32, userAtHost, remotePath string) []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", port),
		userAtHost,
		// %% escapes the literal % of stat's format string.
		fmt.Sprintf("stat -c '%%U:%%G' -- %s 2>/dev/null || true", quotePOSIX(remotePath)),
	}
}

// quotePOSIX single-quotes s for a remote POSIX shell.
func quotePOSIX(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cpDestinationHint runs the bounded ownership probe and turns its output
// into an actionable hint, or "" when there is nothing useful to say. It is
// failure-tolerant by construction: any probe error (unreachable host,
// timeout, ssh missing) or empty/unparseable output yields "" and the caller
// still reports the original scp error.
func cpDestinationHint(keyPath string, port uint32, userAtHost, remotePath, agentUser string) string {
	out, err := cpOwnershipProbeFn(keyPath, port, userAtHost, remotePath)
	if err != nil {
		return ""
	}
	return cpOwnershipHint(out, agentUser)
}

// cpOwnershipHint formats the hint appended to an scp failure when the
// destination already exists on the agent host and is NOT owned by the agent
// user. probeOut is the probe's trimmed output, "<owner>:<group>" (stat -c
// '%U:%G'). Empty, colon-less, or malformed output — and an owner equal to
// the agent user — return "" (no hint).
func cpOwnershipHint(probeOut, agentUser string) string {
	owner, group, ok := strings.Cut(strings.TrimSpace(probeOut), ":")
	if !ok || owner == "" || group == "" || strings.ContainsAny(owner+group, " 	\n") {
		return ""
	}
	if owner == agentUser {
		return ""
	}
	return fmt.Sprintf("destination already exists on the agent host and is owned by %s:%s, not the agent user %q — remove it, pick another path, or write somewhere owned by %s (e.g. /home/%s/); scp cannot overwrite a file it does not own",
		owner, group, agentUser, agentUser, agentUser)
}

// defaultSSHKeyPath returns the default path to the agent's SSH key saved during spawn.
func defaultSSHKeyPath(agentID string) (string, error) {
	cfgDir, err := configFilePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgDir), "keys", agentID), nil
}
