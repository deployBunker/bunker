//go:build unix

package webdav

import (
	"os"
	"syscall"
)

// rootIdentityParts returns the device and inode of the served root, which is
// what makes the E-3 tree token change when a tree is destroyed and re-created
// under the same name (AC-7). A stat failure degrades to the path-only token
// rather than failing the request.
func rootIdentityParts(path string) (dev uint64, ino uint64) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino)
}
