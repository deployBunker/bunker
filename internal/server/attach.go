// GAP-072: interactive attach.
//
// ExecAgent streams a command's output but has no stdin path, so an operator
// cannot get a terminal into an agent session - the flagship use of an
// AI-agent sandbox (`bunker attach <id>`, dropping in the way `docker attach`
// or `ssh` does). This file implements the daemon half: one bidirectional
// stream carries a whole session (stdin, stdout, stderr and terminal
// window-changes) over the transport that already exists, ssh.
//
// Three properties are structural rather than best-effort:
//
//   - attach is NOT a bypass. The session is the SAME ssh session an exec
//     uses: same agent user (`bunker-<id>`), same PAM namespace (so the same
//     private /tmp), same cgroup and resource limits. The only difference is
//     that a pseudo-terminal is allocated (tty=true) and stdin flows to the
//     child. Nothing here widens what an exec could already reach.
//   - attach is audit-recorded. One `attach open` and one `attach close`
//     record are appended to the same hash-chained audit log the interceptor
//     writes, through internal/audit (the single writer). The records carry
//     the redacted command, the tty flag, the close reason, the exit code and
//     the duration - and never a byte of what the operator typed.
//   - attach cannot leak a session. An idle watchdog closes a session that
//     has seen no client frame and no child output for the idle bound
//     (default 30 minutes, per-request overridable), and every exit path -
//     clean exit, idle timeout, client cancellation, handler panic - kills the
//     child and closes the pseudo-terminal.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"

	"github.com/deployBunker/bunker/internal/agent"
	"github.com/deployBunker/bunker/internal/audit"
)

const (
	// attachDefaultCols/Rows are the terminal size a session starts at when
	// the client does not send one (a non-tty client, or a terminal whose size
	// could not be read).
	attachDefaultCols uint32 = 80
	attachDefaultRows uint32 = 24

	// attachIdleTimeout is the default idle bound: a session that has seen
	// neither a client frame nor any child output for this long is closed.
	// It is the guard against an abandoned attach (a dropped client that never
	// sent anything, a laptop that went to sleep) leaking an ssh session in
	// the agent's cgroup forever.
	attachIdleTimeout = 30 * time.Minute
	// attachMaxIdleTimeout caps a per-request override. A client may ask for a
	// shorter bound than the default but cannot disable the guard.
	attachMaxIdleTimeout = 24 * time.Hour

	// attachFirstFrameTimeout bounds how long the handler waits for the
	// mandatory AttachStart frame after the stream is accepted. Without it a
	// client could hold a handler goroutine (and an authenticated stream) open
	// by never sending anything.
	attachFirstFrameTimeout = 30 * time.Second

	// attachChildKillGrace is how long a terminated child (ssh) is given to
	// exit on SIGTERM before it is SIGKILLed, and the equivalent bound on how
	// long the handler waits for the child and its output pumps to settle.
	attachChildKillGrace = 10 * time.Second

	// attachExitIdleTimeout / attachExitClientGone are the AttachExit exit
	// codes for the two abnormal endings. 124 is timeout(1)'s conventional
	// "command timed out" status; -1 marks a session whose child was never
	// reaped because the client went away.
	attachExitIdleTimeout int32 = 124
	attachExitClientGone  int32 = -1

	// attachReadChunk is the per-frame read size off the child's output. It
	// matches the exec path's stream chunking closely enough that both feel
	// the same interactively; the frames are small because a terminal types a
	// character at a time.
	attachReadChunk = 32 * 1024
)

