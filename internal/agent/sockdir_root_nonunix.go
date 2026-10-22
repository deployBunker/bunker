//go:build !linux

package agent

import (
	"errors"
	"fmt"
)

// ISO-002 — non-Linux counterparts of sockdir_root_linux.go. The /run/bunker
// parent's mode/ownership contract is enforced with Stat_t ownership and
// stat-device identity (both Linux-native), so every check here FAILS
// CLOSED with a named error instead of approving or widening anything it
// cannot verify. The hard rule is never widen: an unverifiable boundary is
// a refused boundary (mirrors sockdir_nonunix.go).

// ErrRunRootMode mirrors the linux file's sentinel.
var ErrRunRootMode = errors.New("spawn runtime root (/run/bunker) mode/ownership assertion failed")

// ErrRunRootForeignMount mirrors the linux file's named foreign-mount
// refusal (never reachable here — the check itself fails closed first).
var ErrRunRootForeignMount = errors.New("spawn runtime root is owned by a foreign mount/filesystem (device identity changed around the 0700 repair); refusing to create agent state underneath")

// ErrRunRootUnsupported is the sentinel for the not-implemented-a refusal,
// wrapping ErrRunRootMode like the socket-dir non-Linux stubs do.
var ErrRunRootUnsupported = errors.New("not implemented on this platform")

// EnsureAgentRunRoot fails closed on non-Linux builds (no Stat_t owner, no
// stat-device identity, no honest 0700/owner assertion).
func EnsureAgentRunRoot(root string) error {
	return fmt.Errorf("%w: %w: not implemented on this platform (refusing to create %s without an assertable mode/owner)", ErrRunRootMode, ErrRunRootUnsupported, root)
}

// AssertAgentRunRoot fails closed on non-Linux builds.
func AssertAgentRunRoot(root string) error {
	return fmt.Errorf("%w: %w: not implemented on this platform (%s)", ErrRunRootMode, ErrRunRootUnsupported, root)
}
