//go:build darwin

package cli

import "golang.org/x/sys/unix"

// attachTCGETS/attachTCSETS are the read/write termios ioctls on Darwin
// (Linux spells them TCGETS/TCSETS; the flags they carry are the same).
const (
	attachTCGETS = unix.TIOCGETA
	attachTCSETS = unix.TIOCSETA
)
