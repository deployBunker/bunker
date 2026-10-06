package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// NewAttachCommand returns the `bunker attach` cobra command (GAP-072).
//
// The command is the interactive counterpart of `bunker exec`: instead of
// running one command and streaming its output, it opens a bidirectional
// stream to the agent and wires the operator's terminal to it - stdin,
// stdout/stderr and window-changes - through the daemon's existing SSH
// transport. No local SSH key or direct host access is needed; the daemon
// runs the session in the agent's own cgroup, so attach is not a bypass of the
// resource limits exec obeys.
func NewAttachCommand() *cobra.Command {
	var (
		serverName string
		command    string
		noTTY      bool
		idleSecs   uint32
	)

	cmd := &cobra.Command{
		Use:   "attach <agent-id> [--] [command [args...]]",
		Short: "Attach an interactive terminal to an agent session",
		Long: `Attach the current terminal to an agent session.

With no command, the session is the agent user's login shell - an interactive
REPL, the way ` + "`ssh`" + ` or ` + "`docker attach`" + ` drops you in. Any arguments
after the agent id are run as a command instead (use ` + "`--`" + ` before the command if
it takes flags), or pass --command for a shell command string.

The session runs through the daemon's SSH transport in the agent's own cgroup
and PAM namespace: the same user, private /tmp and resource limits an exec
gets. Nothing is widened by attaching.

When stdin is a terminal the CLI puts it into raw mode (so Ctrl-C, Ctrl-D and
vi/less work key-by-key) and forwards window resizes; use --no-tty to keep the
terminal cooked. The daemon records one audit record when the session opens
and one when it closes - never the keystrokes.

An idle session (no input and no output) is closed after --idle-timeout
seconds (server default 30 minutes), so a dropped client cannot leak a
session.

Examples:
  bunker attach abc12345
  bunker attach abc12345 --command 'cd /srv && bash'
  bunker attach abc12345 -- htop
  bunker attach abc12345 --command 'tail -f /var/log/syslog' --no-tty
  echo 'uname -a' | bunker attach abc12345`,

		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			// Two ways to name a session command, never both: --command takes a
			// whole shell command string, a trailing argv takes the command and
			// its arguments verbatim (cobra has already consumed the `--`
			// separator). With no command the session is the agent user's login
			// shell.
			sessionCommand := command
			var sessionArgs []string
			if len(args) > 1 {
				if command != "" {
					return fmt.Errorf("--command and positional command arguments are mutually exclusive")
				}
				sessionCommand = args[1]
				sessionArgs = args[2:]
			}

			// Load CLI config and resolve the server. Like the other
			// session verbs, the target is explicit (--server or the session
			// env var): an attach must never land on a stale shared default.
			cfg, err := LoadCLIConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved, berr := SessionScopedTarget(serverName, cfg.ActiveServer)
			if berr != nil {
				return berr
			}
			entry, ok := cfg.Servers[resolved]
			if !ok {
				return fmt.Errorf("server %q not found in config", resolved)
			}

			// stdinReader carries the operator's bytes; `in` is the same stream
			// as a file when it IS one, which is what the terminal ioctls
			// (raw mode, window size, resize) need.
			stdinReader := cmd.InOrStdin()
			in, _ := stdinReader.(*os.File)
			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			// A terminal session is the default when our own stdin is a
			// terminal; a piped stdin gets the plain-stream path (tty=false)
			// unless the operator explicitly asked otherwise.
			tty := !noTTY && in != nil && attachIsTerminal(in)

			// The session lives until the operator exits it or a manager
			// signals the CLI: a signal-aware context with NO deadline, the
			// same long-lived posture `bunker ssh` uses.
			ctx, stop := newChildSignalContext()
			defer stop()

			client := newBunkerdAttachClient(entry)
			stream := client.AttachAgent(ctx)
			if token := resolveToken(entry); token != "" {
				stream.RequestHeader().Set("Authorization", "Bearer "+token)
			}
			sender := &attachClientSender{stream: stream}

			var cols, rows uint32
			if tty {
				if c, r, ok := attachTerminalSize(in); ok {
					cols, rows = c, r
				}
			}
			if err := sender.send(&v1.AttachAgentRequest{
				Payload: &v1.AttachAgentRequest_Start{Start: &v1.AttachStart{
					AgentId:            agentID,
					Command:            sessionCommand,
					Args:               sessionArgs,
					Tty:                tty,
					Cols:               cols,
					Rows:               rows,
					IdleTimeoutSeconds: idleSecs,
				}},
			}); err != nil {
				return fmt.Errorf("attach: send start: %w", attachTransportHint(entry, err))
			}

			if tty {
				restore, rerr := attachMakeRaw(in)
				if rerr != nil {
					return fmt.Errorf("attach: put terminal into raw mode: %w", rerr)
				}
				// Restores the terminal before the error text (if any) is
				// printed, so the message is never eaten by raw mode.
				defer restore()
				if err := attachWatchResize(ctx, in, sender, errOut); err != nil {
					return err
				}
			}

			// Local stdin -> the session. Runs until stdin reaches EOF, at
			// which point the write side is half-closed so the remote command
			// sees EOF (a piped attach ends cleanly instead of hanging).
			go attachForwardStdin(stdinReader, sender)

			exitCode, reason, rerr := attachReceive(stream, out, errOut)
			if rerr != nil {
				if ctx.Err() != nil {
					// Stopped by a signal (a manager's SIGTERM/SIGHUP): the
					// session ends because the operator asked it to, not
					// because of a failure.
					return nil
				}
				return fmt.Errorf("attach: %w", attachTransportHint(entry, rerr))
			}

			switch reason {
			case "", "exited":
				// The command ended on its own; nothing to add.
			case "idle_timeout":
				_, _ = fmt.Fprintf(errOut, "bunker: attach session closed after %s with no input or output\n", attachIdleDescription(idleSecs))
			case "client_gone":
				_, _ = fmt.Fprintln(errOut, "bunker: attach session closed: the client went away")
			default:
				_, _ = fmt.Fprintf(errOut, "bunker: attach session ended: %s\n", reason)
			}
			if exitCode != 0 {
				return &ExitError{Code: exitCode}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&serverName, "server", "", "Server alias (required unless BUNKER_SESSION_TARGET is set; session commands never fall back to the shared active default)")
	cmd.Flags().StringVar(&command, "command", "", "Run this command instead of the agent's login shell")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Do not request a pseudo-terminal (no raw mode, no resize; stderr stays separate)")
	cmd.Flags().Uint32Var(&idleSecs, "idle-timeout", 0, "Close the session after this many seconds with no input or output (0 = server default 30m)")

	return cmd
}

// attachIdleDescription renders the idle note for an idle-timeout close.
func attachIdleDescription(seconds uint32) string {
	if seconds == 0 {
		return "30m0s (server default)"
	}
	return fmt.Sprintf("%ds", seconds)
}

// attachClientSender serializes writes to the attach stream. connect's
// BidiStream Send is not safe for concurrent use, and two goroutines send on
// this stream (stdin forwarding and window resizes).
type attachClientSender struct {
	mu     sync.Mutex
	stream *connect.BidiStreamForClient[v1.AttachAgentRequest, v1.AttachAgentResponse]
}

func (s *attachClientSender) send(msg *v1.AttachAgentRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(msg)
}

// attachForwardStdin copies the local stdin into the session until EOF (or a
// write failure), then half-closes the remote stdin. In raw mode the terminal
// hands bytes over as they are typed, which is what makes Ctrl-C/Ctrl-D and
// full-screen programs work.
func attachForwardStdin(in io.Reader, sender *attachClientSender) {
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if serr := sender.send(&v1.AttachAgentRequest{
				Payload: &v1.AttachAgentRequest_Stdin{Stdin: data},
			}); serr != nil {
				return
			}
		}
		if err != nil {
			// EOF: tell the session its input is done. The child sees EOF on
			// its stdin while its output keeps streaming (TTY sessions handle
			// EOF in-band via Ctrl-D instead, and the daemon ignores this
			// frame for them).
			_ = sender.send(&v1.AttachAgentRequest{Payload: &v1.AttachAgentRequest_StdinEof{StdinEof: true}})
			return
		}
	}
}

