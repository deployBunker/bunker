//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
)

// tunnelShutdownSignals returns the signals that stop the tunnel. The Go
// runtime on Windows can only deliver console Ctrl-C (os.Interrupt); SIGTERM
// and SIGHUP have no Windows console equivalent.
func tunnelShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// configureTunnelCommand is a no-op on Windows: POSIX process groups have no
// Windows equivalent (SysProcAttr uses job objects there).
func configureTunnelCommand(cmd *exec.Cmd) {}

// terminateTunnelCommand kills the ssh child directly — there is no POSIX
// process group to signal on Windows. It mirrors the unix implementation's
// contract: return nil once the child is gone (or already was).
func terminateTunnelCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