// AttachAgent is the GAP-072 bidirectional interactive attach handler.
//
// Protocol: the FIRST frame the client sends MUST be an AttachStart; every
// later frame is stdin data, a terminal window-change, or a stdin half-close.
// The server streams stdout/stderr frames as the child produces them and
// finishes with exactly one AttachExit frame.
//
// Error surface (mirrors ExecAgent so a client cannot confuse the two):
//
//   - CodeInvalidArgument  - no/!start first frame, or an empty agent id;
//   - CodeNotFound         - the agent does not exist;
//   - CodeFailedPrecondition + agent_stopped - the agent exists but is
//     stopped; attach must not silently start a session against it (GAP-071).
func (s *bunkerdService) AttachAgent(ctx context.Context, stream *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	firstCtx, cancelFirst := context.WithTimeout(ctx, attachFirstFrameTimeout)
	defer cancelFirst()
	first, err := receiveAttachFrame(firstCtx, stream)
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("attach: the first frame must be a start message"))
	}
	agentID := start.GetAgentId()
	if agentID == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("attach: agent_id is required"))
	}

	// Stamp the target agent id into the streaming audit sink so the audit
	// interceptor records agent_id=<aid> for this attach even for master
	// tokens (the DOGFOOD-012 wiring ExecAgent uses). Must happen before any
	// early return so error paths are covered too.
	audit.StampStreamAgentID(ctx, agentID)

	rec := s.tracker.Get(agentID)
	if rec == nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("agent %q not found", agentID))
	}
	// GAP-071: a stopped agent still EXISTS, and an attach must not be
	// attempted against it - the client must be able to tell "stopped" from
	// "gone" (CodeFailedPrecondition + agent_stopped, never CodeNotFound).
	if err := agent.StoppedStatusError(rec, agentID); err != nil {
		return stoppedPreconditionError(err)
	}

	tty := start.GetTty()
	cols, rows := start.GetCols(), start.GetRows()
	if cols == 0 {
		cols = attachDefaultCols
	}
	if rows == 0 {
		rows = attachDefaultRows
	}

	userHome := "/home/bunker-" + agentID
	disclosed := s.cfg != nil && s.cfg.Containment.Disclosure
	cmd := buildAttachSSHCommand(ctx, agentID, rec.SshPrivateKeyPath, userHome,
		start.GetCommand(), start.GetArgs(), disclosed, rec.Image, tty)

	proc, err := startAttachProcess(cmd, tty, cols, rows)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("attach: start session: %w", err))
	}
	// Whatever happens below - clean exit, idle timeout, client cancellation,
	// a panic in a pump - the child does not outlive the handler, and the
	// session's own descriptors are released after it is gone. The deferred
	// order is deliberate: terminate (declared last) runs first, release
	// second.
	defer proc.release()
	defer proc.terminate()

	// tty may have been downgraded by startAttachProcess when no PTY could be
	// allocated; the audit record and the pumps must reflect what actually
	// runs, not what was asked for.
	tty = proc.tty

	sender := &attachStreamSender{stream: stream, logger: s.logger}
	defer sender.close()

	started := time.Now()
	audit.RecordAttachEvent(ctx, s.auditLog, s.logger, audit.AttachRecord{
		AgentID: agentID,
		Phase:   audit.AttachPhaseOpen,
		TTY:     tty,
		Command: attachCommandSummary(start),
	})

	// ONE close record per session, whatever path ends it. The deferred call
	// is the safety net (client cancellation, a panic); the explicit calls
	// below record the accurate reason first, and the once-mutex makes the
	// deferred one a no-op.
	var closeOnce sync.Once
	recordClose := func(reason string, exitCode *int32) {
		closeOnce.Do(func() {
			audit.RecordAttachEvent(ctx, s.auditLog, s.logger, audit.AttachRecord{
				AgentID:    agentID,
				Phase:      audit.AttachPhaseClose,
				TTY:        tty,
				Command:    attachCommandSummary(start),
				Reason:     reason,
				ExitCode:   exitCode,
				DurationMS: time.Since(started).Milliseconds(),
			})
		})
	}
	defer recordClose(audit.AttachReasonClientGone, nil)

	// Activity drives the idle watchdog: any client frame and any child output
	// count as life. A single buffered channel conflates the two without a
	// mutex, and the watchdog re-arms its timer on every signal.
	activity := make(chan struct{}, 1)
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	idleFired := make(chan struct{})
	watchdogStop := make(chan struct{})
	defer close(watchdogStop)
	go attachIdleWatchdog(attachIdleBound(start.GetIdleTimeoutSeconds()), activity, idleFired, watchdogStop)

	// Output pumps: stdout always, stderr only when the two are separate (a
	// PTY merges them, exactly as a terminal does). Every frame goes through
	// the ONE mutex-guarded sender - connect's Send is not safe for
	// concurrent use, and the same rule already governs the exec path
	// (DF-BUNKER-27).
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		attachPump(sender, proc.stdout, false, touch)
	}()
	if proc.stderr != nil {
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			attachPump(sender, proc.stderr, true, touch)
		}()
	}

	// Client frames. This goroutine is the only reader of the stream; it exits
	// when the client half-closes (io.EOF), when the client cancels the
	// stream, or on a frame error.
	go func() {
		for {
			msg, err := stream.Receive()
			if err != nil {
				return
			}
			touch()
			switch p := msg.GetPayload().(type) {
			case *v1.AttachAgentRequest_Stdin:
				if w := proc.stdinWriter(); w != nil {
					if _, werr := w.Write(p.Stdin); werr != nil {
						// The child's stdin is gone (it exited or the pipe
						// broke); stop reading. Its exit is what ends the
						// session.
						return
					}
				}
			case *v1.AttachAgentRequest_Resize:
				// A resize on a session without a PTY has nothing to resize:
				// ignore it rather than fail the session the operator is in.
				if proc.tty {
					if rerr := proc.resize(p.Resize.GetCols(), p.Resize.GetRows()); rerr != nil {
						s.logger.Debug("attach: resize failed", "error", rerr)
					}
				}
			case *v1.AttachAgentRequest_StdinEof:
				// A half-close on a PTY has no meaning (a terminal's EOF is
				// the terminal driver's job, and the client sends Ctrl-D as
				// data); on plain pipes it is how a piped attach ends input.
				if !proc.tty {
					proc.closeStdin()
				}
			case *v1.AttachAgentRequest_Start:
				// A second start frame is a client bug. The session is already
				// running; ignore it rather than tear down a live terminal.
				s.logger.Warn("attach: duplicate start frame ignored", "agent_id", agentID)
			}
		}
	}()

	waited := make(chan error, 1)
	go func() { waited <- proc.wait() }()

	var reason string
	var exitCode int32
	select {
	case <-waitDone(waited):
		// The command finished on its own. Wait - BOUNDED - until both output
		// pumps have drained, so the exit frame follows the last output frame.
		// The bound matters: a descendant of the child that inherited the
		// session's stdout pipe (or the PTY's slave) would otherwise keep the
		// pumps parked and the client waiting for an exit frame that never
		// comes.
		reason = audit.AttachReasonExited
		exitCode = attachExitCode(proc.waitErr())
		if !waitBounded(pumps.Wait, attachChildKillGrace) {
			s.logger.Warn("attach: output did not drain after the session exited; a descendant is holding the session's descriptors",
				"agent_id", agentID)
		}
	case <-idleFired:
		reason = audit.AttachReasonIdleTimeout
		exitCode = attachExitIdleTimeout
		s.logger.Warn("attach: closing idle session", "agent_id", agentID,
			"idle_timeout", attachIdleBound(start.GetIdleTimeoutSeconds()).String())
		proc.terminate()
		waitBounded(pumps.Wait, attachChildKillGrace)
	case <-ctx.Done():
		reason = audit.AttachReasonClientGone
		exitCode = attachExitClientGone
		proc.terminate()
		// Nobody is there to receive frames; do not wait for the pumps.
	}

	exit := &exitCode
	recordClose(reason, exit)
	sender.send("attach exit", &v1.AttachAgentResponse{
		Payload: &v1.AttachAgentResponse_Exit{Exit: &v1.AttachExit{ExitCode: exitCode, Reason: reason}},
	})
	return nil
}

