//go:build windows

package tunnel

import (
	"context"
	"os/exec"
)

func configureTunnelCommand(cmd *exec.Cmd) {}

func stopTunnelCommand(cmd *exec.Cmd, cancel context.CancelFunc) error {
	cancel()
	return cmd.Wait()
}
