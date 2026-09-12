//go:build !unix

package registry

import (
	"fmt"
	"os"
)

// lockPath returns the advisory lock file guarding the registry.
func (s *Store) lockPath() string { return s.path + ".lock" }

// lockCrossProcess on non-unix platforms creates the lock file but provides
// no cross-process exclusion: flock(2) does not exist there. bunkerd targets
// Linux, so this build is a portability fallback only — in-process
// serialization (Store.mu) still holds on every platform.
func (s *Store) lockCrossProcess() (func(), error) {
	path := s.lockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("registry: open lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}