// receiveAttachFrame reads one frame from the stream with a bound, so a client
// that opens a stream and sends nothing cannot hold the handler open forever.
// The receive happens on its own goroutine because connect's Receive takes no
// context; the buffered result channel lets that goroutine exit cleanly when
// the caller has given up (the stream is closed by connect as the handler
// returns).
func receiveAttachFrame(ctx context.Context, stream *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) (*v1.AttachAgentRequest, error) {
	type result struct {
		msg *v1.AttachAgentRequest
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := stream.Receive()
		ch <- result{msg: msg, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("attach: stream closed before a start frame"))
			}
			return nil, r.err
		}
		return r.msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// attachCommandSummary renders the redacted command summary for an attach
// record. An empty command means the login shell (recorded as such by the
// audit writer); a command is scrubbed exactly as exec scrubs one, because an
// attach command is the same class of value (GAP-142's redaction posture).
func attachCommandSummary(start *v1.AttachStart) string {
	if start == nil || start.GetCommand() == "" {
		return ""
	}
	return audit.RedactCommandSummary(start.GetCommand(), start.GetArgs())
}

// attachIdleBound resolves the session's idle bound: the server default when
// the client asked for none, the client's value otherwise, never more than
// attachMaxIdleTimeout. There is deliberately no way to disable the guard.
func attachIdleBound(seconds uint32) time.Duration {
	if seconds == 0 {
		return attachIdleTimeout
	}
	bound := time.Duration(seconds) * time.Second
	if bound > attachMaxIdleTimeout {
		return attachMaxIdleTimeout
	}
	return bound
}

