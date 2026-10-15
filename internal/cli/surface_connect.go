package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
	bunkerv1connect "github.com/deployBunker/bunker/proto/bunker/v1/bunkerv1connect"
)

// SURF-008: the client attach. SURF-006 made the surface installable, SURF-008
// makes attaching to it a supported command: `bunker surface connect <id>`
// replaces the hand-typed `ssh -N -L` from the recipe's Step 4 with the same
// long-lived-child contract as `bunker tunnel` (signal reaping, own process
// group, Pdeathsig backstop — GAP-079), resolved from the AGENT RECORD so the
// recipe's gotcha #1 (the daemon host is not the agent host) cannot repeat.
// `--check` runs the recipe's three live checks on the agent, over the audited
// exec RPC, without needing any local forward.
//
// There is deliberately NO `surface disconnect`: the ssh forward lives exactly
// as long as this process does — Ctrl-C (or SIGTERM/SIGHUP) tears the child
// process group down (signal reaping + group kill + parent-death backstop, the
// proven tunnel contract), and with it the local socket listener. That is the
// disconnect.

// surfaceConnectLocalSocketDefault is the local socket path the forward is
// published on, unless --socket-path overrides it.
func surfaceConnectLocalSocketDefault(agentID string) string {
	return "/tmp/bunker-toolsd-" + agentID + ".sock"
}

// surfaceToolsdSocketName is the socket file name inside the agent's runtime
// dir. The unit's ListenStream is %t/toolsd.sock (surfaceSocketUnitContent),
// where %t is /run/user/<uid> for the agent's own uid.
const surfaceToolsdSocketName = "toolsd.sock"

// surfaceConnectAgentRecord carries everything the connect command needs from
// the agent record — split out so the resolution is unit-testable without a
// transport.
type surfaceConnectAgentRecord struct {
	// UserAtHost is the "bunker-<id>@<host>" SSH login target parsed from the
	// record (sshfs mount preferred, docker-host tunnel as fallback).
	UserAtHost string
	// ServerHost is the host the RECORD embeds (the daemon's own view). When
	// neither the --ssh-host flag nor the server entry URL resolve a host,
	// this is the last-resort target — the daemon self-reported hostname that
	// may not resolve from a remote client.
	ServerHost string
}

// resolveSurfaceConnectTarget resolves the SSH login target for the connect
// forward from the agent RECORD, never from the daemon host (recipe gotcha
// #1: the daemon host is not the agent host). The sshfs mount line is the
// primary source, the docker-host tunnel the fallback — exactly the two lines
// `bunker info` renders. The host part then goes through the SAME resolution
// chain as tunnel/cp/mount (flag > server URL hostname > record value).
func resolveSurfaceConnectTarget(entry ServerEntry, rec *v1.AgentSummary, sshHostFlag string) (surfaceConnectAgentRecord, error) {
	out := surfaceConnectAgentRecord{}
	if rec == nil {
		return out, fmt.Errorf("agent record is empty")
	}
	userAtHost, err := parseSSHUserHost(rec.GetSshfsMount())
	if err != nil {
		// Fall back to the tunnel command's login target.
		user, host, ok := sshUserHostFromTunnel(rec.GetDockerHostTunnel())
		if !ok {
			return out, fmt.Errorf("cannot resolve an SSH target from the agent record (sshfs mount: %v; docker tunnel: %q) — is the agent fully spawned?",
				err, rec.GetDockerHostTunnel())
		}
		userAtHost = user + "@" + host
	}
	user, serverHost, _ := strings.Cut(userAtHost, "@")
	out.UserAtHost = user + "@" + resolveSSHHost(entry, serverHost, sshHostFlag)
	out.ServerHost = serverHost
	return out, nil
}

