//go:build unix

package tunnel

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func configureTunnelCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopTunnelCommand(cmd *exec.Cmd, cancel context.CancelFunc) error {
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