// attachIdleWatchdog closes idleFired when neither a client frame nor any
// child output has arrived for the idle bound. It re-arms on every activity
// signal and exits when stop is closed.
func attachIdleWatchdog(idle time.Duration, activity <-chan struct{}, idleFired chan<- struct{}, stop <-chan struct{}) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			close(idleFired)
			return
		case <-stop:
			return
		}
	}
}

// attachStreamSender is the ONLY path to the attach stream's frames, for the
// same reason execStreamSender is: connect's Send is not safe for concurrent
// use, and two output pumps plus the exit frame write the same response.
// close() marks the stream done so a pump that finishes after the handler
// returned cannot log a spurious send failure.
type attachStreamSender struct {
	mu     sync.Mutex
	stream *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]
	logger *slog.Logger
	closed bool
}

func (w *attachStreamSender) send(label string, msg *v1.AttachAgentResponse) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	if err := w.stream.Send(msg); err != nil {
		w.logger.Warn(label, "error", err)
	}
}

func (w *attachStreamSender) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// attachPump forwards one of the child's output pipes to the stream as frames
// until EOF, bumping the idle watchdog on every chunk.
func attachPump(sender *attachStreamSender, r io.Reader, stderr bool, touch func()) {
	buf := make([]byte, attachReadChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// The frame owns its bytes: the read buffer is reused next lap.
			frame := make([]byte, n)
			copy(frame, buf[:n])
			msg := &v1.AttachAgentResponse{}
			if stderr {
				msg.Payload = &v1.AttachAgentResponse_Stderr{Stderr: frame}
			} else {
				msg.Payload = &v1.AttachAgentResponse_Stdout{Stdout: frame}
			}
			sender.send("attach output", msg)
			touch()
		}
		if err != nil {
			return
		}
	}
}

// attachPTY is an allocated pseudo-terminal pair: the slave is handed to the
// child (as its stdin/stdout/stderr), the master is what the server reads
// output from, writes stdin to and resizes. The struct is declared here with
// portable fields so the handler compiles on every platform; openAttachPTY and
// resize live in platform files, and where PTYs are unavailable the allocator
// returns errAttachPTYUnsupported and the session falls back to pipes.
type attachPTY struct {
	master *os.File
	slave  *os.File
}

// attachProcess is one running attach child plus the plumbing the handler
// needs: a master PTY (tty) or three pipes (no tty).
type attachProcess struct {
	cmd *exec.Cmd
	tty bool

	pty    *attachPTY    // non-nil in PTY mode; master is read/written
	stdinW *os.File      // non-tty: the child's stdin write end
	stdout io.ReadCloser // PTY master, or the child's stdout read end
	stderr io.ReadCloser // nil in PTY mode (the terminal merges them)

	stdinMu     sync.Mutex
	stdinClosed bool

	waitOnce sync.Once
	waitCh   chan error
	err      error
	killTmr  *time.Timer
}

