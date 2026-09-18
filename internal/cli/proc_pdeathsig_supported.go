//go:build linux || freebsd

package cli

import (
	"os/exec"
	"syscall"
)

// configureChildPdeathsig arms the kernel's parent-death signal for a
// long-lived child: if the CLI disappears without being able to run a single
// line of Go — kill -9, an OOM kill, a manager or cleanup script — the kernel
// SIGKILLs the child, so it cannot be reparented to init and keep whatever it
// holds (GAP-079's ssh forward; GAP-084's transfer, mount attempt and session).
//
// WHY it cannot be caught-but-not-armed: the SIGTERM/SIGHUP path already reaps
// the child through the context cancellation. This is the backstop for the case
// where the parent is GONE and nothing runs: no context, no deferred cancel and
// no reaper can execute.
//
// Pdeathsig exists only on Linux and FreeBSD (the field does not exist in
// syscall.SysProcAttr on darwin/netbsd/openbsd/dragonfly/solaris/illumos —
// cross-compiling those with a bare field is a build failure), which is why
// this assignment is build-tagged instead of sitting in proc_unix.go next to
// Setpgid; the no-op for the other POSIX platforms is in
// proc_pdeathsig_unsupported.go.
//
// The signal is delivered when the THREAD that created the child exits, not
// when the process does, so every child armed here is started through
// startLongLivedChild, which pins that thread for the child's whole life.
func configureChildPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
