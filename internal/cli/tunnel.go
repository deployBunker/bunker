package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
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

// startTunnelChild starts the ssh child from a goroutine that pins its OS
// thread, and holds that pin for as long as the tunnel lives.
//
// WHY the pin is load-bearing (GAP-079): the child is started with Pdeathsig
// (tunnel_proc_pdeathsig_supported.go), and the kernel delivers that signal when
// the THREAD that created the child exits — not when the process does. prctl(2)
// PR_SET_PDEATHSIG is explicit about it ("the signal will be sent when that
// thread terminates ... rather than after all of the threads in the parent
// process terminate"); measured: a helper thread that forks a
// Pdeathsig=SIGKILL child and then pthread_exit()s leaves its process alive and
// the child SIGKILLed, 3/3 runs.
//
// Go's goroutines are multiplexed over OS threads whose lifetime is NOT tied to
// the process: runtime.LockOSThread's own contract is that a locked goroutine
// which exits without unlocking has its thread terminated, threads are torn
// down through mexit() (gogo(&m.g0.sched) — "let mstart0 exit the thread")
// independently of the process, and the forking thread is whichever M runs
// Start(). A bare Pdeathsig field would therefore make the tunnel die at a
// random moment — when the thread that forked ssh goes away — instead of at CLI
// death. That is the difference between a working backstop and a random-kill
// bug, so the pin is not decoration.
//
// The starting goroutine must NOT return while the child runs: it parks on the
// tunnel context, which RunE's deferred stop() cancels on the way out (either a
// signal stop or after Wait reaped the child), so the pin is released exactly
// when the tunnel is over. Start's error is propagated to the caller the same
// way exec.Cmd.Run propagates it; Wait stays in RunE.
func startTunnelChild(cmd *exec.Cmd, tunnelCtx context.Context) error {
	started := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil

		<-tunnelCtx.Done()
	}()

	return <-started
}

// tunnelShutdownGrace is how long the tunnel's ssh process group is given to
// exit after SIGTERM before it is SIGKILLed. It also bounds exec.Cmd's own
// bookkeeping (WaitDelay): if the group is somehow still around after the
// grace, the exec package SIGKILLs the leader and closes its pipes.
const tunnelShutdownGrace = 500 * time.Millisecond

// tunnelShutdownPollInterval is how often the shutdown grace re-checks whether
// the tunnel's process group is still alive.
const tunnelShutdownPollInterval = 25 * time.Millisecond

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

			// The 30s deadline belongs to the GetAgent RPC and NOTHING else.
			// It is cancelled as soon as the RPC returns — deliberately NOT
			// deferred: a deferred cancel never runs when the process is killed
			// by a signal, which is how the tunnel below used to stay wired to
			// an RPC deadline nobody was left to enforce (GAP-079).
			rpcCtx, cancelRPC := context.WithTimeout(context.Background(), 30*time.Second)

			req := connect.NewRequest(&v1.GetAgentRequest{AgentId: agentID})
			token := resolveToken(entry)
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			info, err := client.GetAgent(rpcCtx, req)
			cancelRPC()
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

			// The tunnel runs under its OWN signal-aware context with NO
			// deadline. It used to reuse the 30s GetAgent deadline above, which
			// (a) killed the ssh child ~30s after it opened — the help text
			// promises the foreground until interrupted — and (b) left the
			// child orphaned, because a signal killed the CLI outright and no
			// deferred cancel or reaper ever ran (GAP-079: orphaned root
			// ssh sessions holding the docker-sock forward with ppid=1).
			tunnelCtx, stopTunnel := signal.NotifyContext(context.Background(), tunnelShutdownSignals()...)
			defer stopTunnel()

			c := execCommandContext(tunnelCtx, parts[0], cmdArgs...)
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			// Own process group + group kill + kernel parent-death backstop:
			// see tunnel_proc_unix.go. All of it must be in place before the
			// child is started.
			configureTunnelCommand(c)
			c.Cancel = func() error { return terminateTunnelCommand(c) }
			c.WaitDelay = tunnelShutdownGrace

			// Start from an OS thread this goroutine pins for as long as the
			// tunnel lives: Pdeathsig is delivered when the THREAD that
			// created the child exits, so the starting thread has to outlive
			// the child (see startTunnelChild). Wait stays on the normal path,
			// exactly as exec.Cmd.Run does it internally.
			if err := startTunnelChild(c, tunnelCtx); err != nil {
				return err
			}

			runErr := c.Wait()
			if tunnelCtx.Err() != nil {
				// The tunnel was stopped by a signal (Ctrl-C, SIGTERM, SIGHUP):
				// that is the operator's intent, not a failure — and the child
				// has already been reaped above. Report a clean stop instead of
				// surfacing "context canceled" plus cobra usage.
				return nil
			}
			return runErr
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (default: active server)")
	cmd.Flags().StringVar(&sshHost, "ssh-host", "", "SSH host override (default: hostname from server config URL)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "SSH private key path (default: ~/.bunker/keys/<agent-id>)")
	return cmd
}