// startAttachProcess starts cmd with the requested terminal posture.
//
// tty=true allocates a pseudo-terminal, hands the slave to the child and keeps
// the master: that is what makes vi/less/stty and window-change forwarding
// work, and it is why stderr is merged (a terminal has one stream). If no PTY
// can be allocated (a host or container without /dev/ptmx) the call FALLS BACK
// to the pipe path rather than failing the attach - the session still works,
// it just has no terminal - and the process reports tty=false so the handler
// records and serves what is actually running.
//
// The pipes are created here rather than with cmd.StdoutPipe/StdinPipe so
// cmd.Wait never closes an fd a reader is still using (the os/exec hazard
// documented for Wait); the parent closes only its copies of the child's ends.
func startAttachProcess(cmd *exec.Cmd, tty bool, cols, rows uint32) (*attachProcess, error) {
	if tty {
		pty, err := openAttachPTY(cols, rows)
		if err == nil {
			cmd.Stdin = pty.slave
			cmd.Stdout = pty.slave
			cmd.Stderr = pty.slave
			cmd.SysProcAttr = attachTTYSysProcAttr()
			if serr := cmd.Start(); serr != nil {
				pty.close()
				return nil, serr
			}
			// The child owns the slave now; the parent holds only the master.
			_ = pty.slave.Close()
			return &attachProcess{cmd: cmd, tty: true, pty: pty, stdout: pty.master, waitCh: make(chan error)}, nil
		}
		// Fall through to the pipe path: an attach without a terminal beats no
		// attach at all. The caller logs nothing here because the process's tty
		// flag is the report.
	}

	// closeAll releases the pipe ends this path created. Close errors are
	// deliberately ignored: the descriptors were ours, the operations they
	// guard have already been decided, and the caller is on an error path.
	closeAll := func(files ...*os.File) {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, err
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	// Its own process group, so teardown can signal the whole session (see
	// attachProcess.terminate).
	cmd.SysProcAttr = attachPipeSysProcAttr()
	if serr := cmd.Start(); serr != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, serr
	}
	// The child holds the other ends; close the parent's copies so the child
	// sees its natural EOF and the readers see EOF when it exits.
	closeAll(inR, outW, errW)
	return &attachProcess{cmd: cmd, tty: false, stdinW: inW, stdout: outR, stderr: errR, waitCh: make(chan error)}, nil
}

// stdinWriter returns the write end for client stdin (nil once closed/absent).
//
// With a PTY there is no separate stdin pipe: the terminal IS the input, so
// the master serves both directions (closing it would end the session, not
// just its input - see closeStdin).
func (p *attachProcess) stdinWriter() io.Writer {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	if p.stdinClosed {
		return nil
	}
	if p.pty != nil {
		return p.pty.master
	}
	return p.stdinW
}

// closeStdin half-closes the child's stdin: the child sees EOF while its
// output keeps streaming. Idempotent.
//
// On a PTY session this only marks the input closed: a pseudo-terminal has one
// duplex channel, so there is no half-close that would not also end the output
// side. A terminal's EOF is in-band (Ctrl-D), which the remote terminal driver
// turns into the child's EOF.
func (p *attachProcess) closeStdin() {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	p.stdinClosed = true
	if p.stdinW != nil {
		_ = p.stdinW.Close()
		p.stdinW = nil
	}
}

// resize applies a window-change to the session's PTY. A no-op without one.
func (p *attachProcess) resize(cols, rows uint32) error {
	if p.pty == nil {
		return nil
	}
	return p.pty.resize(cols, rows)
}

// release closes the session's own file descriptors. It must run only after
// the output pumps have stopped: the read ends it closes are what those pumps
// were reading from, and closing them early would truncate the tail.
func (p *attachProcess) release() {
	if p.pty != nil {
		p.pty.close()
		p.pty = nil
		return
	}
	if p.stdout != nil {
		_ = p.stdout.Close()
		p.stdout = nil
	}
	if p.stderr != nil {
		_ = p.stderr.Close()
		p.stderr = nil
	}
}

// wait reaps the child once, however many callers ask.
func (p *attachProcess) wait() error {
	p.waitOnce.Do(func() {
		p.err = p.cmd.Wait()
		if p.killTmr != nil {
			p.killTmr.Stop()
		}
		close(p.waitCh)
	})
	<-p.waitCh
	return p.err
}

