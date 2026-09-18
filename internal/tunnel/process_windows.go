//go:build windows

package tunnel

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// configureTunnelCommand is a no-op on Windows: POSIX process groups have no
// Windows equivalent (SysProcAttr uses job objects there) and there is no
// Pdeathsig field, so the GAP-084 parent-death backstop does not exist here.
func configureTunnelCommand(cmd *exec.Cmd) {}

// stopTunnelCommand cancels the child's context and reaps it, then releases the
// start pin. Same contract as the POSIX arm: return the child's exit status
// (nil once it is gone or already was).
func stopTunnelCommand(cmd *exec.Cmd, cancel context.CancelFunc, pin *childPin) error {
	defer pin.release()

	cancel()
	err := cmd.Wait()
	if err != nil && errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