// surfaceCheckScript answers the recipe's three live checks in ONE exec, as
// CHECK\tkey=value lines so the verdict is parsed, not eyeballed:
//
//	socket_active : systemctl --user is-active toolsd.socket
//	socket_meta   : owner and mode of /run/user/<uid>/toolsd.sock (the AGENT
//	                must own it; the unit sets SocketMode=0600)
//	toolsd_procs  : pgrep -c toolsd BEFORE any connect (must be 0 — the
//	                service is connection-activated; a pre-running toolsd
//	                means something else spawned it)
const surfaceCheckScript = `exec 2>&1
u=$(id -u)
p="/run/user/$u/` + surfaceToolsdSocketName + `"
state=$(systemctl --user is-active ` + surfaceSocketUnitName + ` 2>/dev/null || true)
printf 'CHECK\tsocket_active=%s\n' "$state"
if [ -S "$p" ]; then
  own=$(stat -c '%U' "$p" 2>/dev/null || echo unknown)
  mode=$(stat -c '%a' "$p" 2>/dev/null || echo unknown)
  printf 'CHECK\tsocket_meta=%s:%s\n' "$own" "$mode"
else
  printf 'CHECK\tsocket_meta=absent\n'
fi
procs=$(pgrep -c -x toolsd 2>/dev/null || echo 0)
printf 'CHECK\ttoolsd_procs=%s\n' "$procs"
`

// surfaceCheckState is the parsed verdict of one --check run.
type surfaceCheckState struct {
	Agent        string `json:"agent"`
	SocketActive string `json:"socket_active"`
	Owner        string `json:"socket_owner"`
	Mode         string `json:"socket_mode"`
	ToolsdProcs  string `json:"toolsd_procs"`
	// OK is true when every check passed: socket active, owned by the agent
	// user with mode 0600, and zero pre-connect toolsd processes.
	OK bool `json:"ok"`
	// Failures names every failed check, so a bad state is never a bare
	// non-zero exit.
	Failures []string `json:"failures,omitempty"`
}

// surfaceCheckAgentUser is the agent user name an agent record implies:
// bunker-<agent-id>. Used to verify socket OWNERSHIP without a second exec.
func surfaceCheckAgentUser(agentID string) string {
	return "bunker-" + agentID
}

// parseSurfaceCheckOutput extracts the CHECK lines from a --check run's
// stdout. Missing lines parse as empty and are reported as failures by
// evaluateSurfaceCheck, never silently skipped.
func parseSurfaceCheckOutput(out string) (active, meta, procs string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "CHECK\t") {
			continue
		}
		for _, f := range strings.Split(strings.TrimPrefix(line, "CHECK\t"), "\t") {
			key, val, found := strings.Cut(f, "=")
			if !found {
				continue
			}
			switch key {
			case "socket_active":
				active = val
			case "socket_meta":
				meta = val
			case "toolsd_procs":
				procs = val
			}
		}
	}
	return active, meta, procs
}

// evaluateSurfaceCheck turns the parsed raw values into a verdict with NAMED
// failures. agentUser is the expected socket owner (the agent's own user —
// the identity that owns %t), matching the recipe's
// `stat -c '%U %a'` gate.
func evaluateSurfaceCheck(agentID, active, meta, procs, agentUser string) surfaceCheckState {
	st := surfaceCheckState{Agent: agentID, SocketActive: active}
	var failures []string

	switch active {
	case "active":
		// pass
	case "":
		failures = append(failures, "socket_active: no answer from systemctl --user is-active (manager unreachable or unit absent)")
	default:
		failures = append(failures, fmt.Sprintf("socket_active: %s (want active)", active))
	}

	owner, mode := "absent", "absent"
	if meta != "" && meta != "absent" {
		if o, m, ok := strings.Cut(meta, ":"); ok {
			owner, mode = o, m
		} else {
			owner, mode = meta, "unknown"
		}
	}
	st.Owner, st.Mode = owner, mode
	switch {
	case meta == "" || meta == "absent":
		failures = append(failures, fmt.Sprintf("socket_meta: the socket file is absent at /run/user/<uid>/%s — the socket unit is not started", surfaceToolsdSocketName))
	case owner != agentUser:
		failures = append(failures, fmt.Sprintf("socket_meta: owner is %q, want the agent user %q", owner, agentUser))
	case mode != "600":
		failures = append(failures, fmt.Sprintf("socket_meta: mode is %q, want 600 (SocketMode=0600)", mode))
	}

	if procs == "" {
		failures = append(failures, "toolsd_procs: no answer from pgrep (agent did not report a process count)")
		st.ToolsdProcs = "unknown"
	} else {
		st.ToolsdProcs = procs
		if n, err := strconv.Atoi(procs); err != nil || n != 0 {
			failures = append(failures, fmt.Sprintf("toolsd_procs: %s before any connect (want 0 — the service is connection-activated; something else spawned toolsd)", procs))
		}
	}

	st.Failures = failures
	st.OK = len(failures) == 0
	return st
}

