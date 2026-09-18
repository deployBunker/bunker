//go:build unix && !linux && !freebsd

package tunnel

import "os/exec"

// configureTunnelPdeathsig is a documented no-op on the POSIX platforms whose
// syscall.SysProcAttr has no Pdeathsig field (darwin, netbsd, openbsd,
// dragonfly, solaris, illumos).
//
// There is no parent-death equivalent to arm there, so a SIGKILLed bunkerd on
// those platforms still leaves cloudflared to the signal-driven teardown in
// stopTunnelCommand (which needs a running process to execute) — the GAP-084
// backstop is Linux/FreeBSD-only. Bunker's fleet is Linux, so nothing is
// silently missing where it matters; the file exists so process_unix.go keeps
// ONE call site instead of a build-tag fork.
func configureTunnelPdeathsig(cmd *exec.Cmd) {}
