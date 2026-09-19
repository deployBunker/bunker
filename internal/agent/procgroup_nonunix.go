//go:build !unix

package agent

import "os/exec"

// setOwnProcessGroup is a no-op where POSIX process groups do not exist: the
// SysProcAttr field that carries Setpgid is unix-only. bunkerd targets Linux,
// so this build is a portability fallback only.
func setOwnProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the direct child on a platform without POSIX process
// groups. It is deliberately NOT a group kill: there is no negative-pid kill
// here, and signalling a group this process does not own could reach unrelated
// processes. Killing the direct child is exactly the behavior exec.CommandContext
// uses on cancellation.
func killProcessGroup(cmd *exec.Cmd) error { return cmd.Process.Kill() }