// runSurfaceCheck performs the recipe's live checks on the agent over the
// audited exec RPC — no local forward involved.
func runSurfaceCheck(cmd *cobra.Command, ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string, asJSON bool) error {

	cctx, cancel := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancel()
	checkOut, _, err := execOnAgentScript(cctx, client, entry, agentID, surfaceCheckScript, 0)
	if err != nil {
		return fmt.Errorf("check on agent %q failed: %w", agentID, err)
	}
	active, meta, procs := parseSurfaceCheckOutput(checkOut)
	st := evaluateSurfaceCheck(agentID, active, meta, procs, surfaceCheckAgentUser(agentID))

	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(st)
	}
	fmt.Fprintf(out, "surface check for agent %s:\n", agentID)                                                  //nolint:errcheck // best-effort stdout report
	fmt.Fprintf(out, "  socket unit : %s\n", orUnknown(st.SocketActive))                                        //nolint:errcheck
	fmt.Fprintf(out, "  socket file : owner %s, mode %s\n", st.Owner, st.Mode)                                  //nolint:errcheck
	fmt.Fprintf(out, "  toolsd procs: %s (connection-activated: 0 before the first connect)\n", st.ToolsdProcs) //nolint:errcheck
	if st.OK {
		fmt.Fprintf(out, "  result      : OK — ready for `bunker surface connect %s`\n", agentID) //nolint:errcheck
		return nil
	}
	for _, f := range st.Failures {
		fmt.Fprintf(out, "  FAIL        : %s\n", f) //nolint:errcheck
	}
	return fmt.Errorf("surface check on agent %q failed %d check(s); see the FAIL lines above", agentID, len(st.Failures))
}

// orUnknown renders an empty status word as unknown (honest absence).
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// surfaceConnectOptions is everything the connect RunE resolves before the
// ssh child is started. Split from the cobra layer so the resolution chain is
// unit-testable (the tunnel test pattern: capture through execCommandContext).
type surfaceConnectOptions struct {
	AgentID    string
	LocalPath  string
	RemotePath string // /run/user/<uid>/toolsd.sock on the AGENT host
	UserAtHost string // bunker-<id>@<resolved-host>
	KeyPath    string
	SSHOptions []string // prepended right after the ssh binary
}

// buildSurfaceConnectSSHArgs builds the ssh argv for the forward: the same
// shape as the recipe's Step 4, with the client-local key and the resolved
// host baked in. ExitOnForwardFailure=yes is load-bearing: without it a
// refused remote forward (socket absent, permission denied) leaves a silent
// ssh that forwards nothing — the exact failure a "ready" line must never
// paper over.
func buildSurfaceConnectSSHArgs(opts surfaceConnectOptions) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "IdentitiesOnly=yes",
		"-o", "ExitOnForwardFailure=yes",
		"-i", opts.KeyPath,
		"-N",
		"-L", opts.LocalPath + ":" + opts.RemotePath,
		opts.UserAtHost,
	}
	return append(args, opts.SSHOptions...)
}

// surfaceDiscoverUID asks the AGENT for its own uid over the audited exec RPC
// (recipe gotcha #2: ask the agent who it is — host-side getent lies when the
// agent user is subid-mapped or simply not in the daemon host's passwd). It
// returns the decimal uid string.
func surfaceDiscoverUID(ctx context.Context, client bunkerv1connect.BunkerdClient,
	entry ServerEntry, agentID string) (string, error) {

	uctx, cancel := context.WithTimeout(ctx, surfaceOperationTimeout)
	defer cancel()
	out, exitCode, err := execOnAgentScript(uctx, client, entry, agentID, `id -u`, 0)
	if err != nil {
		return "", fmt.Errorf("discover uid on agent %q: %w", agentID, err)
	}
	uid := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	uid = strings.TrimRight(uid, "\r")
	if exitCode != 0 || uid == "" {
		return "", fmt.Errorf("discover uid on agent %q: `id -u` produced no usable answer (exit %d, output %q)",
			agentID, exitCode, tailSurfaceOutput(out, 200))
	}
	if _, err := strconv.Atoi(uid); err != nil {
		return "", fmt.Errorf("discover uid on agent %q: `id -u` returned %q, which is not a uid", agentID, uid)
	}
	return uid, nil
}

