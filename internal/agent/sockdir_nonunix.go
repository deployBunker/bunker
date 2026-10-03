//go:build !linux

package agent

import (
	"errors"
	"fmt"
	"net"
)

// NET-BUNKER-007 — non-Linux counterparts of sockdir_linux.go. The spec
// (specs/network-isolation.md §6) is Linux-native: SO_PEERCRED does not
// exist on other kernels and Stat_t ownership checks are unix-only, so every
// check here FAILS CLOSED with a named error instead of approving or
// widening anything it cannot verify. The hard rule is never widen: an
// unverifiable boundary is a refused boundary.

// ErrSocketDirMode mirrors the linux file's sentinel (errors lives here so
// the portable sentinel file stays build-tag free).
var ErrSocketDirMode = errors.New("agent socket directory mode/ownership assertion failed")

// EnsureAgentSocketDir fails closed on non-Linux builds (no Stat_t owner, no
// honest ownership assertion).
func EnsureAgentSocketDir(username, dir string) error {
	return fmt.Errorf("%w: not implemented on this platform (refusing to create %s without an assertable owner)", ErrSocketDirMode, dir)
}

// AssertAgentSocketDir fails closed on non-Linux builds.
func AssertAgentSocketDir(dir string, wantUID uint32) error {
	return fmt.Errorf("%w: not implemented on this platform (%s)", ErrSocketDirMode, dir)
}

// EnsureSocketFileNotGroupWorld fails closed on non-Linux builds.
func EnsureSocketFileNotGroupWorld(sockPath string) error {
	return fmt.Errorf("%w: not implemented on this platform (%s)", ErrSocketDirMode, sockPath)
}

// AssertSocketFileNotGroupWorld fails closed on non-Linux builds.
func AssertSocketFileNotGroupWorld(sockPath string) error {
	return fmt.Errorf("%w: not implemented on this platform (%s)", ErrSocketDirMode, sockPath)
}

// CheckUnixSocketPeer refuses every connection on non-Linux builds: there is
// no kernel mechanism here to identify the peer, so the endpoint-owner check
// cannot be honest, and an invented "approved" would be the manufactured
// bound the spec's reporting law (§5.2) forbids.
func CheckUnixSocketPeer(c net.Conn, ownerUID uint32) error {
	return fmt.Errorf("%w: peer uid verification is not implemented on this platform (expected owner %d); refusing unverified peer", ErrForeignPeerUID, ownerUID)
}

// VerifySocketOwnership refuses to use any socket on non-Linux builds: file
// ownership of the endpoint cannot be validated here, so proceeding would
// trust an unverified control channel.
func VerifySocketOwnership(sockPath string, expectedUID uint32) error {
	return fmt.Errorf("%w: socket ownership verification is not implemented on this platform (%s, expected owner %d); refusing to use it", ErrForeignPeerUID, sockPath, expectedUID)
}

// statOwnerForTests mirrors the linux file's read-back seam.
var statOwnerForTests func(dir string) (uid uint32, ok bool)
