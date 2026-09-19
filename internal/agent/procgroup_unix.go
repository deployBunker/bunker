//go:build unix

package agent

import (
	"errors"
	"os/exec"
	"syscall"
)

// setOwnProcessGroup gives the child its own process group, so the
// negative-pid kill below reaches only processes this command started.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup SIGKILLs the command's WHOLE process group (negative pid).
// ESRCH — the group is already gone — is the common, benign case and is not an
// error.
func killProcessGroup(cmd *exec.Cmd) error {
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
