//go:build unix && !linux && !freebsd

package cli

import "os/exec"

// configureChildPdeathsig is a documented no-op on the POSIX platforms whose
// syscall.SysProcAttr has no Pdeathsig field (darwin, netbsd, openbsd,
// dragonfly, solaris, illumos).
//
// There is no parent-death equivalent to arm there, so a SIGKILLed CLI on those
// platforms still leaves the child to the signal-driven teardown
// (terminateChildGroup / terminateChildProcess, which need a running process to
// execute) — the GAP-084 backstop is Linux/FreeBSD-only. Bunker's fleet is
// Linux, so no arm is silently missing where it matters; the file exists so the
// POSIX arm keeps ONE call site instead of a build-tag fork in proc_unix.go.
func configureChildPdeathsig(cmd *exec.Cmd) {}
