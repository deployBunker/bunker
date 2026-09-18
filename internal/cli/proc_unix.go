//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// childShutdownSignals returns the signals that stop a command's long-lived
// children.
//
// SIGTERM/SIGHUP sit next to Ctrl-C because the CLI is routinely started
// detached (nohup/setsid) and stopped by a manager: the live battery's
// "kill $PID" is a SIGTERM, exactly like an init-system stop, and a detached
// process receives a SIGHUP when its session goes away.
func childShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

// newChildSignalContext returns a context that is cancelled by the shutdown
// signals, together with the stop func that unregisters the handler.
//
// The stop func is ALWAYS non-nil and MUST be called (defer it): leaving a
// signal.NotifyContext handler installed changes what the rest of the process
// does with SIGTERM/SIGHUP, and each command's RunE owns exactly one.
func newChildSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), childShutdownSignals()...)
}

// configureDetachedChild arms a child that owns its own process group with the
// group SIGTERM/SIGKILL teardown in terminateChildGroup and the kernel
// parent-death backstop.
//
// Must be applied BEFORE the child is started (Start is what installs both);
// callers reach it through runDetachedChildCommand.
func configureDetachedChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Pdeathsig is Linux/FreeBSD-only, so it lives behind its own build tag
	// (proc_pdeathsig_supported.go) while this file keeps the POSIX arm.
	configureChildPdeathsig(cmd)
}

// configureTerminalChild arms the parent-death backstop for a child that stays
// in the CLI's own process group (see runTerminalChildCommand for why it does
// not get a group of its own).
func configureTerminalChild(cmd *exec.Cmd) {
	configureChildPdeathsig(cmd)
}

// terminateChildGroup stops a child's whole process group: SIGTERM first so the
// child can close what it holds politely (a half-finished transfer is
// aborted, ssh closes the session), a bounded grace, then SIGKILL as the
// backstop for a child that ignores SIGTERM.
//
// The negative pid must be the pid of THIS child: the leading '-' is what makes
// the signal a group signal, and the leader is not reaped until cmd.Wait runs
// (after this function returns), so pid recycling cannot bite here.
func terminateChildGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	// Bounded grace. The leader is still unreaped at this point, so a group
	// containing only the zombie leader reads as "alive" and the loop runs its
	// full budget; it short-circuits only when every member is gone.
	deadline := time.Now().Add(childShutdownGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return nil
		}
		time.Sleep(childShutdownPollInterval)
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// terminateChildProcess terminates the DIRECT child only, for children that
// share the CLI's process group (see runTerminalChildCommand): signalling
// "-pgid" there would reach the CLI itself and its shell.
//
// Same contract as terminateChildGroup: SIGTERM, a bounded grace, then SIGKILL.
// The caller's Wait reaps the child, so the loop polls the pid instead of
// waiting on it.
func terminateChildProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	deadline := time.Now().Add(childShutdownGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(childShutdownPollInterval)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