// waitForSurfaceLocalSocket waits until the local end of the forward exists
// as a socket — the ssh child publishes it as soon as the remote forward is
// accepted. The child's exit short-circuits the wait (ExitOnForwardFailure
// makes a refused remote forward exit instead of hanging), and so does ctx:
// a signal during the wait must tear the forward down promptly, not after
// the full budget. The error names the real cause.
func waitForSurfaceLocalSocket(ctx context.Context, path string, within time.Duration, child *exec.Cmd) error {
	deadline := time.Now().Add(within)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if child != nil && child.Process != nil {
			if err := child.Process.Signal(syscall.Signal(0)); err != nil {
				return fmt.Errorf("the ssh forward exited before the local socket appeared (refused remote forward? check the agent surface with --check): %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the forward was interrupted before the local socket %s appeared: %w", path, ctx.Err())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the local socket %s did not appear within %s — check the agent surface with --check", path, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// surfaceWaitForSocket is the seam the RunE waits through, so flow tests can
// stub the socket appearance (the deliverFileFunc pattern from
// agenttools_install.go). Production always uses the real waiter.
var surfaceWaitForSocket = waitForSurfaceLocalSocket

// newSurfaceConnectCommand implements `bunker surface connect <agent-id>`.
func newSurfaceConnectCommand() *cobra.Command {
	var (
		serverName string
		sshHost    string
		sshKey     string
		socketPath string
		uidFlag    string
		check      bool
		asJSON     bool
		timeout    uint32
	)
	cmd := &cobra.Command{
		Use:   "connect <agent-id>",
		Short: "Forward an agent's toolsd socket to a local path (the supported attach)",
		Long: `Attach to an agent's toolsd socket surface: forward
/run/user/<uid>/toolsd.sock (the agent's own systemd --user socket) to a
local Unix socket over SSH, using the agent's OWN key and identity — never
root, never the daemon host (the agent host is resolved from the agent
record's sshfs mount / docker tunnel line).

While the command runs, the agent's tool surface is available locally:

  bunker surface connect abc12345 &
  # ... speak JSON-RPC (initialize, tools/list, tools/call) over
  # /tmp/bunker-toolsd-abc12345.sock ...

The forward runs in the foreground until interrupted. Ctrl-C (or SIGTERM /
SIGHUP) tears down the ssh child and its process group — the tunnel contract
(GAP-079: signal reaping, group kill, parent-death backstop) — which IS the
disconnect: the forward exists exactly as long as this process does. There
is no separate disconnect command and no residue.

The socket is connection-activated: nothing runs on the agent until the
first local connection arrives. Check the surface first with --check.

Examples:
  bunker surface connect abc12345 --check
  bunker surface connect abc12345
  bunker surface connect abc12345 --socket-path /tmp/my-toolsd.sock
  bunker surface connect abc12345 --ssh-host 203.0.113.10`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			ctx, client, entry, err := surfaceVerbContext(cmd, serverName, timeout)
			if err != nil {
				return err
			}

			if check {
				return runSurfaceCheck(cmd, ctx, client, entry, agentID, asJSON)
			}

			// 1. The real agent host, from the agent record.
			gctx, cancelGet := context.WithTimeout(ctx, surfaceOperationTimeout)
			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			if token := resolveToken(entry); token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			info, err := client.GetAgent(gctx, req)
			cancelGet()
			if err != nil {
				return fmt.Errorf("get agent %s: %w", agentID, err)
			}
			target, err := resolveSurfaceConnectTarget(entry, info.Msg.GetAgent(), sshHost)
			if err != nil {
				return err
			}

			// 2. The agent's uid, asked of the agent (gotcha #2).
			uid := uidFlag
			if uid == "" {
				uid, err = surfaceDiscoverUID(ctx, client, entry, agentID)
				if err != nil {
					return err
				}
			} else if _, aerr := strconv.Atoi(uid); aerr != nil {
				return fmt.Errorf("invalid --uid %q: not a numeric uid", uid)
			}

			// 3. The client-local key (server-side paths are not readable
			// from a remote client — the tunnel rule).
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

			// 4. Local socket path: refuse to clobber a live one, then make
			//    sure the ssh(1) forward can bind it.
			localPath := socketPath
			if localPath == "" {
				localPath = surfaceConnectLocalSocketDefault(agentID)
			}
			if fi, statErr := os.Stat(localPath); statErr == nil && fi.Mode()&os.ModeSocket != 0 {
				return fmt.Errorf("local socket %s already exists — another forward may be live; remove it or pass --socket-path", localPath)
			}
			_ = os.Remove(localPath)

			opts := surfaceConnectOptions{
				AgentID:    agentID,
				LocalPath:  localPath,
				RemotePath: "/run/user/" + uid + "/" + surfaceToolsdSocketName,
				UserAtHost: target.UserAtHost,
				KeyPath:    keyPath,
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "forwarding agent %s toolsd socket\n", agentID)                    //nolint:errcheck // best-effort stdout report
			fmt.Fprintf(out, "  local  : %s\n", opts.LocalPath)                                 //nolint:errcheck
			fmt.Fprintf(out, "  remote : %s (via %s)\n", opts.RemotePath, opts.UserAtHost)      //nolint:errcheck
			fmt.Fprintln(out, "  the forward runs until interrupted; Ctrl-C is the disconnect") //nolint:errcheck

			// 5. Start the ssh child under the SAME long-lived-child
			//    contract as `bunker tunnel` (GAP-079): own signal-aware
			//    context with NO deadline, own process group, group kill on
			//    teardown, Pdeathsig backstop from a pinned thread.
			connectCtx, stopConnect := newChildSignalContext()
			defer stopConnect()

			c := execCommandContext(connectCtx, "ssh", buildSurfaceConnectSSHArgs(opts)...)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			configureDetachedChild(c)
			c.Cancel = func() error { return terminateChildGroup(c) }
			c.WaitDelay = childShutdownGrace

			if err := startTunnelChild(c, connectCtx); err != nil {
				return fmt.Errorf("start ssh forward: %w", err)
			}

			// 6. Wait for the local socket to appear — that is the READY
			//    line's evidence, not optimism. ExitOnForwardFailure=yes
			//    makes a refused remote forward kill the child, which Wait
			//    surfaces below.
			if err := surfaceWaitForSocket(connectCtx, localPath, 15*time.Second, c); err != nil {
				_ = terminateChildGroup(c)
				_ = c.Wait()
				stopConnect()
				return err
			}
			fmt.Fprintf(out, "READY: %s is forwarding the agent toolsd surface (Ctrl-C to stop)\n", opts.LocalPath) //nolint:errcheck // best-effort stdout report

			runErr := c.Wait()
			_ = os.Remove(opts.LocalPath)
			if connectCtx.Err() != nil {
				// Torn down by a signal: the operator's intent, not a
				// failure (the tunnel rule).
				return nil
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; mutating commands never fall back to the shared active default)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: resolved from the agent record, then the server URL hostname)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	cmd.Flags().StringVar(&socketPath, "socket-path", "", "Local socket path for the forward (default: /tmp/bunker-toolsd-<agent-id>.sock)")
	cmd.Flags().StringVar(&uidFlag, "uid", "", "Agent uid override (default: discovered from the agent via 'bunker exec')")
	cmd.Flags().BoolVar(&check, "check", false, "Run the recipe's live checks on the agent (socket active, owner+mode, zero pre-connect toolsd) and exit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "With --check: emit the result as JSON")
	cmd.Flags().Uint32Var(&timeout, "timeout", 60, "Overall timeout in seconds for the setup RPCs")
	return cmd
}
