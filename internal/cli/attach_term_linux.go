//go:build linux

package cli

import "golang.org/x/sys/unix"

// attachTCGETS/attachTCSETS are the read/write termios ioctls on Linux.
const (
	attachTCGETS = unix.TCGETS
	attachTCSETS = unix.TCSETS
)
