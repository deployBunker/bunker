//go:build linux || darwin

package cli

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// attachIsTerminal reports whether f is a terminal. The termios read is the
// terminal test: it succeeds only on a tty.
func attachIsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), attachTCGETS)
	return err == nil
}

// attachMakeRaw puts the terminal into raw mode and returns the restore
// function. This is the same transformation golang.org/x/term.MakeRaw makes:
// no signal generation (so Ctrl-C/Ctrl-D/Ctrl-Z reach the remote as bytes), no
// echo, no line buffering, no output post-processing. The restore is
// idempotent and must be called before printing anything.
func attachMakeRaw(f *os.File) (func(), error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, attachTCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, attachTCSETS, &raw); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.IoctlSetTermios(fd, attachTCSETS, old)
		})
	}, nil
}

// attachTerminalSize reads the terminal's window size. ok is false when the
// size cannot be read (not a terminal, or a size of zero), in which case the
// daemon's default geometry is used.
func attachTerminalSize(f *os.File) (cols, rows uint32, ok bool) {
	if f == nil {
		return 0, 0, false
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return 0, 0, false
	}
	return uint32(ws.Col), uint32(ws.Row), true
}

// attachResizeSignal is the signal the terminal sends when its window changes.
func attachResizeSignal() os.Signal { return unix.SIGWINCH }
