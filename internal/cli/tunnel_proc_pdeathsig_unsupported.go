//go:build unix && !linux && !freebsd

package cli

import "os/exec"

// configureTunnelPdeathsig is a documented no-op on the POSIX platforms whose
// syscall.SysProcAttr has no Pdeathsig field (darwin, netbsd, openbsd,
// dragonfly, solaris, illumos).
//
// There is no parent-death equivalent to arm here, so a SIGKILLed CLI on those
// platforms still leaves the ssh child to the process-group teardown in
// terminateTunnelCommand (which needs a running process to execute) — the
// GAP-079 backstop is Linux/FreeBSD-only. Bunker's fleet is Linux, so no arm
// is silently missing where it matters; the file exists so the POSIX arm keeps
// ONE call site instead of a build-tag fork in tunnel_proc_unix.go.
func configureTunnelPdeathsig(cmd *exec.Cmd) {}
