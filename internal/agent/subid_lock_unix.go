//go:build unix

package agent

import (
	"fmt"
	"os"
	"syscall"
)

// lockSubIDFileAttempt takes one advisory flock attempt on path. With
// nonBlocking set the attempt fails immediately (returning syscall.EWOULDBLOCK)
// when another process holds the lock; otherwise it parks in the kernel until
// the lock is free. On success it returns the release function. flock(2) is per
// open file description, so every process that reaches the allocation with an
// independently opened descriptor contends on the same lock — that is what
// serializes concurrent spawns (two concurrent bunkerd requests, or a spawn
// racing a manual usermod) around read-file -> choose-range -> append-entry.
// bunkerd targets Linux; the non-unix build is a portability fallback (see
// subid_lock_nonunix.go).
func lockSubIDFileAttempt(path string, nonBlocking bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	how := syscall.LOCK_EX
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