// waitErr reports the reap result without blocking; only meaningful after
// wait() has returned.
func (p *attachProcess) waitErr() error { return p.err }

// terminate ends the session: SIGTERM the child's process group (ssh closes
// the remote session on it), SIGKILL the group if it has not exited within
// attachChildKillGrace, and half-close stdin. It is idempotent and safe to
// call from a defer.
//
// The GROUP is signaled, not just ssh: both start paths put the child in its
// own group (a PTY session is its own session via Setsid, the pipe path sets
// Setpgid), and a descendant left holding the session's pty or pipes would
// keep the session alive - and the handler's output pumps blocked - after ssh
// itself is gone.
//
// It deliberately does NOT close the output pipes: the child's own exit closes
// them, which is what lets a reader take the last buffered bytes before EOF.
func (p *attachProcess) terminate() {
	p.closeStdin()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
		return
	}
	proc := p.cmd.Process
	attachSignalChildGroup(proc)
	if p.killTmr == nil {
		p.killTmr = time.AfterFunc(attachChildKillGrace, func() {
			attachKillChildGroup(proc)
		})
	}
}

// waitDone returns a channel closed when the child has been reaped. It is a
// helper so the handler's select can wait on the same channel the reap
// goroutine writes without reading the value twice.
func waitDone(waited <-chan error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		<-waited
		close(done)
	}()
	return done
}

// waitBounded runs fn and reports whether it finished within d. It exists for
// the abort paths: a session that is being torn down must not hang the handler
// on a pipe nobody will ever close.
func waitBounded(fn func(), d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// attachExitCode maps a reap result to an exit status: 0 for a clean exit, the
// process's code for a non-zero exit, -1 for anything else (a signal death
// with no code).
func attachExitCode(err error) int32 {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return int32(code)
		}
	}
	return -1
}

// buildAttachSSHCommand builds the ssh command an attach runs through the
// agent's existing SSH transport.
//
// Two shapes, deliberately:
//
//   - no command: the remote command is omitted entirely, so sshd starts the
//     agent user's LOGIN shell (`ssh host` semantics) - the interactive REPL
//     `bunker attach <id>` opens. Its environment (DOCKER_HOST, TMPDIR) comes
//     from the agent's ~/.profile, which manager_spawn writes for exactly this
//     case.
//   - a command: the remote command is the SAME env-wrapped, agent-relative
//     command an exec builds (including the image-spec variant), so an attach
//     with --command runs exactly what `bunker exec` would, attached.
//
// -tt forces a remote PTY (so the remote side is a terminal even though our
// ssh child's own stdin is a local PTY we allocated); -T explicitly disables
// one on the non-tty path.
//
// NOTE on image-backed agents: a command-less attach opens the agent's host
// login shell, not a container (an exec container is created per command and
// is not a session you can attach to); pass a command to run inside the image.
func buildAttachSSHCommand(ctx context.Context, agentID, sshKeyPath, userHome, command string, args []string, disclosed bool, imageRef string, tty bool) *exec.Cmd {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "IdentitiesOnly=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-i", sshKeyPath,
	}
	if tty {
		sshArgs = append(sshArgs, "-tt")
	} else {
		sshArgs = append(sshArgs, "-T")
	}
	sshArgs = append(sshArgs, fmt.Sprintf("bunker-%s@localhost", agentID))

	if command != "" {
		var wrapped string
		if imageRef != "" {
			wrapped = buildAgentImageExecCommand(agentID, userHome, command, args, disclosed, imageRef)
		} else {
			wrapped = buildAgentExecCommand(agentID, userHome, command, args, disclosed)
		}
		sshArgs = append(sshArgs, fmt.Sprintf("sh -c %s", shellQuoteSingle(wrapped)))
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	// The child is long-lived and interactive: on context cancellation give
	// ssh a chance to close the remote session cleanly (its whole group, so a
	// descendant cannot keep the session's descriptors open), then kill it.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		attachSignalChildGroup(cmd.Process)
		return nil
	}
	cmd.WaitDelay = attachChildKillGrace
	return cmd
}
