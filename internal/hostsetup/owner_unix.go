//go:build unix

package hostsetup

import (
	"os"
	"syscall"
)

// platformOwner extracts the numeric uid/gid a unix stat structure carries.
//
// ok is false when the FileInfo does not carry this platform's stat structure
// (a synthetic FileInfo, or a stat obtained through a shim), which the caller
// renders as an empty owner string. An empty owner string is never trusted
// (see ownerTrusted), so "ownership unobservable" fails the readiness verdict
// closed instead of passing it.
func platformOwner(fi os.FileInfo) (uid, gid uint32, ok bool) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return sys.Uid, sys.Gid, true
}
