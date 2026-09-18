//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// childShutdownSignals returns the signals that stop a command's long-lived
// children. The Go runtime on Windows can only deliver console Ctrl-C
// (os.Interrupt); SIGTERM and SIGHUP have no Windows console equivalent.
func childShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// newChildSignalContext mirrors the POSIX helper: a context cancelled by the
// shutdown signals plus the stop func that MUST be called (deferred).
func newChildSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), childShutdownSignals()...)
}

// configureDetachedChild is a no-op on Windows: POSIX process groups have no
// Windows equivalent (SysProcAttr uses job objects there) and there is no
// Pdeathsig field.
func configureDetachedChild(cmd *exec.Cmd) {}

// configureTerminalChild is a no-op on Windows (no Pdeathsig field, no POSIX
// process groups).
func configureTerminalChild(cmd *exec.Cmd) {}

// terminateChildGroup kills the child directly — there is no POSIX process
// group to signal on Windows. It keeps terminateChildGroup's contract: return
// nil once the child is gone (or already was).
func terminateChildGroup(cmd *exec.Cmd) error {
	return killChildProcess(cmd)
}

// terminateChildProcess kills the child directly; on Windows there is no group
// distinction. Same contract as the POSIX arms.
func terminateChildProcess(cmd *exec.Cmd) error {
	return killChildProcess(cmd)
}

func killChildProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
