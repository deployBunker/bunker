package agent

import (
	"errors"
	"strings"
)

// NET-BUNKER-007 — specs/network-isolation.md §6 (The Unix-Socket Half) and
// §7 (abstract-socket trap).
//
// Two properties make a unix socket private, and both must be ASSERTED, not
// assumed (§6.3):
//
//  1. the socket file and its directory carry filesystem permissions that
//     exclude every other tenant — the directory is 0700 and the socket file
//     is not group- or world-accessible. `chown` does NOT set the mode
//     (§6.1), so the mode is set EXPLICITLY here and stat-ed back, never
//     left to the process bunkerd's umask;
//  2. the server on the socket refuses peers whose kernel-reported uid is
//     not the uid the endpoint is owned for (SO_PEERCRED, §6.2) — see
//     CheckUnixSocketPeer in sockdir_linux.go.

// ErrForeignPeerUID is the sentinel wrapped by every foreign-peer refusal.
// uids are not secrets: the refusal text names the owner uid and the peer
// uid so the operator can diagnose exactly who touched what.
var ErrForeignPeerUID = errors.New("unix socket peer credential check failed")

// IsAbstractSocketName reports whether name designates an ABSTRACT unix
// socket address — the Linux namespace with NO file permissions (§7). Both
// spellings count: a leading NUL (the value actually passed to bind()) and
// a leading '@' (the display form; Go's net package maps "@name" to the
// NUL-prefixed abstract address). §7 treats an abstract-socket bind in
// agent-reachable code as a defect, not a style choice.
func IsAbstractSocketName(name string) bool {
	return strings.HasPrefix(name, "\x00") || strings.HasPrefix(name, "@")
}