// attachWatchResize forwards terminal window-changes to the session until the
// context ends. It is a no-op on platforms without a resize signal. errOut
// carries a one-time note when the platform cannot report resizes at all.
func attachWatchResize(ctx context.Context, in *os.File, sender *attachClientSender, errOut io.Writer) error {
	sig := attachResizeSignal()
	if sig == nil {
		return nil
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sig)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				cols, rows, ok := attachTerminalSize(in)
				if !ok {
					continue
				}
				if err := sender.send(&v1.AttachAgentRequest{
					Payload: &v1.AttachAgentRequest_Resize{Resize: &v1.AttachResize{Cols: cols, Rows: rows}},
				}); err != nil {
					return
				}
			}
		}
	}()
	return nil
}

// attachReceive pumps the session's frames to the local streams until the
// server sends the final exit frame (or the stream ends). It returns the exit
// code and the close reason the daemon reported.
func attachReceive(stream *connect.BidiStreamForClient[v1.AttachAgentRequest, v1.AttachAgentResponse], out, errOut io.Writer) (int, string, error) {
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The daemon closed the stream without an exit frame: report
				// the session as ended rather than hanging the caller.
				return 0, "", nil
			}
			return 0, "", err
		}
		switch p := msg.GetPayload().(type) {
		case *v1.AttachAgentResponse_Stdout:
			if _, werr := out.Write(p.Stdout); werr != nil {
				return 0, "", werr
			}
		case *v1.AttachAgentResponse_Stderr:
			if _, werr := errOut.Write(p.Stderr); werr != nil {
				return 0, "", werr
			}
		case *v1.AttachAgentResponse_Exit:
			return int(p.Exit.GetExitCode()), p.Exit.GetReason(), nil
		}
	}
}
