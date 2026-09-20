package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

// sshfsAttemptTimeout bounds each individual sshfs attempt: every attempt gets
// its own fresh deadline so a signal-aware parent context cannot turn a bounded
// retry loop into a sequence of unbounded attempts. Package-level var (not a
// const) so tests can shrink it — the hang-bounding contract is only testable
// if a test can reach the deadline without waiting 30 real seconds per attempt.
var sshfsAttemptTimeout = 30 * time.Second

// sshfsRetryDelay is the wait between sshfs attempts. Package-level so
// tests can zero it out (no real sleeps).
var sshfsRetryDelay = 2 * time.Second

// sshfsRun runs the sshfs binary with args, streaming its combined output
// to the terminal (os.Stdout/os.Stderr) exactly as before while also
// forwarding it to the caller-provided capture writers. Package-level
// seam so tests can stub the execution.
//
// The child is run through the shared long-lived-child contract
// (runDetachedChildCommand, proc.go): it gets its own process group, so the
// context cancellation that ends an attempt (its own timeout, or a signal)
// reaps the whole attempt — sshfs and the ssh/sftp helpers it spawns — instead
// of leaving them behind as orphans (GAP-084).
var sshfsRun = func(ctx context.Context, path string, args []string, stdout, stderr io.Writer) error {
	cmd := newLongLivedCommand(ctx, path, args...)
	cmd.Stdout = io.MultiWriter(stdout, os.Stdout)
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)
	return runDetachedChildCommand(cmd)
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
		serverName      string
		mountPoint      string
		sshKey          string
		remotePath      string
		expectWorkspace string
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
				resolved, err := defaultMountPoint(agentID)
				if err != nil {
					return err
				}
				mountPoint = resolved
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

			// 3b. Preflight the remote path BEFORE mounting (GAP-103). Without
			// this, mounting an agent whose home is empty (a re-created agent)
			// or a --path that does not exist SUCCEEDS and presents an empty
			// tree that looks correct -- so an editor writes into nothing. The
			// check is bounded and names which of the two causes it hit.
			//
			// BUNKER_SKIP_MOUNT_PREFLIGHT exists for subprocess-based tests that
			// cannot inject the remotePathCheck seam (the preflight shells out to
			// a real host). It is deliberately explicit and loud rather than a
			// silent fallback: an operator who sets it is choosing to mount
			// without the empty-tree guarantee.
			if os.Getenv("BUNKER_SKIP_MOUNT_PREFLIGHT") != "" {
				fmt.Fprintln(os.Stderr, "bunker: WARNING: mount preflight skipped (BUNKER_SKIP_MOUNT_PREFLIGHT set) — an empty or missing remote path will mount as an empty tree")
			} else {
				ident, err := remotePathCheck(userAtHost, keyPath, remotePath)
				if err != nil {
					return err
				}
				// Workspace identity: refuse when the tree is not the one the
				// operator said they expected, so aiming at the wrong project
				// is detectable instead of silent. An empty expectation never
				// refuses — the check exists for a STATED expectation.
				if !ident.MatchesExpected(expectWorkspace) {
					return fmt.Errorf(
						"mount refused: workspace identity does not match --expect-workspace %q\n  resolved: %s\n"+
							"  pass the correct --path, or drop --expect-workspace if any tree is acceptable",
						expectWorkspace, ident.Describe())
				}
				fmt.Printf("Workspace: %s\n", ident.Describe())
			}

			// Default the remote path from the stored command when the operator
			// did not ask for a specific subdirectory. --path is the WORKSPACE
			// axis: mounting a repo inside the home rather than the whole home.
			if remotePath == "" {
				if fromCmd := lastRemoteSourcePath(mountCmd); fromCmd != "" {
					remotePath = fromCmd
				} else {
					remotePath = "."
				}
			}

			// 4. Ensure mount point exists (private to this user).
			if err := os.MkdirAll(mountPoint, 0o700); err != nil {
				return fmt.Errorf("create mount point %s: %w", mountPoint, err)
			}
			if err := checkWritableDir(mountPoint); err != nil {
				return err
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

			// 5. Durability + resource-safety options. These are constructed
			// by the CLIENT rather than inherited from the daemon-stored
			// command string (GAP-107): the stored command carries only the
			// identity/mount basics, so a mount that survives a transport blip,
			// detects a dead peer, bounds its own connect time and releases the
			// mountpoint when the process dies has to be asked for here.
			//
			// Note on allow_other: the daemon's stored command may request it
			// for cross-user sharing. In this CLI's default-user mode sshfs
			// defaults to allow_other OFF for a good reason -- the mountpoint is
			// created 0700, so only this user can enter it, while allow_other
			// would let EVERY local user read the agent's files. The flag cannot
			// be un-done safely at mount time, so every flag whose name starts
			// with allow is refused below with a named cause instead of silently
			// producing a world-readable mount. (A future opt-in can support it
			// by creating a 0755/1777 mountpoint and setting user_allow_other.)
			sshfsArgs := durableSSHFSArgs()
			// Append everything except the leading `sshfs` and trailing mount
			// point, then the mount point last.
			sshfsArgs = append(sshfsArgs, parts[1:len(parts)-1]...)
			sshfsArgs = append(sshfsArgs, parts[len(parts)-1])

			// The daemon's stored command always requests allow_other (see
			// manager_spawn.go:583), which widens the agent's files to every
			// local user. This CLI creates a PRIVATE (0700) mountpoint, so the
			// flag is both useless and harmful here: strip it and say so,
			// rather than either refusing (which would break every real mount)
			// or silently passing it through.
			if stripped, offender, found := stripAllowOption(sshfsArgs); found {
				sshfsArgs = stripped
				fmt.Fprintf(os.Stderr, "bunker: note: dropped %q from the agent's stored SSHFS command — this mountpoint is private (0700) to your user, so the option would gain nothing and would expose the agent's files to other local users\n", offender)
			}

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
			// Each sshfs ATTEMPT is a long-lived local child and must not
			// outlive this CLI (GAP-084): the attempts run under ONE
			// signal-aware context (SIGINT/SIGTERM/SIGHUP cancels it, and the
			// shared contract in proc.go reaps the in-flight attempt's process
			// group), while every attempt still gets its OWN fresh timeout
			// derived from it — a signal-aware context must not turn a bounded
			// retry loop into a sequence of unbounded attempts.
			mountCtx, stopMountSignals := newChildSignalContext()
			defer stopMountSignals()

			for attempt := 1; attempt <= sshfsMaxAttempts; attempt++ {
				combined.Reset()
				stdoutCap := &bytes.Buffer{}
				stderrCap := &bytes.Buffer{}
				attemptCtx, attemptCancel := context.WithTimeout(mountCtx, sshfsAttemptTimeout)
				err := sshfsRun(attemptCtx, parts[0], sshfsArgs, stdoutCap, stderrCap)
				attemptCancel()
				_, _ = combined.Write(stdoutCap.Bytes())
				_, _ = combined.Write(stderrCap.Bytes())
				if err == nil {
					lastErr = nil
					break
				}
				if mountCtx.Err() != nil {
					// Signalled while the attempt ran (Ctrl-C, SIGTERM from a
					// manager): the attempt's group was already reaped, so
					// report the interruption and never retry.
					return fmt.Errorf("sshfs: interrupted by a shutdown signal — the mount attempt was stopped (last output: %s)", trimSSHFSOutput(combined.String()))
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
			fmt.Printf("Unmount with: bunker umount %s\n", agentID)
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&remotePath, "path", "", "Remote path inside the agent to mount (default: the agent's home)")
	cmd.Flags().StringVar(&expectWorkspace, "expect-workspace", "", "Refuse to mount unless the resolved workspace matches this git remote (e.g. deployBunker/bunker)")
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
