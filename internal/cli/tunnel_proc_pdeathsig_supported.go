//go:build linux || freebsd

package cli

import (
	"os/exec"
	"syscall"
)

// configureTunnelPdeathsig arms the kernel's parent-death signal for the ssh
// child: if the CLI disappears without being able to run a single line of Go —
// kill -9, an OOM kill, a manager or cleanup script — the kernel SIGKILLs the
// child, so the "-L 2376:.../docker.sock" forward (and the root sshd session
// behind it) cannot be reparented to init and kept forever (GAP-079).
//
// WHY it cannot be caught-but-not-armed: the SIGTERM/SIGHUP path already reaps
// the group (terminateTunnelCommand). This is the backstop for the case the
// board row describes — the parent is GONE and nothing runs — where no context,
// no deferred cancel and no reaper can execute.
//
// Pdeathsig exists only on Linux and FreeBSD (the field does not exist in
// syscall.SysProcAttr on darwin/netbsd/openbsd/dragonfly/solaris/illumos —
// cross-compiling those with a bare field is a build failure), which is why
// this assignment is build-tagged instead of sitting in tunnel_proc_unix.go
// next to Setpgid; the no-op for the other POSIX platforms is in
// tunnel_proc_pdeathsig_unsupported.go.
//
// The signal is delivered when the THREAD that created the child exits, not
// when the process does, so the child must be started from a thread that is
// pinned for the tunnel's whole life — see startTunnelChild in tunnel.go.
func configureTunnelPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
