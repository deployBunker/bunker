//go:build linux || freebsd

package tunnel

import (
	"os/exec"
	"syscall"
)

// configureTunnelPdeathsig arms the kernel's parent-death signal for the
// cloudflared child: if bunkerd disappears without being able to run a single
// line of Go — kill -9, the OOM killer, a manager's hard stop, a crash — the
// kernel SIGKILLs cloudflared, so the tunnel process cannot be reparented to
// init and keep running with no daemon that knows about it (GAP-084, criterion
// 3).
//
// This is the BACKSTOP, not the teardown: a daemon that gets to run code stops
// the child through stopTunnelCommand (group SIGTERM, then SIGKILL). Pdeathsig
// is the only mechanism that covers a SIGKILLed parent.
//
// WHY the pin: the signal is delivered when the THREAD that created the child
// exits, not when the process does, so every child armed here is started
// through childPin.start (see childpin.go), which pins that thread for the
// child's whole life. Arming Pdeathsig without the pin would be a random-kill
// bug, not a backstop.
//
// Pdeathsig exists only on Linux and FreeBSD (the field does not exist in
// syscall.SysProcAttr on darwin/netbsd/openbsd/dragonfly/solaris/illumos —
// cross-compiling those with a bare field is a build failure), which is why
// this assignment is build-tagged; the documented no-op for the other POSIX
// platforms is in process_pdeathsig_unsupported.go.
func configureTunnelPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
