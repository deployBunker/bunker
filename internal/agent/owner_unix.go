//go:build unix

package agent

import (
	"os"
	"syscall"
)

// statOwnerUID returns the numerical owner of a file. ok is false when the
// FileInfo carries no unix stat structure, and the caller reports that as an
// UNKNOWN owner — never as a match (classifyRuntimeDir fails safe on an unknown
// owner).
func statOwnerUID(fi os.FileInfo) (uint32, bool) {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Uid, true
}
