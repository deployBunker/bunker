//go:build !unix

package hostsetup

import "os"

// platformOwner reports that this platform exposes no file owner. Windows is
// the motivating case: its stat results carry Win32 attribute data, not a
// POSIX uid/gid, so ownership is UNOBSERVABLE there rather than "root-owned".
//
// Returning ok=false keeps the namespace readiness verdict fail-closed: the
// empty owner string that follows is never trusted (see ownerTrusted), exactly
// as an unobservable owner is treated on unix. bunkerd targets Linux, so this
// build is a portability fallback (the same shape as registry.lockCrossProcess).
func platformOwner(os.FileInfo) (uid, gid uint32, ok bool) { return 0, 0, false }
