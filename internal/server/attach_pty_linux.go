//go:build linux

package server

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openAttachPTY allocates a pseudo-terminal pair for an attach session.
//
// The recipe is the kernel's own (and the same one github.com/creack/pty
// uses): open /dev/ptmx, ask it for the slave's number (TIOCGPTN), unlock it
// (TIOCSPTLCK with a zero), then open the slave by path. The ioctls go through
// golang.org/x/sys/unix, already in the module graph, so the termios/pty ABI
// values are the library's rather than hand-copied constants.
func openAttachPTY(cols, rows uint32) (*attachPTY, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	// A zero written through TIOCSPTLCK clears the lock installed by opening
	// /dev/ptmx; until then the slave cannot be opened.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("open pty slave: %w", err)
	}
	p := &attachPTY{master: master, slave: slave}
	if cols > 0 && rows > 0 {
		// A failing initial size must not fail the session: the default
		// terminal geometry still works, and the client can resize later.
		_ = p.resize(cols, rows)
	}
	return p, nil
}

// resize applies a window-change. Setting it on the MASTER is what makes the
// kernel deliver SIGWINCH to the child in the slave's session - which is how
// ssh learns to forward an SSH window-change to the remote sshd, and how vi
// redraws.
func (p *attachPTY) resize(cols, rows uint32) error {
	if p.master == nil {
		return nil
	}
	ws := &unix.Winsize{Row: uint16(rows), Col: uint16(cols)}
	return unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, ws)
}

// close releases both ends. The slave is normally closed right after the child
// starts (the child owns its copy); closing it again is a no-op.
func (p *attachPTY) close() {
	if p.master != nil {
		_ = p.master.Close()
		p.master = nil
	}
	if p.slave != nil {
		_ = p.slave.Close()
		p.slave = nil
	}
}

// attachTTYSysProcAttr is the child's process posture for a PTY session: its
// own session, with the slave becoming its controlling terminal (Ctty 0 is
// correct because the slave is the child's fd 0 via cmd.Stdin). A session
// leader is also its own process group, which is what lets teardown signal the
// whole session.
func attachTTYSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true}
}

// attachPipeSysProcAttr is the child's posture on the non-PTY path: its own
// process group (no controlling terminal, so no Setsid/Setctty).
func attachPipeSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// attachSignalChildGroup asks the child's whole process group to terminate.
// Both start paths give the child its own group, so -pid is exactly that
// session and nothing of ours.
func attachSignalChildGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = syscall.Kill(-proc.Pid, syscall.SIGTERM)
	_ = proc.Signal(syscall.SIGTERM)
}

// attachKillChildGroup is the backstop: the whole group, SIGKILL.
func attachKillChildGroup(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = syscall.Kill(-proc.Pid, syscall.SIGKILL)
	_ = proc.Kill()
}
