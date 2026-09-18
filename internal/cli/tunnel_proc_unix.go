//go:build unix

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// tunnelShutdownSignals returns the signals that stop the tunnel.
//
// SIGTERM/SIGHUP are included next to Ctrl-C because the CLI is routinely
// started detached (nohup/setsid) and stopped by a manager: the battery's
// "kill $PID" is a SIGTERM, exactly like an init-system stop.
func tunnelShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

// configureTunnelCommand puts the ssh child in its OWN process group so the
// negative-pid kill in terminateTunnelCommand reaches only the processes this
// command started — never the CLI itself nor its shell — and arms the
// parent-death backstop that covers the case nothing in this process can run.
//
// This must be applied BEFORE cmd.Start(); Start() is what installs both, so
// setting SysProcAttr here (immediately after the command is built, before the
// child is started from startTunnelChild) is safe.
func configureTunnelCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Pdeathsig is Linux/FreeBSD-only, so it lives behind its own build tag
	// (tunnel_proc_pdeathsig_supported.go) while this file keeps the POSIX arm.
	configureTunnelPdeathsig(cmd)
}

// terminateTunnelCommand stops the tunnel's whole process group: SIGTERM first
// so the ssh child can close the forward politely, a bounded grace, then
// SIGKILL as the backstop.
//
// WHY a group kill (GAP-079): killing only the direct child leaves anything it
// spawned running. The measured leak was the reverse — the CLI itself was
// killed by a signal, so nothing ran at all and the ssh child was reparented to
// init, keeping its "-L 2376:.../docker.sock" forward and its root sshd session
// alive forever. The ssh child gets its own group (configureTunnelCommand), so
// every process it ever started is inside -pgid.
//
// The negative pid must be the pid of THIS child: the leading '-' is what makes
// the signal a group signal. Pid recycling cannot bite here because the leader
// is not reaped until cmd.Wait runs, which is after this function returns.
func terminateTunnelCommand(cmd *exec.Cmd) error {
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
	deadline := time.Now().Add(tunnelShutdownGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return nil
		}
		time.Sleep(tunnelShutdownPollInterval)
	}

	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
