//go:build !linux

package server

import (
	"errors"
	"os"
	"syscall"
)

// errAttachPTYUnsupported is returned by the non-Linux PTY implementation. It
// is not fatal to an attach: startAttachProcess falls back to the pipe path, so
// an attach on a platform without the Linux pty ioctls is still a working
// session - just one without a terminal.
var errAttachPTYUnsupported = errors.New("attach: pseudo-terminal allocation is not supported on this platform")

// openAttachPTY is unavailable off Linux.
func openAttachPTY(cols, rows uint32) (*attachPTY, error) {
	return nil, errAttachPTYUnsupported
}

// resize is a no-op: there is no PTY to resize. It returns the unsupported
// sentinel rather than nil so a caller cannot mistake "no terminal" for a
// successful window-change.
func (p *attachPTY) resize(cols, rows uint32) error {
	return errAttachPTYUnsupported
}

// close is a no-op: off Linux there is never a PTY to release.
func (p *attachPTY) close() {}

// attachTTYSysProcAttr is the plain (no controlling-terminal) posture on
// platforms where this build cannot allocate a PTY, and the child still gets
// its own process group so teardown can signal the whole session.
func attachTTYSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attachPipeSysProcAttr is the child's posture on the non-PTY path.
func attachPipeSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attachSignalChildGroup asks the child to terminate. Off Linux this build has
// no portable process-group signal, so only the child itself is asked.
func attachSignalChildGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
}

// attachKillChildGroup is the backstop: the child itself.
func attachKillChildGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}
