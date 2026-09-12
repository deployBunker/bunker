//go:build unix

package registry

import (
	"fmt"
	"os"
	"syscall"
)

// lockPath returns the advisory lock file guarding the registry.
func (s *Store) lockPath() string { return s.path + ".lock" }

// lockCrossProcess takes an exclusive advisory flock shared by daemon writes
// and `bunker registry compact`, and returns the release function. The lock
// is held only for the duration of a write (or a whole compaction), so a
// long-running daemon and a CLI compaction serialize without either blocking
// on the registry contents itself.
func (s *Store) lockCrossProcess() (func(), error) {
	path := s.lockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("registry: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("registry: flock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
