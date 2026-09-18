//go:build unix

package tunnel

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// configureTunnelCommand puts the cloudflared child in its OWN process group so
// the negative-pid kill in stopTunnelCommand reaches only the processes this
// daemon started — never bunkerd itself nor its shell — and arms the
// parent-death backstop that covers the case nothing in this process can run
// (GAP-084).
//
// Must be applied BEFORE the child is started, and the child must be started
// through childPin.start (childpin.go): Pdeathsig fires when the THREAD that
// created the child exits, so the arming is only meaningful with the pin.
func configureTunnelCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Pdeathsig is Linux/FreeBSD-only, so it lives behind its own build tag
	// (process_pdeathsig_supported.go) while this file keeps the POSIX arm.
	configureTunnelPdeathsig(cmd)
}

// stopTunnelCommand stops the tunnel's whole process group: SIGTERM first so
// cloudflared can deregister politely, then a bounded grace, then SIGKILL as
// the backstop for a child that ignores SIGTERM — and releases the pin that
// kept the parent-death signal armed.
//
// The negative pid must be the pid of THIS child: the leading '-' is what makes
// the signal a group signal. Pid recycling cannot bite here because the leader
// is not reaped until cmd.Wait runs below.
func stopTunnelCommand(cmd *exec.Cmd, cancel context.CancelFunc, pin *childPin) error {
	// The pin is released only after Wait: while the child is alive, the
	// thread that created it must stay alive or Pdeathsig would SIGKILL the
	// child out from under a healthy daemon.
	defer pin.release()

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
		err := syscall.Kill(-pid, syscall.SIGTERM)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			cancel()
			return cmd.Wait()
		}
	}
	cancel()
	err := cmd.Wait()
	if pid != 0 {
		// The group can outlive its leader when a wrapper shell spawned a child.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return err
}
