package cli

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

// This file is the ONE long-lived-child lifecycle used by every CLI command
// that spawns a local child which can outlive the CLI: `tunnel` (GAP-079) and
// `cp`, `deploy`, `mount`, `ssh` (GAP-084).
//
// The defect class, measured once and then repeated at four more sites: the
// child was spawned with a context the command cancelled with `defer cancel()`,
// i.e. a teardown that runs only when THIS process decides to return. A signal
// (Ctrl-C, a manager's SIGTERM, a SIGHUP after a detached start) killed the CLI
// outright, so no deferred function, no context cancellation and no reaper ever
// ran and the child was reparented to init — still holding whatever it held
// (the tunnel's "-L 2376:/run/bunker/<agent>/docker.sock" forward; scp's
// half-finished transfer; an in-flight sshfs mount attempt; an interactive root
// ssh session).
//
// Three mechanisms, applied together, cover every way the CLI can stop:
//
//  1. a signal-aware context (newChildSignalContext) turns SIGINT/SIGTERM/
//     SIGHUP into a context cancellation, so the ordinary Go path runs the
//     teardown below instead of the process dying mid-syscall;
//  2. an own process GROUP plus a group-wide SIGTERM-then-SIGKILL teardown —
//     killing only the direct child leaves everything it spawned behind (a
//     wrapper shell, scp's own ssh, a `sleep`, sshfs's helper);
//  3. a kernel parent-death backstop (Pdeathsig; Linux/FreeBSD) for the path
//     NOTHING in this process can cover: SIGKILL, the OOM killer, a manager's
//     hard stop. That signal is delivered when the THREAD that created the
//     child exits — not when the process does — so every start below goes
//     through startLongLivedChild, which pins its OS thread for as long as the
//     child lives. Without the pin, a Go program would arm a random-kill timer
//     (goroutines do not own their OS threads) instead of a backstop.

// childShutdownGrace is how long a child (or its process group) is given to
// exit after SIGTERM before it is SIGKILLed. It also bounds exec.Cmd's own
// bookkeeping (WaitDelay): if the group somehow survives the grace, the exec
// package SIGKILLs the leader and closes its pipes instead of letting Wait
// block forever.
const childShutdownGrace = 500 * time.Millisecond

// childShutdownPollInterval is how often the teardown grace re-checks whether
// the child (or its group) is still alive.
const childShutdownPollInterval = 25 * time.Millisecond

// startLongLivedChild starts cmd from a goroutine that pins its OS thread and
// holds that pin until one of these happens:
//
//   - cmd.Start() fails (the error is returned to the caller);
//   - ctx is done (the caller's teardown released the child);
//   - stop is closed.
//
// Either of ctx and stop may be nil; a nil channel blocks forever and a nil ctx
// is treated as never-cancelled, so a caller picks the arm that matches who
// knows when the child's life is over — the tunnel parks on its own context
// (RunE cancels it on the way out), the runners park on stop, which they close
// after Wait has reaped the child.
//
// WHY the pin is load-bearing (GAP-079/GAP-084): the child may be armed with
// Pdeathsig, and the kernel delivers that signal when the THREAD that created
// the child terminates — prctl(2) PR_SET_PDEATHSIG says so explicitly ("the
// signal will be sent when that thread terminates ... rather than after all of
// the threads in the parent process terminate"). Go's goroutines are
// multiplexed over OS threads whose lifetime is NOT tied to the process
// (runtime.LockOSThread's contract is that a locked goroutine which exits
// without unlocking has its thread torn down by mstart0), and the forking
// thread is whichever M runs Start(). A bare Pdeathsig field would therefore
// kill the child at a random moment — when the thread that forked it goes away
// — instead of at CLI death. Pinning is the difference between a working
// backstop and a random-kill bug.
func startLongLivedChild(cmd *exec.Cmd, ctx context.Context, stop <-chan struct{}) error {
	if ctx == nil {
		ctx = context.Background()
	}

	started := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil

		select {
		case <-ctx.Done():
		case <-stop:
		}
	}()

	return <-started
}

// newLongLivedCommand builds the command form the two runners below require: a
// CommandContext command.
//
// Both runners override cmd.Cancel (exec's default kills only the leader, which
// is exactly the leak this file exists to fix), and the exec package only
// accepts a non-nil Cancel on a command created through CommandContext
// ("exec: command with a non-nil Cancel was not created with CommandContext",
// exec.go:699). This constructor exists so that requirement cannot be forgotten
// at a call site — and so a test that builds a fixture command does not hit the
// error at Start instead of at the assertion it was written for.
func newLongLivedCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runChildCommand runs cmd to completion under the long-lived-child contract:
// configure() arms the child's group/parent-death posture, the context the
// command was built with terminates it through terminate(), the start is
// thread-pinned, and Wait reaps it on the caller's goroutine (exactly what
// exec.Cmd.Run does internally).
//
// The cancellation path is where the defect lived, so it is not left to exec's
// default: CommandContext installs Cancel = Process.Kill, which kills only the
// LEADER and leaves everything the child started running. Replacing it with
// terminate() is what makes a SIGTERM end the whole subtree. Done this way the
// teardown is also IN FRONT of Wait: exec's ctx watcher calls Cancel and only
// then hands Wait the cancellation result (exec.go:805 + :937), so the group is
// already dead when the caller resumes.
//
// cmd MUST have been created with a context (newLongLivedCommand): the
// teardown is driven by THAT context, not by a second one passed here.
func runChildCommand(
	cmd *exec.Cmd,
	configure func(*exec.Cmd),
	terminate func(*exec.Cmd) error,
) error {
	configure(cmd)
	cmd.Cancel = func() error { return terminate(cmd) }
	cmd.WaitDelay = childShutdownGrace

	// The pin is held for the child's whole life and released once Wait has
	// reaped it: after that the parent-death signal has nothing left to guard.
	stop := make(chan struct{})
	defer close(stop)

	if err := startLongLivedChild(cmd, nil, stop); err != nil {
		return err
	}
	return cmd.Wait()
}

// runDetachedChildCommand runs a child that owns its own process group: it is
// not a terminal user and it may start descendants (scp's own ssh, a wrapper
// shell, sshfs's helper), so the teardown must be a group kill.
func runDetachedChildCommand(cmd *exec.Cmd) error {
	return runChildCommand(cmd, configureDetachedChild, terminateChildGroup)
}

// runTerminalChildCommand runs a child that must stay in the CLI's terminal
// foreground process group (the interactive ssh session) and is therefore
// terminated directly rather than by group.
//
// WHY no own group here: a process in a background process group of the
// controlling terminal is STOPPED by the kernel on tcsetattr and read
// (SIGTTOU/SIGTTIN, POSIX "Terminal Access Control"), and the interactive ssh
// child does both — it puts the terminal into raw mode and reads fd 0 to
// forward keystrokes. Giving it its own group would freeze the session instead
// of forwarding Ctrl-C, which is the one behaviour this command must not
// regress. Measured, 3/3 runs, with a pty probe (session leader holding the
// controlling terminal + two grandchildren reading the tty): the grandchild
// that moved itself to its own process group was reported by
// /proc/<pid>/stat as state 'T' (stopped by SIGTTIN), the one left in the
// foreground group as state 'S' (blocked on read).
func runTerminalChildCommand(cmd *exec.Cmd) error {
	return runChildCommand(cmd, configureTerminalChild, terminateChildProcess)
}
