//go:build linux

package webdav

import (
	"os"
	"syscall"
)

// ctimeUnixNano reports the inode's change time in unix nanoseconds.
//
// It is part of an observed path's identity (events.go) because an edit that
// preserves size AND mtime still moves ctime on Linux: without it, the one edit
// shape this release has already recorded as a defect class (BFS-009 F1, a
// same-size write with the mtime restored) would be invisible to the
// invalidation channel as well as to the read path.
//
// A stat that does not expose the field reports 0 rather than inventing a value:
// an identity of (size, mtime, 0) is still a valid identity, it is simply a
// weaker one, and the caller never has to branch on which platform it is on.
func ctimeUnixNano(fi os.FileInfo) int64 {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
